package crmapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dzebovski/kolss-platform-api/internal/storage"
)

// CRM v2 projects (task P1): mutations. Every one of them returns the refreshed project card
// (same shape as GET /v1/projects/{projectId}) so the UI needs no second request.
//
//	POST /v1/projects/{projectId}/status           Change status (per-status details, Q-P2)
//	POST /v1/projects/{projectId}/cancel           Cancel project (reasons, Q-P6)
//	POST /v1/projects/{projectId}/restore          Restore a cancelled project
//	POST /v1/projects/{projectId}/contract         Add contract (sets status `contract`)
//	POST /v1/projects/{projectId}/payments         Add payment (never above the remaining balance)
//	POST /v1/projects/{projectId}/files/uploads    Presigned upload for a contract file / receipt
//
// All of them need CanEditProject (the responsible manager or an admin, Q-P4).

const projectFilesBucket = leadDocumentsBucket // reuses the private 25 MiB bucket and its S3 setup

var errProjectFileMissing = errors.New("project file is not available")

// lockEditableProject locks the project row, checks edit permission and (unless allowCancelled)
// that the project is not cancelled. On failure it writes the response and returns false.
func (s *Server) lockEditableProject(w http.ResponseWriter, r *http.Request, tx pgx.Tx, actor Actor, allowCancelled bool) (projectHeader, bool) {
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
		s.writeError(w, r, http.StatusInternalServerError, "project_update_failed", "Could not update the project", nil)
		return projectHeader{}, false
	}
	if !actor.CanAccessOffice(h.OfficeID) {
		s.writeError(w, r, http.StatusForbidden, "office_forbidden", "Office access denied", nil)
		return projectHeader{}, false
	}
	if !actor.CanEditProject(h.OfficeID, h.ManagerID) {
		s.writeError(w, r, http.StatusForbidden, "project_edit_forbidden",
			"Only the project manager and admins can change this project", nil)
		return projectHeader{}, false
	}
	if h.Status == projectStatusCancelled && !allowCancelled {
		s.writeError(w, r, http.StatusConflict, "project_cancelled", "The project is cancelled — restore it first", nil)
		return projectHeader{}, false
	}
	return h, true
}

func (s *Server) projectFail(w http.ResponseWriter, r *http.Request) {
	s.writeError(w, r, http.StatusInternalServerError, "project_update_failed", "Could not update the project", nil)
}

// commitAndRespond commits tx and writes the refreshed card with the given status code.
func (s *Server) commitAndRespond(w http.ResponseWriter, r *http.Request, tx pgx.Tx, actor Actor, h projectHeader, code int) {
	raw, err := s.projectDetailJSON(r.Context(), tx, actor, h)
	if err != nil {
		s.projectFail(w, r)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.projectFail(w, r)
		return
	}
	writeJSON(w, code, raw)
}

// ---- status --------------------------------------------------------------------------------

func (s *Server) handleChangeProjectStatus(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	var req projectStatusRequest
	if err := decodeJSON(w, r, 16*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid status change", nil)
		return
	}
	in, fields := validateProjectStatus(req)
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid status change", fields)
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
	if in.ResponsibleID != nil {
		if err := validateAppointmentManager(r, tx, *in.ResponsibleID, h.OfficeID); err != nil {
			if errors.Is(err, errAppointmentManagerInvalid) {
				s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid status change", map[string]string{
					"responsibleId": "Must be an active member of the project's office",
				})
				return
			}
			s.projectFail(w, r)
			return
		}
	}

	var oldDate, oldTime *string
	if err := tx.QueryRow(r.Context(), `
		select to_char(event_date, 'YYYY-MM-DD'), to_char(event_time, 'HH24:MI') from public.projects where id = $1
	`, h.ID).Scan(&oldDate, &oldTime); err != nil {
		s.projectFail(w, r)
		return
	}

	changed := h.Status != in.Status
	var details []byte
	if in.Details != nil {
		details, err = json.Marshal(map[string]any{in.Status: in.Details})
		if err != nil {
			s.projectFail(w, r)
			return
		}
	} else {
		details = []byte(`{}`)
	}
	if _, err := tx.Exec(r.Context(), `
		update public.projects
		set status = $2,
		    status_changed_at = case when status <> $2 then now() else status_changed_at end,
		    event_date = $3::date,
		    event_time = $4::time,
		    status_details = status_details || $5::jsonb,
		    last_activity_at = now()
		where id = $1
	`, h.ID, in.Status, in.EventDate, in.EventTime, details); err != nil {
		s.projectFail(w, r)
		return
	}

	eventType := "status_updated"
	if changed {
		eventType = "status_changed"
	}
	oldValue := map[string]any{"status": h.Status, "eventDate": oldDate, "eventTime": oldTime}
	newValue := map[string]any{"status": in.Status, "eventDate": in.EventDate, "eventTime": in.EventTime}
	if in.Details != nil {
		newValue["details"] = in.Details
	}
	if err := s.insertProjectEvent(r.Context(), tx, h.ID, actor.ID, eventType, oldValue, newValue, in.Comment); err != nil {
		s.projectFail(w, r)
		return
	}
	h.Status = in.Status
	s.commitAndRespond(w, r, tx, actor, h, http.StatusOK)
}

