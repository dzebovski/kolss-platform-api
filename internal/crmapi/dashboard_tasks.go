package crmapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/dzebovski/kolss-platform-api/internal/leadcohorts"
	"github.com/google/uuid"
)

const dashboardTasksPageSize = 50

type dashboardTaskOffice struct {
	ID           string `json:"id"`
	Code         string `json:"code"`
	NameUK       string `json:"nameUk"`
	NamePL       string `json:"namePl"`
	TimezoneName string `json:"timezoneName"`
}

type dashboardTask struct {
	ID        string              `json:"id"`
	SourceID  string              `json:"sourceId"`
	Source    string              `json:"source"`
	Kind      string              `json:"kind"`
	Title     string              `json:"title"`
	Comment   *string             `json:"comment"`
	DueAt     *time.Time          `json:"dueAt"`
	LocalDate *string             `json:"localDate"`
	Office    dashboardTaskOffice `json:"office"`
	ManagerID *string             `json:"managerId"`
	LeadID    *string             `json:"leadId"`
	LeadName  *string             `json:"leadName"`
	Phone     *string             `json:"phone"`
	Status    string              `json:"status"`
	Version   *int64              `json:"version"`
	UpdatedAt time.Time           `json:"updatedAt"`
}

type dashboardTaskResponse struct {
	Items      []dashboardTask `json:"items"`
	NextCursor *string         `json:"nextCursor"`
}

// dashboardTaskCursor carries the sort tuple as well as the filters that made
// it. It is intentionally opaque to callers; rejecting a cursor used with a
// different section/filter avoids skipping rows across independently sorted
// feeds.
type dashboardTaskCursor struct {
	OfficeID  string `json:"o"`
	ManagerID string `json:"m"`
	Section   string `json:"s"`
	Bucket    int    `json:"b"`
	Date      string `json:"d"`
	DueAt     string `json:"a"`
	UpdatedAt string `json:"u"`
	ID        string `json:"i"`
}

func parseDashboardTaskCursor(raw, officeID, managerID, section string) (*dashboardTaskCursor, map[string]string) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	bytes, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, map[string]string{"cursor": "Invalid cursor"}
	}
	var cursor dashboardTaskCursor
	if err := json.Unmarshal(bytes, &cursor); err != nil || cursor.OfficeID != officeID || cursor.ManagerID != managerID || cursor.Section != section || cursor.ID == "" {
		return nil, map[string]string{"cursor": "Invalid cursor"}
	}
	if !isDashboardCursorTime(cursor.UpdatedAt) {
		return nil, map[string]string{"cursor": "Invalid cursor"}
	}
	if section != "done" && section != "canceled" && (cursor.Bucket < 0 || cursor.Bucket > 2 || !isDashboardCursorDate(cursor.Date) || !isDashboardCursorTime(cursor.DueAt)) {
		return nil, map[string]string{"cursor": "Invalid cursor"}
	}
	return &cursor, nil
}

func isDashboardCursorDate(value string) bool {
	_, err := time.Parse(time.DateOnly, value)
	return err == nil
}

func isDashboardCursorTime(value string) bool {
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}

func encodeDashboardTaskCursor(cursor dashboardTaskCursor) string {
	bytes, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(bytes)
}

