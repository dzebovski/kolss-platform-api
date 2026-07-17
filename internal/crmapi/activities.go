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

const (
	activityCallStatus   = "call_status"
	activityClientStatus = "client_status"
	activityComment      = "comment"
	activityReopen       = "reopen"
)

type leadActivityRequest struct {
	Type           string   `json:"type"`
	Status         string   `json:"status"`
	Comment        string   `json:"comment"`
	Reason         string   `json:"reason"`
	ContractNumber string   `json:"contractNumber"`
	Amount         *float64 `json:"amount"`
	Currency       string   `json:"currency"`
}

type activityLead struct {
	OfficeID       uuid.UUID
	AssignedTo     *uuid.UUID
	CallStatus     *string
	ClientStatus   string
	ArchivedAt     *time.Time
	CurrentVersion int64
}

func (s *Server) handleDeprecatedLeadAction(w http.ResponseWriter, r *http.Request) {
	s.writeError(
		w,
		r,
		http.StatusGone,
		"workflow_retired",
		"Workflow actions are retired; use lead activities",
		nil,
	)
}

func (s *Server) handleLeadActivity(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	leadID, err := uuid.Parse(r.PathValue("leadId"))
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid lead id", nil)
		return
	}
	if key == "" {
		s.writeError(w, r, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required", nil)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid activity body", nil)
		return
	}
	var req leadActivityRequest
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid activity body", nil)
		return
	}
	req.Type = strings.TrimSpace(req.Type)
	req.Status = strings.TrimSpace(req.Status)
	req.Comment = strings.TrimSpace(req.Comment)
	req.Reason = strings.TrimSpace(req.Reason)
	req.ContractNumber = strings.TrimSpace(req.ContractNumber)
	req.Currency = strings.ToUpper(strings.TrimSpace(req.Currency))
	if fields := validateLeadActivity(req, actor.IsSuperAdmin()); len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Activity validation failed", fields)
		return
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "activity_failed", "Could not update lead", nil)
		return
	}
	defer tx.Rollback(r.Context())

	hashBytes := sha256.Sum256(append([]byte(leadID.String()+"|"), body...))
	operation := "lead.activity"
	cached, status, cachedBody, err := claimIdempotency(r, tx, actor.ID, operation, key, hex.EncodeToString(hashBytes[:]))
	if err != nil {
		s.writeError(w, r, http.StatusConflict, "idempotency_conflict", "Idempotency key was already used for another request", nil)
		return
	}
	if cached {
		if err := tx.Commit(r.Context()); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "activity_failed", "Could not update lead", nil)
			return
		}
		writeJSON(w, status, cachedBody)
		return
	}

	var lead activityLead
	err = tx.QueryRow(r.Context(), `
		select office_id, assigned_to, call_status, client_status, archived_at, version
		from public.leads where id=$1 for update
	`, leadID).Scan(
		&lead.OfficeID,
		&lead.AssignedTo,
		&lead.CallStatus,
		&lead.ClientStatus,
		&lead.ArchivedAt,
		&lead.CurrentVersion,
	)
	if errors.Is(err, pgx.ErrNoRows) || lead.ArchivedAt != nil {
		s.writeError(w, r, http.StatusNotFound, "lead_not_found", "Lead not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "activity_failed", "Could not update lead", nil)
		return
	}
	if !actor.CanAccessOffice(lead.OfficeID) {
		s.writeError(w, r, http.StatusForbidden, "office_forbidden", "Office access denied", nil)
		return
	}
	terminal := lead.ClientStatus == "closed_lost" || lead.ClientStatus == "contract_signed"
	if req.Type == activityReopen {
		if !terminal {
			s.writeError(w, r, http.StatusConflict, "invalid_transition", "Only terminal leads can be reopened", nil)
			return
		}
	} else if terminal {
		s.writeError(w, r, http.StatusConflict, "lead_terminal", "Terminal leads must be reopened before another activity", nil)
		return
	}
	if req.Type == activityClientStatus && req.Status == lead.ClientStatus {
		s.writeError(w, r, http.StatusConflict, "status_unchanged", "Client status is already selected", nil)
		return
	}

	if err := s.applyLeadActivity(r, tx, actor, leadID, lead, req); err != nil {
		if errors.Is(err, errLossReasonNotFound) {
			s.writeError(w, r, http.StatusBadRequest, "invalid_loss_reason", "Unknown loss reason", map[string]string{"reason": "Unknown loss reason"})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "activity_failed", "Could not update lead", nil)
		return
	}

	response := map[string]any{"ok": true, "version": lead.CurrentVersion + 1}
	rawResponse, _ := json.Marshal(response)
	if _, err := tx.Exec(r.Context(), `
		update public.api_idempotency_keys
		set response_status=$4, response_body=$5::jsonb
		where actor_id=$1 and operation=$2 and idempotency_key=$3
	`, actor.ID, operation, key, http.StatusOK, rawResponse); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "activity_failed", "Could not update lead", nil)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "activity_failed", "Could not update lead", nil)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func validateLeadActivity(req leadActivityRequest, isSuperAdmin bool) map[string]string {
	fields := map[string]string{}
	reject := func(name, value string) {
		if value != "" {
			fields[name] = "Not allowed for this activity type"
		}
	}
	switch req.Type {
	case activityCallStatus:
		reject("reason", req.Reason)
		reject("contractNumber", req.ContractNumber)
		reject("currency", req.Currency)
		if req.Amount != nil {
			fields["amount"] = "Not allowed for this activity type"
		}
		switch req.Status {
		case "reached":
			if req.Comment == "" && !isSuperAdmin {
				fields["comment"] = "Required for a successful call"
			}
		case "no_answer", "callback_requested":
		default:
			fields["status"] = "Unknown call status"
		}
	case activityClientStatus:
		switch req.Status {
		case "showroom_invited", "calculation_in_progress", "thinking":
			reject("reason", req.Reason)
			reject("comment", req.Comment)
			reject("contractNumber", req.ContractNumber)
			reject("currency", req.Currency)
			if req.Amount != nil {
				fields["amount"] = "Not allowed for this status"
			}
		case "closed_lost":
			reject("contractNumber", req.ContractNumber)
			reject("currency", req.Currency)
			if req.Amount != nil {
				fields["amount"] = "Not allowed for this status"
			}
			switch req.Reason {
			case "expensive", "invalid", "other":
			default:
				fields["reason"] = "Must be expensive, invalid, or other"
			}
			if req.Comment == "" {
				fields["comment"] = "Required"
			}
		case "contract_signed":
			reject("reason", req.Reason)
			reject("comment", req.Comment)
			if req.ContractNumber == "" {
				fields["contractNumber"] = "Required"
			}
			if req.Amount == nil || *req.Amount <= 0 {
				fields["amount"] = "Must be greater than zero"
			}
			switch req.Currency {
			case "UAH", "USD", "EUR", "PLN":
			default:
				fields["currency"] = "Must be UAH, USD, EUR, or PLN"
			}
		default:
			fields["status"] = "Unknown client status"
		}
	case activityComment:
		reject("status", req.Status)
		reject("reason", req.Reason)
		reject("contractNumber", req.ContractNumber)
		reject("currency", req.Currency)
		if req.Amount != nil {
			fields["amount"] = "Not allowed for this activity type"
		}
		if req.Comment == "" {
			fields["comment"] = "Required"
		}
	case activityReopen:
		reject("status", req.Status)
		reject("comment", req.Comment)
		reject("reason", req.Reason)
		reject("contractNumber", req.ContractNumber)
		reject("currency", req.Currency)
		if req.Amount != nil {
			fields["amount"] = "Not allowed for this activity type"
		}
	default:
		fields["type"] = "Unknown activity type"
	}
	return fields
}

