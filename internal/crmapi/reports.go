package crmapi

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

var reportClientStatuses = [...]string{
	"new_lead",
	"calculation_in_progress",
	"showroom_invited",
	"thinking",
	"contract_signed",
	"closed_lost",
}

var reportCurrencyOrder = map[string]int{
	"UAH": 0,
	"USD": 1,
	"EUR": 2,
	"PLN": 3,
}

type reportPeriod struct {
	From *string `json:"from"`
	To   *string `json:"to"`
}

type reportTotals struct {
	Total             int                   `json:"total"`
	Active            int                   `json:"active"`
	ContractSigned    int                   `json:"contractSigned"`
	ContractTotals    []reportContractTotal `json:"contractTotals"`
	ClosedLost        int                   `json:"closedLost"`
	Callback          int                   `json:"callback"`
	Inactive7d        int                   `json:"inactive7d"`
	ConversionPercent int                   `json:"conversionPercent"`
	ByClientStatus    map[string]int        `json:"byClientStatus"`
}

type reportContractTotal struct {
	Currency string  `json:"currency"`
	Total    float64 `json:"total"`
}

type reportComment struct {
	Body       string    `json:"body"`
	OccurredAt time.Time `json:"occurredAt"`
	AuthorID   uuid.UUID `json:"authorId"`
	AuthorName string    `json:"authorName"`
	EventType  string    `json:"eventType"`
}

type reportLead struct {
	ID                    uuid.UUID       `json:"id"`
	Name                  string          `json:"name"`
	Phone                 string          `json:"phone"`
	CreatedAt             time.Time       `json:"createdAt"`
	ClientStatus          string          `json:"clientStatus"`
	ClientStatusChangedAt time.Time       `json:"clientStatusChangedAt"`
	CallStatus            *string         `json:"callStatus"`
	CallStatusChangedAt   *time.Time      `json:"callStatusChangedAt"`
	LossReason            *string         `json:"lossReason"`
	LastHumanActivityAt   *time.Time      `json:"lastHumanActivityAt"`
	InactiveDays          int             `json:"inactiveDays"`
	Inactive7d            bool            `json:"inactive7d"`
	Comments              []reportComment `json:"comments"`
	ContractAmount        *float64        `json:"-"`
	ContractCurrency      *string         `json:"-"`
}

type managerLeadReport struct {
	OfficeCode  string       `json:"officeCode"`
	ManagerID   *uuid.UUID   `json:"managerId"`
	ManagerName string       `json:"managerName"`
	Totals      reportTotals `json:"totals"`
	Leads       []reportLead `json:"leads"`
}

type lossReasonReport struct {
	Code    string `json:"code"`
	LabelUK string `json:"labelUk"`
	LabelPL string `json:"labelPl"`
	LabelEN string `json:"labelEn"`
	Count   int    `json:"count"`
	Percent int    `json:"percent"`
}

type leadReportResponse struct {
	GeneratedAt time.Time           `json:"generatedAt"`
	Period      reportPeriod        `json:"period"`
	Totals      reportTotals        `json:"totals"`
	LossReasons []lossReasonReport  `json:"lossReasons"`
	Managers    []managerLeadReport `json:"managers"`
}

func (s *Server) reportOfficeFilter(w http.ResponseWriter, r *http.Request, actor Actor) ([]uuid.UUID, bool) {
	if raw := strings.TrimSpace(r.URL.Query().Get("officeId")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil || !actor.CanAccessOffice(id) {
			s.writeError(w, r, http.StatusForbidden, "office_forbidden", "Office access denied", nil)
			return nil, false
		}
		return []uuid.UUID{id}, true
	}
	if actor.IsSuperAdmin() {
		return nil, true
	}
	ids := make([]uuid.UUID, 0, len(actor.OfficeIDs))
	for id := range actor.OfficeIDs {
		ids = append(ids, id)
	}
	return ids, true
}

