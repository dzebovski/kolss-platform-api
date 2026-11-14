package crmapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CRM v2 project card edits (pipeline 2.1, reworked for pipeline 4 on 2026-11-14).
//
//	PATCH  /v1/projects/{projectId}                          details: manager, designer
//	PATCH  /v1/projects/{projectId}/lead-info                patches the linked lead's info (no copy, Q-P12)
//	PUT    /v1/projects/{projectId}/estimate                 preliminary estimate total (or clear it)
//	POST   /v1/projects/{projectId}/comments                 comment (any user with office access)
//	PATCH  /v1/projects/{projectId}/payments/{paymentId}     edit a payment
//	DELETE /v1/projects/{projectId}/payments/{paymentId}     delete a payment
//
// Every mutation but the comment needs CanEditProject (the manager and admins) and returns the
// refreshed project card; each one writes a timeline event with old / new values.

// loadOfficeProject loads and locks the project header for an action every office member may
// take. It writes the error response itself.
func (s *Server) loadOfficeProject(w http.ResponseWriter, r *http.Request, tx pgx.Tx, actor Actor) (projectHeader, bool) {
	id, err := uuid.Parse(r.PathValue("projectId"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid project id", nil)
		return projectHeader{}, false
	}
	h, err := scanProjectHeader(tx.QueryRow(r.Context(), projectHeaderSelect+" for update of p", id))
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "project_not_found", "Project not found", nil)
		return projectHeader{}, false
	}
	if err != nil {
		s.projectFail(w, r)
		return projectHeader{}, false
	}
	if !actor.CanAccessOffice(h.OfficeID) {
		s.writeError(w, r, http.StatusForbidden, "office_forbidden", "Office access denied", nil)
		return projectHeader{}, false
	}
	return h, true
}

// ---- comments --------------------------------------------------------------------------------

func (s *Server) handleAddProjectComment(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	var req projectCommentRequest
	if err := decodeJSON(w, r, 16*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid comment", nil)
		return
	}
	text, fields := validateProjectComment(req)
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid comment", fields)
		return
	}
	const operation = "project.comment.add"
	tx, key, ok := s.beginIdempotentTx(w, r, actor, operation, r.PathValue("projectId"), text)
	if !ok {
		return
	}
	defer tx.Rollback(r.Context())
	h, ok := s.loadOfficeProject(w, r, tx, actor)
	if !ok {
		return
	}
	// A closed (cancelled) card is read-only; a finished one still takes comments.
	if h.Status == projectStatusCancelled {
		s.writeError(w, r, http.StatusConflict, "project_cancelled", "The project is cancelled — restore it first", nil)
		return
	}
	if err := s.insertProjectEvent(r.Context(), tx, h.ID, actor.ID, "comment", nil, nil, &text); err != nil {
		s.projectFail(w, r)
		return
	}
	raw, err := s.projectDetailJSON(r.Context(), tx, actor, h)
	if err != nil {
		s.projectFail(w, r)
		return
	}
	if err := s.finishIdempotentTx(r, tx, actor, operation, key, http.StatusCreated, raw); err != nil {
		s.projectFail(w, r)
		return
	}
	writeJSON(w, http.StatusCreated, raw)
}

// recordMeasurementVisit writes the project timeline entry for a measurement appointment that was
// marked held (status `visited`). It is a no-op when the lead has no project.
func (s *Server) recordMeasurementVisit(ctx context.Context, tx pgx.Tx, actorID, leadID uuid.UUID, appointmentID uuid.UUID, startsAt string) error {
	var projectID uuid.UUID
	err := tx.QueryRow(ctx, `select id from public.projects where lead_id = $1`, leadID).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.insertProjectEvent(ctx, tx, projectID, actorID, "measurement_visit", nil,
		map[string]any{"appointmentId": appointmentID.String(), "startsAt": startsAt}, nil)
}

// ---- details: manager, designer --------------------------------------------------------------

