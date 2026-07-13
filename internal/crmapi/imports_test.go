package crmapi

import "testing"

func TestMapSheetRowWarsaw(t *testing.T) {
	lead, err := mapSheetRow(importSource{OfficeCode: "warsaw", SpreadsheetID: "sheet", SheetName: "Sheet1"}, map[string]any{
		"id":                          "123",
		"full_name":                   "Anna Kowalska",
		"phone_number":                "00 48 500 100 200",
		"jakiej_kuchni_szukasz?":      "Nowoczesna",
		"na_jakim_etapie_jesteś?":     "Projekt",
		"kiedy_planujesz_realizację?": "3 miesiące",
		"city":                        "Warszawa",
	})
	if err != nil {
		t.Fatal(err)
	}
	if lead.ExternalID != "l:123" || value(lead.Phone) != "+48500100200" {
		t.Fatalf("unexpected identity/phone: %#v", lead)
	}
	if value(lead.ProductInterest) != "Nowoczesna" || value(lead.ProjectStage) != "Projekt" || value(lead.CityRegion) != "Warszawa" || value(lead.SourceNote) != "3 miesiące" {
		t.Fatalf("unexpected Warsaw mapping: %#v", lead)
	}
}

func TestMapSheetRowFallbackIdentityAndKyivMapping(t *testing.T) {
	lead, err := mapSheetRow(importSource{OfficeCode: "kyiv", SpreadsheetID: "sheet-id", SheetName: "Leads"}, map[string]any{
		"_sheet_row":   42,
		"phone_number": "+380 50 111 22 33",
		"що_ви_хочете_замовити?":     "Кухня",
		"на_якому_етапі_ваш_проєкт?": "Заміри",
	})
	if err != nil {
		t.Fatal(err)
	}
	if lead.ExternalID != "google_sheet:sheet-id:Leads:42" || value(lead.ProductInterest) != "Кухня" || value(lead.ProjectStage) != "Заміри" {
		t.Fatalf("unexpected Kyiv mapping: %#v", lead)
	}
}

func TestMapSheetRowKyivSheet2Headers(t *testing.T) {
	lead, err := mapSheetRow(importSource{OfficeCode: "kyiv", SpreadsheetID: "sheet-id", SheetName: "Sheet2"}, map[string]any{
		"id":    "new-campaign-lead",
		"phone": "+380 67 222 33 44",
		"які_меблі_вам_потрібно_виготовити?":   "Меблі у спальню",
		"на_якому_етапі_перебуває_ваш_проєкт?": "Потрібен проєкт",
		"як_вам_зручно_спілкуватися?":          "Telegram",
	})
	if err != nil {
		t.Fatal(err)
	}
	if lead.ExternalID != "l:new-campaign-lead" || value(lead.Phone) != "+380672223344" {
		t.Fatalf("unexpected identity/phone: %#v", lead)
	}
	if value(lead.ProductInterest) != "Меблі у спальню" || value(lead.ProjectStage) != "Потрібен проєкт" || value(lead.CommunicationPreference) != "Telegram" {
		t.Fatalf("unexpected Sheet2 Kyiv mapping: %#v", lead)
	}
	if value(lead.SourceNote) != "Які меблі вам потрібно виготовити?: Меблі у спальню\nНа якому етапі перебуває ваш проєкт?: Потрібен проєкт\nЯк вам зручно спілкуватися?: Telegram" {
		t.Fatalf("unexpected Sheet2 source note: %#v", lead.SourceNote)
	}
}

func TestSourceNoteFromAnswersSkipsEmptyValues(t *testing.T) {
	if got := value(sourceNoteFromAnswers("", "Готовий до замірів", "")); got != "На якому етапі перебуває ваш проєкт?: Готовий до замірів" {
		t.Fatalf("sourceNoteFromAnswers() = %q", got)
	}
}

func value(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
