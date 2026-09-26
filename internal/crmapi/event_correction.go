package crmapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CRM v2 "Edit timeline entry" popup (task W12): correct an entry's type and/or comment with a
// required reason. The entry is rewritten in place; the original values go to
// new_value.corrections (the edit history), so nothing is lost.
//
// Only the board's five types can be corrected: Successful call, Call later, No answer, Client
// thinking, Comment. When the corrected entry is the lead's latest status entry, the lead's
// status follows the correction (v2_status, the v1 call/client status mirror, callback_due_at and
// the no-answer counter); older entries only change the history.

const (
	correctionKindComment = "comment"
	maxCorrectionReason   = 1000
)

// correctionKinds maps a correction kind to how the entry is stored (same shapes the v2_status
// activity and the v1 comment activity write).
var correctionKinds = map[string]struct {
	eventType  string
	category   string
	statusCode string
}{
	v2StatusSuccess:       {"call_status_changed", activityCallStatus, "reached"},
	v2StatusLater:         {"call_status_changed", activityCallStatus, "callback_requested"},
	v2StatusNoAnswer:      {"call_status_changed", activityCallStatus, "no_answer"},
	v2StatusThinking:      {"client_status_changed", activityClientStatus, "thinking"},
	correctionKindComment: {"comment_added", activityComment, ""},
}

// correctionKindOfEvent returns the correction kind of a stored entry, or false when the entry
// is not one of the five correctable types. Pure, unit-tested.
func correctionKindOfEvent(eventType string, statusCode *string) (string, bool) {
	code := ""
	if statusCode != nil {
		code = *statusCode
	}
	switch eventType {
	case "comment_added":
		return correctionKindComment, true
	case "call_status_changed":
		for kind, stored := range correctionKinds {
			if stored.eventType == eventType && stored.statusCode == code {
				return kind, true
			}
		}
	case "client_status_changed":
		if code == "thinking" {
			return v2StatusThinking, true
		}
	}
	return "", false
}

func correctionKindNeedsDate(kind string) bool {
	return kind == v2StatusLater || kind == v2StatusNoAnswer || kind == v2StatusThinking
}

type eventCorrectionRequest struct {
	Type    *string    `json:"type"`
	DueAt   *time.Time `json:"dueAt"`
	Comment *string    `json:"comment"`
	Reason  string     `json:"reason"`
}

// validateEventCorrection checks the request against the entry's current kind and comment.
// Pure, unit-tested. It returns the resulting kind and comment.
func validateEventCorrection(req eventCorrectionRequest, currentKind string, currentComment *string) (kind string, comment *string, fields map[string]string) {
	fields = map[string]string{}
	kind = currentKind
	comment = currentComment
	if req.Type == nil && req.Comment == nil {
		fields["type"] = "Change the entry type, the comment, or both"
	}
	if req.Type != nil {
		next := strings.TrimSpace(*req.Type)
		switch {
		case correctionKinds[next].eventType == "":
			fields["type"] = "Must be success, later, noanswer, thinking, or comment"
		case next == currentKind:
			fields["type"] = "This is the current type — pick another one"
		default:
			kind = next
		}
	}
	if correctionKindNeedsDate(kind) && req.Type != nil && req.DueAt == nil {
		fields["dueAt"] = "Required for this type"
	}
	if !correctionKindNeedsDate(kind) && req.DueAt != nil {
		fields["dueAt"] = "Not allowed for this type"
	}
	if req.Comment != nil {
		comment = clean(*req.Comment)
	}
	if kind == correctionKindComment && comment == nil {
		fields["comment"] = "Comment can't be empty"
	}
	reason := strings.TrimSpace(req.Reason)
	switch {
	case reason == "":
		fields["reason"] = "Required"
	case len([]rune(reason)) > maxCorrectionReason:
		fields["reason"] = "Must be at most 1000 characters"
	}
	return kind, comment, fields
}

type correctionLeadState struct {
	officeID         uuid.UUID
	callStatus       *string
	clientStatus     string
	callbackDueAt    *time.Time
	noAnswerAttempts int
	v2Status         *string
}

