package crmapi

import (
	"os"
	"strings"
	"testing"
)

func TestAppointmentsMigrationBackfillsCanonicalRangesAndPreservesHistory(t *testing.T) {
	content, err := os.ReadFile("../../supabase/migrations/20260723120000_appointments_calendar.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(content)
	for _, fragment := range []string{
		"when 'warsaw' then 'Europe/Warsaw'",
		"when 'kyiv' then 'Europe/Kyiv'",
		"add column if not exists ends_at timestamptz",
		"add column if not exists responsible_manager_id uuid",
		"add column if not exists updated_by uuid",
		"add column if not exists version bigint not null default 1",
		"v.scheduled_at + interval '60 minutes'",
		"coalesce(\n    v.responsible_manager_id,\n    l.assigned_to,\n    v.created_by",
		"on delete set null",
		"where status = 'scheduled'",
		"status = 'rescheduled'",
		"lead_showroom_visits_one_scheduled_idx",
		"lead_showroom_visits_range_status_idx",
		"lead_showroom_visits_manager_range_idx",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("appointments migration missing %q", fragment)
		}
	}
	for _, forbidden := range []string{
		"delete from public.lead_showroom_visits",
		"drop table public.lead_showroom_visits",
	} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("appointments migration must retain visit history: found %q", forbidden)
		}
	}
}

func TestAppointmentsMigrationBackfillsOnlyValidLegacyInvitations(t *testing.T) {
	content, err := os.ReadFile("../../supabase/migrations/20260723120000_appointments_calendar.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(content)
	for _, fragment := range []string{
		"l.archived_at is null",
		"l.client_status = 'showroom_invited'",
		"e.event_category = 'client_status'",
		"e.status_code = 'showroom_invited'",
		"jsonb_typeof(e.new_value->'callback_due_at') = 'string'",
		"not exists (\n  select 1\n  from public.lead_showroom_visits v",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("legacy invitation backfill missing %q", fragment)
		}
	}
}
