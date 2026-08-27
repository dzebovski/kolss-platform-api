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
	GeneratedAt    time.Time             `json:"generatedAt"`
	Period         reportPeriod          `json:"period"`
	Stages         salesFunnelStages     `json:"stages"`
	Potential      salesFunnelPotential  `json:"potential"`
	ContractTotals []reportContractTotal `json:"contractTotals"`
}

type salesFunnelLeadEvidence struct {
	EstimatedBudget      *float64
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
}

func salesFunnelPercent(count, base int) int {
	if base <= 0 {
		return 0
	}
	return int(math.Round(float64(count) / float64(base) * 100))
}

func aggregateSalesFunnel(rows []salesFunnelLeadEvidence) (salesFunnelStages, salesFunnelPotential, []reportContractTotal) {
	var stages salesFunnelStages
	potential := salesFunnelPotential{Currency: "EUR"}
	contractTotals := []reportContractTotal{}

	for _, row := range rows {
		stages.Leads.Count++
		if row.EstimatedBudget != nil && *row.EstimatedBudget > 0 {
			potential.Total += *row.EstimatedBudget
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
	return stages, potential, contractTotals
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
			select l.*
			from public.leads l
			join public.offices o on o.id=l.office_id
			where ($1::uuid[] is null or l.office_id=any($1))
			  and (coalesce(l.source_created_at,l.created_at) at time zone o.timezone_name)::date
				between $2::date and $3::date
		), evidence as (
			select
				l.id,
				l.estimated_budget,
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
			e.estimated_budget,
			e.explicit_reached,
			e.explicit_not_reached,
			e.closed_no_contact,
			e.showroom_invited,
			e.showroom_visited,
			e.measurement_scheduled,
			e.measurement_completed,
			e.calculation_started,
			coalesce(signed_contract.amount,legacy_contract.amount),
			coalesce(signed_contract.currency,legacy_contract.currency)
		from evidence e
		left join lateral (
			select c.amount,c.currency
			from public.lead_contracts c
			where c.lead_id=e.id
			  and c.status='signed'
			  and c.amount is not null
			  and c.currency is not null
			order by c.signed_at desc nulls last,c.created_at desc
			limit 1
		) signed_contract on true
		left join lateral (
			select (event.new_value->>'amount')::numeric as amount,event.new_value->>'currency' as currency
			from public.lead_events event
			where event.lead_id=e.id
			  and event.event_type in ('successful','contract_signed')
			  and event.new_value ? 'amount'
			  and event.new_value ? 'currency'
			order by event.created_at desc
			limit 1
		) legacy_contract on signed_contract.amount is null
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
			&row.EstimatedBudget,
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

	stages, potential, contractTotals := aggregateSalesFunnel(evidence)
	writeJSON(w, http.StatusOK, salesFunnelReportResponse{
		GeneratedAt:    time.Now().UTC(),
		Period:         period,
		Stages:         stages,
		Potential:      potential,
		ContractTotals: contractTotals,
	})
}
