package crmapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dzebovski/kolss-platform-api/internal/deepl"
)

type Translator interface {
	Translate(ctx context.Context, text, sourceLanguage, targetLanguage string) (string, error)
}

type eventTranslationResponse struct {
	Translation    string    `json:"translation"`
	SourceLanguage string    `json:"sourceLanguage"`
	TranslatedAt   time.Time `json:"translatedAt"`
}

func sourceLanguageForOffice(officeCode string) (string, bool) {
	switch officeCode {
	case "kyiv":
		return "UK", true
	case "warsaw":
		return "PL", true
	default:
		return "", false
	}
}

func (s *Server) handleTranslateEvent(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	leadID, leadErr := uuid.Parse(r.PathValue("leadId"))
	eventID, eventErr := uuid.Parse(r.PathValue("eventId"))
	if leadErr != nil || eventErr != nil || !requireIdempotencyKey(s, w, r) {
		if leadErr != nil || eventErr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid timeline event", nil)
		}
		return
	}

	var comment *string
	var existingTranslation *string
	var existingSourceLanguage *string
	var existingTranslatedAt *time.Time
	var officeID uuid.UUID
	var officeCode string
	var archivedAt *time.Time
	var eventCategory *string
	err := s.pool.QueryRow(r.Context(), `
		select e.comment, e.comment_translation_en, e.comment_translation_source_lang,
		       e.comment_translated_at, l.office_id, o.code, l.archived_at, e.event_category
		from public.lead_events e
		join public.leads l on l.id=e.lead_id
		join public.offices o on o.id=l.office_id
		where e.id=$1 and e.lead_id=$2
	`, eventID, leadID).Scan(
		&comment,
		&existingTranslation,
		&existingSourceLanguage,
		&existingTranslatedAt,
		&officeID,
		&officeCode,
		&archivedAt,
		&eventCategory,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "event_not_found", "Timeline event not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "translation_load_failed", "Could not load timeline event", nil)
		return
	}
	if !actor.CanAccessOffice(officeID) {
		s.writeError(w, r, http.StatusForbidden, "office_forbidden", "Office access denied", nil)
		return
	}
	if archivedAt != nil {
		s.writeError(w, r, http.StatusConflict, "lead_archived", "Archived leads are read-only", nil)
		return
	}
	if eventCategory != nil && *eventCategory == activityQuestion {
		s.writeError(w, r, http.StatusUnprocessableEntity, "translation_event_unsupported", "Question translations use the question translation flow", nil)
		return
	}
	if existingTranslation != nil && strings.TrimSpace(*existingTranslation) != "" && existingSourceLanguage != nil && existingTranslatedAt != nil {
		writeJSON(w, http.StatusOK, eventTranslationResponse{
			Translation:    *existingTranslation,
			SourceLanguage: *existingSourceLanguage,
			TranslatedAt:   *existingTranslatedAt,
		})
		return
	}
	if comment == nil || strings.TrimSpace(*comment) == "" {
		s.writeError(w, r, http.StatusUnprocessableEntity, "comment_missing", "Timeline event has no comment to translate", nil)
		return
	}
	sourceLanguage, ok := sourceLanguageForOffice(officeCode)
	if !ok {
		s.writeError(w, r, http.StatusUnprocessableEntity, "translation_language_unsupported", "Office language is not supported", nil)
		return
	}
	if s.translator == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "translation_not_configured", "Translation service is not configured", nil)
		return
	}

	originalComment := *comment
	translated, err := s.translator.Translate(r.Context(), originalComment, sourceLanguage, "EN-GB")
	if err != nil {
		var apiErr *deepl.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode == 456) {
			s.writeError(w, r, http.StatusTooManyRequests, "translation_rate_limited", "Translation quota is temporarily unavailable", nil)
			return
		}
		s.logger.Warn("DeepL translation failed", "error", err, "request_id", requestID(r.Context()), "event_id", eventID)
		s.writeError(w, r, http.StatusServiceUnavailable, "translation_unavailable", "Translation service is temporarily unavailable", nil)
		return
	}

	var saved eventTranslationResponse
	err = s.pool.QueryRow(r.Context(), `
		update public.lead_events
		set comment_translation_en=$3,
		    comment_translation_source_lang=$4,
		    comment_translated_at=now()
		where id=$1 and lead_id=$2
		  and comment is not distinct from $5
		  and comment_translation_en is null
		  and event_category is distinct from 'question'
		  and exists (select 1 from public.leads l where l.id=lead_id and l.archived_at is null)
		returning comment_translation_en, comment_translation_source_lang, comment_translated_at
	`, eventID, leadID, translated, sourceLanguage, originalComment).Scan(
		&saved.Translation,
		&saved.SourceLanguage,
		&saved.TranslatedAt,
	)
	if err == nil {
		writeJSON(w, http.StatusOK, saved)
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusInternalServerError, "translation_save_failed", "Could not save translation", nil)
		return
	}

	var currentComment *string
	var currentTranslation *string
	var currentSourceLanguage *string
	var currentTranslatedAt *time.Time
	err = s.pool.QueryRow(r.Context(), `
		select comment, comment_translation_en, comment_translation_source_lang, comment_translated_at
		from public.lead_events
		where id=$1 and lead_id=$2
	`, eventID, leadID).Scan(
		&currentComment,
		&currentTranslation,
		&currentSourceLanguage,
		&currentTranslatedAt,
	)
	if err == nil && currentTranslation != nil && strings.TrimSpace(*currentTranslation) != "" && currentSourceLanguage != nil && currentTranslatedAt != nil {
		writeJSON(w, http.StatusOK, eventTranslationResponse{
			Translation:    *currentTranslation,
			SourceLanguage: *currentSourceLanguage,
			TranslatedAt:   *currentTranslatedAt,
		})
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "event_not_found", "Timeline event not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "translation_save_failed", "Could not save translation", nil)
		return
	}
	if currentComment == nil || *currentComment != originalComment {
		s.writeError(w, r, http.StatusConflict, "comment_changed", "Comment changed while it was being translated", nil)
		return
	}
	s.writeError(w, r, http.StatusConflict, "translation_conflict", "Translation could not be saved", nil)
}

