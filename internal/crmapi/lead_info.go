package crmapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CRM v2 "Lead info" popup (lead card v1.3): a partial update of the answers the designer needs
// (contract §3.3, task W7). Only the sent fields change; one lead_edited event records the
// changed field keys (the audit shape v1 already shows) plus the new values under "info".

const (
	maxMaterialLength = 200
	maxLeadTimeLength = 60
)

// Audit keys of the lead info fields (CRM core/i18n/field-keys.ts).
const (
	leadInfoFieldBudget      = "budget"
	leadInfoFieldCity        = "cityRegion"
	leadInfoFieldProducts    = "product"
	leadInfoFieldMaterials   = "materials"
	leadInfoFieldLeadTime    = "expectedLeadTime"
	leadInfoFieldMeasurement = "preferredMeasurement"
)

// nullableTime tells an omitted field (Set=false) from an explicit null (Set=true, Value=nil).
type nullableTime struct {
	Set   bool
	Value *time.Time
}

func (n *nullableTime) UnmarshalJSON(data []byte) error {
	n.Set = true
	if string(data) == "null" {
		n.Value = nil
		return nil
	}
	var value time.Time
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	n.Value = &value
	return nil
}

// leadInfoRequest: nil / omitted = unchanged; "" clears a text; [] clears products; null clears
// the measurement date.
type leadInfoRequest struct {
	EstimatedBudgetText     *string      `json:"estimatedBudgetText"`
	EstimatedBudgetCurrency *string      `json:"estimatedBudgetCurrency"`
	CityRegion              *string      `json:"cityRegion"`
	Products                *[]string    `json:"products"`
	MaterialFronts          *string      `json:"materialFronts"`
	MaterialWorktop         *string      `json:"materialWorktop"`
	MaterialAppliances      *string      `json:"materialAppliances"`
	ExpectedLeadTime        *string      `json:"expectedLeadTime"`
	PreferredMeasurementAt  nullableTime `json:"preferredMeasurementAt"`
}

func validateLeadInfo(req leadInfoRequest) map[string]string {
	fields := map[string]string{}
	if req.EstimatedBudgetText != nil {
		if text := strings.TrimSpace(*req.EstimatedBudgetText); text != "" {
			if _, ok := parseBudgetText(text); !ok {
				fields["estimatedBudgetText"] = "Must be a number or a range, up to 60 characters"
			}
		}
	}
	if req.EstimatedBudgetCurrency != nil {
		if _, ok := normalizeCurrency(*req.EstimatedBudgetCurrency); !ok {
			fields["estimatedBudgetCurrency"] = "Must be UAH, USD, EUR, or PLN"
		}
	}
	if req.Products != nil {
		for _, product := range *req.Products {
			if _, ok := leadProducts[product]; !ok {
				fields["products"] = "Must be kitchen, wardrobe, furniture, bathroom, hallway, or other"
				break
			}
		}
	}
	for name, value := range map[string]*string{
		"materialFronts":     req.MaterialFronts,
		"materialWorktop":    req.MaterialWorktop,
		"materialAppliances": req.MaterialAppliances,
	} {
		if value != nil && len([]rune(strings.TrimSpace(*value))) > maxMaterialLength {
			fields[name] = "Must be at most 200 characters"
		}
	}
	if req.ExpectedLeadTime != nil && len([]rune(strings.TrimSpace(*req.ExpectedLeadTime))) > maxLeadTimeLength {
		fields["expectedLeadTime"] = "Must be at most 60 characters"
	}
	return fields
}

// leadInfo is the stored state of the lead info columns.
type leadInfo struct {
	BudgetText        *string
	Budget            *float64
	BudgetCurrency    string
	CityRegion        *string
	Products          []string
	Fronts            *string
	Worktop           *string
	Appliances        *string
	LeadTime          *string
	MeasurementAt     *time.Time
	OfficeCode        string
	BudgetRateChanged bool
}

