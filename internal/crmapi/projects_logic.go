package crmapi

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CRM v2 projects (task P1). Owner decisions of 2026-09-29 (Q-P1…Q-P7) and the behaviour spec of
// the "KOLSS CRM v2" boards (Project-*, Create-project). This file is the pure part: vocabulary,
// validation and money maths, unit-tested without a database.

const (
	projectStatusEstimate      = "estimate"
	projectStatusIntroContract = "intro_contract"
	projectStatusMeasure       = "measure"
	projectStatusDesign        = "design"
	projectStatusMainContract  = "main_contract"
	projectStatusPrepayment    = "prepayment"
	projectStatusFactory       = "factory"
	projectStatusProduction    = "production"
	projectStatusFinalPayment  = "final_payment"
	projectStatusInstallation  = "installation"
	projectStatusCompleted     = "completed"
	projectStatusCancelled     = "cancelled"

	contractKindIntro = "intro"
	contractKindMain  = "main"

	maxProjectTextLength    = 500
	maxProjectCommentLength = 1000
	maxPaymentNoteLength    = 300
	maxContractNumberLength = 60
	maxEstimateNoteLength   = 300
	maxMoneyCents           = int64(100_000_000_000) // 1 000 000 000.00
)

// projectStatuses is the pipeline order shown by the stage tracker and the kanban (cancelled is
// outside it). Owner decision 2026-10-23.
var projectStatuses = []string{
	projectStatusEstimate, projectStatusIntroContract, projectStatusMeasure, projectStatusDesign,
	projectStatusMainContract, projectStatusPrepayment, projectStatusFactory, projectStatusProduction,
	projectStatusFinalPayment, projectStatusInstallation, projectStatusCompleted, projectStatusCancelled,
}

// projectChangeableStatuses can be picked in Change status (by hand, forwards or backwards).
// `intro_contract` / `main_contract` are set only by adding a contract and `cancelled` only by
// Cancel project (Q-P15…Q-P17).
var projectChangeableStatuses = map[string]struct{}{
	projectStatusEstimate: {}, projectStatusMeasure: {}, projectStatusDesign: {}, projectStatusPrepayment: {},
	projectStatusFactory: {}, projectStatusProduction: {}, projectStatusFinalPayment: {},
	projectStatusInstallation: {}, projectStatusCompleted: {},
}

// projectStatusIndex is the position in the pipeline (-1 for unknown / cancelled).
func projectStatusIndex(status string) int {
	for i, candidate := range projectStatuses {
		if candidate == status && status != projectStatusCancelled {
			return i
		}
	}
	return -1
}

// contractStatusFor is the status adding a contract of the given kind moves the project to.
func contractStatusFor(kind string) string {
	if kind == contractKindIntro {
		return projectStatusIntroContract
	}
	return projectStatusMainContract
}

// statusAfterContract applies the "automatic on contract add" rule conservatively: the project
// only moves forward. A contract entered late (project already further along) keeps its status.
func statusAfterContract(current, kind string) string {
	target := contractStatusFor(kind)
	if projectStatusIndex(current) < projectStatusIndex(target) {
		return target
	}
	return current
}

// projectTypes are the values the lead / Create project "Project type" may hold. Since pipeline
// 2.1 the type no longer sets the first status (every project starts as `estimate`).
var projectTypes = map[string]struct{}{"express": {}, "measure": {}, "contract": {}}

// estimateLines are the four optional estimate lines, in display order.
var estimateLines = []string{"kitchen", "appliances", "installation", "countertop"}

var projectCancelReasons = map[string]struct{}{
	"chose_another_supplier": {}, "price_too_high": {}, "postponed_renovation": {},
	"disagreed_design": {}, "not_relevant": {}, "other": {},
}

var projectCurrencies = map[string]struct{}{"PLN": {}, "UAH": {}, "EUR": {}, "USD": {}}

// CanEditProject: the project's responsible manager and admins of its office (super admin
// everywhere) may change it; everyone else with office access only views, comments and adds
// documents (Q-P4).
func (a Actor) CanEditProject(officeID uuid.UUID, managerID *uuid.UUID) bool {
	if a.IsSuperAdmin() {
		return true
	}
	if !a.CanAccessOffice(officeID) {
		return false
	}
	if a.Role == "office_admin" {
		return true
	}
	return managerID != nil && *managerID == a.ID
}

