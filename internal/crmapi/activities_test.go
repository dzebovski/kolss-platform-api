package crmapi

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestValidateLeadActivity(t *testing.T) {
	amount := 1250.0
	dueAt := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	assignee := uuid.MustParse("44444444-4444-4444-4444-444444444444")
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
		{name: "showroom requires date", request: leadActivityRequest{Type: activityClientStatus, Status: "showroom_invited"}, field: "dueAt"},
		{name: "showroom with date", request: leadActivityRequest{Type: activityClientStatus, Status: "showroom_invited", DueAt: &dueAt}},
		{name: "calculation rejects date", request: leadActivityRequest{Type: activityClientStatus, Status: "calculation_in_progress", DueAt: &dueAt}, field: "dueAt"},
		{name: "thinking", request: leadActivityRequest{Type: activityClientStatus, Status: "thinking", DueAt: &dueAt}},
		{name: "thinking without date", request: leadActivityRequest{Type: activityClientStatus, Status: "thinking"}},
		{name: "thinking with comment", request: leadActivityRequest{Type: activityClientStatus, Status: "thinking", Comment: "Asked for time to decide"}},
		{name: "postponed", request: leadActivityRequest{Type: activityClientStatus, Status: "postponed", DueAt: &dueAt, Comment: "Renovation starts in spring"}},
		{name: "postponed without date", request: leadActivityRequest{Type: activityClientStatus, Status: "postponed", Comment: "No concrete date yet"}},
		{name: "postponed requires comment", request: leadActivityRequest{Type: activityClientStatus, Status: "postponed", DueAt: &dueAt}, field: "comment"},
		{name: "close", request: leadActivityRequest{Type: activityClientStatus, Status: "closed_lost", Reason: "invalid", Comment: "Duplicate request"}},
		{name: "close with no_contact reason", request: leadActivityRequest{Type: activityClientStatus, Status: "closed_lost", Reason: "no_contact", Comment: "Unreachable after 5 attempts"}},
		{name: "close requires comment", request: leadActivityRequest{Type: activityClientStatus, Status: "closed_lost", Reason: "other"}, field: "comment"},
		{name: "contract", request: leadActivityRequest{Type: activityClientStatus, Status: "contract_signed", ContractNumber: "K-42", Amount: &amount, Currency: "EUR"}},
		{name: "comment", request: leadActivityRequest{Type: activityComment, Comment: "Customer sent measurements"}},
		{name: "rating", request: leadActivityRequest{Type: activityRating, Rating: "hot"}},
		{name: "rating must be known", request: leadActivityRequest{Type: activityRating, Rating: "warm"}, field: "rating"},
		{name: "rating rejects comment", request: leadActivityRequest{Type: activityRating, Rating: "cold", Comment: "note"}, field: "comment"},
		{name: "rating only on rating activity", request: leadActivityRequest{Type: activityComment, Comment: "x", Rating: "hot"}, field: "rating"},
		{name: "comment with due date", request: leadActivityRequest{Type: activityComment, Comment: "Call back tomorrow", DueAt: &dueAt}},
		{name: "comment with assignee and date", request: leadActivityRequest{Type: activityComment, Comment: "Task for manager", DueAt: &dueAt, AssignedTo: &assignee}},
		{name: "comment assignee requires date", request: leadActivityRequest{Type: activityComment, Comment: "Task for manager", AssignedTo: &assignee}, field: "dueAt"},
		{name: "comment rejects status", request: leadActivityRequest{Type: activityComment, Comment: "Note", Status: "reached"}, field: "status"},
		{name: "call rejects assignee", request: leadActivityRequest{Type: activityCallStatus, Status: "reached", Comment: "Discussed quote", AssignedTo: &assignee}, field: "assignedTo"},
		{name: "clear callback reminder", request: leadActivityRequest{Type: activityClearReminder, Kind: reminderKindCallback}},
		{name: "clear thinking reminder", request: leadActivityRequest{Type: activityClearReminder, Kind: reminderKindThinking}},
		{name: "clear comment reminder", request: leadActivityRequest{Type: activityClearReminder, Kind: reminderKindComment}},
		{name: "clear showroom reminder", request: leadActivityRequest{Type: activityClearReminder, Kind: reminderKindShowroom}},
		{name: "clear reminder requires kind", request: leadActivityRequest{Type: activityClearReminder}, field: "kind"},
		{name: "clear reminder rejects unknown kind", request: leadActivityRequest{Type: activityClearReminder, Kind: "visit"}, field: "kind"},
		{name: "clear reminder rejects dueAt", request: leadActivityRequest{Type: activityClearReminder, Kind: reminderKindCallback, DueAt: &dueAt}, field: "dueAt"},
		{name: "clear reminder rejects comment", request: leadActivityRequest{Type: activityClearReminder, Kind: reminderKindCallback, Comment: "nope"}, field: "comment"},
		{name: "question activity", request: leadActivityRequest{Type: activityQuestion, Comment: "Який розмір кухні?", AssigneeIDs: []uuid.UUID{assignee}}},
		{name: "question requires comment", request: leadActivityRequest{Type: activityQuestion}, field: "comment"},
		{name: "question rejects status", request: leadActivityRequest{Type: activityQuestion, Comment: "Питання", Status: "reached"}, field: "status"},
		{name: "question rejects dueAt", request: leadActivityRequest{Type: activityQuestion, Comment: "Питання", DueAt: &dueAt}, field: "dueAt"},
		{name: "comment rejects assigneeIds", request: leadActivityRequest{Type: activityComment, Comment: "Note", AssigneeIDs: []uuid.UUID{assignee}}, field: "assigneeIds"},
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

func TestClientStatusUnchanged(t *testing.T) {
	tests := []struct {
		name    string
		current string
		request leadActivityRequest
		want    bool
	}{
		{name: "repeated showroom is allowed", current: "showroom_invited", request: leadActivityRequest{Type: activityClientStatus, Status: "showroom_invited"}},
		{name: "repeated thinking is rejected", current: "thinking", request: leadActivityRequest{Type: activityClientStatus, Status: "thinking"}, want: true},
		{name: "repeated postponed is rejected", current: "postponed", request: leadActivityRequest{Type: activityClientStatus, Status: "postponed"}, want: true},
		{name: "different client status is allowed", current: "thinking", request: leadActivityRequest{Type: activityClientStatus, Status: "showroom_invited"}},
		{name: "call status is unrelated", current: "thinking", request: leadActivityRequest{Type: activityCallStatus, Status: "thinking"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := clientStatusUnchanged(test.current, test.request); got != test.want {
				t.Fatalf("clientStatusUnchanged() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestNextClientStatusCallbackDue(t *testing.T) {
	callback := "callback_requested"
	reached := "reached"
	current := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	replacement := time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)

	if got := nextClientStatusCallbackDue(&reached, &current, "showroom_invited", &replacement); got != nil {
		t.Fatalf("showroom date must not use callback_due_at: got %v, want nil", got)
	}
	if got := nextClientStatusCallbackDue(&reached, &current, "showroom_invited", nil); got != nil {
		t.Fatalf("cleared showroom: got %v, want nil", got)
	}
	if got := nextClientStatusCallbackDue(&callback, &current, "showroom_invited", nil); got == nil || !got.Equal(current) {
		t.Fatalf("active callback must be preserved: got %v, want %v", got, current)
	}
	if got := nextClientStatusCallbackDue(&reached, &current, "thinking", &replacement); got == nil || !got.Equal(replacement) {
		t.Fatalf("thinking date: got %v, want %v", got, replacement)
	}
	if got := nextClientStatusCallbackDue(&reached, &current, "postponed", &replacement); got == nil || !got.Equal(replacement) {
		t.Fatalf("postponed date: got %v, want %v", got, replacement)
	}
	if got := nextClientStatusCallbackDue(&reached, &current, "postponed", nil); got != nil {
		t.Fatalf("cleared postponed: got %v, want nil", got)
	}

	// A closed lead keeps no reminder, not even a pending callback.
	for _, status := range []string{"closed_lost", "contract_signed"} {
		if got := nextClientStatusCallbackDue(&callback, &current, status, nil); got != nil {
			t.Errorf("%s must drop the pending callback: got %v, want nil", status, got)
		}
		if got := nextClientStatusCallbackDue(&callback, &current, status, &replacement); got != nil {
			t.Errorf("%s must ignore a requested date: got %v, want nil", status, got)
		}
	}
}

func TestIsTerminalClientStatus(t *testing.T) {
	terminal := map[string]bool{
		"closed_lost":             true,
		"contract_signed":         true,
		"new_lead":                false,
		"showroom_invited":        false,
		"measurement_scheduled":   false,
		"calculation_in_progress": false,
		"thinking":                false,
		"postponed":               false,
		"":                        false,
	}
	for status, want := range terminal {
		if got := isTerminalClientStatus(status); got != want {
			t.Errorf("isTerminalClientStatus(%q) = %v, want %v", status, got, want)
		}
	}
}

func TestQuestionTranslationsAndPermissions(t *testing.T) {
	translations, fields := normalizeQuestionTranslations(map[string]string{"pl": "Pytanie", "EN": "Question"})
	if len(fields) != 0 || translations["PL"] != "Pytanie" || translations["EN"] != "Question" {
		t.Fatalf("normalizeQuestionTranslations valid = %#v, %#v", translations, fields)
	}
	if _, fields := normalizeQuestionTranslations(map[string]string{"DE": "Frage"}); fields["translations"] == "" {
		t.Fatalf("unsupported language must be rejected: %#v", fields)
	}
	officeID := uuid.New()
	for _, test := range []struct { role string; member, want bool }{
		{"office_member", true, false}, {"office_admin", true, true}, {"curator", true, true}, {"super_admin", false, true}, {"curator", false, false},
	} {
		actor := Actor{Role: test.role, OfficeIDs: map[uuid.UUID]struct{}{}}
		if test.member { actor.OfficeIDs[officeID] = struct{}{} }
		if got := actor.CanAskLeadQuestions(officeID); got != test.want { t.Errorf("%s member=%v: got %v, want %v", test.role, test.member, got, test.want) }
	}
}

func TestApplyCommentActivityValuesStoresAssignee(t *testing.T) {
	dueAt := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	assignee := uuid.MustParse("44444444-4444-4444-4444-444444444444")

	withAssignee := map[string]any{}
	applyCommentActivityValues(leadActivityRequest{Type: activityComment, Comment: "Task", DueAt: &dueAt, AssignedTo: &assignee}, withAssignee)
	stored, ok := withAssignee["assigned_to"].(*uuid.UUID)
	if !ok || stored == nil || *stored != assignee {
		t.Fatalf("assigned_to not stored: %#v", withAssignee["assigned_to"])
	}
	if withAssignee["callback_due_at"] == nil {
		t.Fatalf("callback_due_at must remain stored alongside the assignee: %#v", withAssignee)
	}

	withoutAssignee := map[string]any{}
	applyCommentActivityValues(leadActivityRequest{Type: activityComment, Comment: "Plain note"}, withoutAssignee)
	if _, present := withoutAssignee["assigned_to"]; present {
		t.Fatalf("assigned_to must be absent when no manager is assigned: %#v", withoutAssignee)
	}
}

func TestShouldClearLeadDueForCommentReminder(t *testing.T) {
	dueAt := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	other := time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)

	if !shouldClearLeadDueForCommentReminder(&dueAt, &dueAt) {
		t.Fatal("matching dues must clear the lead date")
	}
	if shouldClearLeadDueForCommentReminder(&dueAt, &other) {
		t.Fatal("mismatched dues must keep the lead date")
	}
	if shouldClearLeadDueForCommentReminder(&dueAt, nil) {
		t.Fatal("missing comment due must keep the lead date")
	}
	if shouldClearLeadDueForCommentReminder(nil, &dueAt) {
		t.Fatal("missing lead due is a no-op")
	}
}

func TestCommentAssigneeExistsQueryRestrictsToOfficeStaff(t *testing.T) {
	for _, fragment := range []string{
		"from public.profiles p",
		"join public.user_office_memberships m on m.user_id = p.id",
		"p.id = $1",
		"p.is_active = true",
		"p.role <> 'super_admin'",
		"m.office_id = $2",
	} {
		if !strings.Contains(commentAssigneeExistsQuery, fragment) {
			t.Fatalf("commentAssigneeExistsQuery missing %q\n%s", fragment, commentAssigneeExistsQuery)
		}
	}
}

func TestValidateV2StatusActivity(t *testing.T) {
	dueAt := time.Date(2026, time.September, 25, 11, 0, 0, 0, time.UTC)
	budget := "20 000 – 25 000"
	badBudget := "about 20k"
	city := "Mokotów"
	designer := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	tests := []struct {
		name    string
		request leadActivityRequest
		field   string
	}{
		{name: "successful call without answers", request: leadActivityRequest{Type: activityV2Status, Status: "success"}},
		{name: "successful call with answers", request: leadActivityRequest{Type: activityV2Status, Status: "success", DueAt: &dueAt, EstimatedBudgetText: &budget, EstimatedBudgetCurrency: "PLN", CityRegion: &city, Products: []string{"kitchen", "wardrobe"}, NextAction: "Send a quote"}},
		{name: "budget must parse", request: leadActivityRequest{Type: activityV2Status, Status: "success", EstimatedBudgetText: &badBudget}, field: "estimatedBudgetText"},
		{name: "unknown product", request: leadActivityRequest{Type: activityV2Status, Status: "success", Products: []string{"garage"}}, field: "products"},
		{name: "call later requires date", request: leadActivityRequest{Type: activityV2Status, Status: "later"}, field: "dueAt"},
		{name: "no answer requires next attempt", request: leadActivityRequest{Type: activityV2Status, Status: "noanswer"}, field: "dueAt"},
		{name: "no answer rejects budget", request: leadActivityRequest{Type: activityV2Status, Status: "noanswer", DueAt: &dueAt, EstimatedBudgetText: &budget}, field: "estimatedBudgetText"},
		{name: "unknown v2 status", request: leadActivityRequest{Type: activityV2Status, Status: "reached"}, field: "status"},
		{name: "thinking requires follow-up", request: leadActivityRequest{Type: activityV2Status, Status: "thinking"}, field: "dueAt"},
		{name: "thinking", request: leadActivityRequest{Type: activityV2Status, Status: "thinking", DueAt: &dueAt}},
		{name: "invited requires designer", request: leadActivityRequest{Type: activityV2Status, Status: "invited", DueAt: &dueAt}, field: "designerId"},
		{name: "invited", request: leadActivityRequest{Type: activityV2Status, Status: "invited", DueAt: &dueAt, DesignerID: &designer}},
		{name: "designer only for invited", request: leadActivityRequest{Type: activityV2Status, Status: "later", DueAt: &dueAt, DesignerID: &designer}, field: "designerId"},
		{name: "lost requires a v2 reason", request: leadActivityRequest{Type: activityV2Status, Status: "lost", LossReason: "expensive"}, field: "lossReason"},
		{name: "lost rejects date", request: leadActivityRequest{Type: activityV2Status, Status: "lost", LossReason: "out_of_budget", DueAt: &dueAt}, field: "dueAt"},
		{name: "lost", request: leadActivityRequest{Type: activityV2Status, Status: "lost", LossReason: "bought_elsewhere", Comment: "Chose a cheaper studio"}},
		{name: "lost reasons (plural) requires at least one", request: leadActivityRequest{Type: activityV2Status, Status: "lost", LossReasons: []string{}}, field: "lossReasons"},
		{name: "lost reasons not allowed with lossReason", request: leadActivityRequest{Type: activityV2Status, Status: "lost", LossReason: "other", LossReasons: []string{"price_too_high"}}, field: "lossReason"},
		{name: "lost reasons with other requires a comment", request: leadActivityRequest{Type: activityV2Status, Status: "lost", LossReasons: []string{"price_too_high", "other"}}, field: "comment"},
		{name: "lost reasons with other and a comment", request: leadActivityRequest{Type: activityV2Status, Status: "lost", LossReasons: []string{"price_too_high", "other"}, Comment: "Went with a competitor, price was the deciding factor"}},
		{name: "lost reasons without other needs no comment", request: leadActivityRequest{Type: activityV2Status, Status: "lost", LossReasons: []string{"price_too_high", "project_postponed"}}},
		{name: "lost reasons only allowed for lost", request: leadActivityRequest{Type: activityV2Status, Status: "thinking", DueAt: &dueAt, LossReasons: []string{"other"}}, field: "lossReasons"},
		{name: "v1 activity rejects v2 fields", request: leadActivityRequest{Type: activityComment, Comment: "x", Products: []string{"kitchen"}}, field: "products"},
		{name: "v1 activity rejects lossReasons", request: leadActivityRequest{Type: activityComment, Comment: "x", LossReasons: []string{"other"}}, field: "lossReasons"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fields := validateLeadActivity(test.request, false)
			if test.field == "" && len(fields) > 0 {
				t.Fatalf("unexpected errors: %#v", fields)
			}
			if test.field != "" && fields[test.field] == "" {
				t.Fatalf("expected %s error, got %#v", test.field, fields)
			}
		})
	}
}

func TestParseBudgetText(t *testing.T) {
	for text, want := range map[string]float64{
		"20000":             20000,
		"20 000":            20000,
		"20 000,50":    20000.5,
		"20 000 – 25 000":   20000,
		"20000-25000":       20000,
		" 15 000 — 18 000 ": 15000,
	} {
		got, ok := parseBudgetText(text)
		if !ok || got != want {
			t.Errorf("parseBudgetText(%q) = %v, %v; want %v", text, got, ok, want)
		}
	}
	for _, text := range []string{"", "20k", "25 000 – 20 000", "2 00", "-5", "20 000 – 25 000 – 30 000"} {
		if _, ok := parseBudgetText(text); ok {
			t.Errorf("parseBudgetText(%q) must fail", text)
		}
	}
}

func TestDeriveV2LeadStatus(t *testing.T) {
	str := func(value string) *string { return &value }
	tests := []struct {
		client string
		call   *string
		want   *string
	}{
		{"new_lead", nil, str("new")},
		{"new_lead", str("callback_requested"), str("later")},
		{"new_lead", str("no_answer"), str("noanswer")},
		{"new_lead", str("reached"), str("success")},
		{"thinking", str("reached"), str("thinking")},
		{"showroom_invited", nil, str("invited")},
		{"closed_lost", str("no_answer"), str("lost")},
		{"measurement_scheduled", str("reached"), nil},
		{"postponed", nil, nil},
	}
	for _, test := range tests {
		got := deriveV2LeadStatus(test.client, test.call)
		if !equalStringPtr(got, test.want) {
			t.Errorf("deriveV2LeadStatus(%s, %v) = %v, want %v", test.client, test.call, got, test.want)
		}
	}
}

func TestPickMirroredLossReason(t *testing.T) {
	tests := []struct {
		name    string
		reasons []string
		want    string
	}{
		{name: "a v1/v2-labelled reason first", reasons: []string{"bought_elsewhere", "price_too_high"}, want: "bought_elsewhere"},
		{name: "a labelled reason later in the list", reasons: []string{"price_too_high", "project_postponed", "cant_reach_client"}, want: "cant_reach_client"},
		{name: "none labelled falls back to other", reasons: []string{"price_too_high", "project_postponed", "quote_only"}, want: "other"},
		{name: "other itself", reasons: []string{"other"}, want: "other"},
		{name: "empty falls back to other", reasons: []string{}, want: "other"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := pickMirroredLossReason(test.reasons); got != test.want {
				t.Fatalf("pickMirroredLossReason(%v) = %q, want %q", test.reasons, got, test.want)
			}
		})
	}
}
