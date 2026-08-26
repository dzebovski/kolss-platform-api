package crmapi

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseReportPeriod(t *testing.T) {
	t.Run("all time", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/reports/leads", nil)
		from, to, period, fields := parseReportPeriod(req, false)
		if from != nil || to != nil || period.From != nil || period.To != nil || len(fields) != 0 {
			t.Fatalf("unexpected all-time period: from=%v to=%v period=%#v fields=%#v", from, to, period, fields)
		}
	})

	t.Run("inclusive range", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/reports/leads?from=2026-06-01&to=2026-06-30", nil)
		from, to, period, fields := parseReportPeriod(req, false)
		if len(fields) != 0 || from == nil || to == nil {
			t.Fatalf("valid range rejected: from=%v to=%v fields=%#v", from, to, fields)
		}
		if got := from.Format("2006-01-02"); got != "2026-06-01" {
			t.Fatalf("from=%s", got)
		}
		if got := to.Format("2006-01-02"); got != "2026-06-30" {
			t.Fatalf("to=%s", got)
		}
		if period.From == nil || *period.From != "2026-06-01" || period.To == nil || *period.To != "2026-06-30" {
			t.Fatalf("period=%#v", period)
		}
	})

	for _, test := range []struct {
		name string
		url  string
		key  string
	}{
		{name: "missing to", url: "/v1/reports/leads?from=2026-06-01", key: "to"},
		{name: "calendar requires range", url: "/v1/reports/leads?cohort=calendar", key: "from"},
		{name: "invalid from", url: "/v1/reports/leads?from=01-06-2026&to=2026-06-30", key: "from"},
		{name: "reversed", url: "/v1/reports/leads?from=2026-07-01&to=2026-06-30", key: "to"},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", test.url, nil)
			criteria, fields := parseReportCriteria(req)
			if fields[test.key] == "" {
				t.Fatalf("expected %s error, got %#v", test.key, fields)
			}
			if test.name == "calendar requires range" && criteria.Cohort != reportCohortCalendar {
				t.Fatalf("cohort=%q", criteria.Cohort)
			}
		})
	}
}

func TestParseReportCriteriaDefaultsToActivityAndReadsStatusArrays(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/reports/leads?callStatus=no_answer,callback_undated&clientStatus=thinking,in_work", nil)
	criteria, fields := parseReportCriteria(req)
	if len(fields) != 0 {
		t.Fatalf("fields=%#v", fields)
	}
	if criteria.Cohort != reportCohortActivity {
		t.Fatalf("cohort=%q", criteria.Cohort)
	}
	if strings.Join(criteria.CallStatuses, ",") != "no_answer,callback_undated" {
		t.Fatalf("call statuses=%#v", criteria.CallStatuses)
	}
	if strings.Join(criteria.ClientStatuses, ",") != "thinking,in_work" {
		t.Fatalf("client statuses=%#v", criteria.ClientStatuses)
	}
}

func TestParseReportCriteriaRejectsUnknownCohort(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/reports/leads?cohort=combined", nil)
	_, fields := parseReportCriteria(req)
	if fields["cohort"] == "" {
		t.Fatalf("fields=%#v", fields)
	}
}

func TestReportTotalsUseCurrentIndependentStatuses(t *testing.T) {
	totals := newReportTotals()
	callback := "callback_requested"
	addLeadToTotals(&totals, reportLead{
		ClientStatus: "calculation_in_progress",
		CallStatus:   &callback,
		OverdueDays:  6,
	})
	addLeadToTotals(&totals, reportLead{ClientStatus: "contract_signed", OverdueDays: 30})
	addLeadToTotals(&totals, reportLead{ClientStatus: "closed_lost", OverdueDays: 30})
	finalizeTotals(&totals)

	if totals.Total != 3 || totals.Active != 1 {
		t.Fatalf("total=%d active=%d", totals.Total, totals.Active)
	}
	if totals.Callback != 1 || totals.OverdueNextActionCount != 1 {
		t.Fatalf("callback=%d overdueNextAction=%d", totals.Callback, totals.OverdueNextActionCount)
	}
	if totals.ContractSigned != 1 || totals.ClosedLost != 1 || totals.ConversionPercent != 33 {
		t.Fatalf("sold=%d lost=%d conversion=%d", totals.ContractSigned, totals.ClosedLost, totals.ConversionPercent)
	}
	if totals.ByClientStatus["calculation_in_progress"] != 1 {
		t.Fatalf("status counts=%#v", totals.ByClientStatus)
	}
}