func (s *Server) handleDashboardManagerTasks(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	officeIDs, ok := s.reportOfficeFilter(w, r, actor)
	if !ok {
		return
	}
	section := strings.TrimSpace(r.URL.Query().Get("section"))
	if section != "important" && section != "current" && section != "future" && section != "done" && section != "canceled" {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid dashboard task criteria", map[string]string{"section": "Required; must be important, current, future, done, or canceled"})
		return
	}
	managerRaw := strings.TrimSpace(r.URL.Query().Get("managerId"))
	if managerRaw == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid dashboard task criteria", map[string]string{"managerId": "Required; must be a UUID or unassigned"})
		return
	}
	managerID := uuid.Nil.String()
	if managerRaw != "unassigned" {
		id, err := uuid.Parse(managerRaw)
		if err != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid dashboard task criteria", map[string]string{"managerId": "Must be a UUID or unassigned"})
			return
		}
		managerID = id.String()
	}
	officeCursorID := strings.TrimSpace(r.URL.Query().Get("officeId"))
	cursor, fields := parseDashboardTaskCursor(r.URL.Query().Get("cursor"), officeCursorID, managerRaw, section)
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid dashboard task criteria", fields)
		return
	}

	query, args := buildDashboardManagerTasksQuery(officeIDs, managerID, section, cursor)
	rows, err := s.pool.Query(r.Context(), query, args...)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "dashboard_tasks_load_failed", "Could not load dashboard tasks", nil)
		return
	}
	defer rows.Close()

	items := make([]dashboardTask, 0, dashboardTasksPageSize)
	var next *string
	var lastCursor dashboardTaskCursor
	for rows.Next() {
		var item dashboardTask
		var managerID, leadID *uuid.UUID
		var sortBucket int
		var sortDate string
		var sortDueAt time.Time
		if err := rows.Scan(&item.ID, &item.SourceID, &item.Source, &item.Kind, &item.Title, &item.Comment, &item.DueAt, &item.LocalDate,
			&item.Office.ID, &item.Office.Code, &item.Office.NameUK, &item.Office.NamePL, &item.Office.TimezoneName,
			&managerID, &leadID, &item.LeadName, &item.Phone, &item.Status, &item.Version, &item.UpdatedAt, &sortBucket, &sortDate, &sortDueAt); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "dashboard_tasks_load_failed", "Could not load dashboard tasks", nil)
			return
		}
		if managerID != nil {
			value := managerID.String()
			item.ManagerID = &value
		}
		if leadID != nil {
			value := leadID.String()
			item.LeadID = &value
		}
		if len(items) == dashboardTasksPageSize {
			encoded := encodeDashboardTaskCursor(lastCursor)
			next = &encoded
			break
		}
		items = append(items, item)
		lastCursor = dashboardTaskCursor{OfficeID: officeCursorID, ManagerID: managerRaw, Section: section, Bucket: sortBucket, Date: sortDate, DueAt: sortDueAt.UTC().Format(time.RFC3339Nano), UpdatedAt: item.UpdatedAt.UTC().Format(time.RFC3339Nano), ID: item.ID}
	}
	if err := rows.Err(); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "dashboard_tasks_load_failed", "Could not load dashboard tasks", nil)
		return
	}
	writeJSON(w, http.StatusOK, dashboardTaskResponse{Items: items, NextCursor: next})
}