func (s *Server) applyLeadActivity(r *http.Request, tx pgx.Tx, actor Actor, leadID uuid.UUID, lead activityLead, req leadActivityRequest) error {
	now := time.Now().UTC()
	assignedTo := lead.AssignedTo
	if req.Type != activityReopen && assignedTo == nil {
		assignedTo = &actor.ID
	}

	eventType := ""
	eventCategory := ""
	statusCode := (*string)(nil)
	comment := clean(req.Comment)
	oldValue := map[string]any{}
	newValue := map[string]any{}
	callStatus := lead.CallStatus
	clientStatus := lead.ClientStatus
	changeCall := false
	changeClient := false
	lossReason := (*string)(nil)

	switch req.Type {
	case activityCallStatus:
		eventType = "call_status_changed"
		eventCategory = activityCallStatus
		statusCode = &req.Status
		oldValue["call_status"] = lead.CallStatus
		newValue["call_status"] = req.Status
		callStatus = &req.Status
		changeCall = true
	case activityComment:
		eventType = "comment_added"
		eventCategory = activityComment
	case activityClientStatus:
		eventType = "client_status_changed"
		eventCategory = activityClientStatus
		statusCode = &req.Status
		oldValue["client_status"] = lead.ClientStatus
		newValue["client_status"] = req.Status
		clientStatus = req.Status
		changeClient = true
		if req.Status == "closed_lost" {
			var exists bool
			if err := tx.QueryRow(r.Context(), `select exists(select 1 from public.loss_reasons where code=$1)`, req.Reason).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return errLossReasonNotFound
			}
			lossReason = &req.Reason
			newValue["reason"] = req.Reason
		}
		if req.Status == "contract_signed" {
			if _, err := tx.Exec(r.Context(), `
				insert into public.lead_contracts
				  (lead_id,signed_at,status,contract_number,amount,currency,created_by)
				values ($1,$2,'signed',$3,$4,$5,$6)
			`, leadID, now, req.ContractNumber, req.Amount, req.Currency, actor.ID); err != nil {
				return err
			}
			newValue["contract_number"] = req.ContractNumber
			newValue["amount"] = req.Amount
			newValue["currency"] = req.Currency
			newValue["signed_at"] = now
		}
	case activityReopen:
		eventType = "lead_reopened"
		eventCategory = "system"
		status := "new_lead"
		statusCode = &status
		oldValue["client_status"] = lead.ClientStatus
		oldValue["call_status"] = lead.CallStatus
		newValue["client_status"] = status
		newValue["call_status"] = nil
		clientStatus = status
		callStatus = nil
		changeCall = true
		changeClient = true
	}

	_, err := tx.Exec(r.Context(), `
		update public.leads set
		  call_status=$2,
		  call_status_changed_at=case when $3 then $6 else call_status_changed_at end,
		  client_status=$4,
		  client_status_changed_at=case when $5 then $6 else client_status_changed_at end,
		  assigned_to=$7,
		  loss_reason=case when $8::text is null then loss_reason else $8 end,
		  updated_at=$6,
		  version=version+1
		where id=$1
	`, leadID, callStatus, changeCall, clientStatus, changeClient, now, assignedTo, lossReason)
	if err != nil {
		return err
	}
	_, err = tx.Exec(r.Context(), `
		insert into public.lead_events
		  (lead_id,actor_id,event_type,event_category,status_code,comment,old_value,new_value)
		values ($1,$2,$3,$4,$5,$6,$7,$8)
	`, leadID, actor.ID, eventType, eventCategory, statusCode, comment, oldValue, newValue)
	return err
}