func TestReportTotalsSumSignedContractsByCurrency(t *testing.T) {
	totals := newReportTotals()
	eur := "EUR"
	uah := "UAH"
	eurFirst := 29800.0
	eurSecond := 200.0
	uahAmount := 500000.0

	addLeadToTotals(&totals, reportLead{ClientStatus: "contract_signed", ContractAmount: &eurFirst, ContractCurrency: &eur})
	addLeadToTotals(&totals, reportLead{ClientStatus: "contract_signed", ContractAmount: &uahAmount, ContractCurrency: &uah})
	addLeadToTotals(&totals, reportLead{ClientStatus: "contract_signed", ContractAmount: &eurSecond, ContractCurrency: &eur})
	addLeadToTotals(&totals, reportLead{ClientStatus: "new_lead", ContractAmount: &eurFirst, ContractCurrency: &eur})
	finalizeTotals(&totals)

	if totals.ContractSigned != 3 {
		t.Fatalf("contractSigned=%d", totals.ContractSigned)
	}
	if len(totals.ContractTotals) != 2 {
		t.Fatalf("contractTotals=%#v", totals.ContractTotals)
	}
	if totals.ContractTotals[0].Currency != "UAH" || totals.ContractTotals[0].Total != 500000 {
		t.Fatalf("first contract total=%#v", totals.ContractTotals[0])
	}
	if totals.ContractTotals[1].Currency != "EUR" || totals.ContractTotals[1].Total != 30000 {
		t.Fatalf("second contract total=%#v", totals.ContractTotals[1])
	}
}

func TestReportOverdueDaysUsesOfficeLocalCalendarDates(t *testing.T) {
	asOf := time.Date(2026, 8, 18, 22, 30, 0, 0, time.UTC)
	sixDaysAgo := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	dueTodayInKyiv := time.Date(2026, 8, 18, 21, 30, 0, 0, time.UTC)
	dueTomorrowInKyiv := time.Date(2026, 8, 19, 21, 0, 0, 0, time.UTC)

	for _, test := range []struct {
		name     string
		dueAt    *time.Time
		timezone string
		want     int
	}{
		{name: "missing next action", timezone: "Europe/Kyiv", want: 0},
		{name: "six calendar days overdue", dueAt: &sixDaysAgo, timezone: "Europe/Kyiv", want: 6},
		{name: "due today is not overdue", dueAt: &dueTodayInKyiv, timezone: "Europe/Kyiv", want: 0},
		{name: "future action is not overdue", dueAt: &dueTomorrowInKyiv, timezone: "Europe/Kyiv", want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := reportOverdueDays(asOf, test.dueAt, test.timezone)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("overdueDays=%d, want %d", got, test.want)
			}
		})
	}
}

func TestReportOverdueDaysRejectsUnknownTimezone(t *testing.T) {
	dueAt := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	if _, err := reportOverdueDays(time.Now(), &dueAt, "Mars/Olympus"); err == nil {
		t.Fatal("expected an invalid timezone error")
	}
}

func TestLeadReportQuerySelectsOneMostRecentlyRecordedActiveAction(t *testing.T) {
	query, _, fields := buildLeadReportQuery(reportCriteria{Cohort: reportCohortActivity}, nil)
	if len(fields) != 0 {
		t.Fatalf("fields=%#v", fields)
	}
	normalized := strings.Join(strings.Fields(query), " ")
	for _, fragment := range []string{
		"o.timezone_name",
		"select reminder.due_at",
		"order by reminder.action_at desc, reminder.due_at desc, reminder.kind, reminder.source_id limit 1",
	} {
		if !strings.Contains(normalized, fragment) {
			t.Fatalf("query missing %q\n%s", fragment, query)
		}
	}
	for _, staleFragment := range []string{"last_activity_at", "inactive_days"} {
		if strings.Contains(query, staleFragment) {
			t.Fatalf("query still contains stale inactivity fragment %q", staleFragment)
		}
	}
}

func TestLeadReportActivityCohortUsesCreationOrHumanActivity(t *testing.T) {
	from := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, time.June, 30, 0, 0, 0, 0, time.UTC)
	query, args, fields := buildLeadReportQuery(reportCriteria{
		Cohort: reportCohortActivity,
		From:   &from,
		To:     &to,
	}, nil)
	if len(fields) != 0 {
		t.Fatalf("fields=%#v", fields)
	}
	normalized := strings.Join(strings.Fields(query), " ")
	for _, fragment := range []string{
		"coalesce(l.source_created_at,l.created_at)",
		"period_event.actor_id is not null",
		"period_event.event_category is distinct from 'system'",
		"at time zone o.timezone_name",
	} {
		if !strings.Contains(normalized, fragment) {
			t.Fatalf("activity query missing %q\n%s", fragment, query)
		}
	}
	if len(args) != 3 {
		t.Fatalf("args=%#v", args)
	}
}

