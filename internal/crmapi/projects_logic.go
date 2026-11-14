package crmapi

import (
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
	projectStatusPreliminaryEstimate = "preliminary_estimate"
	projectStatusMeasurementDesign   = "measurement_design"
	projectStatusFactoryApproval     = "factory_approval"
	projectStatusContractSigning     = "contract_signing"
	projectStatusProductionDelivery  = "production_delivery"
	projectStatusInstallation        = "installation"
	projectStatusCompleted           = "completed"
	projectStatusCancelled           = "cancelled"

	maxProjectTextLength    = 500
	maxProjectCommentLength = 1000
	maxPaymentNoteLength    = 300
	maxMoneyCents           = int64(100_000_000_000) // 1 000 000 000.00
)

// projectStages are the six pipeline stages (pipeline 4, owner decision 2026-11-14): the stage
// tracker and the kanban columns. Stages move only by hand (Change status / kanban drag).
var projectStages = []string{
	projectStatusPreliminaryEstimate, projectStatusMeasurementDesign, projectStatusFactoryApproval,
	projectStatusContractSigning, projectStatusProductionDelivery, projectStatusInstallation,
}

// projectStatuses is every value of projects.status: the stages, then the two finals set only by
// Project finished (completed) and Close project (cancelled).
var projectStatuses = append(append([]string{}, projectStages...), projectStatusCompleted, projectStatusCancelled)

// projectBoardStatuses are the kanban columns: the six stages (completed and cancelled are filters).
var projectBoardStatuses = projectStages

func isProjectStage(status string) bool {
	for _, stage := range projectStages {
		if stage == status {
			return true
		}
	}
	return false
}

// isProjectFinal: completed and cancelled projects are read-only until restored.
func isProjectFinal(status string) bool {
	return status == projectStatusCompleted || status == projectStatusCancelled
}

// canFinishProject: Project finished is pressed from the last stage, Installation (Q-P20 default).
func canFinishProject(status string) bool { return status == projectStatusInstallation }

// restoreTarget is the stage a final project returns to: a cancelled one to the stage it was
// closed from (preliminary estimate when unknown or when it was completed), a completed one to
// Installation.
func restoreTarget(status string, beforeCancel *string) (string, bool) {
	switch status {
	case projectStatusCompleted:
		return projectStatusInstallation, true
	case projectStatusCancelled:
		if beforeCancel != nil && isProjectStage(*beforeCancel) {
			return *beforeCancel, true
		}
		return projectStatusPreliminaryEstimate, true
	}
	return "", false
}

// projectTypes are the values the lead / Create project "Project type" may hold. Since pipeline
// 2.1 the type no longer sets the first status (every project starts as `estimate`).
var projectTypes = map[string]struct{}{"express": {}, "measure": {}, "contract": {}}

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

// projectFirstStatus: a project starts at the preliminary estimate.
func projectFirstStatus() string { return projectStatusPreliminaryEstimate }

type projectStatusRequest struct {
	Status    string  `json:"status"`
	EventDate *string `json:"eventDate"`
	EventTime *string `json:"eventTime"`
	// Legacy measure / express fields: accepted and ignored since pipeline 3.
	ResponsibleID *string `json:"responsibleId"`
	What          *string `json:"what"`
	Address       *string `json:"address"`
	Done          *bool   `json:"done"`
	Result        *string `json:"result"`
	Comment       *string `json:"comment"`
}