func buildDashboardManagerTasksQuery(officeIDs []uuid.UUID, managerID, section string, cursor *dashboardTaskCursor) (string, []any) {
	args := []any{nullableUUIDs(officeIDs), managerID}
	cursorWhere := ""
	if cursor != nil {
		if section == "done" || section == "canceled" {
			args = append(args, cursor.UpdatedAt, cursor.ID)
			cursorWhere = "and (updated_at, id) < ($3::timestamptz, $4)"
		} else {
			args = append(args, cursor.Bucket, cursor.Date, cursor.DueAt, cursor.ID)
			cursorWhere = "and (sort_bucket, sort_date, sort_due_at, id) > ($3::int, $4::date, $5::timestamptz, $6)"
		}
	}
	sectionWhere := map[string]string{
		"important": "status='open' and source='manual' and (local_date is null or local_date <= local_today)",
		"current":   "status='open' and source <> 'manual' and (local_date is null or local_date <= local_today)",
		"future":    "status='open' and local_date > local_today",
		"done":      "status='done'",
		"canceled":  "status='canceled'",
	}[section]
	order := "sort_bucket asc, sort_date asc, sort_due_at asc, id asc"
	if section == "done" || section == "canceled" {
		order = "updated_at desc, id desc"
	}

	return `with source_rows as (
		select 'manual:' || t.id::text id, t.id::text source_id, 'manual'::text source, 'manual'::text kind,
		 t.title, null::text as comment, t.due_at, o.id office_id, o.code office_code, o.name_uk, o.name_pl, o.timezone_name,
		 t.assignee_id manager_candidate, null::uuid lead_id, null::text lead_name, null::text phone, t.status::text status,
		 t.version, t.updated_at
		from public.tasks t join public.offices o on o.id=t.office_id
		where t.entity_type is null and t.entity_id is null and t.source='manual' and t.task_type='manual'

		union all
		select 'reminder:' || reminder.kind || ':' || reminder.source_id, reminder.source_id, 'reminder', reminder.kind,
		 case reminder.kind when 'callback' then 'Передзвонити' when 'thinking' then 'Обдумати' else 'Коментар' end,
		 case when reminder.kind='comment' then event.comment else null end, reminder.due_at,
		 o.id,o.code,o.name_uk,o.name_pl,o.timezone_name,
		 case when reminder.kind='comment' then nullif(event.new_value->>'assigned_to','')::uuid else l.assigned_to end,
		 l.id,l.name,l.phone,'open',null::bigint,reminder.action_at
		from public.leads l join public.offices o on o.id=l.office_id
		cross join lateral ` + leadcohorts.ActiveReminderCandidatesSQL + ` reminder
		left join public.lead_events event on event.id::text=reminder.source_id and reminder.kind='comment'
		where reminder.kind in ('callback','thinking','comment')

		union all
		select 'appointment:' || v.id::text,v.id::text,'appointment',v.kind,
		 case v.kind when 'showroom' then 'Шоурум' when 'measurement' then 'Замір' else 'Робота в офісі' end,
		 v.comment,v.scheduled_at,o.id,o.code,o.name_uk,o.name_pl,o.timezone_name,v.responsible_manager_id,
		 l.id,l.name,l.phone,'open',v.version,v.updated_at
		from public.lead_showroom_visits v join public.leads l on l.id=v.lead_id join public.offices o on o.id=l.office_id
		where l.archived_at is null and l.client_status not in ('contract_signed','closed_lost')
		  and v.status='scheduled' and v.kind in ('showroom','measurement','office_work')
		  and (v.scheduled_at at time zone o.timezone_name)::date >= (now() at time zone o.timezone_name)::date - 365

		union all
		select 'lead:' || l.id::text,l.id::text,'lead',l.client_status,
		 coalesce(nullif(l.name,''),l.reference_id),null::text,null::timestamptz,o.id,o.code,o.name_uk,o.name_pl,o.timezone_name,l.assigned_to,
		 l.id,l.name,l.phone,'open',null::bigint,l.updated_at
		from public.leads l join public.offices o on o.id=l.office_id
		where l.archived_at is null and l.client_status not in ('contract_signed','closed_lost')
		  and not exists (select 1 from ` + leadcohorts.ActiveReminderCandidatesSQL + ` active)
		  and not exists (select 1 from public.lead_showroom_visits work where work.lead_id=l.id and work.kind='office_work' and work.status='scheduled')
	), resolved as (
		select s.*,
		 case when p.is_active=true and p.role <> 'super_admin' and exists (select 1 from public.user_office_memberships m where m.user_id=p.id and m.office_id=s.office_id) then s.manager_candidate else null end manager_id,
		 (s.due_at at time zone s.timezone_name)::date local_date,
		 (now() at time zone s.timezone_name)::date local_today
		from source_rows s left join public.profiles p on p.id=s.manager_candidate
	), filtered as (
		select *, case when local_date is null then 2 when local_date < local_today then 0 else 1 end sort_bucket,
		 coalesce(local_date, date '9999-12-31') sort_date,
		 coalesce(due_at, '9999-12-31T23:59:59Z'::timestamptz) sort_due_at
		from resolved
		where ($1::uuid[] is null or office_id=any($1)) and (($2::uuid='00000000-0000-0000-0000-000000000000'::uuid and manager_id is null) or manager_id=$2::uuid)
	)
	select id,source_id,source,kind,title,comment,due_at,local_date::text,office_id::text,office_code,name_uk,name_pl,timezone_name,
	 manager_id,lead_id,lead_name,phone,status,version,updated_at,sort_bucket,sort_date::text,sort_due_at
	from filtered where ` + sectionWhere + ` ` + cursorWhere + ` order by ` + order + ` limit 51`, args
}
