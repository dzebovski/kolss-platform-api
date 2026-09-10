package crmapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dzebovski/kolss-platform-api/internal/deepl"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type answerQuestionRequest struct {
	Answer string `json:"answer"`
}
type answerTranslationRequest struct {
	TargetLanguage string `json:"targetLanguage"`
}

type answerQuestionResponse struct {
	OK      bool           `json:"ok"`
	EventID uuid.UUID      `json:"eventId"`
	LeadID  uuid.UUID      `json:"leadId"`
	Answer  map[string]any `json:"answer"`
}

func questionAnswer(value map[string]any) (map[string]any, bool) {
	question, ok := value["question"].(map[string]any)
	if !ok {
		return nil, false
	}
	answer, ok := question["answer"].(map[string]any)
	return answer, ok
}

// handleUpdateLeadQuestion is reached only after the ordinary history-event
// author/super-admin authorization. Editing the base text intentionally clears
// stale translations; assignee snapshots are revalidated against the office.
func (s *Server) handleUpdateLeadQuestion(w http.ResponseWriter, r *http.Request, actor Actor, leadID, eventID uuid.UUID, req eventUpdateRequest) {
	text := strings.TrimSpace(req.Comment)
	if text == "" {
		s.writeError(w, r, 400, "validation_error", "Question is required", map[string]string{"comment": "Required"})
		return
	}
	translations, fields := normalizeQuestionTranslations(req.Translations)
	if len(fields) > 0 {
		s.writeError(w, r, 400, "validation_error", "Invalid question", fields)
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, 500, "event_update_failed", "Could not update question", nil)
		return
	}
	defer tx.Rollback(r.Context())
	var officeID uuid.UUID
	var existingComment *string
	var raw json.RawMessage
	err = tx.QueryRow(r.Context(), `select l.office_id,e.comment,e.new_value from public.lead_events e join public.leads l on l.id=e.lead_id where e.id=$1 and e.lead_id=$2 and e.event_type='question' and e.event_category='question' and e.status_code='status-question' and l.archived_at is null for update`, eventID, leadID).Scan(&officeID, &existingComment, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, 404, "event_not_found", "Question event not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, 500, "event_update_failed", "Could not load question", nil)
		return
	}
	value := map[string]any{}
	_ = json.Unmarshal(raw, &value)
	question, ok := value["question"].(map[string]any)
	if !ok {
		s.writeError(w, r, 500, "event_update_failed", "Question payload is invalid", nil)
		return
	}
	if req.AssigneeIDs != nil {
		snapshots, err := loadQuestionAssignees(r, tx, *req.AssigneeIDs, officeID)
		if errors.Is(err, errCommentAssigneeInvalid) {
			s.writeError(w, r, 400, "validation_error", "Invalid question", map[string]string{"assigneeIds": "All assignees must be active office members"})
			return
		}
		if err != nil {
			s.writeError(w, r, 500, "event_update_failed", "Could not validate assignees", nil)
			return
		}
		question["assignees"] = snapshots
	}
	if req.Translations != nil {
		question["translations"] = translations
	} else if existingComment == nil || *existingComment != text {
		question["translations"] = map[string]string{}
	}
	value["question"] = question
	editedByName := ""
	if actor.DisplayName != nil {
		editedByName = strings.TrimSpace(*actor.DisplayName)
	}
	value["edit_audit"] = map[string]any{
		"fields":         []string{"message"},
		"edited_at":      time.Now().UTC().Format(time.RFC3339Nano),
		"edited_by":      actor.ID.String(),
		"edited_by_name": editedByName,
	}
	payload, _ := json.Marshal(value)
	_, err = tx.Exec(r.Context(), `update public.lead_events set comment=$3,new_value=$4::jsonb where id=$1 and lead_id=$2`, eventID, leadID, text, payload)
	if err != nil {
		s.writeError(w, r, 500, "event_update_failed", "Could not update question", nil)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		s.writeError(w, r, 500, "event_update_failed", "Could not update question", nil)
		return
	}
	writeJSON(w, 200, map[string]any{"changedFields": []string{"message", "assignees"}})
}

