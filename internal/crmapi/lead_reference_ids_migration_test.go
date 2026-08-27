package crmapi

import (
	"os"
	"strings"
	"testing"
)

func TestLeadReferenceIDsMigrationBackfillsAndProtectsReferences(t *testing.T) {
	content, err := os.ReadFile("../../supabase/migrations/20260827120000_lead_reference_ids.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(content)

	for _, fragment := range []string{
		"create table private.lead_reference_counters",
		"when 'kyiv' then 'k'",
		"when 'warsaw' then 'w'",
		"partition by l.office_id",
		"order by l.created_at, l.id",
		"lpad(ranked.reference_number::text, 4, '0')",
		"alter column reference_id set not null",
		"add constraint leads_reference_id_unique unique (reference_id)",
		"set last_value = last_value + 1",
		"lead office_id is immutable",
		"lead reference_id is immutable",
		"before insert or update of office_id, reference_id on public.leads",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("lead reference migration missing %q", fragment)
		}
	}
}
