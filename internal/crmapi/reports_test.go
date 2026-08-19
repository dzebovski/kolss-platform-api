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
		from, to, period, fields := parseReportPeriod(req)
		if from != nil || to != nil || period.From != nil || period.To != nil || len(fields) != 0 {
			t.Fatalf("unexpected all-time period: from=%v to=%v period=%#v fields=%#v", from, to, period, fields)
		}
	})

	t.Run("inclusive range", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/reports/leads?from=2026-06-01&to=2026-06-30", nil)
		from, to, period, fields := parseReportPeriod(req)
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
		{name: "invalid from", url: "/v1/reports/leads?from=01-06-2026&to=2026-06-30", key: "from"},
		{name: "reversed", url: "/v1/reports/leads?from=2026-07-01&to=2026-06-30", key: "to"},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", test.url, nil)
			_, _, _, fields := parseReportPeriod(req)
			if fields[test.key] == "" {
				t.Fatalf("expected %s error, got %#v", test.key, fields)
			}
		})
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
	normalized := strings.Join(strings.Fields(leadReportQuery), " ")
	for _, fragment := range []string{
		"o.timezone_name",
		"select reminder.due_at",
		"order by reminder.action_at desc, reminder.due_at desc, reminder.kind, reminder.source_id limit 1",
	} {
		if !strings.Contains(normalized, fragment) {
			t.Fatalf("leadReportQuery missing %q\n%s", fragment, leadReportQuery)
		}
	}
	for _, staleFragment := range []string{"last_activity_at", "inactive_days"} {
		if strings.Contains(leadReportQuery, staleFragment) {
			t.Fatalf("leadReportQuery still contains stale inactivity fragment %q", staleFragment)
		}
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
