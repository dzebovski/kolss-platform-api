package biuroimport

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	SourceSystem         = "google_sheet_backfill"
	SourceChannel        = "other"
	ImportSource         = "biuro_leads_status"
	DefaultSpreadsheetID = "18XwMuKXc60-QOhz-nJ9nGVqJZShPC36RjHeuMyPTeeA"
	DefaultSheetID       = int64(1919340999)
	DefaultSheetName     = "Sheet1"
	DefaultOfficeCode    = "warsaw"
	DefaultYear          = 2026

	firstConversationComment = "Pierwsza rozmowa z klientem — import z arkusza Biuro Leads Status"
)

var (
	fullDatePattern  = regexp.MustCompile(`^(\d{1,2})[./-](\d{1,2})[./-](\d{4})$`)
	shortDatePattern = regexp.MustCompile(`^(\d{1,2})[./-](\d{1,2})$`)
	nonDigitsPattern = regexp.MustCompile(`\D`)
)

type SourceConfig struct {
	SpreadsheetID string
	SheetID       int64
	SheetName     string
	OfficeCode    string
}

func (c SourceConfig) Validate() error {
	if strings.TrimSpace(c.SpreadsheetID) == "" {
		return errors.New("spreadsheet id is required")
	}
	if c.SheetID <= 0 {
		return errors.New("sheet id must be positive")
	}
	if strings.TrimSpace(c.SheetName) == "" {
		return errors.New("sheet name is required")
	}
	if c.OfficeCode != DefaultOfficeCode {
		return fmt.Errorf("unsupported office code %q", c.OfficeCode)
	}
	return nil
}

type Warning struct {
	Code     string `json:"code"`
	SheetRow int    `json:"sheetRow,omitempty"`
	LP       string `json:"lp,omitempty"`
	Message  string `json:"message"`
}

type DateValue struct {
	Time         time.Time
	UsedFallback bool
	InferredYear bool
}

type PhoneValue struct {
	Stored     string
	Normalized string
	IsRaw      bool
	IsEmpty    bool
}

type SourceRow struct {
	SheetRow int
	LP       string
	LeadDate DateValue

	ManagerRaw        string
	ManagerCanonical  string
	FirstConversation *time.Time
	Measurement       *time.Time

	Name          string
	Phone         PhoneValue
	Email         string
	CityRegion    string
	Information   string
	InfoSlawek    string
	StatusComment string
}

func (r SourceRow) sourceKey(config SourceConfig) string {
	if r.LP != "" {
		return fmt.Sprintf("%s:%d:lp:%s", config.SpreadsheetID, config.SheetID, r.LP)
	}
	return fmt.Sprintf("%s:%d:row:%d", config.SpreadsheetID, config.SheetID, r.SheetRow)
}

type FirstConversation struct {
	At          time.Time
	ManagerName string
	SourceKey   string
}

type LogicalLead struct {
	ExternalID        string
	SourceCreatedAt   time.Time
	Name              string
	Phone             string
	NormalizedPhone   string
	Email             string
	CityRegion        string
	ManagerName       string
	Rows              []SourceRow
	FirstConversation *FirstConversation
	LastComment       string
	LastCommentAt     *time.Time
}

type ParsedSource struct {
	Config          SourceConfig
	SHA256          string
	SourceRows      int
	LogicalLeads    []LogicalLead
	DuplicateGroups int
	Warnings        []Warning
}

type rawPayload struct {
	ImportSource  string          `json:"import_source"`
	SpreadsheetID string          `json:"spreadsheet_id"`
	SheetID       int64           `json:"sheet_id"`
	SheetName     string          `json:"sheet_name"`
	SourceRows    []rawPayloadRow `json:"source_rows"`
}

type rawPayloadRow struct {
	SheetRow     int    `json:"sheet_row"`
	LP           string `json:"lp,omitempty"`
	ManagerRaw   string `json:"manager_raw,omitempty"`
	PhoneRaw     string `json:"phone_raw,omitempty"`
	LeadDate     string `json:"lead_date"`
	DateFallback bool   `json:"date_fallback,omitempty"`
	YearInferred bool   `json:"year_inferred,omitempty"`
}

