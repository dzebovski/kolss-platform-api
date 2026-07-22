package crmapi

import (
	"testing"
	"time"
)

func TestValidateLeadActivity(t *testing.T) {
	amount := 1250.0
	dueAt := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		request      leadActivityRequest
		isSuperAdmin bool
		field        string
	}{
		{name: "successful call", request: leadActivityRequest{Type: activityCallStatus, Status: "reached", Comment: "Discussed quote"}},
		{name: "successful call requires comment", request: leadActivityRequest{Type: activityCallStatus, Status: "reached"}, field: "comment"},
		{name: "super admin skips successful call comment", request: leadActivityRequest{Type: activityCallStatus, Status: "reached"}, isSuperAdmin: true},
		{name: "no answer", request: leadActivityRequest{Type: activityCallStatus, Status: "no_answer"}},
		{name: "callback", request: leadActivityRequest{Type: activityCallStatus, Status: "callback_requested", DueAt: &dueAt}},
		{name: "callback requires date", request: leadActivityRequest{Type: activityCallStatus, Status: "callback_requested"}, field: "dueAt"},
		{name: "simple status", request: leadActivityRequest{Type: activityClientStatus, Status: "showroom_invited"}},
		{name: "thinking", request: leadActivityRequest{Type: activityClientStatus, Status: "thinking", DueAt: &dueAt}},
		{name: "thinking requires date", request: leadActivityRequest{Type: activityClientStatus, Status: "thinking"}, field: "dueAt"},
		{name: "close", request: leadActivityRequest{Type: activityClientStatus, Status: "closed_lost", Reason: "invalid", Comment: "Duplicate request"}},
		{name: "close requires comment", request: leadActivityRequest{Type: activityClientStatus, Status: "closed_lost", Reason: "other"}, field: "comment"},
		{name: "contract", request: leadActivityRequest{Type: activityClientStatus, Status: "contract_signed", ContractNumber: "K-42", Amount: &amount, Currency: "EUR"}},
		{name: "comment", request: leadActivityRequest{Type: activityComment, Comment: "Customer sent measurements"}},
		{name: "comment with due date", request: leadActivityRequest{Type: activityComment, Comment: "Call back tomorrow", DueAt: &dueAt}},
		{name: "comment rejects status", request: leadActivityRequest{Type: activityComment, Comment: "Note", Status: "reached"}, field: "status"},
		{name: "reopen", request: leadActivityRequest{Type: activityReopen}},
		{name: "reopen rejects comment", request: leadActivityRequest{Type: activityReopen, Comment: "unexpected"}, field: "comment"},
		{name: "unknown type", request: leadActivityRequest{Type: "workflow"}, field: "type"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fields := validateLeadActivity(test.request, test.isSuperAdmin)
			if test.field == "" && len(fields) != 0 {
				t.Fatalf("valid request rejected: %#v", fields)
			}
			if test.field != "" {
				if _, ok := fields[test.field]; !ok {
					t.Fatalf("expected %q validation error, got %#v", test.field, fields)
				}
			}
		})
	}
}