// projectStatusInput is a validated Change status request.
type projectStatusInput struct {
	Status    string
	EventDate *string // YYYY-MM-DD
	EventTime *string // HH:MM
	Comment   *string
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

// validateProjectStatus: Change status picks one of the six stages (forwards or backwards) with
// an optional event date (+ time) and comment. Completed is set by Project finished and cancelled
// by Close project. The old measure fields are accepted and ignored.
func validateProjectStatus(req projectStatusRequest) (projectStatusInput, map[string]string) {
	fields := map[string]string{}
	in := projectStatusInput{Status: strings.TrimSpace(req.Status)}
	if !isProjectStage(in.Status) {
		switch in.Status {
		case projectStatusCompleted:
			fields["status"] = "Use Project finished"
		case projectStatusCancelled:
			fields["status"] = "Use Close project"
		default:
			fields["status"] = "Must be preliminary_estimate, measurement_design, factory_approval, contract_signing, production_delivery, or installation"
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
	return in, fields
}

type projectCancelRequest struct {
	Reasons            []string `json:"reasons"`
	Comment            *string  `json:"comment"`
	CancelFutureEvents bool     `json:"cancelFutureEvents"`
}

// validateProjectCancel: at least one known reason and always a comment describing it in detail
// (pipeline 3, 2026-11-07; before, only "other" needed one).
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
	if comment == "" {
		fields["comment"] = "Describe the reason in detail"
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

// Payment bases (Q-P21 default): what the money is recorded against.
var projectPaymentBases = map[string]struct{}{"receipt": {}, "invoice": {}, "contract": {}, "other": {}}

// projectPaymentRequest: a standalone payment (pipeline 4) — amount, currency, date, basis, an
// optional note and an optional receipt / invoice file.
type projectPaymentRequest struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
	PaidOn   string  `json:"paidOn"`
	Basis    string  `json:"basis"`
	Note     *string `json:"note"`
	FileID   *string `json:"fileId"`
}

type projectPaymentInput struct {
	AmountCents int64
	Currency    string
	PaidOn      string
	Basis       string
	Note        *string
	FileID      *uuid.UUID
}

func validateProjectPayment(req projectPaymentRequest) (projectPaymentInput, map[string]string) {
	fields := map[string]string{}
	in := projectPaymentInput{Currency: strings.TrimSpace(req.Currency), Basis: strings.TrimSpace(req.Basis)}
	if cents, ok := moneyToCents(req.Amount); ok {
		in.AmountCents = cents
	} else if req.Amount == 0 {
		fields["amount"] = "Required"
	} else {
		fields["amount"] = "Must be a positive amount with at most 2 decimals"
	}
	if _, ok := projectCurrencies[in.Currency]; !ok {
		fields["currency"] = "Must be PLN, UAH, EUR, or USD"
	}
	if date, ok := parseProjectDate(req.PaidOn); ok {
		in.PaidOn = date
	} else if strings.TrimSpace(req.PaidOn) == "" {
		fields["paidOn"] = "Required"
	} else {
		fields["paidOn"] = "Must be a date (YYYY-MM-DD)"
	}
	if _, ok := projectPaymentBases[in.Basis]; !ok {
		fields["basis"] = "Must be receipt, invoice, contract, or other"
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

// ---- payment edits ---------------------------------------------------------------------------

type projectPaymentPatch struct {
	Amount   *float64 `json:"amount"`
	Currency *string  `json:"currency"`
	PaidOn   *string  `json:"paidOn"`
	Basis    *string  `json:"basis"`
	Note     *string  `json:"note"` // empty string clears the note
	FileID   *string  `json:"fileId"`
}

type projectPaymentChange struct {
	AmountCents *int64
	Currency    *string
	PaidOn      *string
	Basis       *string
	Note        *string // set when present; "" clears
	FileID      *uuid.UUID
}

func (c projectPaymentChange) empty() bool {
	return c.AmountCents == nil && c.Currency == nil && c.PaidOn == nil && c.Basis == nil && c.Note == nil && c.FileID == nil
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
	if req.Currency != nil {
		currency := strings.TrimSpace(*req.Currency)
		if _, ok := projectCurrencies[currency]; ok {
			out.Currency = &currency
		} else {
			fields["currency"] = "Must be PLN, UAH, EUR, or USD"
		}
	}
	if req.PaidOn != nil {
		if date, ok := parseProjectDate(*req.PaidOn); ok {
			out.PaidOn = &date
		} else {
			fields["paidOn"] = "Must be a date (YYYY-MM-DD)"
		}
	}
	if req.Basis != nil {
		basis := strings.TrimSpace(*req.Basis)
		if _, ok := projectPaymentBases[basis]; ok {
			out.Basis = &basis
		} else {
			fields["basis"] = "Must be receipt, invoice, contract, or other"
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

// ---- estimate: one total ----------------------------------------------------------------------

// projectEstimateRequest: {"amount": 12000, "currency": "PLN"} sets the preliminary estimate
// total, {"amount": null} clears it (Q-P24 default).
type projectEstimateRequest struct {
	Amount   *float64 `json:"amount"`
	Currency *string  `json:"currency"`
}

type projectEstimate struct {
	AmountCents int64
	Currency    string
}

// validateProjectEstimate returns nil for "clear the estimate".
func validateProjectEstimate(req projectEstimateRequest) (*projectEstimate, map[string]string) {
	fields := map[string]string{}
	if req.Amount == nil {
		return nil, fields
	}
	out := projectEstimate{Currency: trimmedText(req.Currency)}
	if cents, ok := moneyToCents(*req.Amount); ok {
		out.AmountCents = cents
	} else {
		fields["amount"] = "Must be a positive amount with at most 2 decimals (send null to clear)"
	}
	if _, ok := projectCurrencies[out.Currency]; !ok {
		fields["currency"] = "Must be PLN, UAH, EUR, or USD"
	}
	return &out, fields
}

func (e *projectEstimate) toMap() map[string]any {
	if e == nil {
		return nil
	}
	return map[string]any{"amount": centsToMoney(e.AmountCents), "currency": e.Currency}
}

func equalProjectEstimate(a, b *projectEstimate) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
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
