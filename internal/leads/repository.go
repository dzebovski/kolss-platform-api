package leads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dzebovski/kolss-platform-api/internal/validation"
)

var (
	ErrSiteNotFound            = errors.New("site not found")
	ErrPrivacyVersionMismatch  = errors.New("privacy version mismatch")
)

type Site struct {
	Code                 string
	OfficeID             uuid.UUID
	OfficeCode           string
	PrivacyPolicyVersion string
	IsActive             bool
}

type CreateLeadInput struct {
	SiteCode string
	Data     validation.ValidatedLeadSubmission
}

type CreateLeadResult struct {
	LeadID    uuid.UUID
	Duplicate bool
}

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) Ping(ctx context.Context) error {
	return r.pool.Ping(ctx)
}

func (r *Repository) GetActiveSite(ctx context.Context, siteCode string) (Site, error) {
	const q = `
select s.code, s.office_id, o.code as office_code, s.privacy_policy_version, s.is_active
from public.sites s
join public.offices o on o.id = s.office_id
where s.code = $1 and s.is_active = true and o.is_active = true
`
	var site Site
	err := r.pool.QueryRow(ctx, q, siteCode).Scan(
		&site.Code,
		&site.OfficeID,
		&site.OfficeCode,
		&site.PrivacyPolicyVersion,
		&site.IsActive,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Site{}, ErrSiteNotFound
	}
	if err != nil {
		return Site{}, err
	}
	return site, nil
}

func (r *Repository) CreateLead(ctx context.Context, in CreateLeadInput) (CreateLeadResult, error) {
	site, err := r.GetActiveSite(ctx, in.SiteCode)
	if err != nil {
		return CreateLeadResult{}, err
	}
	if site.PrivacyPolicyVersion != in.Data.PrivacyPolicyVersion {
		return CreateLeadResult{}, ErrPrivacyVersionMismatch
	}

	externalID := validation.ExternalLeadID(in.SiteCode, in.Data.IdempotencyKey)

	if existing, ok, err := r.findByExternalID(ctx, externalID); err != nil {
		return CreateLeadResult{}, err
	} else if ok {
		return CreateLeadResult{LeadID: existing, Duplicate: true}, nil
	}

	rawPayload, err := buildRawPayload(in.SiteCode, site.OfficeCode, in.Data)
	if err != nil {
		return CreateLeadResult{}, err
	}

	const insertQ = `
insert into public.leads (
  office_id,
  source_system,
  external_lead_id,
  lead_status,
  lead_status_changed_at,
  workflow_status,
  workflow_status_changed_at,
  name,
  phone,
  email,
  city_region,
  order_comment,
  raw_payload
) values (
  $1, 'site_form', $2, 'new', now(), 'new', now(),
  $3, $4, $5, $6, $7, $8::jsonb
)
returning id
`
	var leadID uuid.UUID
	err = r.pool.QueryRow(
		ctx,
		insertQ,
		site.OfficeID,
		externalID,
		in.Data.Name,
		in.Data.Phone,
		in.Data.Email,
		in.Data.City,
		in.Data.ProjectDescription,
		rawPayload,
	).Scan(&leadID)
	if err != nil {
		if isUniqueViolation(err) {
			existing, ok, findErr := r.findByExternalID(ctx, externalID)
			if findErr != nil {
				return CreateLeadResult{}, findErr
			}
			if ok {
				return CreateLeadResult{LeadID: existing, Duplicate: true}, nil
			}
		}
		return CreateLeadResult{}, err
	}

	return CreateLeadResult{LeadID: leadID, Duplicate: false}, nil
}

func (r *Repository) findByExternalID(ctx context.Context, externalID string) (uuid.UUID, bool, error) {
	const q = `
select id
from public.leads
where source_system = 'site_form' and external_lead_id = $1
`
	var id uuid.UUID
	err := r.pool.QueryRow(ctx, q, externalID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	return id, true, nil
}

func buildRawPayload(siteCode, officeCode string, data validation.ValidatedLeadSubmission) ([]byte, error) {
	payload := map[string]any{
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
		"consented_at":           time.Now().UTC().Format(time.RFC3339),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal raw_payload: %w", err)
	}
	return b, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