// projectDetailsRequest keeps raw values so "absent" and null differ: designerId null clears the
// designer, an absent key leaves it. managerId cannot be cleared.
type projectDetailsRequest struct {
	ManagerID  json.RawMessage `json:"managerId"`
	DesignerID json.RawMessage `json:"designerId"`
}

func (s *Server) handleUpdateProjectDetails(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	var req projectDetailsRequest
	if err := decodeJSON(w, r, 4*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid details", nil)
		return
	}
	fields := map[string]string{}
	var managerID, designerID *uuid.UUID
	setManager, setDesigner := req.ManagerID != nil, req.DesignerID != nil
	parseID := func(raw json.RawMessage, field string, nullable bool) *uuid.UUID {
		if string(raw) == "null" {
			if !nullable {
				fields[field] = "Cannot be cleared"
			}
			return nil
		}
		var text string
		id, err := uuid.Parse("")
		if json.Unmarshal(raw, &text) == nil {
			id, err = uuid.Parse(strings.TrimSpace(text))
		}
		if err != nil {
			fields[field] = "Must be a valid id"
			return nil
		}
		return &id
	}
	if setManager {
		managerID = parseID(req.ManagerID, "managerId", false)
	}
	if setDesigner {
		designerID = parseID(req.DesignerID, "designerId", true)
	}
	if !setManager && !setDesigner {
		fields["managerId"] = "Send managerId or designerId"
	}
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid details", fields)
		return
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.projectFail(w, r)
		return
	}
	defer tx.Rollback(r.Context())
	h, ok := s.lockEditableProject(w, r, tx, actor, false)
	if !ok {
		return
	}
	var oldDesigner *uuid.UUID
	if err := tx.QueryRow(r.Context(), `select designer_id from public.projects where id = $1`, h.ID).Scan(&oldDesigner); err != nil {
		s.projectFail(w, r)
		return
	}
	oldValue, newValue := map[string]any{}, map[string]any{}
	idText := func(id *uuid.UUID) any {
		if id == nil {
			return nil
		}
		return id.String()
	}
	for _, check := range []struct {
		field string
		id    *uuid.UUID
	}{{"managerId", managerID}, {"designerId", designerID}} {
		if check.id == nil {
			continue
		}
		if err := validateAppointmentManager(r, tx, *check.id, h.OfficeID); err != nil {
			if errors.Is(err, errAppointmentManagerInvalid) {
				s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid details", map[string]string{
					check.field: "Must be an active member of the project's office",
				})
				return
			}
			s.projectFail(w, r)
			return
		}
	}
	if setManager && (h.ManagerID == nil || *h.ManagerID != *managerID) {
		oldValue["managerId"], newValue["managerId"] = idText(h.ManagerID), idText(managerID)
	}
	if setDesigner && !sameUUID(oldDesigner, designerID) {
		oldValue["designerId"], newValue["designerId"] = idText(oldDesigner), idText(designerID)
	}
	if len(newValue) > 0 {
		if _, err := tx.Exec(r.Context(), `
			update public.projects
			set assigned_to = case when $2::boolean then $3::uuid else assigned_to end,
			    designer_id = case when $4::boolean then $5::uuid else designer_id end
			where id = $1
		`, h.ID, setManager, managerID, setDesigner, designerID); err != nil {
			s.projectFail(w, r)
			return
		}
		if err := s.insertProjectEvent(r.Context(), tx, h.ID, actor.ID, "details_updated", oldValue, newValue, nil); err != nil {
			s.projectFail(w, r)
			return
		}
		if setManager {
			h.ManagerID = managerID
		}
	}
	s.commitAndRespond(w, r, tx, actor, h, http.StatusOK)
}

// ---- lead info through the project -----------------------------------------------------------

