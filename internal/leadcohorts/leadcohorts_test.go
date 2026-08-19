package leadcohorts

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func validParams() Params {
	return Params{OfficeCode: "kyiv", LocalDate: "2026-08-17", Timezone: "Europe/Kyiv"}
}

func TestParamsValidate(t *testing.T) {
	tests := []struct {
		name    string
		params  Params
		wantErr bool
	}{
		{name: "valid", params: validParams(), wantErr: false},
		{name: "missing office code", params: Params{LocalDate: "2026-08-17", Timezone: "Europe/Kyiv"}, wantErr: true},
		{name: "blank office code", params: Params{OfficeCode: "   ", LocalDate: "2026-08-17", Timezone: "Europe/Kyiv"}, wantErr: true},
		{name: "bad local date", params: Params{OfficeCode: "kyiv", LocalDate: "17-08-2026", Timezone: "Europe/Kyiv"}, wantErr: true},
		{name: "local date with time", params: Params{OfficeCode: "kyiv", LocalDate: "2026-08-17T00:00:00Z", Timezone: "Europe/Kyiv"}, wantErr: true},
		{name: "unknown timezone", params: Params{OfficeCode: "kyiv", LocalDate: "2026-08-17", Timezone: "Mars/Olympus"}, wantErr: true},
		{name: "warsaw timezone", params: Params{OfficeCode: "warsaw", LocalDate: "2026-08-17", Timezone: "Europe/Warsaw"}, wantErr: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.params.validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("validate() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestCountsValue(t *testing.T) {
	c := Counts{
		NewLeads:                  1,
		NoAnswerOrCallbackUndated: 2,
		CallbackDueToday:          3,
		VisitsDueToday:            4,
		ReminderDueToday:          5,
		Overdue:                   6,
	}
	tests := []struct {
		group Group
		want  int
	}{
		{GroupNewLeads, 1},
		{GroupNoAnswerOrCallbackUndated, 2},
		{GroupCallbackDueToday, 3},
		{GroupVisitsDueToday, 4},
		{GroupReminderDueToday, 5},
		{GroupOverdue, 6},
		{Group("unknown"), 0},
	}
	for _, test := range tests {
		t.Run(string(test.group), func(t *testing.T) {
			if got := c.Value(test.group); got != test.want {
				t.Fatalf("Value(%q) = %d, want %d", test.group, got, test.want)
			}
		})
	}
}

// TestLeadIDsQueryCoversEveryGroup is the table-driven contract test for all
// six groups: each must produce a query carrying the fragments that make it
// that specific group (and not another), plus the args in the documented
// $1/$2/$3 order.
func TestLeadIDsQueryCoversEveryGroup(t *testing.T) {
	params := validParams()
	tests := []struct {
		name          string
		group         Group
		wantArgs      []any
		wantFragments []string
	}{
		{
			name:          "group 1: new leads",
			group:         GroupNewLeads,
			wantArgs:      []any{params.OfficeCode},
			wantFragments: []string{"l.call_status is null", "l.archived_at is null", "l.client_status not in ('closed_lost','contract_signed')"},
		},
		{
			name:          "group 2: no_answer or undated callback",
			group:         GroupNoAnswerOrCallbackUndated,
			wantArgs:      []any{params.OfficeCode},
			wantFragments: []string{"l.call_status = 'no_answer'", "l.call_status = 'callback_requested' and l.callback_due_at is null", "l.client_status not in ('closed_lost','contract_signed')"},
		},
		{
			// The final select filters `kind = 'callback'`; the surrounding
			// `with reminders as (...)` also carries the other three
			// branches' text (including their own "kind in (...)" checks) —
			// that is the shared-CTE design, not group bleed, so this case
			// only asserts the group's own filter predicate is present.
			name:          "group 3: callback due today",
			group:         GroupCallbackDueToday,
			wantArgs:      []any{params.OfficeCode, params.LocalDate, params.Timezone},
			wantFragments: []string{"where kind = 'callback'", "(due_at at time zone $3)::date = $2::date"},
		},
		{
			name:          "group 4: visits due today",
			group:         GroupVisitsDueToday,
			wantArgs:      []any{params.OfficeCode, params.LocalDate, params.Timezone},
			wantFragments: []string{"where kind in ('showroom', 'measurement')", "(due_at at time zone $3)::date = $2::date"},
		},
		{
			name:          "group 5: thinking/comment due today",
			group:         GroupReminderDueToday,
			wantArgs:      []any{params.OfficeCode, params.LocalDate, params.Timezone},
			wantFragments: []string{"where kind in ('comment', 'thinking')", "(due_at at time zone $3)::date = $2::date"},
		},
		{
			name:          "group 6: overdue, any kind",
			group:         GroupOverdue,
			wantArgs:      []any{params.OfficeCode, params.LocalDate, params.Timezone},
			wantFragments: []string{"select lead_id", "where (due_at at time zone $3)::date < $2::date"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			query, args, ok := leadIDsQuery(test.group, params)
			if !ok {
				t.Fatal("expected ok=true")
			}
			if len(args) != len(test.wantArgs) {
				t.Fatalf("args=%v, want %v", args, test.wantArgs)
			}
			for i := range args {
				if args[i] != test.wantArgs[i] {
					t.Fatalf("args[%d]=%v, want %v", i, args[i], test.wantArgs[i])
				}
			}
			for _, fragment := range test.wantFragments {
				if !strings.Contains(query, fragment) {
					t.Fatalf("query for %s missing %q\n%s", test.group, fragment, query)
				}
			}
			isReminderBased := test.group != GroupNewLeads && test.group != GroupNoAnswerOrCallbackUndated
			if isReminderBased {
				// Groups 3-6 must list one row per reminder, not one per
				// distinct lead — see TestReminderGroupsCountRowsNotLeads.
				if !strings.Contains(query, "select lead_id") {
					t.Fatalf("query for %s must select lead_id from reminders\n%s", test.group, query)
				}
				if strings.Contains(query, "distinct") {
					t.Fatalf("query for %s must not deduplicate reminder rows by lead_id\n%s", test.group, query)
				}
			} else if !strings.HasPrefix(strings.TrimSpace(query), "select l.id") {
				t.Fatalf("query for %s must select lead ids directly from public.leads\n%s", test.group, query)
			}
		})
	}
}

func TestLeadIDsQueryRejectsUnknownGroup(t *testing.T) {
	_, _, ok := leadIDsQuery(Group("bogus"), validParams())
	if ok {
		t.Fatal("expected ok=false for an unknown group")
	}
}

// TestCountsQueryColumnOrderMatchesScan pins the six-column select order in
// countsQuery to FetchCounts' Scan call — if either changes without the
// other, the count in the digest silently lands in the wrong bucket.
func TestCountsQueryColumnOrderMatchesScan(t *testing.T) {
	wantOrder := []string{
		"as new_leads",
		"as no_answer_or_callback_undated",
		"as callback_due_today",
		"as visits_due_today",
		"as reminder_due_today",
		"as overdue",
	}
	lastIdx := -1
	for _, label := range wantOrder {
		idx := strings.Index(countsQuery, label)
		if idx < 0 {
			t.Fatalf("countsQuery missing column label %q", label)
		}
		if idx <= lastIdx {
			t.Fatalf("countsQuery column %q is out of order (want %v)", label, wantOrder)
		}
		lastIdx = idx
	}
}

// TestCountsQueryUsesExactDayAndStrictlyBeforeSemantics is the explicit
// regression test for the brief's correctness requirement: groups 3/5/6 use
// exact-day equality ("on the local date") or strict '<' ("overdue"), never
// '<=' — the old "due on or before" semantics of
// internal/dailyreport.isDueOnOrBeforeLocalDate must not leak in here, since
// overdue is now its own separate group rather than folded into "due today".
func TestCountsQueryUsesExactDayAndStrictlyBeforeSemantics(t *testing.T) {
	if strings.Contains(countsQuery, "<=") {
		t.Fatalf("countsQuery must not use '<=' (that was the old on-or-before semantics); got:\n%s", countsQuery)
	}
	wantEquality := 3 // callback_due_today, visits_due_today, reminder_due_today
	gotEquality := strings.Count(countsQuery, "::date = $2::date")
	if gotEquality != wantEquality {
		t.Fatalf("countsQuery has %d exact-day comparisons, want %d", gotEquality, wantEquality)
	}
	wantStrictBefore := 1 // overdue
	gotStrictBefore := strings.Count(countsQuery, "::date < $2::date")
	if gotStrictBefore != wantStrictBefore {
		t.Fatalf("countsQuery has %d strictly-before comparisons, want %d", gotStrictBefore, wantStrictBefore)
	}
}

// TestCountsQueryCountsReminderRowsNotDistinctLeads pins countsQuery to
// counting raw rows from `reminders`, not distinct leads: the acceptance
// criterion is "the digest number equals the number of rows the manager
// sees after clicking through", and the CRM reminders/calendar view
// (calendar-page.ts's remindersByDate) renders one chip per reminder — a
// separate chip for a callback and for a comment/task reminder — while
// GET /v1/appointments renders one card per scheduled visit. A lead with two
// simultaneous reminders in the same group must therefore count as 2, not 1;
// see TestReminderGroupsCountRowsNotLeads for the arithmetic case that
// demonstrates why `count(distinct lead_id)` (the previous, rejected
// approach) gets that wrong.
func TestCountsQueryCountsReminderRowsNotDistinctLeads(t *testing.T) {
	if strings.Contains(countsQuery, "distinct") {
		t.Fatalf("countsQuery must not deduplicate reminder rows by lead_id:\n%s", countsQuery)
	}
	got := strings.Count(countsQuery, "count(*)")
	want := 6 // new_leads, no_answer_or_callback_undated, and all four reminder-based groups
	if got != want {
		t.Fatalf("countsQuery has %d 'count(*)' occurrences, want %d\n%s", got, want, countsQuery)
	}
}

// TestReminderGroupsCountRowsNotLeads is the behavioral regression test the
// coordinator asked for: given a lead with two simultaneous reminders in the
// same group, the count must be 2, not 1. This repository has no database
// available to unit tests (every other test in this package, and in
// internal/crmapi and internal/dailyreport, asserts SQL text rather than
// executing it), so this test cannot run countsQuery itself. Instead it
// pins the intended counting arithmetic in an executable Go form —
// countRemindersMatching below mirrors countsQuery's "filter by kind set,
// filter by local-date comparison, then count every matching row" shape —
// and explicitly checks that a naive distinct-lead count would have given
// the wrong answer, so the test fails loudly if it stops actually exercising
// the distinction. TestCountsQueryCountsReminderRowsNotDistinctLeads is what
// pins the production SQL text to this same shape.
func TestReminderGroupsCountRowsNotLeads(t *testing.T) {
	leadWithTwoReminders := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	otherLead := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	today := mustParseDate(t, "2026-08-17")
	yesterday := mustParseDate(t, "2026-08-16")

	tests := []struct {
		name           string
		kinds          map[string]bool // nil means "any kind" (group 6)
		strictlyBefore bool            // false: "on" the date (groups 3-5); true: "overdue" (group 6)
		rows           []reminderRow
		date           string
		want           int
	}{
		{
			// Group 5: a lead currently in 'thinking' whose manager also left
			// a dated comment reminder for the same day — two independent
			// sources (the callback_due_at column and the latest comment
			// event), same lead, same day.
			name:  "group 5: thinking + comment on the same lead, same day",
			kinds: map[string]bool{"comment": true, "thinking": true},
			date:  "2026-08-17",
			rows: []reminderRow{
				{leadID: leadWithTwoReminders, kind: "thinking", dueAt: today},
				{leadID: leadWithTwoReminders, kind: "comment", dueAt: today},
				{leadID: otherLead, kind: "thinking", dueAt: yesterday}, // wrong day, must not count
			},
			want: 2,
		},
		{
			// Group 4: the same lead has both a showroom visit and a
			// measurement visit scheduled for the same day.
			name:  "group 4: showroom + measurement visit on the same lead, same day",
			kinds: map[string]bool{"showroom": true, "measurement": true},
			date:  "2026-08-17",
			rows: []reminderRow{
				{leadID: leadWithTwoReminders, kind: "showroom", dueAt: today},
				{leadID: leadWithTwoReminders, kind: "measurement", dueAt: today},
			},
			want: 2,
		},
		{
			// Group 6: overdue, any kind — a lead can accumulate several
			// overdue reminders across independent sources at once.
			name:           "group 6: three overdue reminders on the same lead",
			kinds:          nil,
			strictlyBefore: true,
			date:           "2026-08-17",
			rows: []reminderRow{
				{leadID: leadWithTwoReminders, kind: "thinking", dueAt: yesterday},
				{leadID: leadWithTwoReminders, kind: "comment", dueAt: yesterday},
				{leadID: leadWithTwoReminders, kind: "showroom", dueAt: yesterday},
				{leadID: otherLead, kind: "callback", dueAt: today}, // due today, not overdue, must not count
			},
			want: 3,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := countRemindersMatching(test.rows, test.kinds, test.date, test.strictlyBefore, 0)
			if got != test.want {
				t.Fatalf("count = %d, want %d (one lead with multiple simultaneous reminders must count once per reminder)", got, test.want)
			}

			distinctLeads := map[uuid.UUID]bool{}
			for _, row := range test.rows {
				if test.kinds != nil && !test.kinds[row.kind] {
					continue
				}
				matches := row.dueAt.Format("2006-01-02") == test.date
				if test.strictlyBefore {
					matches = row.dueAt.Format("2006-01-02") < test.date
				}
				if matches {
					distinctLeads[row.leadID] = true
				}
			}
			if len(distinctLeads) == test.want {
				t.Fatalf("fixture does not exercise the row-vs-lead distinction: distinct-lead count (%d) equals the expected row count (%d)", len(distinctLeads), test.want)
			}
		})
	}
}

// reminderRow and countRemindersMatching exist only for
// TestReminderGroupsCountRowsNotLeads above — a minimal, test-only Go model
// of a `reminders` row, deliberately kept separate from any production type
// so this file stays honest about not exercising real SQL.
type reminderRow struct {
	leadID uuid.UUID
	kind   string
	dueAt  time.Time
}

// countRemindersMatching's lookbackDays mirrors countsQuery's overdue lower
// bound (OverdueLookbackDays): 0 means "no lower bound" (groups 3-5, and
// group 6 test cases not exercising the bound); a positive value excludes
// rows older than localDate - lookbackDays, matching
// `(due_at at time zone $3)::date >= $2::date - OverdueLookbackDays`.
func countRemindersMatching(rows []reminderRow, kinds map[string]bool, localDate string, strictlyBefore bool, lookbackDays int) int {
	local, err := time.Parse("2006-01-02", localDate)
	if err != nil {
		panic(err) // test fixture bug, not a runtime condition
	}
	count := 0
	for _, row := range rows {
		if kinds != nil && !kinds[row.kind] {
			continue
		}
		rowDate := row.dueAt.Format("2006-01-02")
		if strictlyBefore {
			if rowDate >= localDate {
				continue
			}
			if lookbackDays > 0 && row.dueAt.Before(local.AddDate(0, 0, -lookbackDays)) {
				continue
			}
			count++
		} else if rowDate == localDate {
			count++
		}
	}
	return count
}

func mustParseDate(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed
}

// TestOverdueLookbackDaysMatches365DayCRMWindow pins OverdueLookbackDays to
// the 365-day window the CRM calendar uses when it pages GET /v1/appointments
// (63-day-per-request cap, internal/crmapi/appointments.go:209) backwards to
// find overdue reminders. See OverdueLookbackDays' doc comment for why these
// two values must stay equal, and why nothing but this test and that comment
// enforces it.
func TestOverdueLookbackDaysMatches365DayCRMWindow(t *testing.T) {
	if OverdueLookbackDays != 365 {
		t.Fatalf("OverdueLookbackDays = %d, want 365 (must mirror the CRM's overdue lookback window)", OverdueLookbackDays)
	}
}

// TestCountsQueryAndLeadIDsQueryOverdueHaveLookbackLowerBound pins both
// group-6 query builders to the same lower-bound expression, derived from
// OverdueLookbackDays rather than a bare literal, so the two can't drift
// from each other or from the constant.
func TestCountsQueryAndLeadIDsQueryOverdueHaveLookbackLowerBound(t *testing.T) {
	wantLowerBound := "(due_at at time zone $3)::date >= $2::date - " + strconv.Itoa(OverdueLookbackDays)

	if !strings.Contains(countsQuery, wantLowerBound) {
		t.Fatalf("countsQuery overdue subquery missing lower bound %q\n%s", wantLowerBound, countsQuery)
	}
	// The upper bound (strictly before the local date) must still be there
	// too — this change must add a floor, not replace the existing ceiling.
	if !strings.Contains(countsQuery, "(due_at at time zone $3)::date < $2::date") {
		t.Fatal("countsQuery overdue subquery lost its upper bound")
	}

	query, _, ok := leadIDsQuery(GroupOverdue, validParams())
	if !ok {
		t.Fatal("expected ok=true for GroupOverdue")
	}
	if !strings.Contains(query, wantLowerBound) {
		t.Fatalf("leadIDsQuery(GroupOverdue) missing lower bound %q\n%s", wantLowerBound, query)
	}
	if !strings.Contains(query, "(due_at at time zone $3)::date < $2::date") {
		t.Fatal("leadIDsQuery(GroupOverdue) lost its upper bound")
	}
}

// TestOverdueExcludesReminderOlderThanLookbackWindow is the regression test
// the coordinator asked for: a reminder 300 days overdue is still inside the
// 365-day CRM lookback window and must count; one 400 days overdue is past
// it and must not — mirroring the fact that the CRM calendar itself cannot
// reach that far back (GET /v1/appointments pages in 63-day chunks, capped
// at OverdueLookbackDays total). Real production data on 2026-08-18 has no
// overdue reminder older than about 39 days, so this boundary currently
// excludes nothing — it exists for when that stops being true.
func TestOverdueExcludesReminderOlderThanLookbackWindow(t *testing.T) {
	localDate := "2026-08-17"
	local := mustParseDate(t, localDate)
	within300Days := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	beyond400Days := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	rows := []reminderRow{
		{leadID: within300Days, kind: "callback", dueAt: local.AddDate(0, 0, -300)},
		{leadID: beyond400Days, kind: "thinking", dueAt: local.AddDate(0, 0, -400)},
	}

	got := countRemindersMatching(rows, nil, localDate, true, OverdueLookbackDays)
	want := 1
	if got != want {
		t.Fatalf("overdue count = %d, want %d (the 300-day-old reminder must count; the 400-day-old one must not, since it is past the %d-day lookback)", got, want, OverdueLookbackDays)
	}

	// Confirm it is specifically the 300-day-old reminder that counted, not
	// some other fixture mistake landing on the right total by accident.
	onlyWithinWindow := countRemindersMatching(rows[:1], nil, localDate, true, OverdueLookbackDays)
	if onlyWithinWindow != 1 {
		t.Fatalf("the 300-day-old reminder alone should count as 1, got %d", onlyWithinWindow)
	}
	onlyBeyondWindow := countRemindersMatching(rows[1:], nil, localDate, true, OverdueLookbackDays)
	if onlyBeyondWindow != 0 {
		t.Fatalf("the 400-day-old reminder alone should count as 0, got %d", onlyBeyondWindow)
	}
}

// TestGroupsOrderIsStable pins the exported Groups slice to the digest's
// six-group display order documented in the group constants' comments.
func TestGroupsOrderIsStable(t *testing.T) {
	want := []Group{
		GroupNewLeads,
		GroupNoAnswerOrCallbackUndated,
		GroupCallbackDueToday,
		GroupVisitsDueToday,
		GroupReminderDueToday,
		GroupOverdue,
	}
	if len(Groups) != len(want) {
		t.Fatalf("Groups=%v, want %v", Groups, want)
	}
	for i := range want {
		if Groups[i] != want[i] {
			t.Fatalf("Groups[%d]=%q, want %q", i, Groups[i], want[i])
		}
	}
}

// TestRemindersCTEKindsMirrorLeadRulesTS cross-checks the reminder kind set
// against activeRemindersForLead in
// kolss-crm-angular/src/app/domain/lead.rules.ts:322-352 (read, not edited,
// while writing this test — that file lives in a sibling repository this
// one cannot import, so the case set is pinned here as literals instead).
// That function pushes, in order: 'callback' (callStatus ==
// 'callback_requested'), 'thinking' (clientStatus == 'thinking'),
// 'showroom', 'measurement', 'comment'. If a future change adds, renames, or
// removes a kind on either side, whoever updates this list is the one who
// must go re-read lead.rules.ts and confirm the two still agree.
func TestRemindersCTEKindsMirrorLeadRulesTS(t *testing.T) {
	wantKinds := []string{
		"'callback'",
		"'thinking'",
		"'comment'",
		"'showroom', 'measurement'", // read from v.kind, not a literal, but this is the allowed set
	}
	for _, fragment := range wantKinds {
		if !strings.Contains(remindersCTE, fragment) {
			t.Fatalf("remindersCTE missing reminder kind %q\n%s", fragment, remindersCTE)
		}
	}
}

func TestCallbackDueContextSQLIsSelfGuardedForTerminalLeads(t *testing.T) {
	const guard = "l.client_status in ('closed_lost','contract_signed') then null"
	if !strings.Contains(CallbackDueContextSQL, guard) {
		t.Fatalf("CallbackDueContextSQL is not self-guarded for terminal leads:\n%s", CallbackDueContextSQL)
	}
}

func TestCommentReminderDueAtSQLDoesNotUseExistenceOperator(t *testing.T) {
	// CommentReminderDueAtSQL must select the latest comment event
	// unconditionally and only then extract its optional due date with `->>`;
	// it must not gate lead_events on `new_value ? 'callback_due_at'` the way
	// CallbackDueContextSQL does, or it would silently skip the latest comment
	// whenever an earlier comment (not the latest) happened to carry a date.
	if strings.Contains(CommentReminderDueAtSQL, "e.new_value ? 'callback_due_at'") {
		t.Fatalf("CommentReminderDueAtSQL must not use the '?' existence operator:\n%s", CommentReminderDueAtSQL)
	}
	for _, fragment := range []string{
		"e.event_category = 'comment'",
		"jsonb_typeof(e.new_value->'callback_due_at') = 'string'",
		"e.new_value->>'callback_due_at'",
		"order by e.created_at desc",
		"limit 1",
	} {
		if !strings.Contains(CommentReminderDueAtSQL, fragment) {
			t.Fatalf("CommentReminderDueAtSQL missing %q\n%s", fragment, CommentReminderDueAtSQL)
		}
	}
}

func TestActiveReminderCandidatesAreScopedAndTimestamped(t *testing.T) {
	for _, fragment := range []string{
		"l.archived_at is null",
		"l.client_status not in (" + TerminalClientStatusesSQL + ")",
		"(e.new_value->>'callback_due_at')::timestamptz = l.callback_due_at",
		"comment_reminder.action_at",
		"v.updated_at",
		"candidate.source_id",
	} {
		if !strings.Contains(ActiveReminderCandidatesSQL, fragment) {
			t.Fatalf("ActiveReminderCandidatesSQL missing %q\n%s", fragment, ActiveReminderCandidatesSQL)
		}
	}
}

func TestRemindersCTEUsesCanonicalCandidatesAndOfficeScope(t *testing.T) {
	for _, fragment := range []string{
		"cross join lateral " + ActiveReminderCandidatesSQL,
		"o.code = $1",
		"reminder.action_at",
	} {
		if !strings.Contains(remindersCTE, fragment) {
			t.Fatalf("remindersCTE missing %q\n%s", fragment, remindersCTE)
		}
	}
}

func TestRemindersCTEShowroomBranchReadsVisitsTableDirectly(t *testing.T) {
	// Group 4 must read public.lead_showroom_visits directly — the same
	// table GET /v1/appointments reads via appointmentSelect in
	// internal/crmapi/appointments.go — not derive due dates by replaying
	// lead_events the way internal/dailyreport's now-superseded active_visit
	// lateral join did.
	for _, fragment := range []string{
		"from public.lead_showroom_visits v",
		"v.status = 'scheduled'",
		"v.kind in ('showroom', 'measurement')",
		"v.scheduled_at",
	} {
		if !strings.Contains(remindersCTE, fragment) {
			t.Fatalf("remindersCTE missing %q\n%s", fragment, remindersCTE)
		}
	}
}
