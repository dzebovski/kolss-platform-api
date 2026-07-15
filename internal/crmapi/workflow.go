package crmapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type actionRequest struct {
	Result         string   `json:"result"`
	Comment        string   `json:"comment"`
	ScheduledAt    string   `json:"scheduledAt"`
	Reason         string   `json:"reason"`
	ContractNumber string   `json:"contractNumber"`
	Amount         *float64 `json:"amount"`
	Prepayment     *float64 `json:"prepayment"`
}

type lockedLead struct {
	OfficeID       uuid.UUID
	AssignedTo     *uuid.UUID
	LeadStatus     string
	Workflow       string
	ArchivedAt     *time.Time
	CurrentVersion int64
}

func (s *Server) handleLeadAction(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	leadID, err := uuid.Parse(r.PathValue("leadId"))
	action := strings.TrimSpace(r.PathValue("action"))
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if err != nil || action == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid lead action", nil)
		return
	}
	if key == "" {
		s.writeError(w, r, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required", nil)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid action body", nil)
		return
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		body = []byte(`{}`)
	}
	var req actionRequest
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid action body", nil)
		return
	}
	if fields := validateAction(action, req); len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Action validation failed", fields)
		return
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "action_failed", "Could not update lead", nil)
		return
	}
	defer tx.Rollback(r.Context())
	hashBytes := sha256.Sum256(append([]byte(action+"|"+leadID.String()+"|"), body...))
	cached, status, cachedBody, err := claimIdempotency(r, tx, actor.ID, "lead.action."+action, key, hex.EncodeToString(hashBytes[:]))
	if err != nil {
		s.writeError(w, r, http.StatusConflict, "idempotency_conflict", "Idempotency key was already used for another request", nil)
		return
	}
	if cached {
		if err := tx.Commit(r.Context()); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "action_failed", "Could not update lead", nil)
			return
		}
		if status == 0 {
			status = http.StatusOK
		}
		writeJSON(w, status, cachedBody)
		return
	}

	var lead lockedLead
	err = tx.QueryRow(r.Context(), `
		select office_id, assigned_to, lead_status, workflow_status, archived_at, version
		from public.leads where id=$1 for update
	`, leadID).Scan(&lead.OfficeID, &lead.AssignedTo, &lead.LeadStatus, &lead.Workflow, &lead.ArchivedAt, &lead.CurrentVersion)
	if errors.Is(err, pgx.ErrNoRows) || lead.ArchivedAt != nil {
		s.writeError(w, r, http.StatusNotFound, "lead_not_found", "Lead not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "action_failed", "Could not update lead", nil)
		return
	}
	if !actor.CanAccessOffice(lead.OfficeID) {
		s.writeError(w, r, http.StatusForbidden, "office_forbidden", "Office access denied", nil)
		return
	}
	if action == "take" && lead.AssignedTo != nil && *lead.AssignedTo != actor.ID {
		s.writeError(w, r, http.StatusConflict, "lead_already_taken", "Lead is already assigned to another manager", nil)
		return
	}
	if action == "take" && lead.AssignedTo != nil && *lead.AssignedTo == actor.ID {
		response := map[string]any{"ok": true, "version": lead.CurrentVersion}
		rawResponse, _ := json.Marshal(response)
		if _, err := tx.Exec(r.Context(), `
			update public.api_idempotency_keys set response_status=$4,response_body=$5::jsonb
			where actor_id=$1 and operation=$2 and idempotency_key=$3
		`, actor.ID, "lead.action."+action, key, http.StatusOK, rawResponse); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "action_failed", "Could not update lead", nil)
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "action_failed", "Could not update lead", nil)
			return
		}
		writeJSON(w, http.StatusOK, response)
		return
	}
	if action == "mark-thinking" && (lead.Workflow == "closed" || lead.Workflow == "successful") {
		s.writeError(w, r, http.StatusConflict, "lead_terminal", "Terminal leads cannot be marked as thinking", nil)
		return
	}
	if action == "mark-thinking" && lead.Workflow == "thinking" {
		response := map[string]any{"ok": true, "version": lead.CurrentVersion}
		rawResponse, _ := json.Marshal(response)
		if _, err := tx.Exec(r.Context(), `
			update public.api_idempotency_keys set response_status=$4,response_body=$5::jsonb
			where actor_id=$1 and operation=$2 and idempotency_key=$3
		`, actor.ID, "lead.action."+action, key, http.StatusOK, rawResponse); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "action_failed", "Could not update lead", nil)
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "action_failed", "Could not update lead", nil)
			return
		}
		writeJSON(w, http.StatusOK, response)
		return
	}

	err = s.applyLeadAction(r, tx, actor, leadID, lead, action, req)
	if err != nil {
		if errors.Is(err, errLossReasonNotFound) {
			s.writeError(w, r, http.StatusBadRequest, "invalid_loss_reason", "Unknown loss reason", map[string]string{"reason": "Unknown loss reason"})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "action_failed", "Could not update lead", nil)
		return
	}
	response := map[string]any{"ok": true, "version": lead.CurrentVersion + 1}
	rawResponse, _ := json.Marshal(response)
	if _, err := tx.Exec(r.Context(), `
		update public.api_idempotency_keys
		set response_status=$4, response_body=$5::jsonb
		where actor_id=$1 and operation=$2 and idempotency_key=$3
	`, actor.ID, "lead.action."+action, key, http.StatusOK, rawResponse); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "action_failed", "Could not update lead", nil)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "action_failed", "Could not update lead", nil)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func claimIdempotency(r *http.Request, tx pgx.Tx, actorID uuid.UUID, operation, key, requestHash string) (bool, int, json.RawMessage, error) {
	result, err := tx.Exec(r.Context(), `
		insert into public.api_idempotency_keys (actor_id,operation,idempotency_key,request_hash)
		values ($1,$2,$3,$4) on conflict (actor_id,operation,idempotency_key) do nothing
	`, actorID, operation, key, requestHash)
	if err != nil {
		return false, 0, nil, err
	}
	if result.RowsAffected() == 1 {
		return false, 0, nil, nil
	}
	var existingHash string
	var status *int
	var body []byte
	if err := tx.QueryRow(r.Context(), `
		select request_hash,response_status,response_body
		from public.api_idempotency_keys
		where actor_id=$1 and operation=$2 and idempotency_key=$3
		for update
	`, actorID, operation, key).Scan(&existingHash, &status, &body); err != nil {
		return false, 0, nil, err
	}
	if existingHash != requestHash {
		return false, 0, nil, errors.New("idempotency hash mismatch")
	}
	if status == nil || len(body) == 0 {
		return false, 0, nil, errors.New("idempotency result incomplete")
	}
	return true, *status, json.RawMessage(body), nil
}

