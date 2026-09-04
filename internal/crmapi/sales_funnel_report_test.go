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
	rates := currencyRateSet{PLNPerEUR: 4.2, UAHPerEUR: 52, UAHPerUSD: 44.2}

	stages, potential, contracts, comparisons := aggregateSalesFunnel([]salesFunnelLeadEvidence{
		{
			OfficeCode:         "kyiv",
			EstimatedBudget:    &budgetA,
			EstimatedCurrency:  "EUR",
			EstimatedRates:     rates,
			ExplicitReached:    true,
			ExplicitNotReached: true,
			ContractAmount:     &eurAmount,
			ContractCurrency:   &eur,
			ContractRates:      rates,
		},
		{OfficeCode: "kyiv", ClosedNoContact: true},
		{OfficeCode: "warsaw", ShowroomVisited: true, EstimatedBudget: &budgetB, EstimatedCurrency: "EUR", EstimatedRates: rates},
		{OfficeCode: "kyiv", MeasurementCompleted: true, ContractAmount: &uahAmount, ContractCurrency: &uah, ContractRates: rates},
		{OfficeCode: "kyiv", CalculationStarted: true},
		{OfficeCode: "warsaw"},
	}, []string{"kyiv", "warsaw"})

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
	if len(comparisons) != 2 {
		t.Fatalf("comparisons=%#v", comparisons)
	}
	if comparisons[0].OfficeCode != "kyiv" || comparisons[0].Currency != "UAH" ||
		comparisons[0].EstimatedTotal != 520_000 || comparisons[0].ActualTotal != 1_460_000 ||
		comparisons[0].Difference != 940_000 || comparisons[0].RealizationPercent == nil ||
		*comparisons[0].RealizationPercent != 280.8 {
		t.Fatalf("kyiv comparison=%#v", comparisons[0])
	}
	if comparisons[1].OfficeCode != "warsaw" || comparisons[1].Currency != "PLN" ||
		comparisons[1].EstimatedTotal != 23_100 || comparisons[1].RealizationPercent == nil ||
		*comparisons[1].RealizationPercent != 0 {
		t.Fatalf("warsaw comparison=%#v", comparisons[1])
	}
}

func TestAggregateSalesFunnelHandlesZeroDenominators(t *testing.T) {
	stages, potential, contracts, comparisons := aggregateSalesFunnel(nil, []string{"kyiv", "warsaw"})
	if stages.Leads.Percent != 0 || stages.Calls.Percent != 0 || stages.Reached.Percent != 0 {
		t.Fatalf("stages=%#v", stages)
	}
	if potential.Currency != "EUR" || potential.Total != 0 || len(contracts) != 0 || len(comparisons) != 2 {
		t.Fatalf("potential=%#v contracts=%#v comparisons=%#v", potential, contracts, comparisons)
	}
	for _, comparison := range comparisons {
		if comparison.EstimatedTotal != 0 || comparison.ActualTotal != 0 || comparison.Difference != 0 || comparison.RealizationPercent != nil {
			t.Fatalf("empty comparison=%#v", comparison)
		}
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
		"l.estimated_budget_currency",
		"public.currency_rate_sets",
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
