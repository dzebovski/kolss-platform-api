package biuroimport

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"
)

func TestParseMergesNormalizedPhoneAndPreservesCommentOrder(t *testing.T) {
	input := fixtureCSV(t, [][]string{
		{
			"5", "", "2.03", "Marta", "", "", "",
			"Jakubowska Justyna", "698 631 622", "", "Łomianki",
			"pierwsza informacja", "", "", "pierwszy status",
		},
		{
			"81", "", "10-06-2026", "Sławek", "10.06.2026", "", "",
			"Justyna Jakubowska", "698-631-622", "JUSTYNA@example.com", "Łomianki",
			"druga informacja\nz nową linią", "komentarz Sławka", "", "drugi status",
		},
		{
			"85", "", "", "Andrii", "", "18.06.2026", "",
			"P. Patrycja", "brak", "", "Warszawa",
			"informacja o pomiarze", "", "", "",
		},
		{"96"},
	})

	source, err := Parse(input, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if source.SourceRows != 3 {
		t.Fatalf("source rows = %d, want 3", source.SourceRows)
	}
	if len(source.LogicalLeads) != 2 || source.DuplicateGroups != 1 {
		t.Fatalf("logical leads/duplicates = %d/%d, want 2/1", len(source.LogicalLeads), source.DuplicateGroups)
	}

	merged := source.LogicalLeads[0]
	if merged.Name != "Justyna Jakubowska" {
		t.Fatalf("latest non-empty name = %q", merged.Name)
	}
	if merged.Phone != "+48698631622" || merged.NormalizedPhone != "+48698631622" {
		t.Fatalf("phone = %q normalized=%q", merged.Phone, merged.NormalizedPhone)
	}
	if merged.ManagerName != "Sławek" {
		t.Fatalf("manager = %q", merged.ManagerName)
	}
	if merged.FirstConversation == nil || merged.FirstConversation.ManagerName != "Sławek" {
		t.Fatalf("first conversation = %#v", merged.FirstConversation)
	}
	if got := merged.SourceCreatedAt.Format("2006-01-02 15:04 MST"); got != "2026-03-02 12:00 CET" {
		t.Fatalf("source created at = %s", got)
	}
	if merged.LastComment != "drugi status" {
		t.Fatalf("last comment = %q", merged.LastComment)
	}

	events := merged.Rows[1].commentEvents(testConfig())
	if len(events) != 3 {
		t.Fatalf("second row comment events = %d, want 3", len(events))
	}
	if events[0].Kind != "information" || events[1].Kind != "info_slawek" || events[2].Kind != "status" {
		t.Fatalf("unexpected event order: %#v", events)
	}
	if events[0].Body != "druga informacja\nz nową linią" {
		t.Fatalf("multiline body changed: %q", events[0].Body)
	}

	rawLead := source.LogicalLeads[1]
	if rawLead.Phone != "brak" || rawLead.NormalizedPhone != "" {
		t.Fatalf("raw phone = %q normalized=%q", rawLead.Phone, rawLead.NormalizedPhone)
	}
	if !rawLead.Rows[0].LeadDate.UsedFallback {
		t.Fatal("expected missing Data Lida fallback")
	}
	measurementEvents := rawLead.Rows[0].commentEvents(testConfig())
	if measurementEvents[0].Kind != "measurement" ||
		measurementEvents[0].Body != "Data pomiaru: 18.06.2026" {
		t.Fatalf("measurement event = %#v", measurementEvents[0])
	}

	if !hasWarning(source.Warnings, "lead_date_year_inferred") ||
		!hasWarning(source.Warnings, "lead_date_fallback") ||
		!hasWarning(source.Warnings, "phone_raw") ||
		!hasWarning(source.Warnings, "manager_unmapped") {
		t.Fatalf("missing expected warnings: %#v", source.Warnings)
	}
}

func TestParseRejectsChangedHeaderAndInvalidDate(t *testing.T) {
	input := fixtureCSV(t, [][]string{{"1", "", "31.02.2026", "Sławek", "", "", "", "Test", "500 100 200"}})
	_, err := Parse(input, testConfig())
	if err == nil || !strings.Contains(err.Error(), "invalid date") {
		t.Fatalf("invalid date error = %v", err)
	}

	records := fixtureRecords()
	records[0][11] = "Other"
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	if err := writer.WriteAll(records); err != nil {
		t.Fatal(err)
	}
	_, err = Parse(buffer.Bytes(), testConfig())
	if err == nil || !strings.Contains(err.Error(), "unexpected header") {
		t.Fatalf("header error = %v", err)
	}
}

func TestParsePhone(t *testing.T) {
	tests := []struct {
		input      string
		stored     string
		normalized string
		raw        bool
		empty      bool
	}{
		{input: "502 187 001", stored: "+48502187001", normalized: "+48502187001"},
		{input: "+48 502-187-001", stored: "+48502187001", normalized: "+48502187001"},
		{input: "0048 502 187 001", stored: "+48502187001", normalized: "+48502187001"},
		{input: "Tel. 695 047 540", stored: "+48695047540", normalized: "+48695047540"},
		{input: "brak", stored: "brak", raw: true},
		{input: "503 150 300, 503 118 940", stored: "503 150 300, 503 118 940", raw: true},
		{input: "(176) 372 968 50", stored: "(176) 372 968 50", raw: true},
		{input: " ", empty: true},
	}
	for _, test := range tests {
		got := parsePhone(test.input)
		if got.Stored != test.stored || got.Normalized != test.normalized || got.IsRaw != test.raw || got.IsEmpty != test.empty {
			t.Errorf("parsePhone(%q) = %#v", test.input, got)
		}
	}
}

func TestCanonicalManagerAndStableIDs(t *testing.T) {
	if canonicalManager(" Slawek ") != "Sławek" {
		t.Fatal("Slawek alias was not resolved")
	}
	if canonicalManager("Danuta Slawek") != "" || canonicalManager("Marta") != "" {
		t.Fatal("ambiguous or historical manager was resolved")
	}
	if canonicalProfileManager("Sławek Kowalski") != "Sławek" ||
		canonicalProfileManager("Andrii Nowak") != "Andrii" ||
		canonicalProfileManager("andrii@kolss.eu") != "Andrii" ||
		canonicalProfileManager("Danuta Wiśniewska") != "Danuta" {
		t.Fatal("full profile display name was not resolved")
	}
	if canonicalProfileManager("Marta Sławek") != "" {
		t.Fatal("profile was resolved from a non-leading name token")
	}
	config := testConfig()
	first := stableID(config, "row:1", "status")
	second := stableID(config, "row:1", "status")
	different := stableID(config, "row:1", "information")
	if first != second || first == different {
		t.Fatalf("stable IDs are not deterministic: %s %s %s", first, second, different)
	}
}

func TestDateParsingUsesWarsawNoonAndDefaultYear(t *testing.T) {
	location, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		t.Fatal(err)
	}
	full, inferred, err := parseDate("19-03-2026", location)
	if err != nil || inferred {
		t.Fatalf("full date = %s inferred=%v err=%v", full, inferred, err)
	}
	short, inferred, err := parseDate("2.03", location)
	if err != nil || !inferred {
		t.Fatalf("short date = %s inferred=%v err=%v", short, inferred, err)
	}
	if got := short.Format(time.RFC3339); got != "2026-03-02T12:00:00+01:00" {
		t.Fatalf("short date = %s", got)
	}
	fallback, err := parseLeadDate("", location)
	if err != nil || !fallback.UsedFallback || fallback.Time.Format("2006-01-02T15:04") != "2026-01-01T12:00" {
		t.Fatalf("fallback = %#v err=%v", fallback, err)
	}
}