func validateAction(action string, req actionRequest) map[string]string {
	fields := map[string]string{}
	switch action {
	case "take", "mark-thinking":
	case "first-call":
		if strings.TrimSpace(req.Result) == "" {
			fields["result"] = "Required"
		}
		if strings.TrimSpace(req.Comment) == "" {
			fields["comment"] = "Required"
		}
	case "schedule-visit":
		if _, err := time.Parse(time.RFC3339, req.ScheduledAt); err != nil {
			fields["scheduledAt"] = "Invalid date"
		}
	case "reschedule-visit":
		if _, err := time.Parse(time.RFC3339, req.ScheduledAt); err != nil {
			fields["scheduledAt"] = "Invalid date"
		}
		if strings.TrimSpace(req.Comment) == "" {
			fields["comment"] = "Required"
		}
	case "complete-visit", "comment":
		if strings.TrimSpace(req.Comment) == "" {
			fields["comment"] = "Required"
		}
	case "close":
		if strings.TrimSpace(req.Reason) == "" {
			fields["reason"] = "Required"
		}
	case "mark-successful":
		if strings.TrimSpace(req.ContractNumber) == "" {
			fields["contractNumber"] = "Required"
		}
		if req.Amount == nil || *req.Amount <= 0 {
			fields["amount"] = "Must be greater than zero"
		}
		if req.Prepayment == nil || *req.Prepayment < 0 {
			fields["prepayment"] = "Must not be negative"
		}
	default:
		fields["action"] = "Unknown action"
	}
	return fields
}

var errLossReasonNotFound = errors.New("loss reason not found")