func (s *Server) handleAnswerLeadQuestion(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFromContext(r.Context())
	if !ok {
		s.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Unauthorized", nil)
		return
	}
	leadID, leadErr := uuid.Parse(r.PathValue("leadId"))
	eventID, eventErr := uuid.Parse(r.PathValue("eventId"))
	if leadErr != nil || eventErr != nil || !requireIdempotencyKey(s, w, r) {
		if leadErr != nil || eventErr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid path parameters", nil)
		}
		return
	}
	var req answerQuestionRequest
	if decodeJSON(w, r, 32*1024, &req) != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid request body", nil)
		return
	}
	text := strings.TrimSpace(req.Answer)
	if text == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Answer is required", map[string]string{"answer": "Answer cannot be empty"})
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	hash := sha256.Sum256([]byte(leadID.String() + "|" + eventID.String() + "|" + text))
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, 500, "answer_failed", "Could not save question answer", nil)
		return
	}
	defer tx.Rollback(r.Context())
	cached, status, cachedBody, err := claimIdempotency(r, tx, actor.ID, "lead.question.answer", key, hex.EncodeToString(hash[:]))
	if err != nil {
		s.writeError(w, r, http.StatusConflict, "idempotency_conflict", "Idempotency key was already used for another request", nil)
		return
	}
	if cached {
		if err := tx.Commit(r.Context()); err != nil {
			s.writeError(w, r, 500, "answer_failed", "Could not save question answer", nil)
			return
		}
		writeJSON(w, status, cachedBody)
		return
	}
	var officeID uuid.UUID
	var archivedAt *time.Time
	var eventType string
	var eventCategory *string
	var statusCode *string
	var raw json.RawMessage
	err = tx.QueryRow(r.Context(), `select l.office_id,l.archived_at,e.event_type,e.event_category,e.status_code,e.new_value from public.lead_events e join public.leads l on l.id=e.lead_id where e.id=$1 and e.lead_id=$2 for update`, eventID, leadID).Scan(&officeID, &archivedAt, &eventType, &eventCategory, &statusCode, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, 404, "event_not_found", "Question event not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, 500, "answer_failed", "Could not load question event", nil)
		return
	}
	if archivedAt != nil {
		s.writeError(w, r, http.StatusConflict, "lead_archived", "Archived leads are read-only", nil)
		return
	}
	if !actor.CanAccessOffice(officeID) {
		s.writeError(w, r, 403, "office_forbidden", "Office access denied", nil)
		return
	}
	if eventType != activityQuestion || eventCategory == nil || *eventCategory != activityQuestion || statusCode == nil || *statusCode != "status-question" {
		s.writeError(w, r, 400, "invalid_event_type", "Event is not a question", nil)
		return
	}
	value := map[string]any{}
	if err := json.Unmarshal(raw, &value); err != nil {
		s.writeError(w, r, 500, "answer_failed", "Question payload is invalid", nil)
		return
	}
	question, questionOK := value["question"].(map[string]any)
	if !questionOK || question["status"] != "pending" {
		s.writeError(w, r, http.StatusConflict, "question_already_answered", "Question is not pending", nil)
		return
	}
	if _, exists := questionAnswer(value); exists {
		s.writeError(w, r, http.StatusConflict, "question_already_answered", "Question already has an answer", nil)
		return
	}
	name := ""
	if actor.DisplayName != nil {
		name = strings.TrimSpace(*actor.DisplayName)
	}
	now := time.Now().UTC()
	answer := map[string]any{"text": text, "actor_id": actor.ID.String(), "actor_name": name, "answered_at": now.Format(time.RFC3339Nano), "translations": map[string]string{}}
	question["status"] = "answered"
	question["answer"] = answer
	value["question"] = question
	payload, _ := json.Marshal(value)
	if _, err = tx.Exec(r.Context(), `update public.lead_events set new_value=$3::jsonb where id=$1 and lead_id=$2`, eventID, leadID, payload); err != nil {
		s.writeError(w, r, 500, "answer_failed", "Could not save question answer", nil)
		return
	}
	response := answerQuestionResponse{OK: true, EventID: eventID, LeadID: leadID, Answer: answer}
	body, _ := json.Marshal(response)
	if _, err = tx.Exec(r.Context(), `update public.api_idempotency_keys set response_status=200,response_body=$4::jsonb where actor_id=$1 and operation=$2 and idempotency_key=$3`, actor.ID, "lead.question.answer", key, body); err != nil {
		s.writeError(w, r, 500, "answer_failed", "Could not save question answer", nil)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		s.writeError(w, r, 500, "answer_failed", "Could not save question answer", nil)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleUpdateLeadQuestionAnswer(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFromContext(r.Context())
	if !ok {
		s.writeError(w, r, 401, "unauthorized", "Unauthorized", nil)
		return
	}
	leadID, e1 := uuid.Parse(r.PathValue("leadId"))
	eventID, e2 := uuid.Parse(r.PathValue("eventId"))
	var req answerQuestionRequest
	if e1 != nil || e2 != nil || decodeJSON(w, r, 32*1024, &req) != nil {
		s.writeError(w, r, 400, "validation_error", "Invalid answer", nil)
		return
	}
	text := strings.TrimSpace(req.Answer)
	if text == "" {
		s.writeError(w, r, 400, "validation_error", "Answer is required", map[string]string{"answer": "Answer cannot be empty"})
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, 500, "answer_failed", "Could not update answer", nil)
		return
	}
	defer tx.Rollback(r.Context())
	var officeID uuid.UUID
	var eventType string
	var raw json.RawMessage
	err = tx.QueryRow(r.Context(), `select l.office_id,e.event_type,e.new_value from public.lead_events e join public.leads l on l.id=e.lead_id where e.id=$1 and e.lead_id=$2 and e.event_category='question' and e.status_code='status-question' and l.archived_at is null for update`, eventID, leadID).Scan(&officeID, &eventType, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, 404, "event_not_found", "Question event not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, 500, "answer_failed", "Could not load answer", nil)
		return
	}
	if eventType != activityQuestion {
		s.writeError(w, r, 400, "invalid_event_type", "Event is not a question", nil)
		return
	}
	if !actor.CanAccessOffice(officeID) {
		s.writeError(w, r, 403, "office_forbidden", "Office access denied", nil)
		return
	}
	value := map[string]any{}
	_ = json.Unmarshal(raw, &value)
	answer, exists := questionAnswer(value)
	if !exists {
		s.writeError(w, r, 409, "answer_missing", "Question has no answer", nil)
		return
	}
	author, _ := answer["actor_id"].(string)
	if !actor.IsSuperAdmin() && author != actor.ID.String() {
		s.writeError(w, r, 403, "answer_mutate_forbidden", "Only the answer author can edit it", nil)
		return
	}
	answer["text"] = text
	answer["translations"] = map[string]string{}
	payload, _ := json.Marshal(value)
	if _, err = tx.Exec(r.Context(), `update public.lead_events set new_value=$3::jsonb where id=$1 and lead_id=$2`, eventID, leadID, payload); err != nil {
		s.writeError(w, r, 500, "answer_failed", "Could not update answer", nil)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		s.writeError(w, r, 500, "answer_failed", "Could not update answer", nil)
		return
	}
	writeJSON(w, 200, map[string]any{"changedFields": []string{"answer"}})
}

