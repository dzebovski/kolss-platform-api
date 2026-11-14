package crmapi

import (
	"context"
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
//	POST   /v1/projects/{projectId}/status             Change status (one of the six stages)
//	POST   /v1/projects/{projectId}/cancel             Close project (reasons + comment, Q-P6)
//	POST   /v1/projects/{projectId}/finish             Project finished (from Installation, Q-P20)
//	POST   /v1/projects/{projectId}/restore            Reopen a closed or finished project
//	POST   /v1/projects/{projectId}/payments           Add a standalone payment (basis, Q-P21)
//	POST   /v1/projects/{projectId}/files/uploads      Presigned upload for a document / receipt
//	POST   /v1/projects/{projectId}/files              Confirm an uploaded document (Documents tab)
//	DELETE /v1/projects/{projectId}/files/{fileId}     Delete a document
// projects_pipeline.go has estimate, payment edit / delete, comments, details and lead info.
//
// Status, payment and estimate changes need CanEditProject (the responsible manager or an admin,
// Q-P4); documents can be added by anyone with office access.

const projectFilesBucket = leadDocumentsBucket // reuses the private 25 MiB bucket and its S3 setup

var errProjectFileMissing = errors.New("project file is not available")

// lockEditableProject locks the project row, checks edit permission and (unless allowFinal) that
// the project is neither closed nor finished. On failure it writes the response and returns false.
func (s *Server) lockEditableProject(w http.ResponseWriter, r *http.Request, tx pgx.Tx, actor Actor, allowFinal bool) (projectHeader, bool) {
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
	if !allowFinal {
		if ok := s.checkProjectOpen(w, r, h); !ok {
			return projectHeader{}, false
		}
	}
	return h, true
}

// checkProjectOpen refuses changes to a closed or finished project (409, restore it first).
func (s *Server) checkProjectOpen(w http.ResponseWriter, r *http.Request, h projectHeader) bool {
	switch h.Status {
	case projectStatusCancelled:
		s.writeError(w, r, http.StatusConflict, "project_cancelled", "The project is closed — restore it first", nil)
		return false
	case projectStatusCompleted:
		s.writeError(w, r, http.StatusConflict, "project_finished", "The project is finished — reopen it first", nil)
		return false
	}
	return true
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
	var oldDate, oldTime *string
	if err := tx.QueryRow(r.Context(), `
		select to_char(event_date, 'YYYY-MM-DD'), to_char(event_time, 'HH24:MI') from public.projects where id = $1
	`, h.ID).Scan(&oldDate, &oldTime); err != nil {
		s.projectFail(w, r)
		return
	}

	changed := h.Status != in.Status
	if _, err := tx.Exec(r.Context(), `
		update public.projects
		set status = $2,
		    status_changed_at = case when status <> $2 then now() else status_changed_at end,
		    event_date = $3::date,
		    event_time = $4::time,
		    last_activity_at = now()
		where id = $1
	`, h.ID, in.Status, in.EventDate, in.EventTime); err != nil {
		s.projectFail(w, r)
		return
	}

	eventType := "status_updated"
	if changed {
		eventType = "status_changed"
	}
	oldValue := map[string]any{"status": h.Status, "eventDate": oldDate, "eventTime": oldTime}
	newValue := map[string]any{"status": in.Status, "eventDate": in.EventDate, "eventTime": in.EventTime}
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
	if !s.checkProjectOpen(w, r, h) {
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
	restored, ok := restoreTarget(h.Status, h.StatusBeforeCancel)
	if !ok {
		s.writeError(w, r, http.StatusConflict, "project_not_cancelled", "Only a closed or finished project can be restored", nil)
		return
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
		map[string]any{"status": h.Status}, map[string]any{"status": restored}, nil); err != nil {
		s.projectFail(w, r)
		return
	}
	h.Status = restored
	s.commitAndRespond(w, r, tx, actor, h, http.StatusOK)
}

// ---- finish --------------------------------------------------------------------------------

type projectFinishRequest struct {
	Comment *string `json:"comment"`
}

// handleFinishProject: Project finished moves a project from Installation to completed (Q-P20
// default). Restore reopens it at Installation.
func (s *Server) handleFinishProject(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	var req projectFinishRequest
	if err := decodeJSON(w, r, 16*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid request", nil)
		return
	}
	var comment *string
	if text := trimmedText(req.Comment); text != "" {
		if len([]rune(text)) > maxProjectCommentLength {
			s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid request", map[string]string{
				"comment": "Must be at most 1000 characters",
			})
			return
		}
		comment = &text
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
	if !canFinishProject(h.Status) {
		s.writeError(w, r, http.StatusConflict, "project_not_installation", "Move the project to Installation before finishing it", nil)
		return
	}
	if _, err := tx.Exec(r.Context(), `
		update public.projects
		set status = 'completed', status_changed_at = now(), event_date = null, event_time = null,
		    last_activity_at = now()
		where id = $1
	`, h.ID); err != nil {
		s.projectFail(w, r)
		return
	}
	if err := s.insertProjectEvent(r.Context(), tx, h.ID, actor.ID, "finished",
		map[string]any{"status": h.Status}, map[string]any{"status": projectStatusCompleted}, comment); err != nil {
		s.projectFail(w, r)
		return
	}
	h.Status = projectStatusCompleted
	s.commitAndRespond(w, r, tx, actor, h, http.StatusOK)
}

// ---- payments ------------------------------------------------------------------------------

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
		r.PathValue("projectId"), formatMoneyCents(in.AmountCents), in.Currency, in.PaidOn, in.Basis,
		trimmedText(req.Note), trimmedText(req.FileID))
	if !ok {
		return
	}
	defer tx.Rollback(r.Context())
	h, ok := s.lockEditableProject(w, r, tx, actor, false)
	if !ok {
		return
	}
	if in.FileID != nil {
		if err := s.confirmProjectFile(r.Context(), tx, h, *in.FileID, "receipt"); err != nil {
			s.writeProjectFileError(w, r, err)
			return
		}
	}
	if _, err := tx.Exec(r.Context(), `
		insert into public.project_payments (project_id, amount, currency, paid_on, basis, note, file_id, created_by)
		values ($1,$2::bigint::numeric / 100,$3,$4::date,$5,$6,$7,$8)
	`, h.ID, in.AmountCents, in.Currency, in.PaidOn, in.Basis, in.Note, in.FileID, actor.ID); err != nil {
		s.projectFail(w, r)
		return
	}
	record := paymentRecord{Amount: in.AmountCents, Currency: in.Currency, PaidOn: in.PaidOn, Basis: in.Basis, Note: in.Note}
	if err := s.insertProjectEvent(r.Context(), tx, h.ID, actor.ID, "payment_added", nil, record.toMap(), nil); err != nil {
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

// projectFileContentType accepts what a document scan or a receipt can be: PDF or an image.
func projectFileContentType(fileName string) (string, bool) {
	_, contentType, ok := documentContentTypeForFileName(fileName)
	if !ok || contentType == "application/octet-stream" {
		return "", false
	}
	return contentType, true
}

func validateProjectFileUpload(req projectFileUploadRequest) (string, map[string]string) {
	fields := map[string]string{}
	if req.Kind != "document" && req.Kind != "receipt" {
		fields["kind"] = "Must be document or receipt"
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
	// A document can be added by anyone with office access (Q-P4); a receipt goes with a payment,
	// which only the manager and admins record.
	var h projectHeader
	var ok bool
	if req.Kind == "document" {
		h, ok = s.lockDocumentProject(w, r, tx, actor)
	} else {
		h, ok = s.lockEditableProject(w, r, tx, actor, false)
	}
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
// file that already belongs to a payment.
func (s *Server) confirmProjectFile(ctx context.Context, tx pgx.Tx, h projectHeader, fileID uuid.UUID, kind string) error {
	var bucket, path, status string
	err := tx.QueryRow(ctx, `
		select f.storage_bucket, f.storage_path, f.status
		from public.project_files f
		where f.id = $1 and f.project_id = $2 and f.kind = $3
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

// ---- documents -----------------------------------------------------------------------------

// lockDocumentProject: documents are added and removed by anyone with office access, also on a
// finished project, but not on a closed one.
func (s *Server) lockDocumentProject(w http.ResponseWriter, r *http.Request, tx pgx.Tx, actor Actor) (projectHeader, bool) {
	h, ok := s.loadOfficeProject(w, r, tx, actor)
	if ok && h.Status == projectStatusCancelled {
		s.writeError(w, r, http.StatusConflict, "project_cancelled", "The project is closed — restore it first", nil)
		return projectHeader{}, false
	}
	return h, ok
}

type projectDocumentConfirmRequest struct {
	FileID string `json:"fileId"`
}

// handleConfirmProjectDocument turns an uploaded document into a ready file of the Documents tab.
func (s *Server) handleConfirmProjectDocument(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	var req projectDocumentConfirmRequest
	if err := decodeJSON(w, r, 4*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid document", nil)
		return
	}
	fileID, err := uuid.Parse(strings.TrimSpace(req.FileID))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid document", map[string]string{"fileId": "Must be a valid id"})
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.projectFail(w, r)
		return
	}
	defer tx.Rollback(r.Context())
	h, ok := s.lockDocumentProject(w, r, tx, actor)
	if !ok {
		return
	}
	var wasReady bool
	if err := tx.QueryRow(r.Context(), `
		select exists(select 1 from public.project_files where id = $1 and project_id = $2 and status = 'ready')
	`, fileID, h.ID).Scan(&wasReady); err != nil {
		s.projectFail(w, r)
		return
	}
	if err := s.confirmProjectFile(r.Context(), tx, h, fileID, "document"); err != nil {
		s.writeProjectFileError(w, r, err)
		return
	}
	if !wasReady {
		var name string
		if err := tx.QueryRow(r.Context(), `select file_name from public.project_files where id = $1`, fileID).Scan(&name); err != nil {
			s.projectFail(w, r)
			return
		}
		if err := s.insertProjectEvent(r.Context(), tx, h.ID, actor.ID, "file_added", nil,
			map[string]any{"fileId": fileID.String(), "fileName": name}, nil); err != nil {
			s.projectFail(w, r)
			return
		}
	}
	s.commitAndRespond(w, r, tx, actor, h, http.StatusCreated)
}

// handleDeleteProjectDocument removes a document of the Documents tab: its uploader, the project
// manager and admins may. The stored object is left in the bucket (no delete in the storage
// client yet); the row and its download link are gone.
func (s *Server) handleDeleteProjectDocument(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	fileID, err := uuid.Parse(r.PathValue("fileId"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid file id", nil)
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.projectFail(w, r)
		return
	}
	defer tx.Rollback(r.Context())
	h, ok := s.lockDocumentProject(w, r, tx, actor)
	if !ok {
		return
	}
	var name string
	var uploadedBy *uuid.UUID
	err = tx.QueryRow(r.Context(), `
		select file_name, uploaded_by from public.project_files
		where id = $1 and project_id = $2 and kind = 'document'
		  and not exists (select 1 from public.project_payments p where p.file_id = $1)
		for update
	`, fileID, h.ID).Scan(&name, &uploadedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "file_not_found", "File not found", nil)
		return
	}
	if err != nil {
		s.projectFail(w, r)
		return
	}
	if !actor.CanEditProject(h.OfficeID, h.ManagerID) && (uploadedBy == nil || *uploadedBy != actor.ID) {
		s.writeError(w, r, http.StatusForbidden, "project_edit_forbidden",
			"Only the uploader, the project manager and admins can delete this document", nil)
		return
	}
	if _, err := tx.Exec(r.Context(), `delete from public.project_files where id = $1`, fileID); err != nil {
		s.projectFail(w, r)
		return
	}
	if err := s.insertProjectEvent(r.Context(), tx, h.ID, actor.ID, "file_deleted",
		map[string]any{"fileId": fileID.String(), "fileName": name}, nil, nil); err != nil {
		s.projectFail(w, r)
		return
	}
	s.commitAndRespond(w, r, tx, actor, h, http.StatusOK)
}
