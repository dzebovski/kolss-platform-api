package crmapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dzebovski/kolss-platform-api/internal/storage"
)

func (s *Server) handleFileDownloadURL(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	fileID, err := uuid.Parse(r.PathValue("fileId"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid file id", nil)
		return
	}
	var officeID uuid.UUID
	var bucket, path, filename, status string
	err = s.pool.QueryRow(r.Context(), `
		select l.office_id,a.storage_bucket,a.storage_path,a.file_name,a.status::text
		from public.lead_attachments a join public.leads l on l.id=a.lead_id
		where a.id=$1 and l.archived_at is null
	`, fileID).Scan(&officeID, &bucket, &path, &filename, &status)
	if errors.Is(err, pgx.ErrNoRows) || !actor.CanAccessOffice(officeID) {
		s.writeError(w, r, http.StatusNotFound, "file_not_found", "File not found", nil)
		return
	}
	if err != nil || status != "ready" || s.storage == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "file_unavailable", "File is not available", nil)
		return
	}
	result, err := s.storage.PresignGet(r.Context(), storage.PresignGetInput{Bucket: bucket, Key: path, Filename: filename, Expires: 10 * time.Minute})
	if err != nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "file_unavailable", "File is not available", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": result.URL, "expiresAt": result.ExpiresAt})
}
