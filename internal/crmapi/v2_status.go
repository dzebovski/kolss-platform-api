package crmapi

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CRM v2 workflow (docs/CRM-V2-WORKFLOW-CONTRACT.md): one v2 lead status that includes the call
// result, stored in leads.v2_status and mirrored 1:1 into the v1 fields.

const (
	activityV2Status = "v2_status"

	v2StatusNew      = "new"
	v2StatusLater    = "later"
	v2StatusNoAnswer = "noanswer"
	v2StatusSuccess  = "success"
	v2StatusThinking = "thinking"
	v2StatusInvited  = "invited"
	v2StatusLost     = "lost"

	maxBudgetTextLength = 60
	maxNextActionLength = 500
)

// v2CallResultStatuses are the v2 statuses that are call results: they write call_status.
var v2CallResultStatuses = map[string]string{
	v2StatusSuccess:  "reached",
	v2StatusLater:    "callback_requested",
	v2StatusNoAnswer: "no_answer",
}

var leadProducts = map[string]struct{}{
	"kitchen": {}, "wardrobe": {}, "furniture": {}, "bathroom": {}, "hallway": {}, "other": {},
}

// deriveV2LeadStatus maps the v1 fields to the v2 status (contract §2 backfill and §3.6): a
// client status beyond new_lead wins, then the call result, else new. The legacy client statuses
// (measurement_scheduled, calculation_in_progress, postponed, contract_signed) have no v2 status
// (nil = shown read-only from v1, decision D1c).
func deriveV2LeadStatus(clientStatus string, callStatus *string) *string {
	status := ""
	switch clientStatus {
	case "showroom_invited":
		status = v2StatusInvited
	case "thinking":
		status = v2StatusThinking
	case "closed_lost":
		status = v2StatusLost
	case "new_lead":
		switch {
		case callStatus == nil:
			status = v2StatusNew
		case *callStatus == "callback_requested":
			status = v2StatusLater
		case *callStatus == "no_answer":
			status = v2StatusNoAnswer
		case *callStatus == "reached":
			status = v2StatusSuccess
		}
	}
	if status == "" {
		return nil
	}
	return &status
}

// validateV2StatusActivity adds field errors for a v2_status activity (contract §3.2).
func validateV2StatusActivity(req leadActivityRequest, fields map[string]string) {
	notAllowed := "Not allowed for this status"
	if _, ok := v2CallResultStatuses[req.Status]; !ok {
		fields["status"] = "Must be success, later, or noanswer"
		return
	}
	if req.DueAt == nil && req.Status != v2StatusSuccess {
		fields["dueAt"] = "Required for this status"
	}
	if req.Status != v2StatusSuccess {
		if req.EstimatedBudgetText != nil {
			fields["estimatedBudgetText"] = notAllowed
		}
		if req.EstimatedBudgetCurrency != "" {
			fields["estimatedBudgetCurrency"] = notAllowed
		}
		if req.CityRegion != nil {
			fields["cityRegion"] = notAllowed
		}
		if req.Products != nil {
			fields["products"] = notAllowed
		}
		if req.NextAction != "" {
			fields["nextAction"] = notAllowed
		}
		return
	}
	if req.EstimatedBudgetText != nil {
		if _, ok := parseBudgetText(*req.EstimatedBudgetText); !ok {
			fields["estimatedBudgetText"] = "Must be a number or a range, up to 60 characters"
		}
	}
	if req.EstimatedBudgetCurrency != "" {
		if _, ok := normalizeCurrency(req.EstimatedBudgetCurrency); !ok {
			fields["estimatedBudgetCurrency"] = "Must be UAH, USD, EUR, or PLN"
		}
	}
	for _, product := range req.Products {
		if _, ok := leadProducts[product]; !ok {
			fields["products"] = "Must be kitchen, wardrobe, furniture, bathroom, hallway, or other"
			break
		}
	}
	if len([]rune(req.NextAction)) > maxNextActionLength {
		fields["nextAction"] = "Must be at most 500 characters"
	}
}