// projectFirstStatus: a project is created by hand while the estimate is being worked on (2026-10-23).
func projectFirstStatus() string { return projectStatusEstimate }

type projectStatusRequest struct {
	Status        string  `json:"status"`
	EventDate     *string `json:"eventDate"`
	EventTime     *string `json:"eventTime"`
	ResponsibleID *string `json:"responsibleId"`
	What          *string `json:"what"` // legacy (express); accepted and ignored since pipeline 2.1
	Address       *string `json:"address"`
	Done          *bool   `json:"done"`
	Result        *string `json:"result"`
	Comment       *string `json:"comment"`
}

// projectStatusInput is a validated Change status request.
type projectStatusInput struct {
	Status        string
	EventDate     *string // YYYY-MM-DD
	EventTime     *string // HH:MM
	ResponsibleID *uuid.UUID
	Details       map[string]any // stored under status_details[status] for measure
	Comment       *string
}

func parseProjectDate(value string) (string, bool) {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(value))
	if err != nil {
		return "", false
	}
	return t.Format("2006-01-02"), true
}

func parseProjectTime(value string) (string, bool) {
	t, err := time.Parse("15:04", strings.TrimSpace(value))
	if err != nil {
		return "", false
	}
	return t.Format("15:04"), true
}

func trimmedText(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

// validateProjectStatus applies the per-status rules of the Change status popup: measure —
// responsible, date + time, address, took place + result; completed — nothing but the optional
// comment; every other stage — an optional event date (+ time). The express rules are gone.
func validateProjectStatus(req projectStatusRequest) (projectStatusInput, map[string]string) {
	fields := map[string]string{}
	in := projectStatusInput{Status: strings.TrimSpace(req.Status)}
	if _, ok := projectChangeableStatuses[in.Status]; !ok {
		switch in.Status {
		case projectStatusIntroContract, projectStatusMainContract:
			fields["status"] = "Contract stages are set by adding a contract"
		case projectStatusCancelled:
			fields["status"] = "Use Cancel project"
		default:
			fields["status"] = "Must be estimate, measure, design, prepayment, factory, production, final_payment, installation, or completed"
		}
		return in, fields
	}

	if comment := trimmedText(req.Comment); comment != "" {
		if len([]rune(comment)) > maxProjectCommentLength {
			fields["comment"] = "Must be at most 1000 characters"
		}
		in.Comment = &comment
	}

	if date := trimmedText(req.EventDate); date != "" {
		if parsed, ok := parseProjectDate(date); ok {
			in.EventDate = &parsed
		} else {
			fields["eventDate"] = "Must be a date (YYYY-MM-DD)"
		}
	}
	if clock := trimmedText(req.EventTime); clock != "" {
		if parsed, ok := parseProjectTime(clock); ok {
			in.EventTime = &parsed
		} else {
			fields["eventTime"] = "Must be a time (HH:MM)"
		}
	}
	if in.EventTime != nil && in.EventDate == nil && fields["eventDate"] == "" {
		fields["eventDate"] = "Required when a time is set"
	}

	switch in.Status {
	case projectStatusCompleted:
		if in.EventDate != nil || in.EventTime != nil {
			fields["eventDate"] = "Not allowed for this status"
		}
	case projectStatusMeasure:
		validateMeasureDetails(&in, req, fields)
	default:
		// The remaining stages keep the optional event parsed above.
	}
	return in, fields
}

func validateMeasureDetails(in *projectStatusInput, req projectStatusRequest, fields map[string]string) {
	details := map[string]any{}

	if id, err := uuid.Parse(trimmedText(req.ResponsibleID)); err != nil {
		fields["responsibleId"] = "Required"
	} else {
		in.ResponsibleID = &id
		details["responsibleId"] = id.String()
	}

	if in.EventDate == nil && fields["eventDate"] == "" {
		fields["eventDate"] = "Required"
	}
	if in.EventTime == nil && fields["eventTime"] == "" {
		fields["eventTime"] = "Required"
	}
	address := trimmedText(req.Address)
	switch {
	case address == "":
		fields["address"] = "Required"
	case len([]rune(address)) > maxProjectTextLength:
		fields["address"] = "Must be at most 500 characters"
	default:
		details["address"] = address
	}

	done := req.Done != nil && *req.Done
	details["done"] = done
	result := trimmedText(req.Result)
	switch {
	case done && result == "":
		fields["result"] = "Required once it is done"
	case len([]rune(result)) > maxProjectTextLength:
		fields["result"] = "Must be at most 500 characters"
	case result != "":
		details["result"] = result
	}
	in.Details = details
}

type projectCancelRequest struct {
	Reasons            []string `json:"reasons"`
	Comment            *string  `json:"comment"`
	CancelFutureEvents bool     `json:"cancelFutureEvents"`
}

// validateProjectCancel: at least one known reason; a comment is required when "other" is picked.
func validateProjectCancel(req projectCancelRequest) ([]string, *string, map[string]string) {
	fields := map[string]string{}
	seen := map[string]struct{}{}
	reasons := make([]string, 0, len(req.Reasons))
	for _, raw := range req.Reasons {
		reason := strings.TrimSpace(raw)
		if _, ok := projectCancelReasons[reason]; !ok {
			fields["reasons"] = "Unknown reason"
			continue
		}
		if _, dup := seen[reason]; dup {
			continue
		}
		seen[reason] = struct{}{}
		reasons = append(reasons, reason)
	}
	if len(reasons) == 0 && fields["reasons"] == "" {
		fields["reasons"] = "Pick at least one reason"
	}
	comment := trimmedText(req.Comment)
	if _, other := seen["other"]; other && comment == "" {
		fields["comment"] = "Describe the reason in a few words"
	}
	if len([]rune(comment)) > maxProjectCommentLength {
		fields["comment"] = "Must be at most 1000 characters"
	}
	var out *string
	if comment != "" {
		out = &comment
	}
	return reasons, out, fields
}

type projectContractRequest struct {
	Kind        *string `json:"kind"` // intro | main; main when omitted (pre-2.36 clients)
	Number      string  `json:"number"`
	SignedOn    string  `json:"signedOn"`
	TotalAmount float64 `json:"totalAmount"`
	Currency    string  `json:"currency"`
	FileID      *string `json:"fileId"`
}

type projectContractInput struct {
	Kind       string
	Number     string
	SignedOn   string
	TotalCents int64
	Currency   string
	FileID     *uuid.UUID
}

func validateProjectContract(req projectContractRequest) (projectContractInput, map[string]string) {
	fields := map[string]string{}
	in := projectContractInput{Kind: contractKindMain, Number: strings.TrimSpace(req.Number), Currency: strings.TrimSpace(req.Currency)}
	if kind := trimmedText(req.Kind); kind != "" {
		if kind != contractKindIntro && kind != contractKindMain {
			fields["kind"] = "Must be intro or main"
		}
		in.Kind = kind
	}
	switch {
	case in.Number == "":
		fields["number"] = "Required"
	case len([]rune(in.Number)) > maxContractNumberLength:
		fields["number"] = "Must be at most 60 characters"
	}
	if date, ok := parseProjectDate(req.SignedOn); ok {
		in.SignedOn = date
	} else if strings.TrimSpace(req.SignedOn) == "" {
		fields["signedOn"] = "Required"
	} else {
		fields["signedOn"] = "Must be a date (YYYY-MM-DD)"
	}
	if cents, ok := moneyToCents(req.TotalAmount); ok {
		in.TotalCents = cents
	} else if req.TotalAmount == 0 {
		fields["totalAmount"] = "Required"
	} else {
		fields["totalAmount"] = "Must be a positive amount with at most 2 decimals"
	}
	if _, ok := projectCurrencies[in.Currency]; !ok {
		fields["currency"] = "Must be PLN, UAH, EUR, or USD"
	}
	if id, ok := parseOptionalUUID(req.FileID, "fileId", fields); ok {
		in.FileID = id
	}
	return in, fields
}

type projectPaymentRequest struct {
	ContractID *string `json:"contractId"` // main contract when omitted (intro if it is the only one)
	Amount     float64 `json:"amount"`
	PaidOn     string  `json:"paidOn"`
	Note       *string `json:"note"`
	FileID     *string `json:"fileId"`
}

type projectPaymentInput struct {
	ContractID  *uuid.UUID
	AmountCents int64
	PaidOn      string
	Note        *string
	FileID      *uuid.UUID
}

func validateProjectPayment(req projectPaymentRequest) (projectPaymentInput, map[string]string) {
	fields := map[string]string{}
	in := projectPaymentInput{}
	if id, ok := parseOptionalUUID(req.ContractID, "contractId", fields); ok {
		in.ContractID = id
	}
	if cents, ok := moneyToCents(req.Amount); ok {
		in.AmountCents = cents
	} else if req.Amount == 0 {
		fields["amount"] = "Required"
	} else {
		fields["amount"] = "Must be a positive amount with at most 2 decimals"
	}
	if date, ok := parseProjectDate(req.PaidOn); ok {
		in.PaidOn = date
	} else if strings.TrimSpace(req.PaidOn) == "" {
		fields["paidOn"] = "Required"
	} else {
		fields["paidOn"] = "Must be a date (YYYY-MM-DD)"
	}
	if note := trimmedText(req.Note); note != "" {
		if len([]rune(note)) > maxPaymentNoteLength {
			fields["note"] = "Must be at most 300 characters"
		}
		in.Note = &note
	}
	if id, ok := parseOptionalUUID(req.FileID, "fileId", fields); ok {
		in.FileID = id
	}
	return in, fields
}

func parseOptionalUUID(value *string, field string, fields map[string]string) (*uuid.UUID, bool) {
	text := trimmedText(value)
	if text == "" {
		return nil, false
	}
	id, err := uuid.Parse(text)
	if err != nil {
		fields[field] = "Must be a valid id"
		return nil, false
	}
	return &id, true
}

// moneyToCents converts a JSON amount in major units to integer minor units. It rejects zero,
// negatives, more than 2 decimals and absurd sizes, so sums are always exact integers.
func moneyToCents(amount float64) (int64, bool) {
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 {
		return 0, false
	}
	scaled := amount * 100
	cents := math.Round(scaled)
	if math.Abs(scaled-cents) > 1e-6 || cents < 1 || cents > float64(maxMoneyCents) {
		return 0, false
	}
	return int64(cents), true
}

// centsToMoney is the inverse used for JSON output (major units).
func centsToMoney(cents int64) float64 { return float64(cents) / 100 }

// formatMoneyCents renders 4000000 as "40 000" / 123456 as "1 234.56" for error messages.
func formatMoneyCents(cents int64) string {
	whole := cents / 100
	frac := cents % 100
	digits := fmt.Sprintf("%d", whole)
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	if frac != 0 {
		return fmt.Sprintf("%s.%02d", b.String(), frac)
	}
	return b.String()
}

// ---- contract / payment edits ---------------------------------------------------------------

// projectContractPatch is a partial contract edit (PATCH): only the fields that are present change.
type projectContractPatch struct {
	Number      *string  `json:"number"`
	SignedOn    *string  `json:"signedOn"`
	TotalAmount *float64 `json:"totalAmount"`
	Currency    *string  `json:"currency"`
	FileID      *string  `json:"fileId"`
}

type projectContractChange struct {
	Number     *string
	SignedOn   *string
	TotalCents *int64
	Currency   *string
	FileID     *uuid.UUID
}

func (c projectContractChange) empty() bool {
	return c.Number == nil && c.SignedOn == nil && c.TotalCents == nil && c.Currency == nil && c.FileID == nil
}

func validateProjectContractPatch(req projectContractPatch) (projectContractChange, map[string]string) {
	fields := map[string]string{}
	var out projectContractChange
	if req.Number != nil {
		number := strings.TrimSpace(*req.Number)
		switch {
		case number == "":
			fields["number"] = "Required"
		case len([]rune(number)) > maxContractNumberLength:
			fields["number"] = "Must be at most 60 characters"
		default:
			out.Number = &number
		}
	}
	if req.SignedOn != nil {
		if date, ok := parseProjectDate(*req.SignedOn); ok {
			out.SignedOn = &date
		} else {
			fields["signedOn"] = "Must be a date (YYYY-MM-DD)"
		}
	}
	if req.TotalAmount != nil {
		if cents, ok := moneyToCents(*req.TotalAmount); ok {
			out.TotalCents = &cents
		} else {
			fields["totalAmount"] = "Must be a positive amount with at most 2 decimals"
		}
	}
	if req.Currency != nil {
		currency := strings.TrimSpace(*req.Currency)
		if _, ok := projectCurrencies[currency]; ok {
			out.Currency = &currency
		} else {
			fields["currency"] = "Must be PLN, UAH, EUR, or USD"
		}
	}
	if id, ok := parseOptionalUUID(req.FileID, "fileId", fields); ok {
		out.FileID = id
	}
	return out, fields
}

type projectPaymentPatch struct {
	Amount *float64 `json:"amount"`
	PaidOn *string  `json:"paidOn"`
	Note   *string  `json:"note"` // empty string clears the note
	FileID *string  `json:"fileId"`
}

type projectPaymentChange struct {
	AmountCents *int64
	PaidOn      *string
	Note        *string // set when present; "" clears
	FileID      *uuid.UUID
}

func (c projectPaymentChange) empty() bool {
	return c.AmountCents == nil && c.PaidOn == nil && c.Note == nil && c.FileID == nil
}

func validateProjectPaymentPatch(req projectPaymentPatch) (projectPaymentChange, map[string]string) {
	fields := map[string]string{}
	var out projectPaymentChange
	if req.Amount != nil {
		if cents, ok := moneyToCents(*req.Amount); ok {
			out.AmountCents = &cents
		} else {
			fields["amount"] = "Must be a positive amount with at most 2 decimals"
		}
	}
	if req.PaidOn != nil {
		if date, ok := parseProjectDate(*req.PaidOn); ok {
			out.PaidOn = &date
		} else {
			fields["paidOn"] = "Must be a date (YYYY-MM-DD)"
		}
	}
	if req.Note != nil {
		note := strings.TrimSpace(*req.Note)
		if len([]rune(note)) > maxPaymentNoteLength {
			fields["note"] = "Must be at most 300 characters"
		}
		out.Note = &note
	}
	if id, ok := parseOptionalUUID(req.FileID, "fileId", fields); ok {
		out.FileID = id
	}
	return out, fields
}

// ---- money: intro credited to main -------------------------------------------------------------

// projectMoney is the money state of one project in integer cents. The paid intro amount is
// credited to the main contract when both are in the same currency; 50% of the main total is a
// target shown on the card, never a gate (owner decision 2026-10-23).
type projectMoney struct {
	HasIntro, HasMain           bool
	IntroTotal, IntroPaid       int64
	MainTotal, MainPaid         int64
	IntroCurrency, MainCurrency string
}

func (m projectMoney) credited() int64 {
	if !m.HasIntro || !m.HasMain || m.IntroCurrency != m.MainCurrency {
		return 0
	}
	return min(m.IntroPaid, m.MainTotal)
}

// mainPaidTotal is the main contract's own payments plus the credit from the intro contract.
func (m projectMoney) mainPaidTotal() int64 { return m.MainPaid + m.credited() }

func (m projectMoney) mainRemaining() int64 { return max(0, m.MainTotal-m.mainPaidTotal()) }

func (m projectMoney) introRemaining() int64 { return max(0, m.IntroTotal-m.IntroPaid) }

// target50 is half of the main total, rounded up to the cent.
func (m projectMoney) target50() int64 { return (m.MainTotal + 1) / 2 }

// maxPayment is the largest amount that can still be recorded on the given contract kind, with
// the reason when it is zero. An intro payment may also not push the main contract past its total
// (the credited amount would exceed it).
func (m projectMoney) maxPayment(kind string) int64 {
	if kind == contractKindIntro {
		limit := m.introRemaining()
		if m.HasMain && m.IntroCurrency == m.MainCurrency {
			limit = min(limit, max(0, m.MainTotal-m.MainPaid-m.IntroPaid))
		}
		return limit
	}
	return m.mainRemaining()
}

// ---- estimate ---------------------------------------------------------------------------------

type estimateLineRequest struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
	Note     *string `json:"note"`
}

