package crmapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQuestionMigrationAndReadProjectionsHaveIntentionalBoundaries(t *testing.T) {
	migration, err := os.ReadFile(filepath.Join("..", "..", "supabase", "migrations", "20260909120000_lead_questions.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(migration), "'question'") {
		t.Fatal("question migration must permit the question event category")
	}

	latestStart := strings.Index(leadJSONExpression, "'latest_timeline_comment'")
	reminderStart := strings.Index(leadJSONExpression, "'comment_reminder_due_at'")
	if latestStart < 0 || reminderStart <= latestStart {
		t.Fatal("latest timeline comment expression boundaries not found")
	}
	latestExpr := leadJSONExpression[latestStart:reminderStart]
	for _, fragment := range []string{
		"e.comment is not null",
		"btrim(e.comment) <> ''",
		"order by e.created_at desc",
		"limit 1",
	} {
		if !strings.Contains(latestExpr, fragment) {
			t.Fatalf("latest timeline comment expression missing %q\n%s", fragment, latestExpr)
		}
	}
	if strings.Contains(latestExpr, "and e.event_category") {
		t.Fatalf("latest timeline comment must include text from every event category\n%s", latestExpr)
	}

	reports, err := os.ReadFile("reports.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(reports), "event_category is distinct from 'question'") < 2 {
		t.Fatal("question events must be excluded from report activity and recent comments")
	}
}
