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
}
