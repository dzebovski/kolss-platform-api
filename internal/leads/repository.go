package leads

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrSiteNotFound           = errors.New("site not found")
	ErrPrivacyVersionMismatch = errors.New("privacy version mismatch")
)

type Site struct {
	Code                 string
	OfficeID             uuid.UUID
	OfficeCode           string
	PrivacyPolicyVersion string
	IsActive             bool
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
