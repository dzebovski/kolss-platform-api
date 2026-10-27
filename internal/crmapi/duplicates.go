package crmapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const lossReasonDuplicate = "duplicate"

type markDuplicateRequest struct {
	OriginalLeadID uuid.UUID `json:"originalLeadId"`
}

// duplicateLeadInfo is what the original lead's "repeat request" event keeps about the duplicate.
type duplicateLeadInfo struct {
	ReferenceID     *string
	DuplicateOf     *uuid.UUID
	Message         *string
	SourceChannel   *string
	SourceCreatedAt *time.Time
}

// handleMarkLeadDuplicate is POST /v1/leads/{leadId}/duplicate: the lead in the path is a repeat
// request of originalLeadId. It is closed (client status closed_lost, loss reason `duplicate`)
// and linked to the original, and the original gets a `repeat_request` event. Reports skip
// leads with duplicate_of_lead_id. Reopening the duplicate removes the link.
func (s *Server) handleMarkLeadDuplicate(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	leadID, err := uuid.Parse(r.PathValue("leadId"))
	if err != nil || !requireIdempotencyKey(s, w, r) {
		if err != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid lead id", nil)
		}
		return
	}
	var req markDuplicateRequest
	if err := decodeJSON(w, r, 4*1024, &req); err != nil || req.OriginalLeadID == uuid.Nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid duplicate data", map[string]string{"originalLeadId": "Required"})
		return
	}
	if req.OriginalLeadID == leadID {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid duplicate data", map[string]string{"originalLeadId": "Must be a different lead"})
		return
	}
	fail := func() {
		s.writeError(w, r, http.StatusInternalServerError, "duplicate_failed", "Could not mark the lead as a duplicate", nil)
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		fail()
		return
	}
	defer tx.Rollback(r.Context())

	lead, err := loadActivityLeadForUpdate(r, tx, leadID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && lead.ArchivedAt != nil) {
		s.writeError(w, r, http.StatusNotFound, "lead_not_found", "Lead not found", nil)
		return
	}
	if err != nil {
		fail()
		return
	}
	if !actor.CanAccessOffice(lead.OfficeID) {
		s.writeError(w, r, http.StatusForbidden, "office_forbidden", "Office access denied", nil)
		return
	}
	var info duplicateLeadInfo
	if err := tx.QueryRow(r.Context(), `
		select reference_id, duplicate_of_lead_id, order_comment, source_channel, coalesce(source_created_at, created_at)
		from public.leads where id=$1
	`, leadID).Scan(&info.ReferenceID, &info.DuplicateOf, &info.Message, &info.SourceChannel, &info.SourceCreatedAt); err != nil {
		fail()
		return
	}
	if info.DuplicateOf != nil {
		s.writeError(w, r, http.StatusConflict, "already_duplicate", "The lead is already marked as a duplicate", nil)
		return
	}
	if lead.ClientStatus == "contract_signed" || (lead.V2Status != nil && *lead.V2Status == "project") {
		s.writeError(w, r, http.StatusConflict, "lead_terminal", "A lead with a contract or a project cannot be a duplicate", nil)
		return
	}

	var originalOfficeID uuid.UUID
	var originalReference *string
	var originalArchivedAt *time.Time
	var originalDuplicateOf *uuid.UUID
	err = tx.QueryRow(r.Context(), `
		select office_id, reference_id, archived_at, duplicate_of_lead_id from public.leads where id=$1 for update
	`, req.OriginalLeadID).Scan(&originalOfficeID, &originalReference, &originalArchivedAt, &originalDuplicateOf)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (originalArchivedAt != nil || originalOfficeID != lead.OfficeID)) {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid duplicate data", map[string]string{"originalLeadId": "Must be an active lead of the same office"})
		return
	}
	if err != nil {
		fail()
		return
	}
	if originalDuplicateOf != nil {
		s.writeError(w, r, http.StatusConflict, "original_is_duplicate", "The original lead is itself a duplicate", nil)
		return
	}

	if lead.ClientStatus == "closed_lost" {
		_, err = tx.Exec(r.Context(), `update public.leads set loss_reason=$2, loss_reasons=array[$2] where id=$1`, leadID, lossReasonDuplicate)
	} else {
		err = s.applyLeadActivity(r, tx, actor, leadID, lead, leadActivityRequest{
			Type:   activityClientStatus,
			Status: "closed_lost",
			Reason: lossReasonDuplicate,
		}, nil)
	}
	if err != nil {
		fail()
		return
	}
	if _, err := tx.Exec(r.Context(), `
		update public.leads set duplicate_of_lead_id=$2, updated_at=now(), version=version+1 where id=$1
	`, leadID, req.OriginalLeadID); err != nil {
		fail()
		return
	}
	if _, err := tx.Exec(r.Context(), `
		insert into public.lead_events (lead_id,actor_id,event_type,event_category,new_value)
		values ($1,$2,'marked_duplicate','system',$3)
	`, leadID, actor.ID, map[string]any{
		"original_lead_id":      req.OriginalLeadID,
		"original_reference_id": originalReference,
	}); err != nil {
		fail()
		return
	}
	if _, err := tx.Exec(r.Context(), `
		insert into public.lead_events (lead_id,actor_id,event_type,event_category,new_value)
		values ($1,$2,'repeat_request','system',$3)
	`, req.OriginalLeadID, actor.ID, map[string]any{
		"duplicate_lead_id":      leadID,
		"duplicate_reference_id": info.ReferenceID,
		"message":                info.Message,
		"source_channel":         info.SourceChannel,
		"source_created_at":      info.SourceCreatedAt,
	}); err != nil {
		fail()
		return
	}
	if _, err := tx.Exec(r.Context(), `update public.leads set updated_at=now(), version=version+1 where id=$1`, req.OriginalLeadID); err != nil {
		fail()
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		fail()
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
