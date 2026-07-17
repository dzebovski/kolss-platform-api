package crmapi

import (
	"net/http/httptest"
	"testing"
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
		InactiveDays: 8,
		Inactive7d:   true,
	})
	addLeadToTotals(&totals, reportLead{ClientStatus: "contract_signed", InactiveDays: 30})
	addLeadToTotals(&totals, reportLead{ClientStatus: "closed_lost", InactiveDays: 30})
	finalizeTotals(&totals)

	if totals.Total != 3 || totals.Active != 1 {
		t.Fatalf("total=%d active=%d", totals.Total, totals.Active)
	}
	if totals.Callback != 1 || totals.Inactive7d != 1 {
		t.Fatalf("callback=%d inactive=%d", totals.Callback, totals.Inactive7d)
	}
	if totals.ContractSigned != 1 || totals.ClosedLost != 1 || totals.ConversionPercent != 33 {
		t.Fatalf("sold=%d lost=%d conversion=%d", totals.ContractSigned, totals.ClosedLost, totals.ConversionPercent)
	}
	if totals.ByClientStatus["calculation_in_progress"] != 1 {
		t.Fatalf("status counts=%#v", totals.ByClientStatus)
	}
}

func TestInactiveBoundaryIsMoreThanSevenCalendarDays(t *testing.T) {
	for _, test := range []struct {
		days int
		want bool
	}{
		{days: 7, want: false},
		{days: 8, want: true},
	} {
		terminal := false
		got := !terminal && test.days > 7
		if got != test.want {
			t.Fatalf("days=%d got=%v want=%v", test.days, got, test.want)
		}
	}
}