func (s *Server) handleDashboardOverview(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	officeIDs, ok := s.reportOfficeFilter(w, r, actor)
	if !ok {
		return
	}
	var total, active, successful, employees int
	err := s.pool.QueryRow(r.Context(), `
		select
			count(*) filter (where l.archived_at is null),
			count(*) filter (where l.archived_at is null and l.workflow_status not in ('closed','successful')),
			count(*) filter (where l.archived_at is null and l.workflow_status='successful')
		from public.leads l
		where ($1::uuid[] is null or l.office_id=any($1))
	`, nullableUUIDs(officeIDs)).Scan(&total, &active, &successful)
	if err == nil {
		err = s.pool.QueryRow(r.Context(), `
			select count(distinct p.id) from public.profiles p
			left join public.user_office_memberships m on m.user_id=p.id
			where p.is_active=true and ($1::uuid[] is null or m.office_id=any($1))
		`, nullableUUIDs(officeIDs)).Scan(&employees)
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "dashboard_load_failed", "Could not load dashboard", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"totalLeads": total, "activeLeads": active, "successfulLeads": successful, "employees": employees})
}

func parseReportPeriod(r *http.Request) (*time.Time, *time.Time, reportPeriod, map[string]string) {
	fromRaw := strings.TrimSpace(r.URL.Query().Get("from"))
	toRaw := strings.TrimSpace(r.URL.Query().Get("to"))
	if fromRaw == "" && toRaw == "" {
		return nil, nil, reportPeriod{}, nil
	}
	fields := map[string]string{}
	if fromRaw == "" {
		fields["from"] = "Required when to is provided"
	}
	if toRaw == "" {
		fields["to"] = "Required when from is provided"
	}
	if len(fields) > 0 {
		return nil, nil, reportPeriod{}, fields
	}
	from, fromErr := time.Parse(time.DateOnly, fromRaw)
	to, toErr := time.Parse(time.DateOnly, toRaw)
	if fromErr != nil {
		fields["from"] = "Must use YYYY-MM-DD"
	}
	if toErr != nil {
		fields["to"] = "Must use YYYY-MM-DD"
	}
	if len(fields) == 0 && from.After(to) {
		fields["to"] = "Must be on or after from"
	}
	if len(fields) > 0 {
		return nil, nil, reportPeriod{}, fields
	}
	return &from, &to, reportPeriod{From: &fromRaw, To: &toRaw}, nil
}

func newReportTotals() reportTotals {
	counts := make(map[string]int, len(reportClientStatuses))
	for _, status := range reportClientStatuses {
		counts[status] = 0
	}
	return reportTotals{ByClientStatus: counts, ContractTotals: []reportContractTotal{}}
}

func addLeadToTotals(totals *reportTotals, lead reportLead) {
	totals.Total++
	totals.ByClientStatus[lead.ClientStatus]++
	terminal := lead.ClientStatus == "closed_lost" || lead.ClientStatus == "contract_signed"
	if !terminal {
		totals.Active++
		if lead.Inactive7d {
			totals.Inactive7d++
		}
	}
	if lead.ClientStatus == "contract_signed" {
		totals.ContractSigned++
		addContractToTotals(totals, lead.ContractAmount, lead.ContractCurrency)
	}
	if lead.ClientStatus == "closed_lost" {
		totals.ClosedLost++
	}
	if lead.CallStatus != nil && (*lead.CallStatus == "no_answer" || *lead.CallStatus == "callback_requested") {
		totals.Callback++
	}
}

func addContractToTotals(totals *reportTotals, amount *float64, currency *string) {
	if amount == nil || *amount <= 0 || currency == nil {
		return
	}
	code := strings.ToUpper(strings.TrimSpace(*currency))
	if _, ok := reportCurrencyOrder[code]; !ok {
		return
	}
	for index := range totals.ContractTotals {
		if totals.ContractTotals[index].Currency == code {
			totals.ContractTotals[index].Total += *amount
			return
		}
	}
	totals.ContractTotals = append(totals.ContractTotals, reportContractTotal{
		Currency: code,
		Total:    *amount,
	})
}

func finalizeTotals(totals *reportTotals) {
	if totals.Total > 0 {
		totals.ConversionPercent = int(math.Round(float64(totals.ContractSigned) / float64(totals.Total) * 100))
	}
	sort.SliceStable(totals.ContractTotals, func(i, j int) bool {
		return reportCurrencyOrder[totals.ContractTotals[i].Currency] < reportCurrencyOrder[totals.ContractTotals[j].Currency]
	})
}