// handleUpdateProjectLeadInfo patches the linked lead's info with the same logic as
// PATCH /v1/leads/{leadId}/info (no copy lives on the project, Q-P12). The project manager and
// admins may edit; If-Match (the lead version) is optional here, without it the changed fields
// are applied on top of the current lead.
func (s *Server) handleUpdateProjectLeadInfo(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	var req leadInfoRequest
	if err := decodeJSON(w, r, 64*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid lead info", nil)
		return
	}
	if fields := validateLeadInfo(req); len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid lead info", fields)
		return
	}
	var version *int64
	if v, has := parseIfMatch(r); has {
		version = &v
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.projectFail(w, r)
		return
	}
	defer tx.Rollback(r.Context())
	h, ok := s.lockEditableProject(w, r, tx, actor, false)
	if !ok {
		return
	}
	_, changed, apiErr := s.applyLeadInfoPatch(r, tx, actor, h.LeadID, version, req, func(uuid.UUID) *leadInfoUpdateError { return nil })
	if apiErr != nil {
		apiErr.write(s, w, r)
		return
	}
	if len(changed) > 0 {
		if err := s.insertProjectEvent(r.Context(), tx, h.ID, actor.ID, "lead_info_updated", nil, map[string]any{"fields": changed}, nil); err != nil {
			s.projectFail(w, r)
			return
		}
	}
	s.commitAndRespond(w, r, tx, actor, h, http.StatusOK)
}

// ---- estimate --------------------------------------------------------------------------------

func (s *Server) handleUpdateProjectEstimate(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	var req projectEstimateRequest
	if err := decodeJSON(w, r, 4*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid estimate", nil)
		return
	}
	next, fields := validateProjectEstimate(req)
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid estimate", fields)
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.projectFail(w, r)
		return
	}
	defer tx.Rollback(r.Context())
	h, ok := s.lockEditableProject(w, r, tx, actor, false)
	if !ok {
		return
	}
	var current *projectEstimate
	var amount *int64
	var currency *string
	if err := tx.QueryRow(r.Context(), `
		select (estimate_amount * 100)::bigint, estimate_currency from public.projects where id = $1
	`, h.ID).Scan(&amount, &currency); err != nil {
		s.projectFail(w, r)
		return
	}
	if amount != nil && currency != nil {
		current = &projectEstimate{AmountCents: *amount, Currency: *currency}
	}
	if !equalProjectEstimate(current, next) {
		var nextAmount *int64
		var nextCurrency *string
		if next != nil {
			nextAmount, nextCurrency = &next.AmountCents, &next.Currency
		}
		if _, err := tx.Exec(r.Context(), `
			update public.projects
			set estimate_amount = $2::bigint::numeric / 100, estimate_currency = $3
			where id = $1
		`, h.ID, nextAmount, nextCurrency); err != nil {
			s.projectFail(w, r)
			return
		}
		if err := s.insertProjectEvent(r.Context(), tx, h.ID, actor.ID, "estimate_updated", current.toMap(), next.toMap(), nil); err != nil {
			s.projectFail(w, r)
			return
		}
	}
	s.commitAndRespond(w, r, tx, actor, h, http.StatusOK)
}

func idPtrText(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

// ---- payment edit / delete -------------------------------------------------------------------

type paymentRecord struct {
	ID       uuid.UUID
	Amount   int64
	Currency string
	PaidOn   string
	Basis    string
	Note     *string
	FileID   *uuid.UUID
}

func (p paymentRecord) toMap() map[string]any {
	out := map[string]any{
		"amount": centsToMoney(p.Amount), "currency": p.Currency, "paidOn": p.PaidOn, "basis": p.Basis,
	}
	if p.Note != nil {
		out["note"] = *p.Note
	}
	return out
}

func (s *Server) lockProjectPayment(w http.ResponseWriter, r *http.Request, tx pgx.Tx, h projectHeader) (paymentRecord, bool) {
	id, err := uuid.Parse(r.PathValue("paymentId"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid payment id", nil)
		return paymentRecord{}, false
	}
	var p paymentRecord
	err = tx.QueryRow(r.Context(), `
		select id, (amount * 100)::bigint, currency, to_char(paid_on, 'YYYY-MM-DD'), basis, note, file_id
		from public.project_payments
		where id = $1 and project_id = $2
		for update
	`, id, h.ID).Scan(&p.ID, &p.Amount, &p.Currency, &p.PaidOn, &p.Basis, &p.Note, &p.FileID)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "payment_not_found", "Payment not found", nil)
		return paymentRecord{}, false
	}
	if err != nil {
		s.projectFail(w, r)
		return paymentRecord{}, false
	}
	return p, true
}

