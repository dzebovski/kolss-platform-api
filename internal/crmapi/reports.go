package crmapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

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

func (s *Server) handleLeadReport(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	officeIDs, ok := s.reportOfficeFilter(w, r, actor)
	if !ok {
		return
	}
	days := 40
	if parsed, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("days"))); err == nil && parsed > 0 && parsed <= 3660 {
		days = parsed
	}
	var created, taken, scheduled, visited, successful, closed int
	err := s.pool.QueryRow(r.Context(), `
		select
			count(*),
			count(*) filter (where l.assigned_to is not null or l.workflow_status <> 'new'),
			count(*) filter (where l.workflow_status in ('visit_scheduled','visit_rescheduled')),
			count(*) filter (where l.workflow_status in ('visit_completed','successful')),
			count(*) filter (where l.workflow_status = 'successful'),
			count(*) filter (where l.workflow_status = 'closed')
		from public.leads l
		where l.archived_at is null and l.created_at >= now()-make_interval(days=>$2)
		  and ($1::uuid[] is null or l.office_id=any($1))
	`, nullableUUIDs(officeIDs), days).Scan(&created, &taken, &scheduled, &visited, &successful, &closed)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "report_load_failed", "Could not load report", nil)
		return
	}
	funnel := map[string]int{"created": created, "taken": taken, "scheduled": scheduled, "visited": visited, "successful": successful, "closed": closed}
	managerRows, err := s.pool.Query(r.Context(), `
		select o.code,l.assigned_to,coalesce(p.display_name,''),count(*)
		from public.leads l join public.offices o on o.id=l.office_id
		left join public.profiles p on p.id=l.assigned_to
		where l.archived_at is null and l.created_at >= now()-make_interval(days=>$2)
		  and (l.assigned_to is not null or l.workflow_status <> 'new')
		  and ($1::uuid[] is null or l.office_id=any($1))
		group by o.code,l.assigned_to,p.display_name order by o.code,p.display_name
	`, nullableUUIDs(officeIDs), days)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "report_load_failed", "Could not load report", nil)
		return
	}
	defer managerRows.Close()
	managers := []map[string]any{}
	for managerRows.Next() {
		var officeCode, managerName string
		var managerID *uuid.UUID
		var count int
		if err := managerRows.Scan(&officeCode, &managerID, &managerName, &count); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "report_load_failed", "Could not load report", nil)
			return
		}
		managers = append(managers, map[string]any{"officeCode": officeCode, "managerId": managerID, "managerName": managerName, "takenCount": count})
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "funnel": funnel, "managers": managers})
}

func nullableUUIDs(ids []uuid.UUID) any {
	if ids == nil {
		return nil
	}
	return ids
}
