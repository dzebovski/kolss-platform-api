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
	projectStatusNone         = "none"
	projectStatusExpress      = "express"
	projectStatusMeasure      = "measure"
	projectStatusDesign       = "design"
	projectStatusContract     = "contract"
	projectStatusProduction   = "production"
	projectStatusInstallation = "installation"
	projectStatusCompleted    = "completed"
	projectStatusCancelled    = "cancelled"

	maxProjectTextLength    = 500
	maxProjectCommentLength = 1000
	maxPaymentNoteLength    = 300
	maxContractNumberLength = 60
	maxMoneyCents           = int64(100_000_000_000) // 1 000 000 000.00
)

// projectStatuses is the lifecycle order shown by the stage tracker (cancelled is outside it).
var projectStatuses = []string{
	projectStatusNone, projectStatusExpress, projectStatusMeasure, projectStatusDesign, projectStatusContract,
	projectStatusProduction, projectStatusInstallation, projectStatusCompleted, projectStatusCancelled,
}

// projectChangeableStatuses can be picked in Change status. `contract` is only set by Add contract,
// `cancelled` only by Cancel project, `none` only at creation.
var projectChangeableStatuses = map[string]struct{}{
	projectStatusExpress: {}, projectStatusMeasure: {}, projectStatusDesign: {},
	projectStatusProduction: {}, projectStatusInstallation: {}, projectStatusCompleted: {},
}

// projectTypeFirstStatus is the status the Create project "Project type" sets (Q-P3).
var projectTypeFirstStatus = map[string]string{
	"express": projectStatusExpress, "measure": projectStatusMeasure, "contract": projectStatusDesign,
}

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

// projectFirstStatus returns the status a new project starts in for the given (optional) type.
func projectFirstStatus(projectType *string) string {
	if projectType == nil {
		return projectStatusNone
	}
	if status, ok := projectTypeFirstStatus[*projectType]; ok {
		return status
	}
	return projectStatusNone
}

type projectStatusRequest struct {
	Status        string  `json:"status"`
	EventDate     *string `json:"eventDate"`
	EventTime     *string `json:"eventTime"`
	ResponsibleID *string `json:"responsibleId"`
	What          *string `json:"what"`
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
	Details       map[string]any // stored under status_details[status] for express / measure
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

// validateProjectStatus applies the per-status rules of the Change status popup (Q-P2):
// express — responsible, what to do, deadline (eventDate), done + result; measure — responsible,
// date + time, address, took place + result; design / production / installation — optional event
// date (+ time); completed — nothing but the optional comment.
func validateProjectStatus(req projectStatusRequest) (projectStatusInput, map[string]string) {
	fields := map[string]string{}
	in := projectStatusInput{Status: strings.TrimSpace(req.Status)}
	if _, ok := projectChangeableStatuses[in.Status]; !ok {
		switch in.Status {
		case projectStatusContract:
			fields["status"] = "Contract signed is set by adding a contract"
		case projectStatusCancelled:
			fields["status"] = "Use Cancel project"
		case projectStatusNone:
			fields["status"] = "A status cannot be cleared"
		default:
			fields["status"] = "Must be express, measure, design, production, installation, or completed"
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
	case projectStatusExpress, projectStatusMeasure:
		validateProjectStageDetails(&in, req, fields)
	default:
		// design / production / installation: the optional event stays as parsed above.
	}
	return in, fields
}

func validateProjectStageDetails(in *projectStatusInput, req projectStatusRequest, fields map[string]string) {
	measure := in.Status == projectStatusMeasure
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
	if measure {
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
	} else {
		what := trimmedText(req.What)
		switch {
		case what == "":
			fields["what"] = "Required"
		case len([]rune(what)) > maxProjectTextLength:
			fields["what"] = "Must be at most 500 characters"
		default:
			details["what"] = what
		}
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
	Number      string  `json:"number"`
	SignedOn    string  `json:"signedOn"`
	TotalAmount float64 `json:"totalAmount"`
	Currency    string  `json:"currency"`
	FileID      *string `json:"fileId"`
}

type projectContractInput struct {
	Number     string
	SignedOn   string
	TotalCents int64
	Currency   string
	FileID     *uuid.UUID
}

func validateProjectContract(req projectContractRequest) (projectContractInput, map[string]string) {
	fields := map[string]string{}
	in := projectContractInput{Number: strings.TrimSpace(req.Number), Currency: strings.TrimSpace(req.Currency)}
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
	Amount float64 `json:"amount"`
	PaidOn string  `json:"paidOn"`
	Note   *string `json:"note"`
	FileID *string `json:"fileId"`
}

type projectPaymentInput struct {
	AmountCents int64
	PaidOn      string
	Note        *string
	FileID      *uuid.UUID
}

func validateProjectPayment(req projectPaymentRequest) (projectPaymentInput, map[string]string) {
	fields := map[string]string{}
	in := projectPaymentInput{}
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