func (lead LogicalLead) RawPayload(config SourceConfig) ([]byte, error) {
	rows := make([]rawPayloadRow, 0, len(lead.Rows))
	for _, row := range lead.Rows {
		item := rawPayloadRow{
			SheetRow:     row.SheetRow,
			LP:           row.LP,
			ManagerRaw:   row.ManagerRaw,
			LeadDate:     row.LeadDate.Time.Format(time.RFC3339),
			DateFallback: row.LeadDate.UsedFallback,
			YearInferred: row.LeadDate.InferredYear,
		}
		if row.Phone.IsRaw {
			item.PhoneRaw = row.Phone.Stored
		}
		rows = append(rows, item)
	}
	return json.Marshal(rawPayload{
		ImportSource:  ImportSource,
		SpreadsheetID: config.SpreadsheetID,
		SheetID:       config.SheetID,
		SheetName:     config.SheetName,
		SourceRows:    rows,
	})
}

func Parse(input []byte, config SourceConfig) (ParsedSource, error) {
	if err := config.Validate(); err != nil {
		return ParsedSource{}, err
	}
	location, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		return ParsedSource{}, fmt.Errorf("load Warsaw timezone: %w", err)
	}

	sum := sha256.Sum256(input)
	source := ParsedSource{
		Config: config,
		SHA256: hex.EncodeToString(sum[:]),
	}

	reader := csv.NewReader(strings.NewReader(string(input)))
	reader.FieldsPerRecord = -1
	reader.ReuseRecord = false
	records, err := reader.ReadAll()
	if err != nil {
		return ParsedSource{}, fmt.Errorf("read CSV: %w", err)
	}
	if len(records) == 0 {
		return ParsedSource{}, errors.New("CSV is empty")
	}
	if err := validateHeaders(records[0]); err != nil {
		return ParsedSource{}, err
	}

	rows := make([]SourceRow, 0, len(records)-1)
	for index, record := range records[1:] {
		sheetRow := index + 2
		if !hasMappedData(record) {
			continue
		}
		row, rowWarnings, err := parseRow(record, sheetRow, location)
		if err != nil {
			return ParsedSource{}, err
		}
		rows = append(rows, row)
		source.Warnings = append(source.Warnings, rowWarnings...)
	}
	source.SourceRows = len(rows)
	source.LogicalLeads, source.DuplicateGroups = mergeRows(rows, config)
	return source, nil
}

func validateHeaders(headers []string) error {
	expected := map[int]string{
		0:  "LP",
		2:  "Data Lida",
		3:  "Kto pierwszy rozmawiał z klientem",
		4:  "Data pierwszej rozmowy z klientem",
		5:  "Data pomiaru",
		6:  "Budżet klienta",
		7:  "Imię i Nazwisko",
		8:  "Nr tel",
		9:  "E-mail",
		10: "Miasto/ adres",
		11: "Informacja",
		12: "Info SŁAWEK",
		14: "Status",
	}
	for index, want := range expected {
		got := strings.TrimSpace(cell(headers, index))
		if index == 0 {
			got = strings.TrimPrefix(got, "\uFEFF")
		}
		if got != want {
			return fmt.Errorf("unexpected header at column %d: got %q, want %q", index+1, got, want)
		}
	}
	return nil
}

func hasMappedData(record []string) bool {
	for _, index := range []int{2, 3, 4, 5, 7, 8, 9, 10, 11, 12, 14} {
		if strings.TrimSpace(cell(record, index)) != "" {
			return true
		}
	}
	return false
}

