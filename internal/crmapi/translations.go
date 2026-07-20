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
	err := s.pool.QueryRow(r.Context(), `
		select e.comment, e.comment_translation_en, e.comment_translation_source_lang,
		       e.comment_translated_at, l.office_id, o.code
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
