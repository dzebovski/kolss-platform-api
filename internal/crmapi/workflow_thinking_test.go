package crmapi

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidateActionMarkSuccessful(t *testing.T) {
	amount := 1200.0
	fields := validateAction("mark-successful", actionRequest{
		ContractNumber: "K-1",
		Amount:         &amount,
		Currency:       "uah",
	})
	if len(fields) != 0 {
		t.Fatalf("valid mark-successful payload rejected: %#v", fields)
	}

	fields = validateAction("mark-successful", actionRequest{
		ContractNumber: "K-1",
		Amount:         &amount,
	})
	if fields["currency"] == "" {
		t.Fatal("expected currency validation error")
	}

	fields = validateAction("mark-successful", actionRequest{
		ContractNumber: "K-1",
		Amount:         &amount,
		Currency:       "GBP",
	})
	if fields["currency"] == "" {
		t.Fatal("expected invalid currency rejection")
	}
}


func TestValidateActionUnknownRejected(t *testing.T) {
	fields := validateAction("nope", actionRequest{})
	if fields["action"] == "" {
		t.Fatal("expected unknown action rejection")
	}
}

func TestArchiveOnlyIncludesThinkingLeads(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(file), "leads.go"))
	if err != nil {
		t.Fatal(err)
	}
	want := "(l.archived_at is not null or l.workflow_status = 'thinking')"
	if !strings.Contains(string(src), want) {
		t.Fatalf("leads.go archived=only filter missing thinking: %q", want)
	}
}

func TestDeleteLeadRouteRegistered(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(file), "server.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `r.Post("/v1/leads/{leadId}/delete", s.handleDeleteLead)`) {
		t.Fatal("delete lead route is not registered")
	}
}

func TestValidateActionActivateAndReopen(t *testing.T) {
	for _, action := range []string{"activate", "reopen"} {
		fields := validateAction(action, actionRequest{})
		if len(fields) != 0 {
			t.Fatalf("%s should need no body fields, got %#v", action, fields)
		}
	}
}

func TestReopenAllowsClosedOrSuccessful(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(file), "workflow.go"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(src)
	wantGuard := `action == "reopen" && lead.Workflow != "closed" && lead.Workflow != "successful"`
	if !strings.Contains(content, wantGuard) {
		t.Fatalf("workflow.go reopen guard missing successful: %q", wantGuard)
	}
	if !strings.Contains(content, "Only closed or successful leads can be reopened") {
		t.Fatal("workflow.go reopen error message should mention closed or successful")
	}
	if strings.Contains(content, "Only closed leads can be reopened") {
		t.Fatal("workflow.go still has old closed-only reopen error message")
	}
}

func TestLeadJSONExpressionIncludesReactivatedAt(t *testing.T) {
	if !strings.Contains(leadJSONExpression, "'reactivated_at'") {
		t.Fatal("leadJSONExpression missing reactivated_at")
	}
	if !strings.Contains(leadJSONExpression, "event_type in ('activated', 'reopened')") {
		t.Fatal("leadJSONExpression missing activated/reopened filter")
	}
}
