package crmapi

import (
	"os"
	"strings"
	"testing"
)

func TestCloseLeadClearsRemindersMigrationBackfillsTerminalLeads(t *testing.T) {
	content, err := os.ReadFile("../../supabase/migrations/20260814120000_close_lead_clears_reminders.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(content)

	for _, fragment := range []string{
		// Pending callbacks left on leads closed before the write path cleared them.
		"update public.leads",
		"callback_due_at = null",
		// Visits still scheduled on closed leads — GET /v1/appointments reads
		// lead_showroom_visits directly and does not filter by lead status.
		"update public.lead_showroom_visits v",
		"status = 'canceled'",
		"v.status = 'scheduled'",
		"version = v.version + 1",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("close-lead reminder migration missing %q", fragment)
		}
	}

	if got := strings.Count(sql, "client_status in ('closed_lost', 'contract_signed')"); got != 2 {
		t.Errorf("both backfills must be scoped to terminal leads: got %d scoped statements, want 2", got)
	}
}