func parseRow(record []string, sheetRow int, location *time.Location) (SourceRow, []Warning, error) {
	lp := strings.TrimSpace(cell(record, 0))
	leadDate, err := parseLeadDate(cell(record, 2), location)
	if err != nil {
		return SourceRow{}, nil, fmt.Errorf("row %d (LP %s) Data Lida: %w", sheetRow, lp, err)
	}
	firstConversation, firstInferred, err := parseOptionalDate(cell(record, 4), location)
	if err != nil {
		return SourceRow{}, nil, fmt.Errorf("row %d (LP %s) first conversation date: %w", sheetRow, lp, err)
	}
	measurement, measurementInferred, err := parseOptionalDate(cell(record, 5), location)
	if err != nil {
		return SourceRow{}, nil, fmt.Errorf("row %d (LP %s) measurement date: %w", sheetRow, lp, err)
	}

	managerRaw := strings.TrimSpace(cell(record, 3))
	managerCanonical := canonicalManager(managerRaw)
	phone := parsePhone(cell(record, 8))
	row := SourceRow{
		SheetRow:          sheetRow,
		LP:                lp,
		LeadDate:          leadDate,
		ManagerRaw:        managerRaw,
		ManagerCanonical:  managerCanonical,
		FirstConversation: firstConversation,
		Measurement:       measurement,
		Name:              strings.TrimSpace(cell(record, 7)),
		Phone:             phone,
		Email:             strings.TrimSpace(cell(record, 9)),
		CityRegion:        strings.TrimSpace(cell(record, 10)),
		Information:       strings.TrimSpace(cell(record, 11)),
		InfoSlawek:        strings.TrimSpace(cell(record, 12)),
		StatusComment:     strings.TrimSpace(cell(record, 14)),
	}

	warnings := make([]Warning, 0, 5)
	addWarning := func(code, message string) {
		warnings = append(warnings, Warning{Code: code, SheetRow: sheetRow, LP: lp, Message: message})
	}
	if leadDate.UsedFallback {
		addWarning("lead_date_fallback", "Data Lida is empty; using 2026-01-01 12:00 Europe/Warsaw")
	} else if leadDate.InferredYear {
		addWarning("lead_date_year_inferred", "Data Lida has no year; using 2026")
	}
	if firstInferred {
		addWarning("first_conversation_year_inferred", "first conversation date has no year; using 2026")
	}
	if measurementInferred {
		addWarning("measurement_year_inferred", "measurement date has no year; using 2026")
	}
	if managerRaw != "" && managerCanonical == "" {
		addWarning("manager_unmapped", "manager is not one of Sławek, Andrii, or Danuta; lead will be unassigned")
	}
	if managerRaw == "" {
		addWarning("manager_empty", "manager is empty; lead will be unassigned")
	}
	if phone.IsEmpty {
		addWarning("phone_empty", "phone is empty; NULL will be stored")
	}
	if phone.IsRaw {
		addWarning("phone_raw", "phone is ambiguous; the trimmed raw value will be stored")
	}
	return row, warnings, nil
}

func parseLeadDate(value string, location *time.Location) (DateValue, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return DateValue{
			Time:         time.Date(DefaultYear, time.January, 1, 12, 0, 0, 0, location),
			UsedFallback: true,
		}, nil
	}
	parsed, inferred, err := parseDate(value, location)
	return DateValue{Time: parsed, InferredYear: inferred}, err
}

func parseOptionalDate(value string, location *time.Location) (*time.Time, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, false, nil
	}
	parsed, inferred, err := parseDate(value, location)
	if err != nil {
		return nil, false, err
	}
	return &parsed, inferred, nil
}

func parseDate(value string, location *time.Location) (time.Time, bool, error) {
	var day, month, year int
	inferred := false
	if matches := fullDatePattern.FindStringSubmatch(value); matches != nil {
		day, _ = strconv.Atoi(matches[1])
		month, _ = strconv.Atoi(matches[2])
		year, _ = strconv.Atoi(matches[3])
	} else if matches := shortDatePattern.FindStringSubmatch(value); matches != nil {
		day, _ = strconv.Atoi(matches[1])
		month, _ = strconv.Atoi(matches[2])
		year = DefaultYear
		inferred = true
	} else {
		return time.Time{}, false, fmt.Errorf("unsupported date %q", value)
	}
	parsed := time.Date(year, time.Month(month), day, 12, 0, 0, 0, location)
	if parsed.Year() != year || int(parsed.Month()) != month || parsed.Day() != day {
		return time.Time{}, false, fmt.Errorf("invalid date %q", value)
	}
	return parsed, inferred, nil
}

func parsePhone(value string) PhoneValue {
	value = strings.TrimSpace(value)
	if value == "" {
		return PhoneValue{IsEmpty: true}
	}
	digits := nonDigitsPattern.ReplaceAllString(value, "")
	switch {
	case len(digits) == 9:
		normalized := "+48" + digits
		return PhoneValue{Stored: normalized, Normalized: normalized}
	case len(digits) == 11 && strings.HasPrefix(digits, "48"):
		normalized := "+" + digits
		return PhoneValue{Stored: normalized, Normalized: normalized}
	case len(digits) == 13 && strings.HasPrefix(digits, "0048"):
		normalized := "+" + strings.TrimPrefix(digits, "00")
		return PhoneValue{Stored: normalized, Normalized: normalized}
	default:
		return PhoneValue{Stored: value, IsRaw: true}
	}
}

func canonicalManager(value string) string {
	normalized := strings.ToLower(strings.Join(strings.Fields(value), " "))
	switch normalized {
	case "sławek", "slawek":
		return "Sławek"
	case "andrii":
		return "Andrii"
	case "danuta":
		return "Danuta"
	default:
		return ""
	}
}

func canonicalProfileManager(displayName string) string {
	fields := strings.Fields(displayName)
	if len(fields) == 0 {
		return ""
	}
	identity := fields[0]
	if at := strings.IndexByte(identity, '@'); at >= 0 {
		identity = identity[:at]
	}
	return canonicalManager(identity)
}