func (s *Server) handleTranslateLeadQuestionAnswer(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFromContext(r.Context())
	if !ok {
		s.writeError(w, r, 401, "unauthorized", "Unauthorized", nil)
		return
	}
	leadID, e1 := uuid.Parse(r.PathValue("leadId"))
	eventID, e2 := uuid.Parse(r.PathValue("eventId"))
	var req answerTranslationRequest
	if e1 != nil || e2 != nil || decodeJSON(w, r, 16*1024, &req) != nil {
		s.writeError(w, r, 400, "validation_error", "Invalid translation request", nil)
		return
	}
	target := strings.ToUpper(strings.TrimSpace(req.TargetLanguage))
	if target != "UK" && target != "PL" && target != "EN" {
		s.writeError(w, r, 400, "validation_error", "Target language must be UK, PL, or EN", map[string]string{"targetLanguage": "Must be UK, PL, or EN"})
		return
	}
	if s.translator == nil {
		s.writeError(w, r, 503, "translation_not_configured", "Translation service is not configured", nil)
		return
	}
	var officeID uuid.UUID
	var raw json.RawMessage
	err := s.pool.QueryRow(r.Context(), `select l.office_id,e.new_value from public.lead_events e join public.leads l on l.id=e.lead_id where e.id=$1 and e.lead_id=$2 and e.event_type='question' and e.event_category='question' and e.status_code='status-question' and l.archived_at is null`, eventID, leadID).Scan(&officeID, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, 404, "event_not_found", "Question event not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, 500, "translation_load_failed", "Could not load answer", nil)
		return
	}
	if !actor.CanAccessOffice(officeID) {
		s.writeError(w, r, 403, "office_forbidden", "Office access denied", nil)
		return
	}
	value := map[string]any{}
	_ = json.Unmarshal(raw, &value)
	answer, exists := questionAnswer(value)
	if !exists {
		s.writeError(w, r, 409, "answer_missing", "Question has no answer", nil)
		return
	}
	original, _ := answer["text"].(string)
	if strings.TrimSpace(original) == "" {
		s.writeError(w, r, 422, "answer_missing", "Question has no answer", nil)
		return
	}
	translated, err := s.translator.Translate(r.Context(), original, "", normalizeTargetLanguage(target))
	if err != nil {
		writeQuestionTranslationError(s, w, r, err, eventID)
		return
	}
	command, err := s.pool.Exec(r.Context(), `update public.lead_events e set new_value=jsonb_set(coalesce(e.new_value,'{}'::jsonb),'{question,answer,translations}',coalesce(e.new_value#>'{question,answer,translations}','{}'::jsonb) || jsonb_build_object($3,$4::text),true) where e.id=$1 and e.lead_id=$2 and e.event_type='question' and e.event_category='question' and e.status_code='status-question' and e.new_value#>>'{question,answer,text}'=$5 and exists (select 1 from public.leads l where l.id=e.lead_id and l.archived_at is null)`, eventID, leadID, target, translated, original)
	if err != nil {
		s.writeError(w, r, 500, "translation_save_failed", "Could not save answer translation", nil)
		return
	}
	if command.RowsAffected() == 0 {
		s.writeError(w, r, 409, "answer_changed", "Answer changed while it was being translated", nil)
		return
	}
	writeJSON(w, 200, map[string]string{"translation": translated, "targetLanguage": target})
}

func writeQuestionTranslationError(s *Server, w http.ResponseWriter, r *http.Request, err error, eventID uuid.UUID) {
	var apiErr *deepl.APIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == 429 || apiErr.StatusCode == 456) {
		s.writeError(w, r, 429, "translation_rate_limited", "Translation quota is temporarily unavailable", nil)
		return
	}
	s.logger.Warn("DeepL translation failed", "error", err, "request_id", requestID(r.Context()), "event_id", eventID)
	s.writeError(w, r, 503, "translation_unavailable", "Translation service is temporarily unavailable", nil)
}
