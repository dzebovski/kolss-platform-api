package crmapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLeadJSONExpressionEmbedsChronologicalFirstContactAttempt(t *testing.T) {
	expr := leadJSONExpression
	firstAttemptStart := strings.Index(expr, "'first_contact_attempt'")
	callStatusActorStart := strings.Index(expr, "'call_status_actor'")
	if firstAttemptStart < 0 || callStatusActorStart <= firstAttemptStart {
		t.Fatal("first_contact_attempt expression boundaries not found")
	}
	firstAttemptExpr := expr[firstAttemptStart:callStatusActorStart]

	for _, fragment := range []string{
		"'first_contact_attempt'",
		"from public.lead_contact_attempts a",
		"where a.lead_id = l.id",
		"order by a.created_at asc",
		"limit 1",
		"'result', a.result",
		"'comment', a.comment",
		"'created_at', a.created_at",
		"'manager_id', a.manager_id",
	} {
		if !strings.Contains(firstAttemptExpr, fragment) {
			t.Fatalf("first_contact_attempt expression missing %q\n%s", fragment, firstAttemptExpr)
		}
	}

	if strings.Contains(firstAttemptExpr, "order by a.created_at desc") {
		t.Fatal("first_contact_attempt must use chronological first attempt (asc), not latest (desc)")
	}
}

func TestLeadJSONExpressionEmbedsCurrentCallStatusActor(t *testing.T) {
	expr := leadJSONExpression
	for _, fragment := range []string{
		"'call_status_actor'",
		"when l.call_status is null then null",
		"from public.lead_events e",
		"join public.profiles p on p.id = e.actor_id",
		"e.event_category = 'call_status'",
		"e.status_code = l.call_status",
		"'actor_id', e.actor_id",
		"'actor_name', p.display_name",
		"order by e.created_at desc",
		"from public.lead_contact_attempts a",
		"join public.profiles p on p.id = a.manager_id",
		"'actor_id', a.manager_id",
		"'actor_name', p.display_name",
		"when 'cannot_talk' then 'callback_requested'",
		"end = l.call_status",
		"order by a.created_at desc",
	} {
		if !strings.Contains(expr, fragment) {
			t.Fatalf("leadJSONExpression missing %q\n%s", fragment, expr)
		}
	}
}

