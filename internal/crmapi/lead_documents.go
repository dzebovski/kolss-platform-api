package crmapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dzebovski/kolss-platform-api/internal/storage"
)

// CRM v2 "Add documents" popup (task W11): a two-step upload so a 25 MB file never streams
// through the API (BODY_LIMIT_BYTES defaults to 64 KB and the API is a single container).
//
//  1. POST /v1/leads/{leadId}/documents/uploads — validate the declared name/size, create a
//     pending public.lead_attachments row (status = awaiting_upload), return a presigned PUT URL
//     the browser uploads directly to object storage.
//  2. POST /v1/leads/{leadId}/documents — confirm: HEAD the object to verify it was actually
//     uploaded and check its real size, mark the row ready, write one timeline event with the
//     tag and the optional note.
//
// Both return the LeadDocument shape (camelCase), never the raw table row.
//
// GET /v1/leads/{leadId}/documents lists the confirmed (status = ready) documents; downloads
// reuse the existing GET /v1/files/{fileId}/download-url, which already gates on status = ready.

const (
	leadDocumentsBucket   = "lead-attachments"
	maxDocumentSizeBytes  = 25 * 1024 * 1024 // board: "PDF, JPG, PNG, HEIC, DWG · up to 25 MB each"
	maxDocumentNoteLength = 1000
)

// leadDocumentTags are the Add documents board's TAGS, in snake_case.
var leadDocumentTags = map[string]struct{}{
	"plan": {}, "photo": {}, "drawing": {}, "estimate": {}, "other": {},
}

// leadDocumentContentTypes maps the board's allowed extensions (case-insensitive) to a content
// type. DWG has no reliable standard MIME type, so it is stored as application/octet-stream, as
// most browsers and OSes already send it.
var leadDocumentContentTypes = map[string]string{
	"pdf":  "application/pdf",
	"jpg":  "image/jpeg",
	"jpeg": "image/jpeg",
	"png":  "image/png",
	"heic": "image/heic",
	"dwg":  "application/octet-stream",
}

// documentContentTypeForFileName derives the extension and content type from a file name (never
// trusts a client-declared content type). Pure logic, unit-tested on its own.
func documentContentTypeForFileName(fileName string) (ext, contentType string, ok bool) {
	name := strings.TrimSpace(fileName)
	dot := strings.LastIndex(name, ".")
	if dot < 0 || dot == len(name)-1 {
		return "", "", false
	}
	ext = strings.ToLower(name[dot+1:])
	contentType, ok = leadDocumentContentTypes[ext]
	return ext, contentType, ok
}

// sanitizeDocumentFileName keeps the storage key readable and free of path separators, without
// rejecting the many legitimate filename characters (accents, spaces, parentheses...). Pure
// logic, unit-tested on its own.
func sanitizeDocumentFileName(fileName string) string {
	name := strings.TrimSpace(fileName)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '/' || r == '\\':
			b.WriteRune('_')
		case r < 0x20:
			// drop control characters
		default:
			b.WriteRune(r)
		}
	}
	sanitized := strings.TrimSpace(b.String())
	if sanitized == "" {
		return "file"
	}
	return sanitized
}

// leadDocumentStorageKey builds the object key. The office/lead/attachment prefix mirrors the
// storage.objects RLS policies' existing convention (office id first segment) even though this
// flow's presigned URLs are signed with the service S3 key and bypass those policies entirely;
// keeping the convention avoids a second, inconsistent key shape in the same bucket. Pure logic,
// unit-tested on its own.
func leadDocumentStorageKey(officeID, leadID, attachmentID uuid.UUID, fileName string) string {
	return officeID.String() + "/" + leadID.String() + "/" + attachmentID.String() + "/" + sanitizeDocumentFileName(fileName)
}

type createDocumentUploadRequest struct {
	FileName  string `json:"fileName"`
	SizeBytes int64  `json:"sizeBytes"`
}

// validateCreateDocumentUpload is pure (no DB, no storage call), unit-tested on its own.
func validateCreateDocumentUpload(req createDocumentUploadRequest) (ext, contentType string, fields map[string]string) {
	fields = map[string]string{}
	name := strings.TrimSpace(req.FileName)
	if name == "" {
		fields["fileName"] = "Required"
		return "", "", fields
	}
	var ok bool
	ext, contentType, ok = documentContentTypeForFileName(name)
	if !ok {
		fields["fileName"] = "Must be PDF, JPG, PNG, HEIC, or DWG"
	}
	switch {
	case req.SizeBytes <= 0:
		fields["sizeBytes"] = "Required"
	case req.SizeBytes > maxDocumentSizeBytes:
		fields["sizeBytes"] = "Larger than 25 MB — export it as PDF or share a link in the note"
	}
	return ext, contentType, fields
}