func (s *Server) handleCorrectEvent(w http.ResponseWriter, r *http.Request) {
	leadID, leadErr := uuid.Parse(r.PathValue("leadId"))
	eventID, eventErr := uuid.Parse(r.PathValue("eventId"))
	var req eventCorrectionRequest
	if leadErr != nil || eventErr != nil || decodeJSON(w, r, 32*1024, &req) != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid correction", nil)
		return
	}
	actor, ok := s.authorizeLeadEventMutation(w, r, leadID, eventID)
	if !ok {
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "event_correction_failed", "Could not correct the entry", nil)
		return
	}
	defer tx.Rollback(r.Context())

	var lead correctionLeadState
	err = tx.QueryRow(r.Context(), `
		select office_id, call_status, client_status, callback_due_at, no_answer_attempts, v2_status
		from public.leads where id=$1 and archived_at is null for update
	`, leadID).Scan(&lead.officeID, &lead.callStatus, &lead.clientStatus, &lead.callbackDueAt, &lead.noAnswerAttempts, &lead.v2Status)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "lead_not_found", "Lead not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "event_correction_failed", "Could not correct the entry", nil)
		return
	}

	var eventType string
	var statusCode, comment *string
	var createdAt time.Time
	var oldValueRaw, newValueRaw []byte
	err = tx.QueryRow(r.Context(), `
		select event_type, status_code, comment, created_at, coalesce(old_value,'{}'::jsonb), coalesce(new_value,'{}'::jsonb)
		from public.lead_events where id=$1 and lead_id=$2 for update
	`, eventID, leadID).Scan(&eventType, &statusCode, &comment, &createdAt, &oldValueRaw, &newValueRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "event_not_found", "History event not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "event_correction_failed", "Could not correct the entry", nil)
		return
	}
	currentKind, ok := correctionKindOfEvent(eventType, statusCode)
	if !ok {
		s.writeError(w, r, http.StatusBadRequest, "event_not_correctable", "Only calls, follow-ups and comments can be corrected", nil)
		return
	}
	kind, nextComment, fields := validateEventCorrection(req, currentKind, comment)
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid correction", fields)
		return
	}

	oldValue := map[string]any{}
	newValue := map[string]any{}
	_ = json.Unmarshal(oldValueRaw, &oldValue)
	_ = json.Unmarshal(newValueRaw, &newValue)

	// Is this the lead's latest status entry (before and after the correction)?
	var latestStatusEventID *uuid.UUID
	if err := tx.QueryRow(r.Context(), `
		select id from public.lead_events
		where lead_id=$1 and event_category in ('call_status','client_status')
		order by created_at desc, id desc limit 1
	`, leadID).Scan(&latestStatusEventID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusInternalServerError, "event_correction_failed", "Could not correct the entry", nil)
		return
	}
	wasLatest := latestStatusEventID != nil && *latestStatusEventID == eventID
	becomesLatest := false
	if currentKind == correctionKindComment && kind != correctionKindComment {
		becomesLatest = true
		if latestStatusEventID != nil {
			var latestAt time.Time
			if err := tx.QueryRow(r.Context(), `select created_at from public.lead_events where id=$1`, *latestStatusEventID).Scan(&latestAt); err != nil {
				s.writeError(w, r, http.StatusInternalServerError, "event_correction_failed", "Could not correct the entry", nil)
				return
			}
			becomesLatest = createdAt.After(latestAt)
		}
	}
	changesLeadStatus := kind != currentKind && (wasLatest || becomesLatest) && !isTerminalClientStatus(lead.clientStatus)

	now := time.Now().UTC()
	editedByName := ""
	if actor.DisplayName != nil {
		editedByName = *actor.DisplayName
	}
	history, _ := newValue["corrections"].([]any)
	history = append(history, map[string]any{
		"at":      now,
		"by":      actor.ID.String(),
		"by_name": editedByName,
		"reason":  strings.TrimSpace(req.Reason),
		"from":    map[string]any{"type": currentKind, "comment": comment, "due_at": newValue["due_at"]},
		"to":      map[string]any{"type": kind, "comment": nextComment, "due_at": req.DueAt},
	})
	newValue["corrections"] = history
	if req.Comment != nil && !equalStringPtr(comment, nextComment) {
		newValue["edit_audit"] = map[string]any{
			"fields": []string{"message"}, "edited_at": now, "edited_by": actor.ID.String(), "edited_by_name": editedByName,
		}
	}

	stored := correctionKinds[kind]
	var nextStatusCode *string
	if stored.statusCode != "" {
		code := stored.statusCode
		nextStatusCode = &code
	}
	if kind != currentKind {
		delete(newValue, "attempt")
		if kind == correctionKindComment {
			for _, key := range []string{"v2_status", "call_status", "client_status", "callback_due_at", "due_at"} {
				delete(newValue, key)
			}
		} else {
			newValue["v2_status"] = kind
			newValue["due_at"] = req.DueAt
			newValue["callback_due_at"] = req.DueAt
			if stored.category == activityCallStatus {
				newValue["call_status"] = stored.statusCode
				delete(newValue, "client_status")
			} else {
				newValue["client_status"] = stored.statusCode
				delete(newValue, "call_status")
			}
		}
	} else if req.DueAt != nil {
		newValue["due_at"] = req.DueAt
		newValue["callback_due_at"] = req.DueAt
	}

	if changesLeadStatus {
		callStatus := lead.callStatus
		clientStatus := lead.clientStatus
		// Undo what the entry's old type did, from its own old_value.
		if wasLatest {
			switch correctionKinds[currentKind].category {
			case activityCallStatus:
				callStatus = stringFromAny(oldValue["call_status"])
			case activityClientStatus:
				if prev := stringFromAny(oldValue["client_status"]); prev != nil {
					clientStatus = *prev
				} else {
					clientStatus = "new_lead"
				}
			}
			if currentKind == v2StatusNoAnswer && lead.noAnswerAttempts > 0 {
				lead.noAnswerAttempts--
			}
		} else {
			// A comment becomes a status entry: keep the lead's current values as its old_value.
			oldValue["call_status"] = lead.callStatus
			oldValue["client_status"] = lead.clientStatus
			oldValue["v2_status"] = lead.v2Status
		}
		var callbackDueAt *time.Time
		switch stored.category {
		case activityCallStatus:
			code := stored.statusCode
			callStatus = &code
			callbackDueAt = req.DueAt
		case activityClientStatus:
			clientStatus = stored.statusCode
			callbackDueAt = req.DueAt
		}
		if kind == v2StatusNoAnswer {
			lead.noAnswerAttempts++
			newValue["attempt"] = lead.noAnswerAttempts
		}
		v2Status := deriveV2LeadStatus(clientStatus, callStatus)
		if _, err := tx.Exec(r.Context(), `
			update public.leads set
			  call_status=$2, client_status=$3, callback_due_at=$4, v2_status=$5,
			  v2_status_changed_at=$6, no_answer_attempts=$7, updated_at=$6, version=version+1
			where id=$1
		`, leadID, callStatus, clientStatus, callbackDueAt, v2Status, now, lead.noAnswerAttempts); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "event_correction_failed", "Could not correct the entry", nil)
			return
		}
	} else if kind == currentKind && req.DueAt != nil && wasLatest && !isTerminalClientStatus(lead.clientStatus) {
		// Same type, new date on the latest status entry: the lead's reminder follows.
		if _, err := tx.Exec(r.Context(), `update public.leads set callback_due_at=$2, updated_at=$3, version=version+1 where id=$1`, leadID, req.DueAt, now); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "event_correction_failed", "Could not correct the entry", nil)
			return
		}
	} else {
		if _, err := tx.Exec(r.Context(), `update public.leads set updated_at=$2, version=version+1 where id=$1`, leadID, now); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "event_correction_failed", "Could not correct the entry", nil)
			return
		}
	}

	if _, err := tx.Exec(r.Context(), `
		update public.lead_events set
		  event_type=$3, event_category=$4, status_code=$5, comment=$6,
		  comment_translation_en=case when $6 is distinct from comment then null else comment_translation_en end,
		  comment_translation_source_lang=case when $6 is distinct from comment then null else comment_translation_source_lang end,
		  comment_translated_at=case when $6 is distinct from comment then null else comment_translated_at end,
		  old_value=$7, new_value=$8
		where id=$1 and lead_id=$2
	`, eventID, leadID, stored.eventType, stored.category, nextStatusCode, nextComment, oldValue, newValue); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "event_correction_failed", "Could not correct the entry", nil)
		return
	}
	var version int64
	if err := tx.QueryRow(r.Context(), `select version from public.leads where id=$1`, leadID).Scan(&version); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "event_correction_failed", "Could not correct the entry", nil)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "event_correction_failed", "Could not correct the entry", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version, "leadStatusChanged": changesLeadStatus})
}

func stringFromAny(value any) *string {
	text, ok := value.(string)
	if !ok || text == "" {
		return nil
	}
	return &text
}