func TestLeadReportCalendarCohortUsesCurrentReminderAndAppointmentExists(t *testing.T) {
	from := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, time.June, 30, 0, 0, 0, 0, time.UTC)
	query, args, fields := buildLeadReportQuery(reportCriteria{
		Cohort: reportCohortCalendar,
		From:   &from,
		To:     &to,
	}, nil)
	if len(fields) != 0 {
		t.Fatalf("fields=%#v", fields)
	}
	normalized := strings.Join(strings.Fields(query), " ")
	for _, fragment := range []string{
		"exists ( select 1 from",
		"reminder.kind in ('callback','thinking','comment')",
		"calendar_appointment.kind in ('showroom','measurement')",
		"calendar_appointment.status in ('scheduled','visited','no_show','canceled')",
		"calendar_appointment.scheduled_at at time zone o.timezone_name",
	} {
		if !strings.Contains(normalized, fragment) {
			t.Fatalf("calendar query missing %q\n%s", fragment, query)
		}
	}
	if strings.Contains(query, "calendar_appointment.status in ('scheduled','visited','no_show','canceled','rescheduled')") {
		t.Fatal("rescheduled appointments must not be selected")
	}
	if strings.Contains(normalized, "join public.lead_showroom_visits calendar_appointment") {
		t.Fatal("calendar appointments must be filtered with EXISTS to keep one row per lead")
	}
	if len(args) != 3 {
		t.Fatalf("args=%#v", args)
	}
}

func TestLeadReportStatusFiltersUseOrWithinAndAcrossDimensions(t *testing.T) {
	query, _, fields := buildLeadReportQuery(reportCriteria{
		Cohort:         reportCohortActivity,
		CallStatuses:   []string{"none", "callback_undated"},
		ClientStatuses: []string{"in_work", "thinking"},
	}, nil)
	if len(fields) != 0 {
		t.Fatalf("fields=%#v", fields)
	}
	normalized := strings.Join(strings.Fields(query), " ")
	for _, fragment := range []string{
		"(l.call_status is null or (l.call_status = $2 and l.callback_due_at is null))",
		"((l.client_status = $3 and l.call_status is not null) or l.client_status = $4)",
	} {
		if !strings.Contains(normalized, fragment) {
			t.Fatalf("query missing %q\n%s", fragment, query)
		}
	}
}

func TestLeadReportAcceptsEveryLeadStatusFilterValue(t *testing.T) {
	callStatuses := []string{"reached", "no_answer", "callback_requested", "none", "callback_undated"}
	clientStatuses := []string{
		"new_lead",
		"showroom_invited",
		"measurement_scheduled",
		"calculation_in_progress",
		"thinking",
		"postponed",
		"contract_signed",
		"closed_lost",
		"in_work",
		"active",
	}
	for _, status := range callStatuses {
		_, _, fields := buildLeadReportQuery(reportCriteria{Cohort: reportCohortActivity, CallStatuses: []string{status}}, nil)
		if len(fields) != 0 {
			t.Fatalf("call status %q rejected: %#v", status, fields)
		}
	}
	for _, status := range clientStatuses {
		_, _, fields := buildLeadReportQuery(reportCriteria{Cohort: reportCohortActivity, ClientStatuses: []string{status}}, nil)
		if len(fields) != 0 {
			t.Fatalf("client status %q rejected: %#v", status, fields)
		}
	}
}

func TestLeadReportRejectsUnknownStatusFilters(t *testing.T) {
	_, _, fields := buildLeadReportQuery(reportCriteria{
		Cohort:         reportCohortActivity,
		CallStatuses:   []string{"busy"},
		ClientStatuses: []string{"won"},
	}, nil)
	if fields["callStatus"] == "" || fields["clientStatus"] == "" {
		t.Fatalf("fields=%#v", fields)
	}
}

func TestReportClientStatusesCoverEveryStoredStatus(t *testing.T) {
	// The report must count every status the leads CHECK constraint allows,
	// because ReportStatusCounts requires all of them in the contract.
	want := map[string]bool{
		"new_lead":                true,
		"showroom_invited":        true,
		"measurement_scheduled":   true,
		"calculation_in_progress": true,
		"thinking":                true,
		"postponed":               true,
		"contract_signed":         true,
		"closed_lost":             true,
	}
	if len(reportClientStatuses) != len(want) {
		t.Fatalf("got %d statuses, want %d: %#v", len(reportClientStatuses), len(want), reportClientStatuses)
	}
	for _, status := range reportClientStatuses {
		if !want[status] {
			t.Fatalf("unexpected report status %q", status)
		}
		delete(want, status)
	}
	for status := range want {
		t.Fatalf("report is missing client status %q", status)
	}
}
