package crmapi

import (
	"encoding/json"
	"slices"
	"testing"
)

func decodeLeadInfo(t *testing.T, body string) leadInfoRequest {
	t.Helper()
	var req leadInfoRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return req
}

func TestValidateLeadInfo(t *testing.T) {
	valid := decodeLeadInfo(t, `{"estimatedBudgetText":"20 000 – 25 000","estimatedBudgetCurrency":"PLN","products":["kitchen"],"expectedLeadTime":"8–12 weeks","preferredMeasurementAt":null}`)
	if fields := validateLeadInfo(valid); len(fields) != 0 {
		t.Fatalf("valid request rejected: %v", fields)
	}
	if fields := validateLeadInfo(decodeLeadInfo(t, `{"estimatedBudgetText":""}`)); len(fields) != 0 {
		t.Fatalf("clearing the budget must be allowed: %v", fields)
	}
	invalid := decodeLeadInfo(t, `{"estimatedBudgetText":"about 20k","estimatedBudgetCurrency":"GBP","products":["boat"]}`)
	fields := validateLeadInfo(invalid)
	for _, key := range []string{"estimatedBudgetText", "estimatedBudgetCurrency", "products"} {
		if fields[key] == "" {
			t.Fatalf("expected an error on %s, got %v", key, fields)
		}
	}
}

func TestApplyLeadInfoOnlyChangesSentFields(t *testing.T) {
	city := "Warsaw"
	current := leadInfo{BudgetCurrency: "EUR", CityRegion: &city, Products: []string{"kitchen"}, OfficeCode: "warsaw"}

	req := decodeLeadInfo(t, `{"estimatedBudgetText":" 20 000 – 25 000 ","cityRegion":"Warsaw","products":["kitchen","bathroom","kitchen"],"materialFronts":"Oak veneer","preferredMeasurementAt":"2026-10-05T08:00:00Z"}`)
	next, changed, values := applyLeadInfo(current, req)

	if want := []string{"budget", "product", "materials", "preferredMeasurement"}; !slices.Equal(changed, want) {
		t.Fatalf("changed = %v, want %v", changed, want)
	}
	if next.BudgetText == nil || *next.BudgetText != "20 000 – 25 000" || next.Budget == nil || *next.Budget != 20000 {
		t.Fatalf("budget = %v / %v", next.BudgetText, next.Budget)
	}
	if next.BudgetCurrency != "PLN" || !next.BudgetRateChanged {
		t.Fatalf("a first budget takes the office currency: %s", next.BudgetCurrency)
	}
	if !slices.Equal(next.Products, []string{"kitchen", "bathroom"}) {
		t.Fatalf("products = %v", next.Products)
	}
	if next.Worktop != nil || next.LeadTime != nil {
		t.Fatal("fields that were not sent must stay as they are")
	}
	if _, ok := values["city_region"]; ok {
		t.Fatal("an unchanged city must not be in the event")
	}
}

func TestApplyLeadInfoClearsAndKeeps(t *testing.T) {
	text := "20 000"
	budget := 20000.0
	current := leadInfo{BudgetText: &text, Budget: &budget, BudgetCurrency: "PLN", OfficeCode: "warsaw"}

	next, changed, _ := applyLeadInfo(current, decodeLeadInfo(t, `{"estimatedBudgetText":""}`))
	if !slices.Equal(changed, []string{"budget"}) || next.BudgetText != nil || next.Budget != nil {
		t.Fatalf("clearing: changed=%v text=%v budget=%v", changed, next.BudgetText, next.Budget)
	}

	_, changed, _ = applyLeadInfo(current, decodeLeadInfo(t, `{"estimatedBudgetText":"20 000","preferredMeasurementAt":null}`))
	if len(changed) != 0 {
		t.Fatalf("same budget value and an already empty date are no change: %v", changed)
	}
}