// ---- cancel / restore ----------------------------------------------------------------------

func (s *Server) handleCancelProject(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	var req projectCancelRequest
	if err := decodeJSON(w, r, 16*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid cancellation", nil)
		return
	}
	reasons, comment, fields := validateProjectCancel(req)
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid cancellation", fields)
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.projectFail(w, r)
		return
	}
	defer tx.Rollback(r.Context())
	h, ok := s.lockEditableProject(w, r, tx, actor, true)
	if !ok {
		return
	}
	if h.Status == projectStatusCancelled {
		s.writeError(w, r, http.StatusConflict, "project_cancelled", "The project is already cancelled", nil)
		return
	}
	if _, err := tx.Exec(r.Context(), `
		update public.projects
		set status = 'cancelled', status_before_cancel = status, status_changed_at = now(),
		    cancelled_at = now(), cancelled_by = $2, cancel_reasons = $3, cancel_comment = $4,
		    event_date = null, event_time = null, last_activity_at = now()
		where id = $1
	`, h.ID, actor.ID, reasons, comment); err != nil {
		s.projectFail(w, r)
		return
	}
	if req.CancelFutureEvents {
		if err := cancelScheduledAppointmentsForLead(r.Context(), tx, actor.ID, h.LeadID); err != nil {
			s.projectFail(w, r)
			return
		}
	}
	oldValue := map[string]any{"status": h.Status}
	newValue := map[string]any{"status": projectStatusCancelled, "reasons": reasons, "futureEventsCancelled": req.CancelFutureEvents}
	if err := s.insertProjectEvent(r.Context(), tx, h.ID, actor.ID, "cancelled", oldValue, newValue, comment); err != nil {
		s.projectFail(w, r)
		return
	}
	h.Status = projectStatusCancelled
	s.commitAndRespond(w, r, tx, actor, h, http.StatusOK)
}

func (s *Server) handleRestoreProject(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.projectFail(w, r)
		return
	}
	defer tx.Rollback(r.Context())
	h, ok := s.lockEditableProject(w, r, tx, actor, true)
	if !ok {
		return
	}
	if h.Status != projectStatusCancelled {
		s.writeError(w, r, http.StatusConflict, "project_not_cancelled", "Only a cancelled project can be restored", nil)
		return
	}
	restored := projectStatusNone
	if h.StatusBeforeCancel != nil {
		restored = *h.StatusBeforeCancel
	}
	if _, err := tx.Exec(r.Context(), `
		update public.projects
		set status = $2, status_before_cancel = null, status_changed_at = now(),
		    cancelled_at = null, cancelled_by = null, cancel_reasons = '{}', cancel_comment = null,
		    last_activity_at = now()
		where id = $1
	`, h.ID, restored); err != nil {
		s.projectFail(w, r)
		return
	}
	if err := s.insertProjectEvent(r.Context(), tx, h.ID, actor.ID, "restored",
		map[string]any{"status": projectStatusCancelled}, map[string]any{"status": restored}, nil); err != nil {
		s.projectFail(w, r)
		return
	}
	h.Status = restored
	s.commitAndRespond(w, r, tx, actor, h, http.StatusOK)
}

// ---- contract / payments -------------------------------------------------------------------