func (s *Server) handleLeadReport(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	officeIDs, ok := s.reportOfficeFilter(w, r, actor)
	if !ok {
		return
	}
	from, to, period, fields := parseReportPeriod(r)
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid report period", fields)
		return
	}

	rows, err := s.pool.Query(r.Context(), `
		with scoped_leads as (
			select
				l.*,
				o.code as office_code,
				case o.code
					when 'warsaw' then 'Europe/Warsaw'
					when 'london' then 'Europe/London'
					else 'Europe/Kyiv'
				end as timezone_name
			from public.leads l
			join public.offices o on o.id=l.office_id
			where l.archived_at is null
			  and ($1::uuid[] is null or l.office_id=any($1))
		)
		select
			l.id,
			l.office_code,
			l.assigned_to,
			coalesce(manager.display_name,''),
			coalesce(l.name,''),
			coalesce(l.phone,''),
			coalesce(l.source_created_at,l.created_at),
			l.client_status,
			l.client_status_changed_at,
			l.call_status,
			l.call_status_changed_at,
			l.loss_reason,
			activity.last_activity_at,
			greatest(0, (
				(now() at time zone l.timezone_name)::date -
				(coalesce(activity.last_activity_at,l.source_created_at,l.created_at) at time zone l.timezone_name)::date
			))::int as inactive_days,
			coalesce(signed_contract.amount, legacy_contract.amount),
			coalesce(signed_contract.currency, legacy_contract.currency),
			comments.items
		from scoped_leads l
		left join public.profiles manager on manager.id=l.assigned_to
		left join lateral (
			select c.amount,c.currency
			from public.lead_contracts c
			where c.lead_id=l.id
			  and c.status='signed'
			  and c.amount is not null
			  and c.currency is not null
			order by c.signed_at desc nulls last,c.created_at desc
			limit 1
		) signed_contract on true
		left join lateral (
			select (e.new_value->>'amount')::numeric as amount,e.new_value->>'currency' as currency
			from public.lead_events e
			where e.lead_id=l.id
			  and e.event_type in ('successful','contract_signed')
			  and e.new_value ? 'amount'
			  and e.new_value ? 'currency'
			order by e.created_at desc
			limit 1
		) legacy_contract on true
		left join lateral (
			select max(e.created_at) as last_activity_at
			from public.lead_events e
			where e.lead_id=l.id
			  and e.actor_id is not null
			  and e.event_category is distinct from 'system'
		) activity on true
		left join lateral (
			select coalesce(jsonb_agg(jsonb_build_object(
				'body', recent.comment,
				'occurredAt', recent.created_at,
				'authorId', recent.actor_id,
				'authorName', recent.author_name,
				'eventType', recent.event_type
			) order by recent.created_at desc),'[]'::jsonb) as items
			from (
				select e.comment,e.created_at,e.actor_id,coalesce(author.display_name,'') as author_name,e.event_type
				from public.lead_events e
				left join public.profiles author on author.id=e.actor_id
				where e.lead_id=l.id
				  and e.actor_id is not null
				  and e.event_category is distinct from 'system'
				  and e.comment is not null
				  and btrim(e.comment) <> ''
				order by e.created_at desc
				limit 2
			) recent
		) comments on true
		where $2::date is null or (
			(coalesce(l.source_created_at,l.created_at) at time zone l.timezone_name)::date between $2::date and $3::date
			or exists (
				select 1 from public.lead_events period_event
				where period_event.lead_id=l.id
				  and period_event.actor_id is not null
				  and period_event.event_category is distinct from 'system'
				  and (period_event.created_at at time zone l.timezone_name)::date between $2::date and $3::date
			)
		)
		order by l.office_code,manager.display_name nulls last,l.id
	`, nullableUUIDs(officeIDs), from, to)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "report_load_failed", "Could not load report", nil)
		return
	}
	defer rows.Close()

	totals := newReportTotals()
	managersByKey := map[string]*managerLeadReport{}
	lossCounts := map[string]int{}
	for rows.Next() {
		var lead reportLead
		var officeCode, managerName string
		var managerID *uuid.UUID
		var commentsJSON []byte
		if err := rows.Scan(
			&lead.ID,
			&officeCode,
			&managerID,
			&managerName,
			&lead.Name,
			&lead.Phone,
			&lead.CreatedAt,
			&lead.ClientStatus,
			&lead.ClientStatusChangedAt,
			&lead.CallStatus,
			&lead.CallStatusChangedAt,
			&lead.LossReason,
			&lead.LastHumanActivityAt,
			&lead.InactiveDays,
			&lead.ContractAmount,
			&lead.ContractCurrency,
			&commentsJSON,
		); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "report_load_failed", "Could not load report", nil)
			return
		}
		lead.Comments = []reportComment{}
		if err := json.Unmarshal(commentsJSON, &lead.Comments); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "report_load_failed", "Could not decode report comments", nil)
			return
		}
		terminal := lead.ClientStatus == "closed_lost" || lead.ClientStatus == "contract_signed"
		lead.Inactive7d = !terminal && lead.InactiveDays > 7
		addLeadToTotals(&totals, lead)

		managerKey := officeCode + "|"
		if managerID != nil {
			managerKey += managerID.String()
		}
		manager := managersByKey[managerKey]
		if manager == nil {
			manager = &managerLeadReport{
				OfficeCode:  officeCode,
				ManagerID:   managerID,
				ManagerName: managerName,
				Totals:      newReportTotals(),
				Leads:       []reportLead{},
			}
			managersByKey[managerKey] = manager
		}
		manager.Leads = append(manager.Leads, lead)
		addLeadToTotals(&manager.Totals, lead)

		if lead.ClientStatus == "closed_lost" {
			code := "unspecified"
			if lead.LossReason != nil && strings.TrimSpace(*lead.LossReason) != "" {
				code = strings.TrimSpace(*lead.LossReason)
			}
			lossCounts[code]++
		}
	}
	if err := rows.Err(); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "report_load_failed", "Could not load report", nil)
		return
	}
	finalizeTotals(&totals)

	managers := make([]managerLeadReport, 0, len(managersByKey))
	for _, manager := range managersByKey {
		finalizeTotals(&manager.Totals)
		sort.SliceStable(manager.Leads, func(i, j int) bool {
			if manager.Leads[i].InactiveDays != manager.Leads[j].InactiveDays {
				return manager.Leads[i].InactiveDays > manager.Leads[j].InactiveDays
			}
			return strings.ToLower(manager.Leads[i].Name) < strings.ToLower(manager.Leads[j].Name)
		})
		managers = append(managers, *manager)
	}
	sort.SliceStable(managers, func(i, j int) bool {
		if managers[i].OfficeCode != managers[j].OfficeCode {
			return managers[i].OfficeCode < managers[j].OfficeCode
		}
		if (managers[i].ManagerID == nil) != (managers[j].ManagerID == nil) {
			return managers[i].ManagerID != nil
		}
		return strings.ToLower(managers[i].ManagerName) < strings.ToLower(managers[j].ManagerName)
	})

	lossReasons, err := s.loadReportLossReasons(r, lossCounts, totals.ClosedLost)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "report_load_failed", "Could not load loss reasons", nil)
		return
	}
	writeJSON(w, http.StatusOK, leadReportResponse{
		GeneratedAt: time.Now().UTC(),
		Period:      period,
		Totals:      totals,
		LossReasons: lossReasons,
		Managers:    managers,
	})
}

