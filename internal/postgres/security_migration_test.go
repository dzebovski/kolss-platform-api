package postgres

import (
	"os"
	"strings"
	"testing"
)

func TestOperationalTablesAreRestrictedToAPIRuntime(t *testing.T) {
	content, err := os.ReadFile("../../supabase/migrations/20260723130000_secure_lead_markers_and_daily_report_runs.sql")
	if err != nil {
		t.Fatal(err)
	}

	sql := string(content)
	for _, fragment := range []string{
		"alter table public.lead_markers enable row level security;",
		"alter table public.daily_report_runs enable row level security;",
		"from public, anon, authenticated;",
		"create policy lead_markers_api_runtime on public.lead_markers",
		"for all to kolss_api",
		"create policy daily_report_runs_api_select on public.daily_report_runs",
		"for select to kolss_api",
		"create policy daily_report_runs_api_insert on public.daily_report_runs",
		"for insert to kolss_api",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("security migration missing %q", fragment)
		}
	}
}