func (s *Server) handleCreateDocumentUpload(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	leadID, err := uuid.Parse(r.PathValue("leadId"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid lead id", nil)
		return
	}
	var req createDocumentUploadRequest
	if err := decodeJSON(w, r, 16*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid document upload", nil)
		return
	}
	_, contentType, fields := validateCreateDocumentUpload(req)
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid document upload", fields)
		return
	}
	if s.storage == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "file_unavailable", "File storage is not available", nil)
		return
	}
	var officeID uuid.UUID
	if err := s.pool.QueryRow(r.Context(), `select office_id from public.leads where id=$1 and archived_at is null`, leadID).Scan(&officeID); err != nil {
		s.writeError(w, r, http.StatusNotFound, "lead_not_found", "Lead not found", nil)
		return
	}
	if !actor.CanEditLead(officeID) {
		s.writeError(w, r, http.StatusForbidden, "lead_edit_forbidden", "Lead editing is not allowed", nil)
		return
	}

	attachmentID := uuid.New()
	fileName := strings.TrimSpace(req.FileName)
	key := leadDocumentStorageKey(officeID, leadID, attachmentID, fileName)
	// Sign first: when storage is not configured no pending row is left behind.
	result, err := s.storage.PresignPut(r.Context(), storage.PresignPutInput{
		Bucket:      leadDocumentsBucket,
		Key:         key,
		ContentType: contentType,
		Expires:     10 * time.Minute,
	})
	if err != nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "file_unavailable", "File storage is not available", nil)
		return
	}
	if _, err := s.pool.Exec(r.Context(), `
		insert into public.lead_attachments
		  (id, lead_id, uploaded_by, file_name, storage_path, storage_bucket, mime_type, size_bytes, status)
		values ($1,$2,$3,$4,$5,$6,$7,$8,'awaiting_upload')
	`, attachmentID, leadID, actor.ID, fileName, key, leadDocumentsBucket, contentType, req.SizeBytes); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "document_upload_failed", "Could not start the upload", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"attachmentId": attachmentID,
		"uploadUrl":    result.URL,
		"method":       result.Method,
		"headers":      result.Headers,
		"expiresAt":    result.ExpiresAt,
	})
}

type confirmDocumentRequest struct {
	AttachmentID string `json:"attachmentId"`
	Tag          string `json:"tag"`
	Note         string `json:"note"`
}

// validateConfirmDocument is pure (no DB, no storage call), unit-tested on its own.
func validateConfirmDocument(req confirmDocumentRequest) (attachmentID uuid.UUID, tag *string, note *string, fields map[string]string) {
	fields = map[string]string{}
	id, err := uuid.Parse(strings.TrimSpace(req.AttachmentID))
	if err != nil {
		fields["attachmentId"] = "Required"
	}
	trimmedTag := strings.TrimSpace(req.Tag)
	if trimmedTag != "" {
		if _, ok := leadDocumentTags[trimmedTag]; !ok {
			fields["tag"] = "Must be plan, photo, drawing, estimate, or other"
		} else {
			tag = &trimmedTag
		}
	}
	note = clean(req.Note)
	if note != nil && len([]rune(*note)) > maxDocumentNoteLength {
		fields["note"] = "Must be at most 1000 characters"
	}
	return id, tag, note, fields
}

