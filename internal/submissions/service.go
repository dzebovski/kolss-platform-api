package submissions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dzebovski/kolss-platform-api/internal/leads"
	"github.com/dzebovski/kolss-platform-api/internal/notifications"
	"github.com/dzebovski/kolss-platform-api/internal/storage"
	"github.com/dzebovski/kolss-platform-api/internal/validation"
)

var (
	ErrSiteNotFound           = leads.ErrSiteNotFound
	ErrPrivacyVersionMismatch = leads.ErrPrivacyVersionMismatch
	ErrInvalidToken           = errors.New("invalid submission token")
	ErrSubmissionNotFound     = errors.New("submission not found")
	ErrSubmissionExpired      = errors.New("submission expired")
	ErrSubmissionConflict     = errors.New("submission state conflict")
	ErrUploadIncomplete       = errors.New("upload incomplete")
)

type UploadSession struct {
	FileID       uuid.UUID
	ClientFileID uuid.UUID
	Method       string
	UploadURL    string
	Headers      map[string]string
	ExpiresAt    time.Time
}

type CreateResult struct {
	SubmissionID    uuid.UUID
	Status          string
	Duplicate       bool
	SubmissionToken string
	Uploads         []UploadSession
	LeadID          *uuid.UUID
}

type CompleteResult struct {
	LeadID       uuid.UUID
	SubmissionID uuid.UUID
	Status       string
	Duplicate    bool
	FileCount    int
}

type Service struct {
	pool          *pgxpool.Pool
	sites         *leads.Repository
	objects       storage.ObjectStorage
	notify        notifications.Enqueuer
	pepper        string
	bucket        string
	submissionTTL time.Duration
	presignTTL    time.Duration
}

func NewService(
	pool *pgxpool.Pool,
	sites *leads.Repository,
	objects storage.ObjectStorage,
	notify notifications.Enqueuer,
	pepper, bucket string,
	submissionTTL, presignTTL time.Duration,
) *Service {
	return &Service{
		pool:          pool,
		sites:         sites,
		objects:       objects,
		notify:        notify,
		pepper:        pepper,
		bucket:        bucket,
		submissionTTL: submissionTTL,
		presignTTL:    presignTTL,
	}
}

