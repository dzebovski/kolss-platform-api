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

// CRM v2 project pipeline 2.1 (task P1b, owner decisions of 2026-10-23).
//
//	PATCH  /v1/projects/{projectId}                          details: manager, designer
//	PATCH  /v1/projects/{projectId}/lead-info                patches the linked lead's info (no copy, Q-P12)
//	PUT    /v1/projects/{projectId}/estimate                 estimate lines, until a main contract exists
//	POST   /v1/projects/{projectId}/comments                 timeline comment (any user with office access)
//	PATCH  /v1/projects/{projectId}/contracts/{contractId}   edit a contract
//	DELETE /v1/projects/{projectId}/contracts/{contractId}   delete a contract (no payments on it)
//	PATCH  /v1/projects/{projectId}/payments/{paymentId}     edit a payment
//	DELETE /v1/projects/{projectId}/payments/{paymentId}     delete a payment
//
// Every mutation but the comment needs CanEditProject (the manager and admins) and returns the
// refreshed project card; each one writes a timeline event with old / new values.

// loadProjectContracts returns the project's contracts with the sum of their payments. A payment
// id can be excluded, to check an edit against the balance without the payment being edited.
func loadProjectContracts(ctx context.Context, tx pgx.Tx, projectID uuid.UUID, excludePayment *uuid.UUID) ([]projectContractRow, error) {
	rows, err := tx.Query(ctx, `
		select c.id, c.kind, (c.total_amount * 100)::bigint, c.currency,
		  coalesce((select sum(pp.amount * 100) from public.project_payments pp
		            where pp.contract_id = c.id and pp.id is distinct from $2), 0)::bigint
		from public.project_contracts c
		where c.project_id = $1
		order by c.kind
	`, projectID, excludePayment)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []projectContractRow
	for rows.Next() {
		var row projectContractRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.TotalCents, &row.Currency, &row.PaidCents); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// loadProjectForComment loads and locks the project header for an action every office member may
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
	// A cancelled card is read-only. TODO(v2): confirm with the owner whether comments stay open
	// on a cancelled project (they do on a lead that became a project).
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

type projectEstimateRequest struct {
	Lines map[string]json.RawMessage `json:"lines"`
}

func (s *Server) handleUpdateProjectEstimate(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	var req projectEstimateRequest
	if err := decodeJSON(w, r, 16*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid estimate", nil)
		return
	}
	lines, fields := validateEstimate(req.Lines)
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
	var locked bool
	if err := tx.QueryRow(r.Context(), `select exists(select 1 from public.project_contracts where project_id = $1 and kind = 'main')`, h.ID).Scan(&locked); err != nil {
		s.projectFail(w, r)
		return
	}
	if locked {
		s.writeError(w, r, http.StatusConflict, "estimate_locked", "The estimate cannot be changed once the main contract exists", nil)
		return
	}
	current := map[string]*estimateLine{}
	rows, err := tx.Query(r.Context(), `
		select line, (amount * 100)::bigint, currency, note from public.project_estimate_lines where project_id = $1
	`, h.ID)
	if err != nil {
		s.projectFail(w, r)
		return
	}
	for rows.Next() {
		var name string
		var line estimateLine
		if err := rows.Scan(&name, &line.AmountCents, &line.Currency, &line.Note); err != nil {
			rows.Close()
			s.projectFail(w, r)
			return
		}
		current[name] = &line
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.projectFail(w, r)
		return
	}

	oldValue, newValue := map[string]any{}, map[string]any{}
	for _, name := range estimateLines {
		next, requested := lines[name]
		if !requested || equalEstimateLine(current[name], next) {
			continue
		}
		oldValue[name], newValue[name] = current[name].toMap(), next.toMap()
		if next == nil {
			_, err = tx.Exec(r.Context(), `delete from public.project_estimate_lines where project_id = $1 and line = $2`, h.ID, name)
		} else {
			_, err = tx.Exec(r.Context(), `
				insert into public.project_estimate_lines (project_id, line, amount, currency, note, updated_by)
				values ($1,$2,$3::bigint::numeric / 100,$4,$5,$6)
				on conflict (project_id, line) do update
				set amount = excluded.amount, currency = excluded.currency, note = excluded.note,
				    updated_by = excluded.updated_by, updated_at = now()
			`, h.ID, name, next.AmountCents, next.Currency, next.Note, actor.ID)
		}
		if err != nil {
			s.projectFail(w, r)
			return
		}
	}
	if len(newValue) > 0 {
		if err := s.insertProjectEvent(r.Context(), tx, h.ID, actor.ID, "estimate_updated", oldValue, newValue, nil); err != nil {
			s.projectFail(w, r)
			return
		}
	}
	s.commitAndRespond(w, r, tx, actor, h, http.StatusOK)
}

// ---- contract edit / delete ------------------------------------------------------------------

type contractRecord struct {
	ID       uuid.UUID
	Kind     string
	Number   string
	SignedOn string
	Total    int64
	Currency string
	FileID   *uuid.UUID
}

func (s *Server) lockProjectContract(w http.ResponseWriter, r *http.Request, tx pgx.Tx, h projectHeader) (contractRecord, bool) {
	id, err := uuid.Parse(r.PathValue("contractId"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid contract id", nil)
		return contractRecord{}, false
	}
	var c contractRecord
	err = tx.QueryRow(r.Context(), `
		select id, kind, number, to_char(signed_on, 'YYYY-MM-DD'), (total_amount * 100)::bigint, currency, file_id
		from public.project_contracts where id = $1 and project_id = $2 for update
	`, id, h.ID).Scan(&c.ID, &c.Kind, &c.Number, &c.SignedOn, &c.Total, &c.Currency, &c.FileID)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "contract_not_found", "Contract not found", nil)
		return contractRecord{}, false
	}
	if err != nil {
		s.projectFail(w, r)
		return contractRecord{}, false
	}
	return c, true
}