type estimateLine struct {
	AmountCents int64
	Currency    string
	Note        *string
}

// validateEstimate parses {"lines": {"kitchen": {...}|null, ...}}. A present key replaces the
// line, `null` removes it, a missing key leaves it alone. The result maps line -> new value
// (nil = remove).
func validateEstimate(raw map[string]json.RawMessage) (map[string]*estimateLine, map[string]string) {
	fields := map[string]string{}
	out := map[string]*estimateLine{}
	known := map[string]struct{}{}
	for _, name := range estimateLines {
		known[name] = struct{}{}
	}
	for name, value := range raw {
		if _, ok := known[name]; !ok {
			fields["lines."+name] = "Must be kitchen, appliances, installation, or countertop"
			continue
		}
		if strings.TrimSpace(string(value)) == "null" {
			out[name] = nil
			continue
		}
		var req estimateLineRequest
		decoder := json.NewDecoder(strings.NewReader(string(value)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			fields["lines."+name] = "Must be {amount, currency, note} or null"
			continue
		}
		line := estimateLine{Currency: strings.TrimSpace(req.Currency)}
		if cents, ok := moneyToCents(req.Amount); ok {
			line.AmountCents = cents
		} else {
			fields["lines."+name+".amount"] = "Must be a positive amount with at most 2 decimals (send null to remove the line)"
		}
		if _, ok := projectCurrencies[line.Currency]; !ok {
			fields["lines."+name+".currency"] = "Must be PLN, UAH, EUR, or USD"
		}
		if note := trimmedText(req.Note); note != "" {
			if len([]rune(note)) > maxEstimateNoteLength {
				fields["lines."+name+".note"] = "Must be at most 300 characters"
			}
			line.Note = &note
		}
		out[name] = &line
	}
	if len(raw) == 0 {
		fields["lines"] = "Send at least one line"
	}
	return out, fields
}

func (l *estimateLine) toMap() map[string]any {
	if l == nil {
		return nil
	}
	out := map[string]any{"amount": centsToMoney(l.AmountCents), "currency": l.Currency}
	if l.Note != nil {
		out["note"] = *l.Note
	}
	return out
}

func equalEstimateLine(a, b *estimateLine) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.AmountCents != b.AmountCents || a.Currency != b.Currency {
		return false
	}
	if a.Note == nil || b.Note == nil {
		return a.Note == b.Note
	}
	return *a.Note == *b.Note
}