func (s *Service) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *Service) Create(ctx context.Context, siteCode string, data validation.ValidatedLeadSubmission) (CreateResult, error) {
	site, err := s.sites.GetActiveSite(ctx, siteCode)
	if err != nil {
		return CreateResult{}, err
	}
	if site.PrivacyPolicyVersion != data.PrivacyPolicyVersion {
		return CreateResult{}, ErrPrivacyVersionMismatch
	}

	if existing, ok, err := s.findByIdempotency(ctx, siteCode, data.IdempotencyKey); err != nil {
		return CreateResult{}, err
	} else if ok {
		return s.replayCreate(ctx, existing)
	}

	token, tokenHash, err := s.newToken()
	if err != nil {
		return CreateResult{}, err
	}

	submissionID := uuid.New()
	expiresAt := time.Now().UTC().Add(s.submissionTTL)
	consentedAt := time.Now().UTC()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CreateResult{}, err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
insert into public.lead_submissions (
  id, site_code, idempotency_key, name, phone, email, city, project_description,
  privacy_policy_version, consented_at, page_url, completion_token_hash, status, expires_at
) values (
  $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'awaiting_upload',$13
)`,
		submissionID, siteCode, data.IdempotencyKey, data.Name, data.Phone, data.Email, data.City, data.ProjectDescription,
		data.PrivacyPolicyVersion, consentedAt, data.PageURL, tokenHash, expiresAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			existing, ok, findErr := s.findByIdempotency(ctx, siteCode, data.IdempotencyKey)
			if findErr != nil {
				return CreateResult{}, findErr
			}
			if ok {
				_ = tx.Rollback(ctx)
				return s.replayCreate(ctx, existing)
			}
		}
		return CreateResult{}, err
	}

	uploads := make([]UploadSession, 0, len(data.Files))
	for _, f := range data.Files {
		fileID := uuid.New()
		objectKey := storage.ObjectKey(storage.ObjectKeyParts{
			SiteCode:     siteCode,
			SubmissionID: submissionID.String(),
			FileID:       fileID.String(),
		})
		if _, err := tx.Exec(ctx, `
insert into public.lead_submission_uploads (
  id, submission_id, client_file_id, storage_bucket, storage_path,
  original_filename, declared_content_type, declared_size_bytes, status
) values ($1,$2,$3,$4,$5,$6,$7,$8,'awaiting_upload')
`, fileID, submissionID, f.ClientFileID, s.bucket, objectKey, f.Filename, f.ContentType, f.SizeBytes); err != nil {
			return CreateResult{}, err
		}

		presigned, err := s.objects.PresignPut(ctx, storage.PresignPutInput{
			Bucket:      s.bucket,
			Key:         objectKey,
			ContentType: f.ContentType,
			Expires:     s.presignTTL,
		})
		if err != nil {
			return CreateResult{}, err
		}
		uploads = append(uploads, UploadSession{
			FileID:       fileID,
			ClientFileID: f.ClientFileID,
			Method:       "PUT",
			UploadURL:    presigned.URL,
			Headers:      presigned.Headers,
			ExpiresAt:    presigned.ExpiresAt,
		})
	}

	if err := tx.Commit(ctx); err != nil {
		return CreateResult{}, err
	}

	result := CreateResult{
		SubmissionID:    submissionID,
		Status:          "awaiting_upload",
		Duplicate:       false,
		SubmissionToken: token,
		Uploads:         uploads,
	}

	if len(data.Files) == 0 {
		complete, err := s.Complete(ctx, siteCode, submissionID, token, nil)
		if err != nil {
			return CreateResult{}, err
		}
		leadID := complete.LeadID
		result.Status = "accepted"
		result.LeadID = &leadID
		result.SubmissionToken = ""
		result.Uploads = nil
		result.Duplicate = complete.Duplicate
	}

	return result, nil
}

type submissionRow struct {
	ID                   uuid.UUID
	LeadID               *uuid.UUID
	SiteCode             string
	IdempotencyKey       uuid.UUID
	Name                 string
	Phone                string
	Email                *string
	City                 *string
	ProjectDescription   *string
	PrivacyPolicyVersion string
	PageURL              *string
	TokenHash            string
	Status               string
	ExpiresAt            time.Time
}

func (s *Service) findByIdempotency(ctx context.Context, siteCode string, key uuid.UUID) (submissionRow, bool, error) {
	var row submissionRow
	err := s.pool.QueryRow(ctx, `
select id, lead_id, site_code, idempotency_key, name, phone, email, city, project_description,
       privacy_policy_version, page_url, completion_token_hash, status::text, expires_at
from public.lead_submissions
where site_code = $1 and idempotency_key = $2
`, siteCode, key).Scan(
		&row.ID, &row.LeadID, &row.SiteCode, &row.IdempotencyKey, &row.Name, &row.Phone, &row.Email, &row.City, &row.ProjectDescription,
		&row.PrivacyPolicyVersion, &row.PageURL, &row.TokenHash, &row.Status, &row.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return submissionRow{}, false, nil
	}
	if err != nil {
		return submissionRow{}, false, err
	}
	return row, true, nil
}

func (s *Service) replayCreate(ctx context.Context, existing submissionRow) (CreateResult, error) {
	if existing.Status == "accepted" && existing.LeadID != nil {
		return CreateResult{
			SubmissionID: existing.ID,
			Status:       "accepted",
			Duplicate:    true,
			LeadID:       existing.LeadID,
		}, nil
	}
	if existing.Status != "awaiting_upload" {
		return CreateResult{}, ErrSubmissionConflict
	}
	if time.Now().UTC().After(existing.ExpiresAt) {
		return CreateResult{}, ErrSubmissionExpired
	}

	token, tokenHash, err := s.newToken()
	if err != nil {
		return CreateResult{}, err
	}
	if _, err := s.pool.Exec(ctx, `update public.lead_submissions set completion_token_hash = $2 where id = $1`, existing.ID, tokenHash); err != nil {
		return CreateResult{}, err
	}

	rows, err := s.pool.Query(ctx, `
select id, client_file_id, storage_path, declared_content_type
from public.lead_submission_uploads
where submission_id = $1 and status = 'awaiting_upload'
`, existing.ID)
	if err != nil {
		return CreateResult{}, err
	}
	defer rows.Close()

	var uploads []UploadSession
	for rows.Next() {
		var fileID, clientID uuid.UUID
		var path, contentType string
		if err := rows.Scan(&fileID, &clientID, &path, &contentType); err != nil {
			return CreateResult{}, err
		}
		presigned, err := s.objects.PresignPut(ctx, storage.PresignPutInput{
			Bucket:      s.bucket,
			Key:         path,
			ContentType: contentType,
			Expires:     s.presignTTL,
		})
		if err != nil {
			return CreateResult{}, err
		}
		uploads = append(uploads, UploadSession{
			FileID:       fileID,
			ClientFileID: clientID,
			Method:       "PUT",
			UploadURL:    presigned.URL,
			Headers:      presigned.Headers,
			ExpiresAt:    presigned.ExpiresAt,
		})
	}
	if err := rows.Err(); err != nil {
		return CreateResult{}, err
	}

	return CreateResult{
		SubmissionID:    existing.ID,
		Status:          "awaiting_upload",
		Duplicate:       true,
		SubmissionToken: token,
		Uploads:         uploads,
	}, nil
}

func (s *Service) Complete(ctx context.Context, siteCode string, submissionID uuid.UUID, token string, files []validation.ValidatedCompleteFile) (CompleteResult, error) {
	site, err := s.sites.GetActiveSite(ctx, siteCode)
	if err != nil {
		return CompleteResult{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CompleteResult{}, err
	}
	defer tx.Rollback(ctx)

	var row submissionRow
	err = tx.QueryRow(ctx, `
select id, lead_id, site_code, idempotency_key, name, phone, email, city, project_description,
       privacy_policy_version, page_url, completion_token_hash, status::text, expires_at
from public.lead_submissions
where id = $1 and site_code = $2
for update
`, submissionID, siteCode).Scan(
		&row.ID, &row.LeadID, &row.SiteCode, &row.IdempotencyKey, &row.Name, &row.Phone, &row.Email, &row.City, &row.ProjectDescription,
		&row.PrivacyPolicyVersion, &row.PageURL, &row.TokenHash, &row.Status, &row.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CompleteResult{}, ErrSubmissionNotFound
	}
	if err != nil {
		return CompleteResult{}, err
	}

	if row.Status == "accepted" && row.LeadID != nil {
		count, err := s.countUploads(ctx, tx, row.ID)
		if err != nil {
			return CompleteResult{}, err
		}
		return CompleteResult{
			LeadID:       *row.LeadID,
			SubmissionID: row.ID,
			Status:       "accepted",
			Duplicate:    true,
			FileCount:    count,
		}, nil
	}

	if row.Status != "awaiting_upload" {
		return CompleteResult{}, ErrSubmissionConflict
	}
	if time.Now().UTC().After(row.ExpiresAt) {
		return CompleteResult{}, ErrSubmissionExpired
	}
	if !s.tokenMatches(token, row.TokenHash) {
		return CompleteResult{}, ErrInvalidToken
	}

	uploadRows, err := s.loadUploads(ctx, tx, row.ID)
	if err != nil {
		return CompleteResult{}, err
	}
	if len(files) != len(uploadRows) {
		return CompleteResult{}, ErrUploadIncomplete
	}

	fileMeta := make(map[uuid.UUID]validation.ValidatedCompleteFile, len(files))
	for _, f := range files {
		fileMeta[f.FileID] = f
	}

	for _, u := range uploadRows {
		meta, ok := fileMeta[u.ID]
		if !ok {
			return CompleteResult{}, ErrUploadIncomplete
		}
		head, err := s.objects.Head(ctx, u.Bucket, u.Path)
		if err != nil {
			return CompleteResult{}, ErrUploadIncomplete
		}
		if head.SizeBytes != u.DeclaredSize {
			return CompleteResult{}, ErrUploadIncomplete
		}
		etag := head.ETag
		if meta.ETag != nil && *meta.ETag != "" {
			etag = *meta.ETag
		}
		if _, err := tx.Exec(ctx, `
update public.lead_submission_uploads
set status = 'uploaded', actual_size_bytes = $2, actual_content_type = $3, etag = $4, uploaded_at = now()
where id = $1
`, u.ID, head.SizeBytes, u.DeclaredType, etag); err != nil {
			return CompleteResult{}, err
		}
	}

	externalID := validation.ExternalLeadID(siteCode, row.IdempotencyKey)
	if existing, ok, err := s.findLeadByExternalID(ctx, tx, externalID); err != nil {
		return CompleteResult{}, err
	} else if ok {
		return CompleteResult{
			LeadID:       existing,
			SubmissionID: row.ID,
			Status:       "accepted",
			Duplicate:    true,
			FileCount:    len(uploadRows),
		}, nil
	}

	rawPayload, err := buildRawPayload(siteCode, site.OfficeCode, row)
	if err != nil {
		return CompleteResult{}, err
	}

	var leadID uuid.UUID
	err = tx.QueryRow(ctx, `
insert into public.leads (
  office_id, source_system, external_lead_id, lead_status, lead_status_changed_at,
  workflow_status, workflow_status_changed_at, name, phone, email, city_region, order_comment, raw_payload
) values (
  $1, 'site_form', $2, 'new', now(), 'new', now(), $3, $4, $5, $6, $7, $8::jsonb
)
returning id
`, site.OfficeID, externalID, row.Name, row.Phone, row.Email, row.City, row.ProjectDescription, rawPayload).Scan(&leadID)
	if err != nil {
		if isUniqueViolation(err) {
			existing, ok, findErr := s.findLeadByExternalID(ctx, tx, externalID)
			if findErr != nil {
				return CompleteResult{}, findErr
			}
			if ok {
				leadID = existing
			} else {
				return CompleteResult{}, err
			}
		} else {
			return CompleteResult{}, err
		}
	}

	for _, u := range uploadRows {
		if _, err := tx.Exec(ctx, `
insert into public.lead_attachments (
  lead_id, uploaded_by, file_name, storage_path, mime_type, size_bytes,
  source, status, storage_bucket
) values (
  $1, null, $2, $3, $4, $5, 'site_form', 'pending_scan', $6
)`, leadID, u.Filename, u.Path, u.DeclaredType, u.DeclaredSize, u.Bucket); err != nil {
			return CompleteResult{}, err
		}
		if _, err := tx.Exec(ctx, `update public.lead_submission_uploads set status = 'pending_scan' where id = $1`, u.ID); err != nil {
			return CompleteResult{}, err
		}
	}

	if _, err := tx.Exec(ctx, `
update public.lead_submissions
set lead_id = $2, status = 'accepted', completed_at = now()
where id = $1
`, row.ID, leadID); err != nil {
		return CompleteResult{}, err
	}

	name := row.Name
	phone := row.Phone
	if err := s.notify.Enqueue(ctx, tx, notifications.LeadInfo{
		ID:           leadID,
		Name:         &name,
		Phone:        &phone,
		Email:        row.Email,
		ClientInfo:   row.ProjectDescription,
		OfficeCode:   site.OfficeCode,
		SourceSystem: "site_form",
	}); err != nil {
		return CompleteResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return CompleteResult{}, err
	}

	return CompleteResult{
		LeadID:       leadID,
		SubmissionID: row.ID,
		Status:       "accepted",
		Duplicate:    false,
		FileCount:    len(uploadRows),
	}, nil
}

type uploadRow struct {
	ID           uuid.UUID
	ClientFileID uuid.UUID
	Bucket       string
	Path         string
	Filename     string
	DeclaredType string
	DeclaredSize int64
	Status       string
}

func (s *Service) loadUploads(ctx context.Context, tx pgx.Tx, submissionID uuid.UUID) ([]uploadRow, error) {
	rows, err := tx.Query(ctx, `
select id, client_file_id, storage_bucket, storage_path, original_filename,
       declared_content_type, declared_size_bytes, status::text
from public.lead_submission_uploads
where submission_id = $1
order by created_at
`, submissionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uploadRow
	for rows.Next() {
		var u uploadRow
		if err := rows.Scan(&u.ID, &u.ClientFileID, &u.Bucket, &u.Path, &u.Filename, &u.DeclaredType, &u.DeclaredSize, &u.Status); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Service) countUploads(ctx context.Context, tx pgx.Tx, submissionID uuid.UUID) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `select count(*)::int from public.lead_submission_uploads where submission_id = $1`, submissionID).Scan(&n)
	return n, err
}

func (s *Service) findLeadByExternalID(ctx context.Context, tx pgx.Tx, externalID string) (uuid.UUID, bool, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
select id from public.leads where source_system = 'site_form' and external_lead_id = $1
`, externalID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	return id, true, nil
}

func (s *Service) newToken() (plain, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	plain = hex.EncodeToString(buf)
	hash = s.hashToken(plain)
	return plain, hash, nil
}

func (s *Service) hashToken(token string) string {
	sum := sha256.Sum256([]byte(s.pepper + token))
	return hex.EncodeToString(sum[:])
}

func (s *Service) tokenMatches(token, hash string) bool {
	if token == "" || hash == "" {
		return false
	}
	return s.hashToken(token) == hash
}

func buildRawPayload(siteCode, officeCode string, row submissionRow) ([]byte, error) {
	payload := map[string]any{
		"site_code":              siteCode,
		"office_code":            officeCode,
		"idempotency_key":        row.IdempotencyKey.String(),
		"name":                   row.Name,
		"phone":                  row.Phone,
		"email":                  row.Email,
		"city":                   row.City,
		"project_description":    row.ProjectDescription,
		"privacy_accepted":       true,
		"privacy_policy_version": row.PrivacyPolicyVersion,
		"page_url":               row.PageURL,
		"consented_at":           time.Now().UTC().Format(time.RFC3339),
	}
	return json.Marshal(payload)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