func TestCallStatusActorListJSONShape(t *testing.T) {
	actorID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	raw, err := json.Marshal(map[string]any{
		"items": []any{
			map[string]any{
				"id": "22222222-2222-2222-2222-222222222222",
				"call_status_actor": map[string]any{
					"actor_id":   actorID.String(),
					"actor_name": "Kyiv Manager",
				},
			},
			map[string]any{
				"id":                "33333333-3333-3333-3333-333333333333",
				"call_status_actor": nil,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var decoded struct {
		Items []struct {
			ID              string `json:"id"`
			CallStatusActor *struct {
				ActorID   uuid.UUID `json:"actor_id"`
				ActorName string    `json:"actor_name"`
			} `json:"call_status_actor"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Items) != 2 {
		t.Fatalf("items=%d", len(decoded.Items))
	}
	if decoded.Items[0].CallStatusActor == nil {
		t.Fatal("lead with call status actor decoded as nil")
	}
	if decoded.Items[0].CallStatusActor.ActorID != actorID ||
		decoded.Items[0].CallStatusActor.ActorName != "Kyiv Manager" {
		t.Fatalf("unexpected call status actor: %#v", decoded.Items[0].CallStatusActor)
	}
	if decoded.Items[1].CallStatusActor != nil {
		t.Fatalf("lead without call status actor: want nil, got %#v", decoded.Items[1].CallStatusActor)
	}
}

func TestLeadJSONExpressionEmbedsSharedMarkers(t *testing.T) {
	expr := leadJSONExpression
	for _, fragment := range []string{
		"'markers'",
		"from public.lead_markers m",
		"left join public.profiles mp on mp.id = m.actor_id",
		"'kind', m.kind",
		"'actor_id', m.actor_id",
		"'actor_name', coalesce(mp.display_name, '')",
		"'marked_at', m.marked_at",
		"'[]'::jsonb",
	} {
		if !strings.Contains(expr, fragment) {
			t.Fatalf("leadJSONExpression missing %q\n%s", fragment, expr)
		}
	}
}

func TestLeadJSONExpressionEmbedsCallbackDueContext(t *testing.T) {
	expr := leadJSONExpression
	for _, fragment := range []string{
		"'callback_due_context'",
		"e.new_value ? 'callback_due_at'",
		"jsonb_typeof(e.new_value->'callback_due_at') = 'string'",
		"'event_category', e.event_category",
		"'status_code', e.status_code",
		"l.call_status = 'callback_requested'",
	} {
		if !strings.Contains(expr, fragment) {
			t.Fatalf("leadJSONExpression missing %q\n%s", fragment, expr)
		}
	}
}

func TestLeadJSONExpressionEmbedsIndependentShowroomDueDate(t *testing.T) {
	expr := leadJSONExpression
	for _, fragment := range []string{
		"'showroom_due_at'",
		"from public.lead_showroom_visits v",
		"v.lead_id = l.id",
		"v.status = 'scheduled'",
		"order by v.scheduled_at desc",
	} {
		if !strings.Contains(expr, fragment) {
			t.Fatalf("leadJSONExpression missing %q\n%s", fragment, expr)
		}
	}
}

func TestLeadJSONExpressionEmbedsLatestExplicitCommentReminder(t *testing.T) {
	expr := leadJSONExpression
	for _, fragment := range []string{
		"'comment_reminder_due_at'",
		"e.event_category = 'comment'",
		"jsonb_typeof(e.new_value->'callback_due_at') = 'string'",
		"e.new_value->>'callback_due_at'",
		"order by e.created_at desc",
		"limit 1",
	} {
		if !strings.Contains(expr, fragment) {
			t.Fatalf("leadJSONExpression missing %q\n%s", fragment, expr)
		}
	}

	commentReminderStart := strings.Index(expr, "'comment_reminder_due_at'")
	callbackContextStart := strings.Index(expr, "'callback_due_context'")
	if commentReminderStart < 0 || callbackContextStart <= commentReminderStart {
		t.Fatal("comment reminder expression boundaries not found")
	}
	commentReminderExpr := expr[commentReminderStart:callbackContextStart]
	if strings.Contains(commentReminderExpr, "e.new_value ? 'callback_due_at'") {
		t.Fatal("latest comment must be selected before extracting its optional reminder date")
	}
}

func TestFirstContactAttemptListJSONShape(t *testing.T) {
	managerID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	createdAt := time.Date(2026, 7, 14, 14, 14, 0, 0, time.UTC)

	withAttempt := map[string]any{
		"id": "22222222-2222-2222-2222-222222222222",
		"first_contact_attempt": map[string]any{
			"result":     "reached",
			"comment":    "Клиент підтвердив потребу",
			"created_at": createdAt.Format(time.RFC3339),
			"manager_id": managerID.String(),
		},
	}
	withoutAttempt := map[string]any{
		"id":                    "33333333-3333-3333-3333-333333333333",
		"first_contact_attempt": nil,
	}

	raw, err := json.Marshal(map[string]any{"items": []any{withAttempt, withoutAttempt}})
	if err != nil {
		t.Fatal(err)
	}

	var decoded struct {
		Items []struct {
			ID                  string          `json:"id"`
			FirstContactAttempt json.RawMessage `json:"first_contact_attempt"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Items) != 2 {
		t.Fatalf("items=%d", len(decoded.Items))
	}

	var attempt struct {
		Result    string    `json:"result"`
		Comment   string    `json:"comment"`
		CreatedAt time.Time `json:"created_at"`
		ManagerID uuid.UUID `json:"manager_id"`
	}
	if err := json.Unmarshal(decoded.Items[0].FirstContactAttempt, &attempt); err != nil {
		t.Fatalf("lead with attempt: %v", err)
	}
	if attempt.Result != "reached" || attempt.Comment == "" || attempt.ManagerID != managerID {
		t.Fatalf("unexpected attempt: %#v", attempt)
	}

	if string(decoded.Items[1].FirstContactAttempt) != "null" {
		t.Fatalf("lead without attempt: want null, got %s", decoded.Items[1].FirstContactAttempt)
	}
}

func TestLeadJSONExpressionEmbedsContractFromSuccessfulEvent(t *testing.T) {
	expr := leadJSONExpression

	for _, fragment := range []string{
		"'contract'",
		"from public.lead_events e",
		"e.event_type in ('successful', 'contract_signed')",
		"e.new_value ? 'amount'",
		"'contract_number', e.new_value->>'contract_number'",
		"'amount', (e.new_value->>'amount')::numeric",
		"'currency', e.new_value->>'currency'",
		"order by e.created_at desc",
		"limit 1",
	} {
		if !strings.Contains(expr, fragment) {
			t.Fatalf("leadJSONExpression missing %q\n%s", fragment, expr)
		}
	}
}

func TestContractListJSONShape(t *testing.T) {
	signedAt := time.Date(2026, 6, 18, 13, 20, 0, 0, time.UTC)

	withContract := map[string]any{
		"id": "22222222-2222-2222-2222-222222222222",
		"contract": map[string]any{
			"contract_number": "K-KY-2026-0618",
			"amount":          29800,
			"currency":        "EUR",
			"signed_at":       signedAt.Format(time.RFC3339),
		},
	}
	withoutContract := map[string]any{
		"id":       "33333333-3333-3333-3333-333333333333",
		"contract": nil,
	}

	raw, err := json.Marshal(map[string]any{"items": []any{withContract, withoutContract}})
	if err != nil {
		t.Fatal(err)
	}

	var decoded struct {
		Items []struct {
			ID       string          `json:"id"`
			Contract json.RawMessage `json:"contract"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Items) != 2 {
		t.Fatalf("items=%d", len(decoded.Items))
	}

	var contract struct {
		ContractNumber string  `json:"contract_number"`
		Amount         float64 `json:"amount"`
		Currency       string  `json:"currency"`
		SignedAt       string  `json:"signed_at"`
	}
	if err := json.Unmarshal(decoded.Items[0].Contract, &contract); err != nil {
		t.Fatalf("lead with contract: %v", err)
	}
	if contract.ContractNumber != "K-KY-2026-0618" || contract.Amount != 29800 || contract.Currency != "EUR" {
		t.Fatalf("unexpected contract: %#v", contract)
	}

	if string(decoded.Items[1].Contract) != "null" {
		t.Fatalf("lead without contract: want null, got %s", decoded.Items[1].Contract)
	}
}

func TestParseSourceCreatedAtLocalUsesOfficeTimezone(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		officeCode string
		wantUTC    string
	}{
		{name: "Kyiv winter", value: "2026-01-15T12:00", officeCode: "kyiv", wantUTC: "2026-01-15T10:00:00Z"},
		{name: "Kyiv summer", value: "2026-07-20T12:00", officeCode: "kyiv", wantUTC: "2026-07-20T09:00:00Z"},
		{name: "Warsaw winter", value: "2026-01-15T12:00", officeCode: "warsaw", wantUTC: "2026-01-15T11:00:00Z"},
		{name: "Warsaw summer", value: "2026-07-20T12:00", officeCode: "warsaw", wantUTC: "2026-07-20T10:00:00Z"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseSourceCreatedAtLocal(test.value, test.officeCode)
			if err != nil {
				t.Fatal(err)
			}
			if got.UTC().Format(time.RFC3339) != test.wantUTC {
				t.Fatalf("got %s, want %s", got.UTC().Format(time.RFC3339), test.wantUTC)
			}
		})
	}
}

func TestParseSourceCreatedAtLocalRejectsInvalidValues(t *testing.T) {
	for _, test := range []struct {
		name       string
		value      string
		officeCode string
	}{
		{name: "empty", officeCode: "kyiv"},
		{name: "invalid date", value: "2026-02-30T12:00", officeCode: "kyiv"},
		{name: "missing time", value: "2026-07-20", officeCode: "kyiv"},
		{name: "DST gap", value: "2026-03-29T02:30", officeCode: "warsaw"},
		{name: "unknown office", value: "2026-07-20T12:00", officeCode: "london"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseSourceCreatedAtLocal(test.value, test.officeCode); err == nil {
				t.Fatal("expected parsing error")
			}
		})
	}
}

func TestManualLeadCreationUsesSelectedSourceTimestamp(t *testing.T) {
	officeID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	leadID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	selectedAt := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	req := createLeadRequest{
		OfficeID:        officeID,
		Name:            "  Марина  ",
		Phone:           "  +380672148819  ",
		ProductInterest: "Кухня",
	}

	args := createLeadInsertArgs(req, "manual", "office", "crm:external", selectedAt)
	storedAt, ok := args[4].(time.Time)
	if !ok || !storedAt.Equal(selectedAt) {
		t.Fatalf("source_created_at argument = %#v, want %s", args[4], selectedAt)
	}

	notification := manualLeadNotification(leadID, req, "manual", "kyiv", selectedAt)
	if notification.CreatedAt == nil || !notification.CreatedAt.Equal(selectedAt) {
		t.Fatalf("notification CreatedAt = %#v, want %s", notification.CreatedAt, selectedAt)
	}
	if notification.Name == nil || *notification.Name != "Марина" {
		t.Fatalf("notification Name = %#v", notification.Name)
	}
}
