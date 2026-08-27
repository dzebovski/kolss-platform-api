package crmapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAggregateSalesFunnelUsesUniqueLeadsAndRestoresUpstreamStages(t *testing.T) {
	budgetA, budgetB := 10_000.0, 5_500.0
	eurAmount, uahAmount := 20_000.0, 420_000.0
	eur, uah := "EUR", "UAH"

	stages, potential, contracts := aggregateSalesFunnel([]salesFunnelLeadEvidence{
		{
			EstimatedBudget:    &budgetA,
			ExplicitReached:    true,
			ExplicitNotReached: true,
			ContractAmount:     &eurAmount,
			ContractCurrency:   &eur,
		},
		{ClosedNoContact: true},
		{ShowroomVisited: true, EstimatedBudget: &budgetB},
		{MeasurementCompleted: true, ContractAmount: &uahAmount, ContractCurrency: &uah},
		{CalculationStarted: true},
		{},
	})

	if stages.Leads.Count != 6 || stages.Leads.Percent != 100 {
		t.Fatalf("leads=%#v", stages.Leads)
	}
	if stages.Calls.Count != 5 || stages.Calls.Percent != 83 {
		t.Fatalf("calls=%#v", stages.Calls)
	}
	if stages.Reached.Count != 4 || stages.Reached.Percent != 80 {
		t.Fatalf("reached=%#v", stages.Reached)
	}
	if stages.NotReached.Count != 1 || stages.NotReached.Percent != 20 {
		t.Fatalf("notReached=%#v", stages.NotReached)
	}
	if stages.ShowroomInvited.Count != 1 || stages.ShowroomInvited.Percent != 25 ||
		stages.ShowroomVisited.Count != 1 || stages.ShowroomVisited.Percent != 100 {
		t.Fatalf("showroom invited=%#v visited=%#v", stages.ShowroomInvited, stages.ShowroomVisited)
	}
	if stages.MeasurementScheduled.Count != 1 || stages.MeasurementScheduled.Percent != 25 ||
		stages.MeasurementCompleted.Count != 1 || stages.MeasurementCompleted.Percent != 100 {
		t.Fatalf("measurement scheduled=%#v completed=%#v", stages.MeasurementScheduled, stages.MeasurementCompleted)
	}
	if stages.CalculationStarted.Count != 1 || stages.CalculationStarted.Percent != 25 {
		t.Fatalf("calculation=%#v", stages.CalculationStarted)
	}
	if potential.Currency != "EUR" || potential.Total != 15_500 {
		t.Fatalf("potential=%#v", potential)
	}
	if len(contracts) != 2 || contracts[0].Currency != "UAH" || contracts[0].Total != 420_000 ||
		contracts[1].Currency != "EUR" || contracts[1].Total != 20_000 {
		t.Fatalf("contracts=%#v", contracts)
	}
}

func TestAggregateSalesFunnelHandlesZeroDenominators(t *testing.T) {
	stages, potential, contracts := aggregateSalesFunnel(nil)
	if stages.Leads.Percent != 0 || stages.Calls.Percent != 0 || stages.Reached.Percent != 0 {
		t.Fatalf("stages=%#v", stages)
	}
	if potential.Currency != "EUR" || potential.Total != 0 || len(contracts) != 0 {
		t.Fatalf("potential=%#v contracts=%#v", potential, contracts)
	}
}

func TestBuildSalesFunnelQueryUsesOfficeLocalCohortAndHistoricalEvidence(t *testing.T) {
	query := strings.Join(strings.Fields(buildSalesFunnelQuery(nil)), " ")
	for _, fragment := range []string{
		"(coalesce(l.source_created_at,l.created_at) at time zone o.timezone_name)::date between $2::date and $3::date",
		"public.lead_contact_attempts",
		"e.event_category='call_status'",
		"l.loss_reason='no_contact'",
		"v.kind='showroom'",
		"v.kind='measurement'",
		"e.status_code='calculation_in_progress'",
		"order by c.signed_at desc nulls last,c.created_at desc limit 1",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("query missing %q", fragment)
		}
	}
	if strings.Contains(query, "l.archived_at is null") {
		t.Fatal("sales funnel cohort must include archived leads")
	}
}

func TestSalesFunnelPeriodIsRequired(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/reports/sales-funnel", nil)
	_, _, _, fields := parseReportPeriod(req, true)
	if fields["from"] == "" || fields["to"] == "" {
		t.Fatalf("fields=%#v", fields)
	}
}
