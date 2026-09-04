package crmapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestClientStatusFilterWhere(t *testing.T) {
	addArg := func(value any) string {
		return fmt.Sprintf("%q", value)
	}

	tests := []struct {
		name    string
		raw     string
		wantOK  bool
		wantSQL []string
	}{
		{
			name:   "new lead only without call",
			raw:    "new_lead",
			wantOK: true,
			wantSQL: []string{
				`l.client_status = "new_lead"`,
				"l.call_status is null",
			},
		},
		{
			name:   "in progress new lead with call",
			raw:    "in_work",
			wantOK: true,
			wantSQL: []string{
				`l.client_status = "new_lead"`,
				"l.call_status is not null",
			},
		},
		{
			name:    "exact client status",
			raw:     "thinking",
			wantOK:  true,
			wantSQL: []string{`l.client_status = "thinking"`},
		},
		{
			name:    "measurement scheduled",
			raw:     "measurement_scheduled",
			wantOK:  true,
			wantSQL: []string{`l.client_status = "measurement_scheduled"`},
		},
		{
			name:    "postponed",
			raw:     "postponed",
			wantOK:  true,
			wantSQL: []string{`l.client_status = "postponed"`},
		},
		{
			name:   "unknown status",
			raw:    "taken",
			wantOK: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := clientStatusFilterWhere(test.raw, addArg)
			if ok != test.wantOK {
				t.Fatalf("ok=%v, want %v", ok, test.wantOK)
			}
			if !test.wantOK {
				if got != nil {
					t.Fatalf("clauses=%v, want nil", got)
				}
				return
			}
			if len(got) != len(test.wantSQL) {
				t.Fatalf("clauses=%v, want %v", got, test.wantSQL)
			}
			for i := range got {
				if got[i] != test.wantSQL[i] {
					t.Fatalf("clause[%d]=%q, want %q", i, got[i], test.wantSQL[i])
				}
			}
		})
	}
}

func TestSplitQueryValues(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "normal split", raw: "reached,no_answer", want: []string{"reached", "no_answer"}},
		{name: "empty string", raw: "", want: []string{}},
		{name: "trims and drops empty elements", raw: " reached , ,no_answer ", want: []string{"reached", "no_answer"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := splitQueryValues(test.raw)
			if len(got) != len(test.want) {
				t.Fatalf("values=%v, want %v", got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Fatalf("values=%v, want %v", got, test.want)
				}
			}
		})
	}
}

func TestLeadSearchWhereIncludesExactCaseInsensitiveReference(t *testing.T) {
	args := []any{}
	addArg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}

	clause := leadSearchWhere("K0042", addArg)
	for _, fragment := range []string{
		"coalesce(l.name, '') ilike $1",
		"coalesce(l.phone, '') ilike $1",
		"coalesce(l.email, '') ilike $1",
		"l.reference_id = $2",
	} {
		if !strings.Contains(clause, fragment) {
			t.Errorf("search clause missing %q: %s", fragment, clause)
		}
	}
	if got, want := args, []any{"%K0042%", "k0042"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("args=%v, want %v", got, want)
	}
}

func TestClientStatusFilterWhereMulti(t *testing.T) {
	addArg := func(value any) string {
		return fmt.Sprintf("%q", value)
	}

	tests := []struct {
		name    string
		values  []string
		wantOK  bool
		wantSQL string
	}{
		{name: "no values", values: nil, wantOK: true, wantSQL: ""},
		{
			name:    "single value",
			values:  []string{"thinking"},
			wantOK:  true,
			wantSQL: `l.client_status = "thinking"`,
		},
		{
			name:    "ORs multi-clause and single-clause groups together",
			values:  []string{"new_lead", "thinking"},
			wantOK:  true,
			wantSQL: `((l.client_status = "new_lead" and l.call_status is null) or l.client_status = "thinking")`,
		},
		{
			name:   "unknown value fails the whole set",
			values: []string{"thinking", "taken"},
			wantOK: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := clientStatusFilterWhereMulti(test.values, addArg)
			if ok != test.wantOK {
				t.Fatalf("ok=%v, want %v", ok, test.wantOK)
			}
			if test.wantOK && got != test.wantSQL {
				t.Fatalf("sql=%q, want %q", got, test.wantSQL)
			}
		})
	}
}

