package crmapi

import (
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

type salesFunnelStage struct {
	Count   int `json:"count"`
	Percent int `json:"percent"`
}

type salesFunnelStages struct {
	Leads                salesFunnelStage `json:"leads"`
	Calls                salesFunnelStage `json:"calls"`
	Reached              salesFunnelStage `json:"reached"`
	NotReached           salesFunnelStage `json:"notReached"`
	ShowroomInvited      salesFunnelStage `json:"showroomInvited"`
	ShowroomVisited      salesFunnelStage `json:"showroomVisited"`
	MeasurementScheduled salesFunnelStage `json:"measurementScheduled"`
	MeasurementCompleted salesFunnelStage `json:"measurementCompleted"`
	CalculationStarted   salesFunnelStage `json:"calculationStarted"`
}

type salesFunnelPotential struct {
	Currency string  `json:"currency"`
	Total    float64 `json:"total"`
}

type salesFunnelReportResponse struct {
	GeneratedAt          time.Time                        `json:"generatedAt"`
	Period               reportPeriod                     `json:"period"`
	Stages               salesFunnelStages                `json:"stages"`
	Potential            salesFunnelPotential             `json:"potential"`
	ContractTotals       []reportContractTotal            `json:"contractTotals"`
	FinancialComparisons []salesFunnelFinancialComparison `json:"financialComparisons"`
}

type salesFunnelFinancialComparison struct {
	OfficeCode         string   `json:"officeCode"`
	Currency           string   `json:"currency"`
	EstimatedTotal     float64  `json:"estimatedTotal"`
	ActualTotal        float64  `json:"actualTotal"`
	Difference         float64  `json:"difference"`
	RealizationPercent *float64 `json:"realizationPercent"`
}

type salesFunnelLeadEvidence struct {
	OfficeCode           string
	EstimatedBudget      *float64
	EstimatedCurrency    string
	EstimatedRates       currencyRateSet
	ExplicitReached      bool
	ExplicitNotReached   bool
	ClosedNoContact      bool
	ShowroomInvited      bool
	ShowroomVisited      bool
	MeasurementScheduled bool
	MeasurementCompleted bool
	CalculationStarted   bool
	ContractAmount       *float64
	ContractCurrency     *string
	ContractRates        currencyRateSet
}

func salesFunnelPercent(count, base int) int {
	if base <= 0 {
		return 0
	}
	return int(math.Round(float64(count) / float64(base) * 100))
}

func aggregateSalesFunnel(rows []salesFunnelLeadEvidence, officeCodes []string) (salesFunnelStages, salesFunnelPotential, []reportContractTotal, []salesFunnelFinancialComparison) {
	var stages salesFunnelStages
	potential := salesFunnelPotential{Currency: "EUR"}
	contractTotals := []reportContractTotal{}
	comparisonsByOffice := map[string]*salesFunnelFinancialComparison{}
	for _, officeCode := range officeCodes {
		comparisonsByOffice[officeCode] = &salesFunnelFinancialComparison{
			OfficeCode: officeCode,
			Currency:   reportingCurrencyForOffice(officeCode),
		}
	}

	for _, row := range rows {
		stages.Leads.Count++
		targetCurrency := reportingCurrencyForOffice(row.OfficeCode)
		comparison := comparisonsByOffice[row.OfficeCode]
		if comparison == nil {
			comparison = &salesFunnelFinancialComparison{OfficeCode: row.OfficeCode, Currency: targetCurrency}
			comparisonsByOffice[row.OfficeCode] = comparison
		}
		if row.EstimatedBudget != nil && *row.EstimatedBudget > 0 {
			if estimatedEUR, ok := convertAmount(*row.EstimatedBudget, row.EstimatedCurrency, currencyEUR, row.EstimatedRates); ok {
				potential.Total += estimatedEUR
			}
			if estimated, ok := convertAmount(*row.EstimatedBudget, row.EstimatedCurrency, targetCurrency, row.EstimatedRates); ok {
				comparison.EstimatedTotal += estimated
			}
		}

		showroomInvited := row.ShowroomInvited || row.ShowroomVisited
		measurementScheduled := row.MeasurementScheduled || row.MeasurementCompleted
		downstreamEvidence := showroomInvited || measurementScheduled || row.CalculationStarted
		reached := row.ExplicitReached || downstreamEvidence
		notReached := !reached && (row.ExplicitNotReached || row.ClosedNoContact)
		called := reached || notReached

		if called {
			stages.Calls.Count++
		}
		if reached {
			stages.Reached.Count++
		}
		if notReached {
			stages.NotReached.Count++
		}
		if showroomInvited {
			stages.ShowroomInvited.Count++
		}
		if row.ShowroomVisited {
			stages.ShowroomVisited.Count++
		}
		if measurementScheduled {
			stages.MeasurementScheduled.Count++
		}
		if row.MeasurementCompleted {
			stages.MeasurementCompleted.Count++
		}
		if row.CalculationStarted {
			stages.CalculationStarted.Count++
		}

		addSalesFunnelContract(&contractTotals, row.ContractAmount, row.ContractCurrency)
		if row.ContractAmount != nil && *row.ContractAmount > 0 && row.ContractCurrency != nil {
			if actual, ok := convertAmount(*row.ContractAmount, *row.ContractCurrency, targetCurrency, row.ContractRates); ok {
				comparison.ActualTotal += actual
			}
		}
	}

	stages.Leads.Percent = salesFunnelPercent(stages.Leads.Count, stages.Leads.Count)
	stages.Calls.Percent = salesFunnelPercent(stages.Calls.Count, stages.Leads.Count)
	stages.Reached.Percent = salesFunnelPercent(stages.Reached.Count, stages.Calls.Count)
	stages.NotReached.Percent = salesFunnelPercent(stages.NotReached.Count, stages.Calls.Count)
	stages.ShowroomInvited.Percent = salesFunnelPercent(stages.ShowroomInvited.Count, stages.Reached.Count)
	stages.ShowroomVisited.Percent = salesFunnelPercent(stages.ShowroomVisited.Count, stages.ShowroomInvited.Count)
	stages.MeasurementScheduled.Percent = salesFunnelPercent(stages.MeasurementScheduled.Count, stages.Reached.Count)
	stages.MeasurementCompleted.Percent = salesFunnelPercent(stages.MeasurementCompleted.Count, stages.MeasurementScheduled.Count)
	stages.CalculationStarted.Percent = salesFunnelPercent(stages.CalculationStarted.Count, stages.Reached.Count)

	sort.SliceStable(contractTotals, func(i, j int) bool {
		return reportCurrencyOrder[contractTotals[i].Currency] < reportCurrencyOrder[contractTotals[j].Currency]
	})
	potential.Total = roundMoney(potential.Total)
	for index := range contractTotals {
		contractTotals[index].Total = roundMoney(contractTotals[index].Total)
	}
	comparisons := make([]salesFunnelFinancialComparison, 0, len(comparisonsByOffice))
	for _, comparison := range comparisonsByOffice {
		comparison.EstimatedTotal = roundMoney(comparison.EstimatedTotal)
		comparison.ActualTotal = roundMoney(comparison.ActualTotal)
		comparison.Difference = roundMoney(comparison.ActualTotal - comparison.EstimatedTotal)
		if comparison.EstimatedTotal > 0 {
			value := math.Round(comparison.ActualTotal/comparison.EstimatedTotal*1000) / 10
			comparison.RealizationPercent = &value
		}
		comparisons = append(comparisons, *comparison)
	}
	sort.SliceStable(comparisons, func(i, j int) bool {
		return officeReportOrder(comparisons[i].OfficeCode) < officeReportOrder(comparisons[j].OfficeCode)
	})
	return stages, potential, contractTotals, comparisons
}

func roundMoney(value float64) float64 {
	return math.Round(value*100) / 100
}

func officeReportOrder(code string) int {
	if code == "kyiv" {
		return 0
	}
	if code == "warsaw" {
		return 1
	}
	return 2
}

func addSalesFunnelContract(totals *[]reportContractTotal, amount *float64, currency *string) {
	if amount == nil || *amount <= 0 || currency == nil {
		return
	}
	code := strings.ToUpper(strings.TrimSpace(*currency))
	if _, ok := reportCurrencyOrder[code]; !ok {
		return
	}
	for index := range *totals {
		if (*totals)[index].Currency == code {
			(*totals)[index].Total += *amount
			return
		}
	}
	*totals = append(*totals, reportContractTotal{Currency: code, Total: *amount})
}

func buildSalesFunnelQuery(officeIDs []uuid.UUID) string {
	return `
		with cohort as (
			select l.*, o.code as office_code
			from public.leads l
			join public.offices o on o.id=l.office_id
			where ($1::uuid[] is null or l.office_id=any($1))
			  and (coalesce(l.source_created_at,l.created_at) at time zone o.timezone_name)::date
				between $2::date and $3::date
		), evidence as (
			select
				l.id,
				l.office_code,
				l.estimated_budget,
				l.estimated_budget_currency,
				l.estimated_budget_rate_set_id,
				(
					coalesce(l.call_status in ('reached','callback_requested'),false)
					or exists (
						select 1 from public.lead_contact_attempts ca
						where ca.lead_id=l.id and ca.result in ('reached','cannot_talk','bad_lead')
					)
					or exists (
						select 1 from public.lead_events e
						where e.lead_id=l.id
						  and e.event_category='call_status'
						  and e.status_code in ('reached','callback_requested')
					)
				) as explicit_reached,
				(
					coalesce(l.call_status='no_answer',false)
					or exists (
						select 1 from public.lead_contact_attempts ca
						where ca.lead_id=l.id and ca.result='no_answer'
					)
					or exists (
						select 1 from public.lead_events e
						where e.lead_id=l.id
						  and e.event_category='call_status'
						  and e.status_code='no_answer'
					)
				) as explicit_not_reached,
				coalesce(l.client_status='closed_lost' and l.loss_reason='no_contact',false) as closed_no_contact,
				(
					l.client_status='showroom_invited'
					or exists (
						select 1 from public.lead_events e
						where e.lead_id=l.id
						  and (
							e.event_type in ('visit_scheduled','visit_rescheduled')
							or (e.event_category='client_status' and e.status_code='showroom_invited')
						  )
					)
					or exists (
						select 1 from public.lead_showroom_visits v
						where v.lead_id=l.id and v.kind='showroom'
					)
				) as showroom_invited,
				(
					exists (
						select 1 from public.lead_showroom_visits v
						where v.lead_id=l.id and v.kind='showroom' and v.status='visited'
					)
					or exists (
						select 1 from public.lead_events e
						where e.lead_id=l.id and e.event_type='visit_completed'
					)
				) as showroom_visited,
				(
					l.client_status='measurement_scheduled'
					or exists (
						select 1 from public.lead_events e
						where e.lead_id=l.id
						  and e.event_category='client_status'
						  and e.status_code='measurement_scheduled'
					)
					or exists (
						select 1 from public.lead_showroom_visits v
						where v.lead_id=l.id and v.kind='measurement'
					)
				) as measurement_scheduled,
				exists (
					select 1 from public.lead_showroom_visits v
					where v.lead_id=l.id and v.kind='measurement' and v.status='visited'
				) as measurement_completed,
				(
					l.client_status='calculation_in_progress'
					or exists (
						select 1 from public.lead_events e
						where e.lead_id=l.id
						  and e.event_category='client_status'
						  and e.status_code='calculation_in_progress'
					)
				) as calculation_started
			from cohort l
		)
		select
			e.office_code,
			e.estimated_budget,
			e.estimated_budget_currency,
			coalesce(budget_rates.pln_per_eur, baseline_rates.pln_per_eur),
			coalesce(budget_rates.uah_per_eur, baseline_rates.uah_per_eur),
			coalesce(budget_rates.uah_per_usd, baseline_rates.uah_per_usd),
			e.explicit_reached,
			e.explicit_not_reached,
			e.closed_no_contact,
			e.showroom_invited,
			e.showroom_visited,
			e.measurement_scheduled,
			e.measurement_completed,
			e.calculation_started,
			coalesce(signed_contract.amount,legacy_contract.amount),
			coalesce(signed_contract.currency,legacy_contract.currency),
			coalesce(contract_rates.pln_per_eur, baseline_rates.pln_per_eur),
			coalesce(contract_rates.uah_per_eur, baseline_rates.uah_per_eur),
			coalesce(contract_rates.uah_per_usd, baseline_rates.uah_per_usd)
		from evidence e
		cross join lateral (
			select rs.pln_per_eur,rs.uah_per_eur,rs.uah_per_usd
			from public.currency_rate_sets rs
			order by rs.effective_from asc
			limit 1
		) baseline_rates
		left join public.currency_rate_sets budget_rates
			on budget_rates.id=e.estimated_budget_rate_set_id
		left join lateral (
			select c.amount,c.currency,c.currency_rate_set_id,c.signed_at
			from public.lead_contracts c
			where c.lead_id=e.id
			  and c.status='signed'
			  and c.amount is not null
			  and c.currency is not null
			order by c.signed_at desc nulls last,c.created_at desc
			limit 1
		) signed_contract on true
		left join lateral (
			select
				(event.new_value->>'amount')::numeric as amount,
				event.new_value->>'currency' as currency,
				coalesce((event.new_value->>'signed_at')::timestamptz,event.created_at) as signed_at
			from public.lead_events event
			where event.lead_id=e.id
			  and event.event_type in ('successful','contract_signed')
			  and event.new_value ? 'amount'
			  and event.new_value ? 'currency'
			order by event.created_at desc
			limit 1
		) legacy_contract on signed_contract.amount is null
		left join lateral (
			select rs.pln_per_eur,rs.uah_per_eur,rs.uah_per_usd
			from public.currency_rate_sets rs
			where rs.id=signed_contract.currency_rate_set_id
			   or (
				 signed_contract.currency_rate_set_id is null
				 and rs.effective_from <= coalesce(signed_contract.signed_at,legacy_contract.signed_at)
			   )
			order by
				case when rs.id=signed_contract.currency_rate_set_id then 0 else 1 end,
				rs.effective_from desc
			limit 1
		) contract_rates on true
		order by e.id
	`
}

func (s *Server) handleSalesFunnelReport(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	officeIDs, ok := s.reportOfficeFilter(w, r, actor)
	if !ok {
		return
	}
	from, to, period, fields := parseReportPeriod(r, true)
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid report period", fields)
		return
	}
	officeCodes, err := s.loadReportOfficeCodes(r, officeIDs)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "sales_funnel_report_load_failed", "Could not load sales funnel report", nil)
		return
	}

	rows, err := s.pool.Query(r.Context(), buildSalesFunnelQuery(officeIDs), nullableUUIDs(officeIDs), *from, *to)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "sales_funnel_report_load_failed", "Could not load sales funnel report", nil)
		return
	}
	defer rows.Close()

	evidence := []salesFunnelLeadEvidence{}
	for rows.Next() {
		var row salesFunnelLeadEvidence
		if err := rows.Scan(
			&row.OfficeCode,
			&row.EstimatedBudget,
			&row.EstimatedCurrency,
			&row.EstimatedRates.PLNPerEUR,
			&row.EstimatedRates.UAHPerEUR,
			&row.EstimatedRates.UAHPerUSD,
			&row.ExplicitReached,
			&row.ExplicitNotReached,
			&row.ClosedNoContact,
			&row.ShowroomInvited,
			&row.ShowroomVisited,
			&row.MeasurementScheduled,
			&row.MeasurementCompleted,
			&row.CalculationStarted,
			&row.ContractAmount,
			&row.ContractCurrency,
			&row.ContractRates.PLNPerEUR,
			&row.ContractRates.UAHPerEUR,
			&row.ContractRates.UAHPerUSD,
		); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "sales_funnel_report_load_failed", "Could not load sales funnel report", nil)
			return
		}
		evidence = append(evidence, row)
	}
	if err := rows.Err(); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "sales_funnel_report_load_failed", "Could not load sales funnel report", nil)
		return
	}

	stages, potential, contractTotals, financialComparisons := aggregateSalesFunnel(evidence, officeCodes)
	writeJSON(w, http.StatusOK, salesFunnelReportResponse{
		GeneratedAt:          time.Now().UTC(),
		Period:               period,
		Stages:               stages,
		Potential:            potential,
		ContractTotals:       contractTotals,
		FinancialComparisons: financialComparisons,
	})
}

func (s *Server) loadReportOfficeCodes(r *http.Request, officeIDs []uuid.UUID) ([]string, error) {
	rows, err := s.pool.Query(r.Context(), `
		select code
		from public.offices
		where is_active = true
		  and ($1::uuid[] is null or id=any($1))
		order by code
	`, nullableUUIDs(officeIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	codes := []string{}
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, err
		}
		codes = append(codes, code)
	}
	return codes, rows.Err()
}