func mergeRows(rows []SourceRow, config SourceConfig) ([]LogicalLead, int) {
	groups := make(map[string][]SourceRow)
	order := make([]string, 0, len(rows))
	for _, row := range rows {
		key := "source:" + row.sourceKey(config)
		if row.Phone.Normalized != "" {
			key = "phone:" + row.Phone.Normalized
		}
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], row)
	}

	leads := make([]LogicalLead, 0, len(groups))
	duplicateGroups := 0
	for _, key := range order {
		group := append([]SourceRow(nil), groups[key]...)
		sort.SliceStable(group, func(i, j int) bool {
			if group[i].LeadDate.Time.Equal(group[j].LeadDate.Time) {
				return group[i].SheetRow < group[j].SheetRow
			}
			return group[i].LeadDate.Time.Before(group[j].LeadDate.Time)
		})
		if len(group) > 1 {
			duplicateGroups++
		}
		lead := LogicalLead{
			ExternalID:      "sheet:" + group[0].sourceKey(config),
			SourceCreatedAt: group[0].LeadDate.Time,
			Rows:            group,
		}
		for _, row := range group {
			if row.Name != "" {
				lead.Name = row.Name
			}
			if row.Phone.Stored != "" {
				lead.Phone = row.Phone.Stored
			}
			if row.Phone.Normalized != "" {
				lead.NormalizedPhone = row.Phone.Normalized
			}
			if row.Email != "" {
				lead.Email = row.Email
			}
			if row.CityRegion != "" {
				lead.CityRegion = row.CityRegion
			}
			if row.ManagerCanonical != "" {
				lead.ManagerName = row.ManagerCanonical
			}
			if row.FirstConversation != nil {
				if lead.FirstConversation == nil || row.FirstConversation.Before(lead.FirstConversation.At) {
					lead.FirstConversation = &FirstConversation{
						At:          *row.FirstConversation,
						ManagerName: row.ManagerCanonical,
						SourceKey:   row.sourceKey(config),
					}
				}
			}
			for _, comment := range row.commentEvents(config) {
				if lead.LastCommentAt == nil || comment.At.After(*lead.LastCommentAt) {
					at := comment.At
					lead.LastComment = comment.Body
					lead.LastCommentAt = &at
				}
			}
		}
		leads = append(leads, lead)
	}
	return leads, duplicateGroups
}

type commentEvent struct {
	ID          uuid.UUID
	At          time.Time
	Body        string
	ManagerName string
	Kind        string
	SourceKey   string
}

func (row SourceRow) commentEvents(config SourceConfig) []commentEvent {
	key := row.sourceKey(config)
	events := make([]commentEvent, 0, 4)
	appendEvent := func(kind, body, manager string, at time.Time) {
		if body == "" {
			return
		}
		events = append(events, commentEvent{
			ID:          stableID(config, key, kind),
			At:          at,
			Body:        body,
			ManagerName: manager,
			Kind:        kind,
			SourceKey:   key,
		})
	}
	if row.Measurement != nil {
		appendEvent(
			"measurement",
			"Data pomiaru: "+row.Measurement.Format("02.01.2006"),
			row.ManagerCanonical,
			row.Measurement.Add(30*time.Second),
		)
	}
	appendEvent("information", row.Information, row.ManagerCanonical, row.LeadDate.Time.Add(time.Minute))
	appendEvent("info_slawek", row.InfoSlawek, "Sławek", row.LeadDate.Time.Add(2*time.Minute))
	appendEvent("status", row.StatusComment, row.ManagerCanonical, row.LeadDate.Time.Add(3*time.Minute))
	return events
}

func stableID(config SourceConfig, parts ...string) uuid.UUID {
	all := []string{ImportSource, config.SpreadsheetID, strconv.FormatInt(config.SheetID, 10)}
	all = append(all, parts...)
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(strings.Join(all, "|")))
}

func sourceCreatedEventID(config SourceConfig, externalID string) uuid.UUID {
	return stableID(config, externalID, "source_created")
}

func firstConversationAttemptID(config SourceConfig, sourceKey string) uuid.UUID {
	return stableID(config, sourceKey, "first_conversation_attempt")
}

func firstConversationEventID(config SourceConfig, sourceKey string) uuid.UUID {
	return stableID(config, sourceKey, "first_conversation_event")
}

func cell(record []string, index int) string {
	if index < 0 || index >= len(record) {
		return ""
	}
	return record[index]
}