func (s *Server) applyLeadAction(r *http.Request, tx pgx.Tx, actor Actor, leadID uuid.UUID, lead lockedLead, action string, req actionRequest) error {
	now := time.Now().UTC()
	eventType := action
	var oldValue any
	newValue := map[string]any{}
	comment := clean(req.Comment)
	workflow := lead.Workflow
	leadStatus := lead.LeadStatus
	assignedTo := lead.AssignedTo
	var callbackDue any

	switch action {
	case "take":
		assignedTo = &actor.ID
		workflow = "taken"
		leadStatus = "in_progress"
		eventType = "taken"
		oldValue = map[string]any{"workflow_status": lead.Workflow}
		newValue["workflow_status"] = workflow
	case "mark-thinking":
		workflow = "thinking"
		if leadStatus == "new" {
			leadStatus = "in_progress"
		}
		eventType = "thinking"
		oldValue = map[string]any{"workflow_status": lead.Workflow}
		newValue["workflow_status"] = workflow
	case "first-call":
		if _, err := tx.Exec(r.Context(), `insert into public.lead_contact_attempts (lead_id,manager_id,result,comment) values ($1,$2,$3,$4)`, leadID, actor.ID, strings.TrimSpace(req.Result), strings.TrimSpace(req.Comment)); err != nil {
			return err
		}
		workflow = "first_call_done"
		leadStatus = "in_progress"
		eventType = "contact_attempt"
		newValue = map[string]any{"result": strings.TrimSpace(req.Result), "workflow_status": workflow}
	case "schedule-visit", "reschedule-visit":
		scheduledAt, _ := time.Parse(time.RFC3339, req.ScheduledAt)
		if action == "reschedule-visit" {
			if _, err := tx.Exec(r.Context(), `update public.lead_showroom_visits set status='rescheduled' where id=(select id from public.lead_showroom_visits where lead_id=$1 order by created_at desc limit 1)`, leadID); err != nil {
				return err
			}
			workflow = "visit_rescheduled"
			eventType = "visit_rescheduled"
		} else {
			workflow = "visit_scheduled"
			eventType = "showroom_visit_scheduled"
		}
		if _, err := tx.Exec(r.Context(), `insert into public.lead_showroom_visits (lead_id,scheduled_at,status,comment,created_by) values ($1,$2,'scheduled',$3,$4)`, leadID, scheduledAt, comment, actor.ID); err != nil {
			return err
		}
		callbackDue = nil
		newValue = map[string]any{"scheduled_at": scheduledAt, "workflow_status": workflow}
	case "complete-visit":
		if _, err := tx.Exec(r.Context(), `update public.lead_showroom_visits set status='visited' where id=(select id from public.lead_showroom_visits where lead_id=$1 order by created_at desc limit 1)`, leadID); err != nil {
			return err
		}
		workflow = "visit_completed"
		eventType = "showroom_visit_completed"
		newValue["workflow_status"] = workflow
	case "comment":
		if _, err := tx.Exec(r.Context(), `insert into public.lead_comments (lead_id,author_id,lead_status,body) values ($1,$2,$3,$4)`, leadID, actor.ID, lead.LeadStatus, strings.TrimSpace(req.Comment)); err != nil {
			return err
		}
		eventType = "comment"
	case "close":
		var exists bool
		if err := tx.QueryRow(r.Context(), `select exists(select 1 from public.loss_reasons where code=$1)`, strings.TrimSpace(req.Reason)).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errLossReasonNotFound
		}
		workflow = "closed"
		leadStatus = "failed"
		eventType = "closed"
		callbackDue = nil
		newValue = map[string]any{"reason": strings.TrimSpace(req.Reason), "workflow_status": workflow}
	case "mark-successful":
		if _, err := tx.Exec(r.Context(), `insert into public.lead_contracts (lead_id,signed_at,status,comment,created_by) values ($1,$2,'signed',$3,$4)`, leadID, now, comment, actor.ID); err != nil {
			return err
		}
		workflow = "successful"
		leadStatus = "converted"
		eventType = "successful"
		callbackDue = nil
		newValue = map[string]any{"workflow_status": workflow, "contract_number": strings.TrimSpace(req.ContractNumber), "amount": req.Amount, "prepayment": req.Prepayment}
	}

	query := `update public.leads set workflow_status=$2, lead_status=$3, assigned_to=$4,
		workflow_status_changed_at=case when workflow_status is distinct from $2 then $5 else workflow_status_changed_at end,
		lead_status_changed_at=case when lead_status is distinct from $3 then $5 else lead_status_changed_at end,
		last_comment=case when $6::text is null then last_comment else $6 end,
		loss_reason=case when $7::text is null then loss_reason else $7 end,
		callback_due_at=case when $8::boolean then null else callback_due_at end,
		updated_at=$5, version=version+1 where id=$1`
	clearCallback := callbackDue == nil && (action == "schedule-visit" || action == "close" || action == "mark-successful")
	lossReason := (*string)(nil)
	if action == "close" {
		reason := strings.TrimSpace(req.Reason)
		lossReason = &reason
	}
	if _, err := tx.Exec(r.Context(), query, leadID, workflow, leadStatus, assignedTo, now, comment, lossReason, clearCallback); err != nil {
		return err
	}
	_, err := tx.Exec(r.Context(), `insert into public.lead_events (lead_id,actor_id,event_type,comment,old_value,new_value) values ($1,$2,$3,$4,$5,$6)`, leadID, actor.ID, eventType, comment, oldValue, newValue)
	return err
}
