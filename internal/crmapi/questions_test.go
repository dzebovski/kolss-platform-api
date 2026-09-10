package crmapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQuestionMigrationAndReadProjectionsStayIsolated(t *testing.T) {
	migration, err := os.ReadFile(filepath.Join("..", "..", "supabase", "migrations", "20260909120000_lead_questions.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(migration), "'question'") {
		t.Fatal("question migration must permit the question event category")
	}
	if !strings.Contains(leadJSONExpression, "'latest_timeline_comment'") || !strings.Contains(leadJSONExpression, "e.event_category = 'comment'") {
		t.Fatal("latest timeline comment must only project comment events")
	}
	reports, err := os.ReadFile("reports.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(reports), "event_category is distinct from 'question'") < 2 {
		t.Fatal("question events must be excluded from report activity and recent comments")
	}
}
