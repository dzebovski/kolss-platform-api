package crmapi

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidateActionMarkThinking(t *testing.T) {
	fields := validateAction("mark-thinking", actionRequest{})
	if len(fields) != 0 {
		t.Fatalf("mark-thinking should need no body fields, got %#v", fields)
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
