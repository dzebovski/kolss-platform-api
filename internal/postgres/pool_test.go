package postgres

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestPoolConfigUsesPoolerSafeQueryMode(t *testing.T) {
	cfg, err := poolConfig("postgres://postgres:password@localhost:5432/postgres")
	if err != nil {
		t.Fatalf("poolConfig() error = %v", err)
	}

	if got, want := cfg.ConnConfig.DefaultQueryExecMode, pgx.QueryExecModeCacheDescribe; got != want {
		t.Errorf("DefaultQueryExecMode = %v, want %v", got, want)
	}
	if got, want := cfg.ConnConfig.RuntimeParams["lock_timeout"], "5s"; got != want {
		t.Errorf("lock_timeout = %q, want %q", got, want)
	}
	if got, want := cfg.ConnConfig.RuntimeParams["statement_timeout"], "20s"; got != want {
		t.Errorf("statement_timeout = %q, want %q", got, want)
	}
	if got, want := cfg.MaxConns, int32(10); got != want {
		t.Errorf("MaxConns = %d, want %d", got, want)
	}
}