func (s *Server) handleAddProjectContract(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	var req projectContractRequest
	if err := decodeJSON(w, r, 16*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid contract", nil)
		return
	}
	in, fields := validateProjectContract(req)
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid contract", fields)
		return
	}
	const operation = "project.contract.add"
	tx, key, ok := s.beginIdempotentTx(w, r, actor, operation,
		r.PathValue("projectId"), in.Number, in.SignedOn, in.Currency, formatMoneyCents(in.TotalCents), trimmedText(req.FileID))
	if !ok {
		return
	}
	defer tx.Rollback(r.Context())
	h, ok := s.lockEditableProject(w, r, tx, actor, false)
	if !ok {
		return
	}
	var exists bool
	if err := tx.QueryRow(r.Context(), `select exists(select 1 from public.project_contracts where project_id = $1)`, h.ID).Scan(&exists); err != nil {
		s.projectFail(w, r)
		return
	}
	if exists {
		s.writeError(w, r, http.StatusConflict, "contract_exists", "The project already has a contract", nil)
		return
	}
	if in.FileID != nil {
		if err := s.confirmProjectFile(r.Context(), tx, h, *in.FileID, "contract"); err != nil {
			s.writeProjectFileError(w, r, err)
			return
		}
	}
	if _, err := tx.Exec(r.Context(), `
		insert into public.project_contracts (project_id, number, signed_on, total_amount, currency, file_id, created_by)
		values ($1,$2,$3::date,$4::bigint::numeric / 100,$5,$6,$7)
	`, h.ID, in.Number, in.SignedOn, in.TotalCents, in.Currency, in.FileID, actor.ID); err != nil {
		s.projectFail(w, r)
		return
	}
	if _, err := tx.Exec(r.Context(), `
		update public.projects
		set status = 'contract', status_changed_at = now(), event_date = null, event_time = null, last_activity_at = now()
		where id = $1
	`, h.ID); err != nil {
		s.projectFail(w, r)
		return
	}
	newValue := map[string]any{
		"status": projectStatusContract, "number": in.Number, "signedOn": in.SignedOn,
		"totalAmount": centsToMoney(in.TotalCents), "currency": in.Currency,
	}
	if err := s.insertProjectEvent(r.Context(), tx, h.ID, actor.ID, "contract_added",
		map[string]any{"status": h.Status}, newValue, nil); err != nil {
		s.projectFail(w, r)
		return
	}
	h.Status = projectStatusContract
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