func TestCallStatusFilterWhere(t *testing.T) {
	addArg := func(value any) string {
		return fmt.Sprintf("%q", value)
	}

	tests := []struct {
		name    string
		raw     string
		wantOK  bool
		wantSQL []string
	}{
		{
			name:    "none means no call recorded",
			raw:     "none",
			wantOK:  true,
			wantSQL: []string{"l.call_status is null"},
		},
		{
			name:   "callback_undated pairs status with a null due date",
			raw:    "callback_undated",
			wantOK: true,
			wantSQL: []string{
				`l.call_status = "callback_requested"`,
				"l.callback_due_at is null",
			},
		},
		{
			name:    "reached exact status",
			raw:     "reached",
			wantOK:  true,
			wantSQL: []string{`l.call_status = "reached"`},
		},
		{
			name:    "no_answer exact status",
			raw:     "no_answer",
			wantOK:  true,
			wantSQL: []string{`l.call_status = "no_answer"`},
		},
		{
			name:    "callback_requested exact status",
			raw:     "callback_requested",
			wantOK:  true,
			wantSQL: []string{`l.call_status = "callback_requested"`},
		},
		{
			name:   "unknown status",
			raw:    "escalated",
			wantOK: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := callStatusFilterWhere(test.raw, addArg)
			if ok != test.wantOK {
				t.Fatalf("ok=%v, want %v", ok, test.wantOK)
			}
			if !test.wantOK {
				if got != nil {
					t.Fatalf("clauses=%v, want nil", got)
				}
				return
			}
			if len(got) != len(test.wantSQL) {
				t.Fatalf("clauses=%v, want %v", got, test.wantSQL)
			}
			for i := range got {
				if got[i] != test.wantSQL[i] {
					t.Fatalf("clause[%d]=%q, want %q", i, got[i], test.wantSQL[i])
				}
			}
		})
	}
}

func TestCallStatusFilterWhereMulti(t *testing.T) {
	addArg := func(value any) string {
		return fmt.Sprintf("%q", value)
	}

	tests := []struct {
		name    string
		values  []string
		wantOK  bool
		wantSQL string
	}{
		{name: "no values", values: nil, wantOK: true, wantSQL: ""},
		{
			name:    "single exact status",
			values:  []string{"reached"},
			wantOK:  true,
			wantSQL: `l.call_status = "reached"`,
		},
		{
			// This is the digest's "Недозвон + перезвон" cohort. A no_answer lead can
			// legitimately keep a non-null callback_due_at (see applyLeadActivity in
			// activities.go, which preserves callback_due_at when client_status is
			// thinking), so callback_undated must stay its own OR'd clause group rather
			// than an AND'd "callback_due_at is null" applied across the whole filter —
			// otherwise it would wrongly exclude such no_answer leads from this cohort.
			name:    "no_answer OR undated callback keeps no_answer leads with a due date",
			values:  []string{"no_answer", "callback_undated"},
			wantOK:  true,
			wantSQL: `(l.call_status = "no_answer" or (l.call_status = "callback_requested" and l.callback_due_at is null))`,
		},
		{
			name:    "ORs multi-clause and single-clause groups together",
			values:  []string{"callback_undated", "reached"},
			wantOK:  true,
			wantSQL: `((l.call_status = "callback_requested" and l.callback_due_at is null) or l.call_status = "reached")`,
		},
		{
			name:   "unknown value fails the whole set",
			values: []string{"reached", "escalated"},
			wantOK: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := callStatusFilterWhereMulti(test.values, addArg)
			if ok != test.wantOK {
				t.Fatalf("ok=%v, want %v", ok, test.wantOK)
			}
			if test.wantOK && got != test.wantSQL {
				t.Fatalf("sql=%q, want %q", got, test.wantSQL)
			}
		})
	}
}

// TestCallStatusNoAnswerWithDueDateStillMatchesCallbackUndatedGroup is the
// explicit regression test for the §3 correctness trap: a no_answer lead
// that still carries a non-null callback_due_at (possible when
// client_status is thinking — applyLeadActivity in activities.go only clears
// callback_due_at on a call-status change when client_status isn't thinking)
// must still be matched by callStatus=no_answer,callback_undated, because the
// no_answer branch of the OR does not reference callback_due_at at all.
func TestCallStatusNoAnswerWithDueDateStillMatchesCallbackUndatedGroup(t *testing.T) {
	addArg := func(value any) string {
		return fmt.Sprintf("%q", value)
	}

	sql, ok := callStatusFilterWhereMulti([]string{"no_answer", "callback_undated"}, addArg)
	if !ok {
		t.Fatal("expected ok=true")
	}
	const want = `(l.call_status = "no_answer" or (l.call_status = "callback_requested" and l.callback_due_at is null))`
	if sql != want {
		t.Fatalf("sql=%q, want %q", sql, want)
	}
	// The no_answer disjunct is a bare status equality with no callback_due_at
	// condition, so it cannot exclude a no_answer lead based on its due date —
	// this is what makes the trap-in-brief scenario (no_answer lead with a
	// non-null callback_due_at) still match the whole OR group.
	noAnswerDisjunct := `l.call_status = "no_answer"`
	if !strings.Contains(sql, noAnswerDisjunct) || strings.Contains(sql, noAnswerDisjunct+" and") {
		t.Fatalf("no_answer disjunct must be a bare equality, not ANDed with a due-date condition: %q", sql)
	}
}

