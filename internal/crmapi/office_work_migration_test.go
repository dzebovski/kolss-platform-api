package crmapi

import (
	"os"
	"strings"
	"testing"
)

func TestOfficeWorkMigrationAllowsSeveralActiveBlocks(t *testing.T) {
	content, err := os.ReadFile("../../supabase/migrations/20260904120000_office_work_appointments.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(content)

	for _, fragment := range []string{
		"check (kind in ('showroom', 'measurement', 'office_work'))",
		"drop index if exists lead_showroom_visits_one_scheduled_kind_idx",
		"on public.lead_showroom_visits (lead_id, kind)",
		"where status = 'scheduled'",
		"and kind in ('showroom', 'measurement')",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("office work migration missing %q", fragment)
		}
	}
}