func (s *Server) handleAddProjectPayment(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	var req projectPaymentRequest
	if err := decodeJSON(w, r, 16*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid payment", nil)
		return
	}
	in, fields := validateProjectPayment(req)
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid payment", fields)
		return
	}
	const operation = "project.payment.add"
	tx, key, ok := s.beginIdempotentTx(w, r, actor, operation,
		r.PathValue("projectId"), formatMoneyCents(in.AmountCents), in.PaidOn, trimmedText(req.Note), trimmedText(req.FileID))
	if !ok {
		return
	}
	defer tx.Rollback(r.Context())
	h, ok := s.lockEditableProject(w, r, tx, actor, false)
	if !ok {
		return
	}
	var totalCents, paidCents int64
	var currency string
	err := tx.QueryRow(r.Context(), `
		select (c.total_amount * 100)::bigint, c.currency,
		  (select coalesce(sum(pp.amount * 100), 0)::bigint from public.project_payments pp where pp.project_id = c.project_id)
		from public.project_contracts c where c.project_id = $1
	`, h.ID).Scan(&totalCents, &currency, &paidCents)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusConflict, "contract_required", "Add the contract before recording payments", nil)
		return
	}
	if err != nil {
		s.projectFail(w, r)
		return
	}
	if remaining := totalCents - paidCents; in.AmountCents > remaining {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid payment", map[string]string{
			"amount": "More than the remaining " + formatMoneyCents(remaining) + " " + currency + " — check the amount",
		})
		return
	}
	if in.FileID != nil {
		if err := s.confirmProjectFile(r.Context(), tx, h, *in.FileID, "receipt"); err != nil {
			s.writeProjectFileError(w, r, err)
			return
		}
	}
	if _, err := tx.Exec(r.Context(), `
		insert into public.project_payments (project_id, amount, paid_on, note, file_id, created_by)
		values ($1,$2::bigint::numeric / 100,$3::date,$4,$5,$6)
	`, h.ID, in.AmountCents, in.PaidOn, in.Note, in.FileID, actor.ID); err != nil {
		s.projectFail(w, r)
		return
	}
	newValue := map[string]any{
		"amount": centsToMoney(in.AmountCents), "currency": currency, "paidOn": in.PaidOn,
		"paidAfter": centsToMoney(paidCents + in.AmountCents), "total": centsToMoney(totalCents),
	}
	if in.Note != nil {
		newValue["note"] = *in.Note
	}
	if err := s.insertProjectEvent(r.Context(), tx, h.ID, actor.ID, "payment_added", nil, newValue, nil); err != nil {
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

// ---- files ---------------------------------------------------------------------------------

type projectFileUploadRequest struct {
	Kind      string `json:"kind"`
	FileName  string `json:"fileName"`
	SizeBytes int64  `json:"sizeBytes"`
}

// projectFileContentType accepts what a contract scan or a bank receipt can be: PDF or an image.
func projectFileContentType(fileName string) (string, bool) {
	_, contentType, ok := documentContentTypeForFileName(fileName)
	if !ok || contentType == "application/octet-stream" {
		return "", false
	}
	return contentType, true
}

func validateProjectFileUpload(req projectFileUploadRequest) (string, map[string]string) {
	fields := map[string]string{}
	if req.Kind != "contract" && req.Kind != "receipt" {
		fields["kind"] = "Must be contract or receipt"
	}
	name := strings.TrimSpace(req.FileName)
	contentType := ""
	if name == "" {
		fields["fileName"] = "Required"
	} else if ct, ok := projectFileContentType(name); ok {
		contentType = ct
	} else {
		fields["fileName"] = "Must be PDF, JPG, PNG, or HEIC"
	}
	switch {
	case req.SizeBytes <= 0:
		fields["sizeBytes"] = "Required"
	case req.SizeBytes > maxDocumentSizeBytes:
		fields["sizeBytes"] = "Larger than 25 MB — export it as PDF or share a link in the note"
	}
	return contentType, fields
}

func projectFileStorageKey(officeID, projectID, fileID uuid.UUID, fileName string) string {
	return officeID.String() + "/" + projectID.String() + "/" + fileID.String() + "/" + sanitizeDocumentFileName(fileName)
}

func (s *Server) handleCreateProjectFileUpload(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	var req projectFileUploadRequest
	if err := decodeJSON(w, r, 16*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid file upload", nil)
		return
	}
	contentType, fields := validateProjectFileUpload(req)
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid file upload", fields)
		return
	}
	if s.storage == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "file_unavailable", "File storage is not available", nil)
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
	fileID := uuid.New()
	fileName := strings.TrimSpace(req.FileName)
	key := projectFileStorageKey(h.OfficeID, h.ID, fileID, fileName)
	// Sign first: when storage is not configured no pending row is left behind.
	result, err := s.storage.PresignPut(r.Context(), storage.PresignPutInput{
		Bucket: projectFilesBucket, Key: key, ContentType: contentType, Expires: 10 * time.Minute,
	})
	if err != nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "file_unavailable", "File storage is not available", nil)
		return
	}
	if _, err := tx.Exec(r.Context(), `
		insert into public.project_files
		  (id, project_id, kind, file_name, storage_bucket, storage_path, mime_type, size_bytes, status, uploaded_by)
		values ($1,$2,$3,$4,$5,$6,$7,$8,'awaiting_upload',$9)
	`, fileID, h.ID, req.Kind, fileName, projectFilesBucket, key, contentType, req.SizeBytes, actor.ID); err != nil {
		s.projectFail(w, r)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.projectFail(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"fileId":    fileID,
		"uploadUrl": result.URL,
		"method":    result.Method,
		"headers":   result.Headers,
		"expiresAt": result.ExpiresAt,
	})
}

// confirmProjectFile turns a pending upload into a ready file of the given kind. It checks the
// object really exists and its real size (same two-step flow as lead documents), and refuses a
// file that already belongs to another contract or payment.
func (s *Server) confirmProjectFile(ctx context.Context, tx pgx.Tx, h projectHeader, fileID uuid.UUID, kind string) error {
	var bucket, path, status string
	err := tx.QueryRow(ctx, `
		select f.storage_bucket, f.storage_path, f.status
		from public.project_files f
		where f.id = $1 and f.project_id = $2 and f.kind = $3
		  and not exists (select 1 from public.project_contracts c where c.file_id = f.id)
		  and not exists (select 1 from public.project_payments p where p.file_id = f.id)
		for update of f
	`, fileID, h.ID, kind).Scan(&bucket, &path, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return errProjectFileMissing
	}
	if err != nil {
		return err
	}
	if status == "ready" {
		return nil
	}
	if status != "awaiting_upload" || s.storage == nil {
		return errProjectFileMissing
	}
	head, err := s.storage.HeadObject(ctx, storage.HeadObjectInput{Bucket: bucket, Key: path})
	if err != nil || head.SizeBytes <= 0 || head.SizeBytes > maxDocumentSizeBytes {
		return errProjectFileMissing
	}
	_, err = tx.Exec(ctx, `update public.project_files set status = 'ready', size_bytes = $2 where id = $1`, fileID, head.SizeBytes)
	return err
}

func (s *Server) writeProjectFileError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errProjectFileMissing) {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid file", map[string]string{
			"fileId": "File was not uploaded or cannot be used here",
		})
		return
	}
	s.projectFail(w, r)
}
