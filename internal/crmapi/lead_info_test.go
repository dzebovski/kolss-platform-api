package crmapi

import (
	"encoding/json"
	"slices"
	"strings"
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

func TestValidateLeadInfoW9Fields(t *testing.T) {
	valid := decodeLeadInfo(t, `{"aboutClient":"Wants an island kitchen.","referredBy":"Jan Kowalski","projectType":"express","responsibleManagerId":"11111111-1111-1111-1111-111111111111","clientInformed":true,"checklistBudget":true}`)
	if fields := validateLeadInfo(valid); len(fields) != 0 {
		t.Fatalf("valid W9 request rejected: %v", fields)
	}
	if fields := validateLeadInfo(decodeLeadInfo(t, `{"projectType":""}`)); len(fields) != 0 {
		t.Fatalf("clearing projectType must be allowed: %v", fields)
	}
	if fields := validateLeadInfo(decodeLeadInfo(t, `{"responsibleManagerId":""}`)); len(fields) != 0 {
		t.Fatalf("clearing responsibleManagerId must be allowed: %v", fields)
	}

	invalid := decodeLeadInfo(t, `{"aboutClient":"`+strings.Repeat("a", maxAboutClientLength+1)+`","referredBy":"`+strings.Repeat("a", maxReferredByLength+1)+`","projectType":"measurement","responsibleManagerId":"not-a-uuid"}`)
	fields := validateLeadInfo(invalid)
	for _, key := range []string{"aboutClient", "referredBy", "projectType", "responsibleManagerId"} {
		if fields[key] == "" {
			t.Fatalf("expected an error on %s, got %v", key, fields)
		}
	}
}

func TestApplyLeadInfoW9ChecklistAndProjectFields(t *testing.T) {
	current := leadInfo{BudgetCurrency: "EUR", OfficeCode: "warsaw"}

	req := decodeLeadInfo(t, `{"checklistBudget":true,"checklistLocation":true,"clientInformed":true,"projectType":"express","responsibleManagerId":"11111111-1111-1111-1111-111111111111","aboutClient":"New house","referredBy":"Anna"}`)
	next, changed, values := applyLeadInfo(current, req)

	wantChanged := []string{"aboutClient", "referredBy", "checklist", "clientInformed", "projectType", "responsibleManager"}
	if !slices.Equal(changed, wantChanged) {
		t.Fatalf("changed = %v, want %v", changed, wantChanged)
	}
	if next.ChecklistBudget == nil || !*next.ChecklistBudget || next.ChecklistLocation == nil || !*next.ChecklistLocation {
		t.Fatalf("checklist not applied: budget=%v location=%v", next.ChecklistBudget, next.ChecklistLocation)
	}
	if next.ChecklistPeriod != nil || next.ChecklistMaterials != nil || next.ChecklistProduct != nil {
		t.Fatal("checklist items that were not sent must stay as they are")
	}
	if next.ClientInformed == nil || !*next.ClientInformed {
		t.Fatal("clientInformed not applied")
	}
	if next.ProjectType == nil || *next.ProjectType != "express" {
		t.Fatalf("projectType = %v", next.ProjectType)
	}
	if next.ResponsibleManagerID == nil || next.ResponsibleManagerID.String() != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("responsibleManagerId = %v", next.ResponsibleManagerID)
	}
	if _, ok := values["checklist"]; !ok {
		t.Fatal("checklist change must be recorded in the event")
	}

	// Re-sending the same checklist state is not a change.
	_, changed, _ = applyLeadInfo(next, decodeLeadInfo(t, `{"checklistBudget":true,"checklistLocation":true}`))
	if len(changed) != 0 {
		t.Fatalf("same checklist values are no change: %v", changed)
	}

	// Clearing projectType and responsibleManagerId.
	cleared, changed, _ := applyLeadInfo(next, decodeLeadInfo(t, `{"projectType":"","responsibleManagerId":""}`))
	if !slices.Equal(changed, []string{"projectType", "responsibleManager"}) {
		t.Fatalf("changed = %v", changed)
	}
	if cleared.ProjectType != nil || cleared.ResponsibleManagerID != nil {
		t.Fatalf("clearing failed: projectType=%v responsibleManagerId=%v", cleared.ProjectType, cleared.ResponsibleManagerID)
	}
}