type textTranslationRequest struct {
	Text           string `json:"text"`
	SourceLanguage string `json:"sourceLanguage"`
	TargetLanguage string `json:"targetLanguage"`
}

type textTranslationResponse struct {
	Translation    string `json:"translation"`
	SourceLanguage string `json:"sourceLanguage"`
	TargetLanguage string `json:"targetLanguage"`
}

func normalizeTargetLanguage(lang string) string {
	clean := strings.ToUpper(strings.TrimSpace(lang))
	if clean == "EN" {
		return "EN-GB"
	}
	return clean
}

func normalizeSourceLanguage(lang string) string {
	return strings.ToUpper(strings.TrimSpace(lang))
}

func (s *Server) handleTranslateText(w http.ResponseWriter, r *http.Request) {
	_, ok := actorFromContext(r.Context())
	if !ok {
		s.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Unauthorized", nil)
		return
	}

	var req textTranslationRequest
	if decodeJSON(w, r, 32*1024, &req) != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid request body", nil)
		return
	}

	text := strings.TrimSpace(req.Text)
	if text == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Text is required", map[string]string{"text": "Text cannot be empty"})
		return
	}

	targetAPI := strings.ToUpper(strings.TrimSpace(req.TargetLanguage))
	if targetAPI != "UK" && targetAPI != "PL" && targetAPI != "EN" {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Target language must be UK, PL, or EN", map[string]string{"targetLanguage": "Must be UK, PL, or EN"})
		return
	}
	targetLang := normalizeTargetLanguage(targetAPI)

	sourceLang := normalizeSourceLanguage(req.SourceLanguage)
	if sourceLang != "" && sourceLang != "UK" && sourceLang != "PL" && sourceLang != "EN" {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Source language must be UK, PL, or EN", map[string]string{"sourceLanguage": "Must be UK, PL, or EN"})
		return
	}
	if sourceLang != "" && sourceLang == targetAPI {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Source and target language must differ", map[string]string{"targetLanguage": "Must differ from sourceLanguage"})
		return
	}
	if s.translator == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "translation_not_configured", "Translation service is not configured", nil)
		return
	}

	translated, err := s.translator.Translate(r.Context(), text, sourceLang, targetLang)
	if err != nil {
		var apiErr *deepl.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode == 456) {
			s.writeError(w, r, http.StatusTooManyRequests, "translation_rate_limited", "Translation quota is temporarily unavailable", nil)
			return
		}
		s.logger.Warn("DeepL translation failed", "error", err, "request_id", requestID(r.Context()))
		s.writeError(w, r, http.StatusServiceUnavailable, "translation_unavailable", "Translation service is temporarily unavailable", nil)
		return
	}

	writeJSON(w, http.StatusOK, textTranslationResponse{
		Translation:    translated,
		SourceLanguage: sourceLang,
		TargetLanguage: targetAPI,
	})
}
