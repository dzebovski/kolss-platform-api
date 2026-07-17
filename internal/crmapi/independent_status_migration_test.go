package crmapi

import (
	"os"
	"strings"
	"testing"
)

func TestIndependentStatusMigrationKeepsLegacyDataAndBackfillsSnapshots(t *testing.T) {
	content, err := os.ReadFile("../../supabase/migrations/20260717120000_independent_lead_statuses.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(content)

	clientMappings := []string{
		"when 'new' then 'new_lead'",
		"when 'taken' then 'new_lead'",
		"when 'callback_required' then 'new_lead'",
		"when 'first_call_done' then 'new_lead'",
		"when 'visit_completed' then 'showroom_invited'",
		"when 'thinking' then 'thinking'",
		"when 'closed' then 'closed_lost'",
		"when 'bad_lead' then 'closed_lost'",
		"when 'successful' then 'contract_signed'",
		"when 'prepayment_received' then 'contract_signed'",
	}
	for _, mapping := range clientMappings {
		if !strings.Contains(sql, mapping) {
			t.Errorf("missing client snapshot mapping %q", mapping)
		}
	}

	callMappings := []string{
		"when 'reached' then 'reached'",
		"when 'no_answer' then 'no_answer'",
		"when 'cannot_talk' then 'callback_requested'",
		"when 'bad_lead' then 'reached'",
		"workflow_status = 'callback_required'",
		"workflow_status = 'bad_lead'",
	}
	for _, mapping := range callMappings {
		if !strings.Contains(sql, mapping) {
			t.Errorf("missing call snapshot mapping %q", mapping)
		}
	}

	for _, forbidden := range []string{
		"update public.lead_events",
		"set workflow_status =",
		"set lead_status =",
	} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("migration must not mutate legacy data: found %q", forbidden)
		}
	}
}
