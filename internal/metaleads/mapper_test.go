package metaleads

import "testing"

func TestMapLeadPreservesCustomAnswersAndEmailOnlyContact(t *testing.T) {
	lead := Lead{
		ID:           "12345",
		CreatedTime:  "2026-07-21T12:30:00+0000",
		FormID:       "form-1",
		CampaignID:   "campaign-1",
		CampaignName: "Kyiv kitchens",
		FieldData: []LeadField{
			{Name: "first_name", Values: []string{"Олена"}},
			{Name: "last_name", Values: []string{"Коваль"}},
			{Name: "email", Values: []string{"OLENA@EXAMPLE.COM"}},
			{Name: "які_меблі_вам_потрібно_виготовити?", Values: []string{"Кухня"}},
			{Name: "бажаний_термін", Values: []string{"Вересень"}},
		},
	}
	mapped := MapLead(lead)
	if value(mapped.Name) != "Олена Коваль" || mapped.Phone != nil || value(mapped.Email) != "olena@example.com" {
		t.Fatalf("unexpected contacts: %#v", mapped)
	}
	if value(mapped.ProductInterest) != "Кухня" {
		t.Fatalf("product=%v", mapped.ProductInterest)
	}
	if note := value(mapped.SourceNote); note == "" || !containsAll(note, "Кухня", "Вересень") {
		t.Fatalf("custom answers were not preserved: %q", note)
	}
	if mapped.SourceCreatedAt == nil || mapped.SourceCreatedAt.Location().String() != "UTC" {
		t.Fatalf("created time was not parsed: %#v", mapped.SourceCreatedAt)
	}
	if mapped.ExternalID != "l:12345" {
		t.Fatalf("external ID=%q", mapped.ExternalID)
	}
}

func TestMapLeadPolishAliasesAndPhoneNormalization(t *testing.T) {
	lead := Lead{ID: "pl-1", FieldData: []LeadField{
		{Name: "full_name", Values: []string{"Jan Kowalski"}},
		{Name: "phone_number", Values: []string{"00 48 600-100-200"}},
		{Name: "jakiej_kuchni_szukasz?", Values: []string{"Nowoczesna"}},
		{Name: "na_jakim_etapie_jesteś?", Values: []string{"Projekt"}},
	}}
	mapped := MapLead(lead)
	if value(mapped.Phone) != "+48600100200" || value(mapped.ProductInterest) != "Nowoczesna" || value(mapped.ProjectStage) != "Projekt" {
		t.Fatalf("unexpected Polish mapping: %#v", mapped)
	}
}

func TestMapLeadKeepsPhoneOnlyLead(t *testing.T) {
	mapped := MapLead(Lead{ID: "phone-only", FieldData: []LeadField{
		{Name: "phone", Values: []string{"+380 50 123 45 67"}},
	}})
	if mapped.Name != nil || mapped.Email != nil || value(mapped.Phone) != "+380501234567" {
		t.Fatalf("unexpected phone-only mapping: %#v", mapped)
	}
}

func TestMapLeadKeepsUnknownAnswersWithoutStandardFields(t *testing.T) {
	mapped := MapLead(Lead{ID: "custom-only", FieldData: []LeadField{
		{Name: "Коли плануєте замовлення?", Values: []string{"Восени"}},
	}})
	if mapped.Name != nil || mapped.Phone != nil || mapped.Email != nil {
		t.Fatalf("unexpected standard fields: %#v", mapped)
	}
	if note := value(mapped.SourceNote); !containsAll(note, "Коли плануєте замовлення?", "Восени") {
		t.Fatalf("unknown answer was lost: %q", note)
	}
}

func value(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		found := false
		for i := 0; i+len(part) <= len(value); i++ {
			if value[i:i+len(part)] == part {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