func (s *Server) handleUpdateProjectContract(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	var req projectContractPatch
	if err := decodeJSON(w, r, 16*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid contract", nil)
		return
	}
	change, fields := validateProjectContractPatch(req)
	if len(fields) == 0 && change.empty() {
		fields["number"] = "Send at least one field to change"
	}
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid contract", fields)
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
	current, ok := s.lockProjectContract(w, r, tx, h)
	if !ok {
		return
	}
	rows, err := loadProjectContracts(r.Context(), tx, h.ID, nil)
	if err != nil {
		s.projectFail(w, r)
		return
	}
	next := current
	oldValue, newValue := map[string]any{"kind": current.Kind}, map[string]any{"kind": current.Kind}
	if change.Number != nil && *change.Number != current.Number {
		oldValue["number"], newValue["number"] = current.Number, *change.Number
		next.Number = *change.Number
	}
	if change.SignedOn != nil && *change.SignedOn != current.SignedOn {
		oldValue["signedOn"], newValue["signedOn"] = current.SignedOn, *change.SignedOn
		next.SignedOn = *change.SignedOn
	}
	if change.TotalCents != nil && *change.TotalCents != current.Total {
		oldValue["totalAmount"], newValue["totalAmount"] = centsToMoney(current.Total), centsToMoney(*change.TotalCents)
		next.Total = *change.TotalCents
	}
	if change.Currency != nil && *change.Currency != current.Currency {
		oldValue["currency"], newValue["currency"] = current.Currency, *change.Currency
		next.Currency = *change.Currency
	}
	for i := range rows {
		if rows[i].ID != current.ID {
			continue
		}
		if next.Currency != current.Currency && rows[i].PaidCents > 0 {
			s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid contract", map[string]string{
				"currency": "Cannot change while payments are recorded on this contract",
			})
			return
		}
		rows[i].TotalCents, rows[i].Currency = next.Total, next.Currency
	}
	if field, message := moneyFromContracts(rows).overpaid(); field != "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid contract", map[string]string{field: message})
		return
	}
	if change.FileID != nil {
		if err := s.confirmProjectFile(r.Context(), tx, h, *change.FileID, "contract"); err != nil {
			s.writeProjectFileError(w, r, err)
			return
		}
		oldValue["fileId"], newValue["fileId"] = idPtrText(current.FileID), change.FileID.String()
	}
	if len(newValue) > 1 {
		if _, err := tx.Exec(r.Context(), `
			update public.project_contracts
			set number = $2, signed_on = $3::date, total_amount = $4::bigint::numeric / 100, currency = $5,
			    file_id = coalesce($6, file_id), updated_at = now()
			where id = $1
		`, current.ID, next.Number, next.SignedOn, next.Total, next.Currency, change.FileID); err != nil {
			s.projectFail(w, r)
			return
		}
		if err := s.insertProjectEvent(r.Context(), tx, h.ID, actor.ID, "contract_updated", oldValue, newValue, nil); err != nil {
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

// handleDeleteProjectContract removes a contract that has no payments (delete the payments
// first: money is never dropped silently). The project status is left as it is.
// TODO(v2): confirm with the owner whether deleting the main contract should move the stage back.
func (s *Server) handleDeleteProjectContract(w http.ResponseWriter, r *http.Request) {
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
	current, ok := s.lockProjectContract(w, r, tx, h)
	if !ok {
		return
	}
	var payments int
	if err := tx.QueryRow(r.Context(), `select count(*) from public.project_payments where contract_id = $1`, current.ID).Scan(&payments); err != nil {
		s.projectFail(w, r)
		return
	}
	if payments > 0 {
		s.writeError(w, r, http.StatusConflict, "contract_has_payments", "Delete the payments of this contract first", nil)
		return
	}
	if _, err := tx.Exec(r.Context(), `delete from public.project_contracts where id = $1`, current.ID); err != nil {
		s.projectFail(w, r)
		return
	}
	oldValue := map[string]any{
		"kind": current.Kind, "number": current.Number, "signedOn": current.SignedOn,
		"totalAmount": centsToMoney(current.Total), "currency": current.Currency,
	}
	if err := s.insertProjectEvent(r.Context(), tx, h.ID, actor.ID, "contract_deleted", oldValue, nil, nil); err != nil {
		s.projectFail(w, r)
		return
	}
	s.commitAndRespond(w, r, tx, actor, h, http.StatusOK)
}

// ---- payment edit / delete -------------------------------------------------------------------

type paymentRecord struct {
	ID           uuid.UUID
	ContractID   uuid.UUID
	ContractKind string
	Currency     string
	Amount       int64
	PaidOn       string
	Note         *string
	FileID       *uuid.UUID
}

func (s *Server) lockProjectPayment(w http.ResponseWriter, r *http.Request, tx pgx.Tx, h projectHeader) (paymentRecord, bool) {
	id, err := uuid.Parse(r.PathValue("paymentId"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid payment id", nil)
		return paymentRecord{}, false
	}
	var p paymentRecord
	err = tx.QueryRow(r.Context(), `
		select pp.id, pp.contract_id, c.kind, c.currency, (pp.amount * 100)::bigint,
		  to_char(pp.paid_on, 'YYYY-MM-DD'), pp.note, pp.file_id
		from public.project_payments pp
		join public.project_contracts c on c.id = pp.contract_id
		where pp.id = $1 and pp.project_id = $2
		for update of pp
	`, id, h.ID).Scan(&p.ID, &p.ContractID, &p.ContractKind, &p.Currency, &p.Amount, &p.PaidOn, &p.Note, &p.FileID)
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
	oldValue := map[string]any{"contractKind": current.ContractKind, "currency": current.Currency}
	newValue := map[string]any{"contractKind": current.ContractKind, "currency": current.Currency}
	next := current
	if change.AmountCents != nil && *change.AmountCents != current.Amount {
		rows, err := loadProjectContracts(r.Context(), tx, h.ID, &current.ID)
		if err != nil {
			s.projectFail(w, r)
			return
		}
		if limit := moneyFromContracts(rows).maxPayment(current.ContractKind); *change.AmountCents > limit {
			s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid payment", map[string]string{
				"amount": "More than the remaining " + formatMoneyCents(limit) + " " + current.Currency + " — check the amount",
			})
			return
		}
		oldValue["amount"], newValue["amount"] = centsToMoney(current.Amount), centsToMoney(*change.AmountCents)
		next.Amount = *change.AmountCents
	}
	if change.PaidOn != nil && *change.PaidOn != current.PaidOn {
		oldValue["paidOn"], newValue["paidOn"] = current.PaidOn, *change.PaidOn
		next.PaidOn = *change.PaidOn
	}
	if change.Note != nil {
		var nextNote *string
		if *change.Note != "" {
			nextNote = change.Note
		}
		if (current.Note == nil) != (nextNote == nil) || (nextNote != nil && *current.Note != *nextNote) {
			oldValue["note"], newValue["note"] = current.Note, nextNote
			next.Note = nextNote
		}
	}
	if change.FileID != nil {
		if err := s.confirmProjectFile(r.Context(), tx, h, *change.FileID, "receipt"); err != nil {
			s.writeProjectFileError(w, r, err)
			return
		}
		oldValue["fileId"], newValue["fileId"] = idPtrText(current.FileID), change.FileID.String()
	}
	if len(newValue) > 2 {
		if _, err := tx.Exec(r.Context(), `
			update public.project_payments
			set amount = $2::bigint::numeric / 100, paid_on = $3::date, note = $4,
			    file_id = coalesce($5, file_id), updated_at = now()
			where id = $1
		`, current.ID, next.Amount, next.PaidOn, next.Note, change.FileID); err != nil {
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
	oldValue := map[string]any{
		"contractId": current.ContractID.String(), "contractKind": current.ContractKind,
		"amount": centsToMoney(current.Amount), "currency": current.Currency, "paidOn": current.PaidOn,
	}
	if current.Note != nil {
		oldValue["note"] = *current.Note
	}
	if err := s.insertProjectEvent(r.Context(), tx, h.ID, actor.ID, "payment_deleted", oldValue, nil, nil); err != nil {
		s.projectFail(w, r)
		return
	}
	s.commitAndRespond(w, r, tx, actor, h, http.StatusOK)
}
