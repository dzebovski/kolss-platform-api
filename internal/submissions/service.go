package submissions

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dzebovski/kolss-platform-api/internal/leads"
	"github.com/dzebovski/kolss-platform-api/internal/notifications"
	"github.com/dzebovski/kolss-platform-api/internal/validation"
)

var (
	ErrSiteNotFound           = leads.ErrSiteNotFound
	ErrPrivacyVersionMismatch = leads.ErrPrivacyVersionMismatch
	ErrSubmissionConflict     = errors.New("submission state conflict")
)

type CreateResult struct {
	SubmissionID uuid.UUID
	LeadID       uuid.UUID
	Status       string
	Duplicate    bool
}

type Service struct {
	pool   database
	sites  siteRepository
	outbox notifications.Outbox
	waker  notifications.Waker
}

type database interface {
	Ping(ctx context.Context) error
	Begin(ctx context.Context) (pgx.Tx, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type siteRepository interface {
	GetActiveSite(ctx context.Context, siteCode string) (leads.Site, error)
}

func NewService(pool database, sites siteRepository, outbox notifications.Outbox, waker notifications.Waker) *Service {
	return &Service{pool: pool, sites: sites, outbox: outbox, waker: waker}
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
		return replay(existing)
	}

	consentedAt := time.Now().UTC()
	rawPayload, err := buildRawPayload(siteCode, site.OfficeCode, data, consentedAt)
	if err != nil {
		return CreateResult{}, err
	}
	externalID := validation.ExternalLeadID(siteCode, data.IdempotencyKey)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CreateResult{}, err
	}
	defer tx.Rollback(ctx)

	leadID := uuid.Nil
	leadInserted := true
	err = tx.QueryRow(ctx, `
		insert into public.leads (
		  office_id, source_system, external_lead_id, lead_status, lead_status_changed_at,
		  workflow_status, workflow_status_changed_at, name, phone, email, city_region,
		  order_comment, raw_payload
		) values (
		  $1, 'site_form', $2, 'new', now(), 'new', now(), $3, $4, $5, $6, $7, $8::jsonb
		)
		on conflict (source_system, external_lead_id) do nothing
		returning id
	`, site.OfficeID, externalID, data.Name, data.Phone, data.Email, data.City, data.ProjectDescription, rawPayload).Scan(&leadID)
	if errors.Is(err, pgx.ErrNoRows) {
		leadInserted = false
		err = tx.QueryRow(ctx, `
			select id from public.leads
			where source_system='site_form' and external_lead_id=$1
		`, externalID).Scan(&leadID)
	}
	if err != nil {
		return CreateResult{}, err
	}

	submissionID := uuid.New()
	err = tx.QueryRow(ctx, `
		insert into public.lead_submissions (
		  id, lead_id, site_code, idempotency_key, name, phone, email, city,
		  project_description, privacy_policy_version, consented_at, page_url,
		  completion_token_hash, status, expires_at, completed_at
		) values (
		  $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
		  null, 'accepted', null, now()
		)
		on conflict (site_code, idempotency_key) do nothing
		returning id
	`, submissionID, leadID, siteCode, data.IdempotencyKey, data.Name, data.Phone, data.Email, data.City,
		data.ProjectDescription, data.PrivacyPolicyVersion, consentedAt, data.PageURL).Scan(&submissionID)
	if errors.Is(err, pgx.ErrNoRows) {
		var existing submissionRow
		err = tx.QueryRow(ctx, `
			select id, lead_id, status::text
			from public.lead_submissions
			where site_code=$1 and idempotency_key=$2
		`, siteCode, data.IdempotencyKey).Scan(&existing.ID, &existing.LeadID, &existing.Status)
		if err != nil {
			return CreateResult{}, err
		}
		_ = tx.Rollback(ctx)
		return replay(existing)
	}
	if err != nil {
		return CreateResult{}, err
	}

	if leadInserted {
		name, phone := data.Name, data.Phone
		if err := s.outbox.Enqueue(ctx, tx, notifications.LeadInfo{
			ID:           leadID,
			Name:         &name,
			Phone:        &phone,
			Email:        data.Email,
			ClientInfo:   data.ProjectDescription,
			CreatedAt:    &consentedAt,
			OfficeCode:   site.OfficeCode,
			SourceSystem: "site_form",
		}); err != nil {
			return CreateResult{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return CreateResult{}, err
	}
	if leadInserted && s.waker != nil {
		s.waker.Wake()
	}
	return CreateResult{
		SubmissionID: submissionID,
		LeadID:       leadID,
		Status:       "accepted",
		Duplicate:    !leadInserted,
	}, nil
}

type submissionRow struct {
	ID     uuid.UUID
	LeadID *uuid.UUID
	Status string
}

func (s *Service) findByIdempotency(ctx context.Context, siteCode string, key uuid.UUID) (submissionRow, bool, error) {
	var row submissionRow
	err := s.pool.QueryRow(ctx, `
		select id, lead_id, status::text
		from public.lead_submissions
		where site_code=$1 and idempotency_key=$2
	`, siteCode, key).Scan(&row.ID, &row.LeadID, &row.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return submissionRow{}, false, nil
	}
	if err != nil {
		return submissionRow{}, false, err
	}
	return row, true, nil
}

func replay(existing submissionRow) (CreateResult, error) {
	if existing.Status != "accepted" || existing.LeadID == nil {
		return CreateResult{}, ErrSubmissionConflict
	}
	return CreateResult{
		SubmissionID: existing.ID,
		LeadID:       *existing.LeadID,
		Status:       "accepted",
		Duplicate:    true,
	}, nil
}

func buildRawPayload(siteCode, officeCode string, data validation.ValidatedLeadSubmission, consentedAt time.Time) ([]byte, error) {
	return json.Marshal(map[string]any{
		"site_code":              siteCode,
		"office_code":            officeCode,
		"idempotency_key":        data.IdempotencyKey.String(),
		"name":                   data.Name,
		"phone":                  data.Phone,
		"email":                  data.Email,
		"city":                   data.City,
		"project_description":    data.ProjectDescription,
		"privacy_accepted":       true,
		"privacy_policy_version": data.PrivacyPolicyVersion,
		"page_url":               data.PageURL,
		"consented_at":           consentedAt.Format(time.RFC3339),
	})
}
