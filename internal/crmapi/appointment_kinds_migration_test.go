package crmapi

import (
	"os"
	"strings"
	"testing"
)

func TestAppointmentKindsMigrationAddsKindAndWidensUniqueness(t *testing.T) {
	content, err := os.ReadFile("../../supabase/migrations/20260807120000_appointment_kinds.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(content)

	for _, fragment := range []string{
		"add column if not exists kind text not null default 'showroom'",
		"check (kind in ('showroom', 'measurement'))",
		// The per-lead index is replaced by a per-lead-per-kind one so a lead can
		// hold a showroom meeting and a measurement at the same time.
		"drop index if exists lead_showroom_visits_one_scheduled_idx",
		"create unique index if not exists lead_showroom_visits_one_scheduled_kind_idx",
		"on public.lead_showroom_visits (lead_id, kind)",
		"where status = 'scheduled'",
		"'measurement_scheduled'",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("appointment kinds migration missing %q", fragment)
		}
	}

	// Every pre-existing status must survive the widened CHECK constraint.
	for _, status := range []string{
		"'new_lead'",
		"'showroom_invited'",
		"'calculation_in_progress'",
		"'thinking'",
		"'closed_lost'",
		"'contract_signed'",
	} {
		if !strings.Contains(sql, status) {
			t.Errorf("client status check dropped %s", status)
		}
	}

	for _, forbidden := range []string{
		"delete from public.lead_showroom_visits",
		"drop table public.lead_showroom_visits",
		"update public.lead_showroom_visits",
	} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("appointment kinds migration must not rewrite visit history: found %q", forbidden)
		}
	}
}