func (s *Server) handleUpdateProjectPayment(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	var req projectPaymentPatch
	if err := decodeJSON(w, r, 16*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid payment", nil)
		return
	}
	change, fields := validateProjectPaymentPatch(req)
	if len(fields) == 0 && change.empty() {
		fields["amount"] = "Send at least one field to change"
	}
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid payment", fields)
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.projectFail(w, r)
		return
	}
	defer tx.Rollback(r.Context())
	h, ok := s.lockEditableProject(w, r, tx, actor, false)
	if !ok {
		return
	}
	current, ok := s.lockProjectPayment(w, r, tx, h)
	if !ok {
		return
	}
	next := current
	if change.AmountCents != nil {
		next.Amount = *change.AmountCents
	}
	if change.Currency != nil {
		next.Currency = *change.Currency
	}
	if change.PaidOn != nil {
		next.PaidOn = *change.PaidOn
	}
	if change.Basis != nil {
		next.Basis = *change.Basis
	}
	if change.Note != nil {
		next.Note = nil
		if *change.Note != "" {
			next.Note = change.Note
		}
	}
	oldValue, newValue := map[string]any{}, map[string]any{}
	before, after := current.toMap(), next.toMap()
	for _, key := range []string{"amount", "currency", "paidOn", "basis", "note"} {
		if before[key] != after[key] {
			oldValue[key], newValue[key] = before[key], after[key]
		}
	}
	if change.FileID != nil && !sameUUID(current.FileID, change.FileID) {
		if err := s.confirmProjectFile(r.Context(), tx, h, *change.FileID, "receipt"); err != nil {
			s.writeProjectFileError(w, r, err)
			return
		}
		oldValue["fileId"], newValue["fileId"] = idPtrText(current.FileID), change.FileID.String()
		next.FileID = change.FileID
	}
	if len(newValue) > 0 {
		if _, err := tx.Exec(r.Context(), `
			update public.project_payments
			set amount = $2::bigint::numeric / 100, currency = $3, paid_on = $4::date, basis = $5, note = $6,
			    file_id = $7, updated_at = now()
			where id = $1
		`, current.ID, next.Amount, next.Currency, next.PaidOn, next.Basis, next.Note, next.FileID); err != nil {
			s.projectFail(w, r)
			return
		}
		if err := s.insertProjectEvent(r.Context(), tx, h.ID, actor.ID, "payment_updated", oldValue, newValue, nil); err != nil {
			s.projectFail(w, r)
			return
		}
	}
	s.commitAndRespond(w, r, tx, actor, h, http.StatusOK)
}

func (s *Server) handleDeleteProjectPayment(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.projectFail(w, r)
		return
	}
	defer tx.Rollback(r.Context())
	h, ok := s.lockEditableProject(w, r, tx, actor, false)
	if !ok {
		return
	}
	current, ok := s.lockProjectPayment(w, r, tx, h)
	if !ok {
		return
	}
	if _, err := tx.Exec(r.Context(), `delete from public.project_payments where id = $1`, current.ID); err != nil {
		s.projectFail(w, r)
		return
	}
	if err := s.insertProjectEvent(r.Context(), tx, h.ID, actor.ID, "payment_deleted", current.toMap(), nil, nil); err != nil {
		s.projectFail(w, r)
		return
	}
	s.commitAndRespond(w, r, tx, actor, h, http.StatusOK)
}