// ---- comment / details ------------------------------------------------------------------------

type projectCommentRequest struct {
	Text string `json:"text"`
}

func validateProjectComment(req projectCommentRequest) (string, map[string]string) {
	fields := map[string]string{}
	text := strings.TrimSpace(req.Text)
	switch {
	case text == "":
		fields["text"] = "Required"
	case len([]rune(text)) > maxProjectCommentLength:
		fields["text"] = "Must be at most 1000 characters"
	}
	return text, fields
}

// projectContractRow is a contract with its own payments (cents), used for the balance rules.
type projectContractRow struct {
	ID         uuid.UUID
	Kind       string
	TotalCents int64
	Currency   string
	PaidCents  int64
}

func moneyFromContracts(rows []projectContractRow) projectMoney {
	var m projectMoney
	for _, row := range rows {
		switch row.Kind {
		case contractKindIntro:
			m.HasIntro, m.IntroTotal, m.IntroPaid, m.IntroCurrency = true, row.TotalCents, row.PaidCents, row.Currency
		case contractKindMain:
			m.HasMain, m.MainTotal, m.MainPaid, m.MainCurrency = true, row.TotalCents, row.PaidCents, row.Currency
		}
	}
	return m
}

// overpaid reports a state the money rules forbid: more paid on a contract than its total (the
// main contract counting the full paid intro amount of the same currency). The message is for
// the contract total field.
func (m projectMoney) overpaid() (field, message string) {
	if m.HasIntro && m.IntroPaid > m.IntroTotal {
		return "totalAmount", "Less than the already paid " + formatMoneyCents(m.IntroPaid) + " " + m.IntroCurrency
	}
	if m.HasMain {
		paid := m.MainPaid
		if m.HasIntro && m.IntroCurrency == m.MainCurrency {
			paid += m.IntroPaid
		}
		if paid > m.MainTotal {
			return "totalAmount", "Less than the already paid " + formatMoneyCents(paid) + " " + m.MainCurrency + " (the intro payment counts)"
		}
	}
	return "", ""
}

// pickPaymentContract chooses the contract a payment belongs to: the given id, otherwise the
// main contract, otherwise the intro contract (clients written before pipeline 2.1 send no id).
func pickPaymentContract(rows []projectContractRow, id *uuid.UUID) (projectContractRow, bool) {
	var main, intro *projectContractRow
	for i := range rows {
		if id != nil && rows[i].ID == *id {
			return rows[i], true
		}
		switch rows[i].Kind {
		case contractKindMain:
			main = &rows[i]
		case contractKindIntro:
			intro = &rows[i]
		}
	}
	if id != nil {
		return projectContractRow{}, false
	}
	if main != nil {
		return *main, true
	}
	if intro != nil {
		return *intro, true
	}
	return projectContractRow{}, false
}