func TestClientStatusFilterWhereActive(t *testing.T) {
	addArg := func(value any) string {
		return fmt.Sprintf("%q", value)
	}

	got, ok := clientStatusFilterWhere("active", addArg)
	if !ok {
		t.Fatal("expected ok=true")
	}
	want := []string{`l.client_status not in ("closed_lost","contract_signed")`}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("clauses=%v, want %v", got, want)
	}
}

// TestDigestGroupOneCombinesCallStatusNoneWithClientStatusActive covers the
// exact URL shape the daily-digest deep link for "New leads" (group 1) will
// use: callStatus=none combined with clientStatus=active, each parsed
// independently through the same addArg counter handleListLeads uses.
func TestDigestGroupOneCombinesCallStatusNoneWithClientStatusActive(t *testing.T) {
	var args []any
	addArg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}

	callSQL, ok := callStatusFilterWhereMulti([]string{"none"}, addArg)
	if !ok {
		t.Fatal("expected callStatus ok=true")
	}
	clientSQL, ok := clientStatusFilterWhereMulti([]string{"active"}, addArg)
	if !ok {
		t.Fatal("expected clientStatus ok=true")
	}

	if callSQL != "l.call_status is null" {
		t.Fatalf("callStatus sql=%q, want %q", callSQL, "l.call_status is null")
	}
	wantClientSQL := `l.client_status not in ($1,$2)`
	if clientSQL != wantClientSQL {
		t.Fatalf("clientStatus sql=%q, want %q", clientSQL, wantClientSQL)
	}
	wantArgs := []any{"closed_lost", "contract_signed"}
	if len(args) != len(wantArgs) || args[0] != wantArgs[0] || args[1] != wantArgs[1] {
		t.Fatalf("args=%v, want %v", args, wantArgs)
	}
}

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

// A closed lead has no reminders: every reminder-bearing field is suppressed at
// serialization, which also covers leads closed before the write path started
// clearing them.
func TestLeadJSONExpressionSuppressesRemindersForTerminalLeads(t *testing.T) {
	expr := leadJSONExpression
	const guard = "l.client_status in ('closed_lost','contract_signed') then null"

	fields := []struct{ field, until string }{
		{"'callback_due_at'", "'showroom_due_at'"},
		{"'showroom_due_at'", "'measurement_due_at'"},
		{"'measurement_due_at'", "'latest_timeline_comment'"},
		{"'comment_reminder_due_at'", "'comment_reminder_assigned_to'"},
		{"'comment_reminder_assigned_to'", "'callback_due_context'"},
		{"'callback_due_context'", "'markers'"},
	}
	for _, f := range fields {
		start := strings.Index(expr, f.field)
		end := strings.Index(expr, f.until)
		if start < 0 || end <= start {
			t.Fatalf("could not slice %s..%s out of leadJSONExpression", f.field, f.until)
		}
		if !strings.Contains(expr[start:end], guard) {
			t.Errorf("%s is not suppressed for terminal leads\n%s", f.field, expr[start:end])
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

func TestLeadJSONExpressionEmbedsLatestCommentAssignee(t *testing.T) {
	expr := leadJSONExpression
	for _, fragment := range []string{
		"'comment_reminder_assigned_to'",
		"jsonb_typeof(e.new_value->'assigned_to') = 'string'",
		"e.new_value->>'assigned_to'",
		"e.event_category = 'comment'",
		"order by e.created_at desc",
		"limit 1",
	} {
		if !strings.Contains(expr, fragment) {
			t.Fatalf("leadJSONExpression missing %q\n%s", fragment, expr)
		}
	}

	assigneeStart := strings.Index(expr, "'comment_reminder_assigned_to'")
	callbackContextStart := strings.Index(expr, "'callback_due_context'")
	if assigneeStart < 0 || callbackContextStart <= assigneeStart {
		t.Fatal("comment assignee expression boundaries not found")
	}
	assigneeExpr := expr[assigneeStart:callbackContextStart]
	dueStart := strings.Index(expr, "'comment_reminder_due_at'")
	if dueStart < 0 || assigneeStart <= dueStart {
		t.Fatal("comment_reminder_assigned_to must follow comment_reminder_due_at")
	}
	if !strings.Contains(assigneeExpr, "e.event_category = 'comment'") {
		t.Fatalf("comment assignee must filter on the latest comment event\n%s", assigneeExpr)
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

	budgetCurrency := "PLN"
	rateSetID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	args := createLeadInsertArgs(req, "manual", "office", "crm:external", selectedAt, budgetCurrency, &rateSetID)
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