// v2ActivityFieldsSent reports the v2-only request fields, which other activity types reject.
func v2ActivityFieldsSent(req leadActivityRequest) []string {
	var sent []string
	if req.EstimatedBudgetText != nil {
		sent = append(sent, "estimatedBudgetText")
	}
	if req.EstimatedBudgetCurrency != "" {
		sent = append(sent, "estimatedBudgetCurrency")
	}
	if req.CityRegion != nil {
		sent = append(sent, "cityRegion")
	}
	if req.Products != nil {
		sent = append(sent, "products")
	}
	if req.NextAction != "" {
		sent = append(sent, "nextAction")
	}
	return sent
}

var (
	budgetNumberPattern = `[0-9]+(?:[ \x{00A0}\x{202F}][0-9]{3})*(?:[.,][0-9]{1,2})?`
	budgetTextPattern   = regexp.MustCompile(`^(` + budgetNumberPattern + `)(?:\s*[-–—]\s*(` + budgetNumberPattern + `))?$`)
)

// parseBudgetText reads a budget typed as one number ("20000", "20 000") or a range
// ("20 000 – 25 000", hyphen or dash) and returns the lower bound, which goes to the v1
// estimated_budget (contract §3.7).
func parseBudgetText(text string) (float64, bool) {
	text = strings.TrimSpace(text)
	if text == "" || len([]rune(text)) > maxBudgetTextLength {
		return 0, false
	}
	match := budgetTextPattern.FindStringSubmatch(text)
	if match == nil {
		return 0, false
	}
	lower, ok := parseBudgetNumber(match[1])
	if !ok {
		return 0, false
	}
	if match[2] != "" {
		upper, ok := parseBudgetNumber(match[2])
		if !ok || upper < lower {
			return 0, false
		}
	}
	return lower, true
}

func parseBudgetNumber(value string) (float64, bool) {
	value = strings.NewReplacer(" ", "", "\u00a0", "", "\u202f", "", ",", ".").Replace(value)
	number, err := strconv.ParseFloat(value, 64)
	return number, err == nil && number >= 0
}

// officeBudgetCurrency is the default budget currency of an office (contract §6.3).
func officeBudgetCurrency(officeCode string) string {
	switch officeCode {
	case "warsaw":
		return currencyPLN
	case "kyiv":
		return currencyUAH
	default:
		return currencyEUR
	}
}

// applySuccessfulCallInfo writes the optional Successful call answers (budget, city, products)
// and records them in the event's new_value. The lead version is bumped by the caller.
func applySuccessfulCallInfo(ctx context.Context, tx pgx.Tx, leadID uuid.UUID, lead activityLead, req leadActivityRequest, now time.Time, newValue map[string]any) error {
	if req.EstimatedBudgetText != nil {
		text := strings.TrimSpace(*req.EstimatedBudgetText)
		lower, _ := parseBudgetText(text)
		currency := lead.BudgetCurrency
		if req.EstimatedBudgetCurrency != "" {
			currency, _ = normalizeCurrency(req.EstimatedBudgetCurrency)
		} else if lead.Budget == nil {
			currency = officeBudgetCurrency(lead.OfficeCode)
		}
		rates, err := loadCurrencyRateSetAt(ctx, tx, now)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			update public.leads set
			  estimated_budget_text=$2,
			  estimated_budget=$3,
			  estimated_budget_currency=$4,
			  estimated_budget_rate_set_id=$5
			where id=$1
		`, leadID, text, lower, currency, rates.ID); err != nil {
			return err
		}
		newValue["budget"] = map[string]any{"text": text, "currency": currency}
	}
	if req.CityRegion != nil {
		city := strings.TrimSpace(*req.CityRegion)
		if _, err := tx.Exec(ctx, `update public.leads set city_region=$2 where id=$1`, leadID, clean(city)); err != nil {
			return err
		}
		newValue["city_region"] = city
	}
	if req.Products != nil {
		products := uniqueStrings(req.Products)
		if _, err := tx.Exec(ctx, `update public.leads set products=$2 where id=$1`, leadID, products); err != nil {
			return err
		}
		newValue["products"] = products
	}
	if next := strings.TrimSpace(req.NextAction); next != "" {
		newValue["next_action"] = next
	}
	return nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func equalStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