func (s *Server) handleConfirmDocument(w http.ResponseWriter, r *http.Request) {
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
	var req confirmDocumentRequest
	if err := decodeJSON(w, r, 16*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid document", nil)
		return
	}
	attachmentID, tag, note, fields := validateConfirmDocument(req)
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid document", fields)
		return
	}
	if s.storage == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "file_unavailable", "File storage is not available", nil)
		return
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "document_confirm_failed", "Could not confirm the document", nil)
		return
	}
	defer tx.Rollback(r.Context())

	requestHash := sha256.Sum256([]byte(leadID.String() + "|" + attachmentID.String() + "|" + req.Tag + "|" + req.Note))
	cached, cachedStatus, cachedBody, err := claimIdempotency(r, tx, actor.ID, "lead.document.confirm", key, hex.EncodeToString(requestHash[:]))
	if err != nil {
		s.writeError(w, r, http.StatusConflict, "idempotency_conflict", "Idempotency key was already used for another request", nil)
		return
	}
	if cached {
		if err := tx.Commit(r.Context()); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "document_confirm_failed", "Could not confirm the document", nil)
			return
		}
		writeJSON(w, cachedStatus, cachedBody)
		return
	}

	var officeID uuid.UUID
	var bucket, storagePath, fileName, status string
	err = tx.QueryRow(r.Context(), `
		select l.office_id, a.storage_bucket, a.storage_path, a.file_name, a.status::text
		from public.lead_attachments a join public.leads l on l.id=a.lead_id
		where a.id=$1 and a.lead_id=$2 and l.archived_at is null
		for update of a
	`, attachmentID, leadID).Scan(&officeID, &bucket, &storagePath, &fileName, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "document_not_found", "Document not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "document_confirm_failed", "Could not confirm the document", nil)
		return
	}
	if !actor.CanEditLead(officeID) {
		s.writeError(w, r, http.StatusForbidden, "lead_edit_forbidden", "Lead editing is not allowed", nil)
		return
	}
	if status != "awaiting_upload" {
		s.writeError(w, r, http.StatusConflict, "document_already_confirmed", "Document was already confirmed", nil)
		return
	}

	head, err := s.storage.HeadObject(r.Context(), storage.HeadObjectInput{Bucket: bucket, Key: storagePath})
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "document_not_uploaded", "File was not uploaded yet", nil)
		return
	}
	if head.SizeBytes <= 0 || head.SizeBytes > maxDocumentSizeBytes {
		// Best-effort: mark the row blocked instead of leaving it stuck awaiting_upload forever.
		// The object itself is not deleted here (kept simple; an orphan-cleanup sweep is a
		// separate concern — see the report).
		_, _ = tx.Exec(r.Context(), `update public.lead_attachments set status='blocked' where id=$1`, attachmentID)
		_ = tx.Commit(r.Context())
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid document", map[string]string{
			"sizeBytes": "Larger than 25 MB — export it as PDF or share a link in the note",
		})
		return
	}

	var raw []byte
	err = tx.QueryRow(r.Context(), `
		with updated as (
			update public.lead_attachments set status='ready', size_bytes=$2, tag=$3
			where id=$1
			returning *
		)
		select jsonb_build_object(
			'id', a.id, 'fileName', a.file_name, 'mimeType', a.mime_type, 'sizeBytes', a.size_bytes,
			'tag', a.tag, 'uploadedBy', a.uploaded_by, 'uploadedByName', coalesce(p.display_name, ''),
			'createdAt', a.created_at)
		from updated a left join public.profiles p on p.id = a.uploaded_by
	`, attachmentID, head.SizeBytes, tag).Scan(&raw)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "document_confirm_failed", "Could not confirm the document", nil)
		return
	}

	newValue := map[string]any{"attachment_id": attachmentID, "file_name": fileName, "tag": tag}
	if note != nil {
		newValue["note"] = *note
	}
	// event_type "attachment" reuses the CRM v1/v2 title already shipped for it
	// (event.attachment: Attachment / Вкладення / Załącznik) — no new v1 fallback needed.
	if _, err := tx.Exec(r.Context(), `
		insert into public.lead_events (lead_id, actor_id, event_type, event_category, new_value)
		values ($1,$2,'attachment','system',$3)
	`, leadID, actor.ID, newValue); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "document_confirm_failed", "Could not confirm the document", nil)
		return
	}

	if _, err := tx.Exec(r.Context(), `
		update public.api_idempotency_keys
		set response_status=$4, response_body=$5::jsonb
		where actor_id=$1 and operation=$2 and idempotency_key=$3
	`, actor.ID, "lead.document.confirm", key, http.StatusCreated, raw); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "document_confirm_failed", "Could not confirm the document", nil)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "document_confirm_failed", "Could not confirm the document", nil)
		return
	}
	writeJSON(w, http.StatusCreated, json.RawMessage(raw))
}

func (s *Server) handleListDocuments(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	leadID, err := uuid.Parse(r.PathValue("leadId"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid lead id", nil)
		return
	}
	var officeID uuid.UUID
	if err := s.pool.QueryRow(r.Context(), `select office_id from public.leads where id=$1 and archived_at is null`, leadID).Scan(&officeID); err != nil {
		s.writeError(w, r, http.StatusNotFound, "lead_not_found", "Lead not found", nil)
		return
	}
	if !actor.CanAccessOffice(officeID) {
		s.writeError(w, r, http.StatusForbidden, "office_forbidden", "Office access denied", nil)
		return
	}
	rows, err := s.pool.Query(r.Context(), `
		select jsonb_build_object(
			'id', a.id, 'fileName', a.file_name, 'mimeType', a.mime_type, 'sizeBytes', a.size_bytes,
			'tag', a.tag, 'uploadedBy', a.uploaded_by, 'uploadedByName', coalesce(p.display_name, ''),
			'createdAt', a.created_at)
		from public.lead_attachments a left join public.profiles p on p.id = a.uploaded_by
		where a.lead_id=$1 and a.status='ready'
		order by a.created_at desc
	`, leadID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "documents_load_failed", "Could not load documents", nil)
		return
	}
	defer rows.Close()
	items := []json.RawMessage{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "documents_load_failed", "Could not load documents", nil)
			return
		}
		items = append(items, json.RawMessage(raw))
	}
	if err := rows.Err(); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "documents_load_failed", "Could not load documents", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
