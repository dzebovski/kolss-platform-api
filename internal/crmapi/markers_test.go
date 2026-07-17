package crmapi

import (
	"os"
	"strings"
	"testing"
)

func TestParseLeadMarkerKind(t *testing.T) {
	for _, value := range []string{"reviewed", "manager_aware"} {
		kind, ok := parseLeadMarkerKind(value)
		if !ok || string(kind) != value {
			t.Fatalf("parseLeadMarkerKind(%q) = %q, %v", value, kind, ok)
		}
	}
	for _, value := range []string{"", "seen", "manager-aware", "REVIEWED"} {
		if _, ok := parseLeadMarkerKind(value); ok {
			t.Fatalf("parseLeadMarkerKind(%q) unexpectedly succeeded", value)
		}
	}
}

func TestLeadMarkerMigrationDefinesResetLifecycle(t *testing.T) {
	content, err := os.ReadFile("../../supabase/migrations/20260717180000_lead_markers.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(content)
	for _, fragment := range []string{
		"primary key (lead_id, kind)",
		"kind in ('reviewed', 'manager_aware')",
		"old.assigned_to is distinct from new.assigned_to",
		"kind = 'manager_aware'",
		"after insert or update or delete on public.lead_events",
		"kind = 'reviewed'",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("lead marker migration missing %q", fragment)
		}
	}
	for _, bookkeepingColumn := range []string{"old.updated_at", "old.version", "old.raw_payload"} {
		if strings.Contains(sql, bookkeepingColumn) {
			t.Errorf("review reset must ignore bookkeeping column %q", bookkeepingColumn)
		}
	}
}