// applyLeadInfo returns the new state, the changed audit field keys and the event values.
// A budget sent for a lead without one takes the office currency unless a currency is sent.
func applyLeadInfo(current leadInfo, req leadInfoRequest) (leadInfo, []string, map[string]any) {
	next := current
	next.BudgetRateChanged = false
	var changed []string
	values := map[string]any{}

	if req.EstimatedBudgetText != nil || req.EstimatedBudgetCurrency != nil {
		if req.EstimatedBudgetText != nil {
			next.BudgetText = clean(*req.EstimatedBudgetText)
			next.Budget = nil
			if next.BudgetText != nil {
				lower, _ := parseBudgetText(*next.BudgetText)
				next.Budget = &lower
			}
		}
		if req.EstimatedBudgetCurrency != nil {
			next.BudgetCurrency, _ = normalizeCurrency(*req.EstimatedBudgetCurrency)
		} else if current.Budget == nil && next.Budget != nil {
			next.BudgetCurrency = officeBudgetCurrency(current.OfficeCode)
		}
		budgetChanged := !equalStringPtr(current.BudgetText, next.BudgetText) ||
			!optionalFloatEqual(current.Budget, next.Budget)
		if budgetChanged || current.BudgetCurrency != next.BudgetCurrency {
			next.BudgetRateChanged = true
			changed = append(changed, leadInfoFieldBudget)
			values["budget"] = map[string]any{"text": next.BudgetText, "currency": next.BudgetCurrency}
		}
	}
	if req.CityRegion != nil {
		next.CityRegion = clean(*req.CityRegion)
		if !equalStringPtr(current.CityRegion, next.CityRegion) {
			changed = append(changed, leadInfoFieldCity)
			values["city_region"] = next.CityRegion
		}
	}
	if req.Products != nil {
		next.Products = uniqueStrings(*req.Products)
		if !slices.Equal(current.Products, next.Products) {
			changed = append(changed, leadInfoFieldProducts)
			values["products"] = next.Products
		}
	}
	materialsChanged := false
	for _, pair := range []struct {
		sent   *string
		target **string
		now    *string
	}{
		{req.MaterialFronts, &next.Fronts, current.Fronts},
		{req.MaterialWorktop, &next.Worktop, current.Worktop},
		{req.MaterialAppliances, &next.Appliances, current.Appliances},
	} {
		if pair.sent == nil {
			continue
		}
		*pair.target = clean(*pair.sent)
		if !equalStringPtr(pair.now, *pair.target) {
			materialsChanged = true
		}
	}
	if materialsChanged {
		changed = append(changed, leadInfoFieldMaterials)
		values["materials"] = map[string]any{"fronts": next.Fronts, "worktop": next.Worktop, "appliances": next.Appliances}
	}
	if req.ExpectedLeadTime != nil {
		next.LeadTime = clean(*req.ExpectedLeadTime)
		if !equalStringPtr(current.LeadTime, next.LeadTime) {
			changed = append(changed, leadInfoFieldLeadTime)
			values["expected_lead_time"] = next.LeadTime
		}
	}
	if req.PreferredMeasurementAt.Set {
		next.MeasurementAt = req.PreferredMeasurementAt.Value
		if !equalTimePtr(current.MeasurementAt, next.MeasurementAt) {
			changed = append(changed, leadInfoFieldMeasurement)
			values["preferred_measurement_at"] = next.MeasurementAt
		}
	}
	return next, changed, values
}

func equalTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func (s *Server) handleUpdateLeadInfo(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	leadID, err := uuid.Parse(r.PathValue("leadId"))
	version, hasVersion := parseIfMatch(r)
	if err != nil || !hasVersion {
		s.writeError(w, r, http.StatusPreconditionRequired, "version_required", "If-Match lead version is required", nil)
		return
	}
	var req leadInfoRequest
	if err := decodeJSON(w, r, 64*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid lead info", nil)
		return
	}
	if fields := validateLeadInfo(req); len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid lead info", fields)
		return
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "lead_update_failed", "Could not update lead info", nil)
		return
	}
	defer tx.Rollback(r.Context())

	var officeID uuid.UUID
	var currentVersion int64
	var current leadInfo
	err = tx.QueryRow(r.Context(), `
		select l.office_id, l.version, l.estimated_budget_text, l.estimated_budget,
		  l.estimated_budget_currency, l.city_region, l.products, l.material_fronts,
		  l.material_worktop, l.material_appliances, l.expected_lead_time,
		  l.preferred_measurement_at, o.code
		from public.leads l join public.offices o on o.id = l.office_id
		where l.id=$1 and l.archived_at is null
		for update of l
	`, leadID).Scan(
		&officeID, &currentVersion, &current.BudgetText, &current.Budget, &current.BudgetCurrency,
		&current.CityRegion, &current.Products, &current.Fronts, &current.Worktop, &current.Appliances,
		&current.LeadTime, &current.MeasurementAt, &current.OfficeCode,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "lead_not_found", "Lead not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "lead_update_failed", "Could not update lead info", nil)
		return
	}
	if !actor.CanEditLead(officeID) {
		s.writeError(w, r, http.StatusForbidden, "lead_edit_forbidden", "Lead editing is not allowed", nil)
		return
	}
	if currentVersion != version {
		s.writeError(w, r, http.StatusConflict, "version_conflict", "Lead was changed by another user", nil)
		return
	}

	next, changed, values := applyLeadInfo(current, req)
	if len(changed) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"version": currentVersion})
		return
	}

	var rateSetID *uuid.UUID
	rateSetKeep := !next.BudgetRateChanged
	if next.BudgetRateChanged && next.Budget != nil {
		rates, rateErr := loadCurrencyRateSetAt(r.Context(), tx, time.Now().UTC())
		if rateErr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "lead_update_failed", "Could not load currency rates", nil)
			return
		}
		rateSetID = &rates.ID
	}
	products := next.Products
	if products == nil {
		products = []string{}
	}
	var nextVersion int64
	err = tx.QueryRow(r.Context(), `
		update public.leads set
		  estimated_budget_text=$3, estimated_budget=$4, estimated_budget_currency=$5,
		  estimated_budget_rate_set_id=case when $6 then estimated_budget_rate_set_id else $7 end,
		  city_region=$8, products=$9, material_fronts=$10, material_worktop=$11,
		  material_appliances=$12, expected_lead_time=$13, preferred_measurement_at=$14,
		  updated_at=now(), version=version+1
		where id=$1 and version=$2 and archived_at is null
		returning version
	`, leadID, version, next.BudgetText, next.Budget, next.BudgetCurrency, rateSetKeep, rateSetID,
		next.CityRegion, products, next.Fronts, next.Worktop, next.Appliances, next.LeadTime,
		next.MeasurementAt).Scan(&nextVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusConflict, "version_conflict", "Lead was changed by another user", nil)
		return
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `
			insert into public.lead_events (lead_id, actor_id, event_type, new_value)
			values ($1, $2, 'lead_edited', $3)
		`, leadID, actor.ID, map[string]any{"fields": changed, "edited_by_name": actor.DisplayName, "info": values})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "lead_update_failed", "Could not update lead info", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": nextVersion})
}