func fixtureCSV(t *testing.T, dataRows [][]string) []byte {
	t.Helper()
	records := [][]string{fixtureHeaders()}
	records = append(records, dataRows...)
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	writer.UseCRLF = true
	if err := writer.WriteAll(records); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func fixtureRecords() [][]string {
	return [][]string{fixtureHeaders()}
}

func fixtureHeaders() []string {
	return []string{
		"LP", "", "Data Lida", "Kto pierwszy rozmawiał z klientem",
		"Data pierwszej rozmowy z klientem", "Data pomiaru", "Budżet klienta",
		"Imię i Nazwisko", "Nr tel", "E-mail", "Miasto/ adres", "Informacja",
		"Info SŁAWEK", "", "Status", "Manager/Projektant", "source/źródło",
		"Status ENG", "CURRENT STATUS", "WYCENA projektanta",
	}
}

func testConfig() SourceConfig {
	return SourceConfig{
		SpreadsheetID: DefaultSpreadsheetID,
		SheetID:       DefaultSheetID,
		SheetName:     DefaultSheetName,
		OfficeCode:    DefaultOfficeCode,
	}
}

func hasWarning(warnings []Warning, code string) bool {
	for _, warning := range warnings {
		if warning.Code == code {
			return true
		}
	}
	return false
}