func (s *Server) loadReportLossReasons(r *http.Request, counts map[string]int, total int) ([]lossReasonReport, error) {
	labels := map[string]lossReasonReport{
		"unspecified": {
			Code:    "unspecified",
			LabelUK: "Причину не вказано",
			LabelPL: "Nie podano powodu",
			LabelEN: "Reason not specified",
		},
	}
	rows, err := s.pool.Query(r.Context(), `select code,label_uk,label_pl from public.loss_reasons order by code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item lossReasonReport
		if err := rows.Scan(&item.Code, &item.LabelUK, &item.LabelPL); err != nil {
			return nil, err
		}
		item.LabelEN = item.Code
		labels[item.Code] = item
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := make([]lossReasonReport, 0, len(counts))
	for code, count := range counts {
		item, ok := labels[code]
		if !ok {
			item = lossReasonReport{Code: code, LabelUK: code, LabelPL: code, LabelEN: code}
		}
		item.Count = count
		if total > 0 {
			item.Percent = int(math.Round(float64(count) / float64(total) * 100))
		}
		result = append(result, item)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Count != result[j].Count {
			return result[i].Count > result[j].Count
		}
		return result[i].Code < result[j].Code
	})
	return result, nil
}

func nullableUUIDs(ids []uuid.UUID) any {
	if ids == nil {
		return nil
	}
	return ids
}
