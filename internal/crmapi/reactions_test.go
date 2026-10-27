package crmapi

import (
	"os"
	"strings"
	"testing"
)

func TestParseLeadReactionKind(t *testing.T) {
	for _, value := range []string{"like", "star", "question", "cross"} {
		kind, ok := parseLeadReactionKind(value)
		if !ok || string(kind) != value {
			t.Fatalf("parseLeadReactionKind(%q) = %q, %v", value, kind, ok)
		}
	}
	for _, value := range []string{"", "reviewed", "LIKE", "heart"} {
		if _, ok := parseLeadReactionKind(value); ok {
			t.Fatalf("parseLeadReactionKind(%q) unexpectedly succeeded", value)
		}
	}
}

func TestLeadReactionMigrationIsPerUser(t *testing.T) {
	content, err := os.ReadFile("../../supabase/migrations/20261016120000_lead_reactions.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(content)
	for _, fragment := range []string{
		"primary key (lead_id, kind, user_id)",
		"kind in ('like', 'star', 'question')",
		"enable row level security",
		"to kolss_api",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("lead reaction migration missing %q", fragment)
		}
	}
	if strings.Contains(sql, "lead_events") {
		t.Error("reactions must not touch lead_events")
	}
}

func TestLeadJSONExpressionEmbedsReactionsAndActivity(t *testing.T) {
	expr := leadJSONExpression
	for _, fragment := range []string{
		"'reactions'",
		"from public.lead_reactions lr",
		"group by lr.kind",
		"'user_ids', rx.user_ids",
		"'last_activity_at', greatest(",
		"select max(ae.created_at) from public.lead_events ae where ae.lead_id = l.id",
	} {
		if !strings.Contains(expr, fragment) {
			t.Fatalf("leadJSONExpression missing %q\n%s", fragment, expr)
		}
	}
}

func TestLeadReactionCrossMigrationAllowsCross(t *testing.T) {
	content, err := os.ReadFile("../../supabase/migrations/20261027120000_lead_reaction_cross.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "kind in ('like', 'star', 'question', 'cross')") {
		t.Error("cross reaction migration must allow every reaction kind")
	}
}
