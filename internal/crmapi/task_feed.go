package crmapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dzebovski/kolss-platform-api/internal/leadcohorts"
)

// The CRM v2 Tasks page reads one unified feed: standalone manual tasks (personal, list and
// linked ones) plus the automatic items GET /v1/dashboard/manager-tasks already unions
// (call-backs, comment reminders, showroom / measurement / office-work visits and leads with no
// next step). Automatic items are read-only here: they close through their lead action.

const taskFeedLimit = 500

var taskFeedViews = map[string]struct{}{"my_day": {}, "upcoming": {}, "all": {}, "done": {}, "list": {}}

// taskFeedKinds are the source chips of the boards.
var taskFeedKinds = map[string]struct{}{"call": {}, "visit": {}, "comment": {}, "nonext": {}, "list": {}, "personal": {}}

type taskFeedLink struct {
	Type  string    `json:"type"`
	ID    uuid.UUID `json:"id"`
	Name  *string   `json:"name"`
	Code  *string   `json:"code"`
	Phone *string   `json:"phone"`
}

type taskFeedList struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Color string    `json:"color"`
}

type taskFeedItem struct {
	ID            string              `json:"id"`
	SourceID      string              `json:"sourceId"`
	Source        string              `json:"source"`
	Kind          string              `json:"kind"`
	SubKind       string              `json:"subKind"`
	Title         *string             `json:"title"`
	Note          *string             `json:"note"`
	Status        string              `json:"status"`
	InProgress    bool                `json:"inProgress"`
	DueDate       *string             `json:"dueDate"`
	DueTime       *string             `json:"dueTime"`
	DueAt         *time.Time          `json:"dueAt"`
	OverdueDays   int                 `json:"overdueDays"`
	Office        dashboardTaskOffice `json:"office"`
	AssigneeID    *uuid.UUID          `json:"assigneeId"`
	AssigneeName  *string             `json:"assigneeName"`
	CreatedByID   *uuid.UUID          `json:"createdById"`
	CreatedByName *string             `json:"createdByName"`
	Link          *taskFeedLink       `json:"link"`
	List          *taskFeedList       `json:"list"`
	Version       *int64              `json:"version"`
	CanManage     bool                `json:"canManage"`
	CanEdit       bool                `json:"canEdit"`
	UpdatedAt     time.Time           `json:"updatedAt"`
}

// taskFeedIncludeNoNextStep switches the automatic "no next step" items (active leads with no
// call-back, reminder, visit or office-work block, due today) on. The owner decided on 2026-09-29
// not to show them yet: flip this to true to bring them back into the feed, the counts and every
// view (also update the OpenAPI notes, the contract §8 and TestTasksV2DB, which follows the flag).
const taskFeedIncludeNoNextStep = false

// taskFeedNoNextStepSQL is the union branch behind taskFeedIncludeNoNextStep. It mirrors the "lead"
// rows of the v1 dashboard "current" section.
const taskFeedNoNextStepSQL = `
	union all
	select 'lead:' || l.id::text, l.id::text, 'lead', 'nonext', 'no_next_step',
	 null::text, null::text, null::timestamptz, null::text,
	 o.id, o.code, o.name_uk, o.name_pl, o.timezone_name,
	 l.assigned_to, null::uuid,
	 'open', false, null::bigint, l.updated_at, null::timestamptz,
	 null::uuid, l.id, null::uuid, null::uuid, l.name, l.reference_id, l.phone
	from public.leads l join public.offices o on o.id=l.office_id
	where l.archived_at is null and l.client_status not in ('contract_signed','closed_lost')
	  and not exists (select 1 from ` + leadcohorts.ActiveReminderCandidatesSQL + ` active)
	  and not exists (select 1 from public.lead_showroom_visits work where work.lead_id=l.id and work.kind='office_work' and work.status='scheduled')
`

func taskFeedNoNextStepBranch() string {
	if taskFeedIncludeNoNextStep {
		return taskFeedNoNextStepSQL
	}
	return ""
}

// taskFeedSourceSQL yields one row per task-like item with the office-local date columns. It has
// no parameters; filtering happens in the query built around it.
var taskFeedSourceSQL = `with source_rows as (
	select 'manual:' || t.id::text id, t.id::text source_id, 'manual'::text source,
	 case when t.list_id is not null then 'list'
	      when t.link_lead_id is not null or t.link_project_id is not null or t.link_client_id is not null then 'comment'
	      else 'personal' end kind,
	 'task'::text sub_kind, t.title, t.note, t.due_at, to_char(t.due_time,'HH24:MI') due_time,
	 o.id office_id, o.code office_code, o.name_uk, o.name_pl, o.timezone_name,
	 t.assignee_id manager_candidate, t.created_by,
	 t.status::text status, t.in_progress, t.version, t.updated_at, coalesce(t.completed_at, t.updated_at) done_at,
	 t.list_id, ll.id lead_id, t.link_project_id, t.link_client_id, ll.name lead_name, ll.reference_id lead_code, ll.phone
	from public.tasks t
	join public.offices o on o.id=t.office_id
	left join public.leads ll on ll.id=t.link_lead_id
	where t.entity_type is null and t.entity_id is null and t.source='manual' and t.task_type='manual' and t.status<>'canceled'

	union all
	select 'reminder:' || reminder.kind || ':' || reminder.source_id, reminder.source_id, 'reminder',
	 case when reminder.kind='comment' then 'comment' else 'call' end, reminder.kind,
	 case when reminder.kind='comment' then event.comment else null end, null::text, reminder.due_at, null::text,
	 o.id, o.code, o.name_uk, o.name_pl, o.timezone_name,
	 case when reminder.kind='comment' then nullif(event.new_value->>'assigned_to','')::uuid else l.assigned_to end,
	 case when reminder.kind='comment' then event.actor_id else null::uuid end,
	 'open', false, null::bigint, reminder.action_at, null::timestamptz,
	 null::uuid, l.id, null::uuid, null::uuid, l.name, l.reference_id, l.phone
	from public.leads l join public.offices o on o.id=l.office_id
	cross join lateral ` + leadcohorts.ActiveReminderCandidatesSQL + ` reminder
	left join public.lead_events event on event.id::text=reminder.source_id and reminder.kind='comment'
	where reminder.kind in ('callback','thinking','comment')

	union all
	select 'appointment:' || v.id::text, v.id::text, 'appointment', 'visit', v.kind,
	 null::text, v.comment, v.scheduled_at, null::text,
	 o.id, o.code, o.name_uk, o.name_pl, o.timezone_name,
	 v.responsible_manager_id, v.created_by,
	 case when v.status='visited' then 'done' else 'open' end, false, v.version, v.updated_at, v.updated_at,
	 null::uuid, l.id, null::uuid, null::uuid, l.name, l.reference_id, l.phone
	from public.lead_showroom_visits v join public.leads l on l.id=v.lead_id join public.offices o on o.id=l.office_id
	where l.archived_at is null and v.kind in ('showroom','measurement','office_work') and (
	  (v.status='scheduled' and l.client_status not in ('contract_signed','closed_lost')
	    and (v.scheduled_at at time zone o.timezone_name)::date >= (now() at time zone o.timezone_name)::date - 365)
	  or (v.status='visited' and v.updated_at >= now() - interval '60 days'))

` + taskFeedNoNextStepBranch() + `
), resolved as (
	select s.*,
	 case when p.is_active=true and (p.role = 'super_admin' or exists (select 1 from public.user_office_memberships m where m.user_id=p.id and m.office_id=s.office_id)) then s.manager_candidate else null end manager_id,
	 (now() at time zone s.timezone_name)::date local_today,
	 case when s.source='lead' then (now() at time zone s.timezone_name)::date else (s.due_at at time zone s.timezone_name)::date end local_date,
	 case when s.source='manual' then s.due_time
	      when s.due_at is null then null
	      when s.source='reminder' then nullif(to_char(s.due_at at time zone s.timezone_name,'HH24:MI'),'00:00')
	      else to_char(s.due_at at time zone s.timezone_name,'HH24:MI') end local_time
	from source_rows s left join public.profiles p on p.id=s.manager_candidate
)`

// taskFeedViewPredicate is the row filter of each view, over the columns of "resolved". "Mine" is
// applied separately through the assignee argument. Automatic "no next step" rows count as due today.
func taskFeedViewPredicate(view string) string {
	switch view {
	case "my_day":
		return `(status='open' and local_date <= local_today) or (status='done' and local_date = local_today)`
	case "upcoming":
		return `status='open' and (local_date is null or local_date > local_today)`
	case "done":
		return `status='done'`
	case "all":
		return `status='open' or (status='done' and local_date = local_today)`
	default: // list
		return `true`
	}
}

func taskFeedOrder(view string) string {
	if view == "done" {
		return "done_at desc nulls last, id"
	}
	return "local_date asc nulls last, local_time asc nulls last, id"
}

type taskFeedQuery struct {
	View       string
	OfficeIDs  []uuid.UUID // nil = every office
	AssigneeID *uuid.UUID  // nil = anyone
	ListID     *uuid.UUID
	Kinds      []string // nil = every kind
	Search     string
}

// buildTaskFeedQuery returns the SQL and its fixed argument list ($1 offices, $2 assignee,
// $3 list, $4 kinds, $5 search).
func buildTaskFeedQuery(q taskFeedQuery) (string, []any) {
	var kinds any
	if q.Kinds != nil {
		kinds = q.Kinds
	}
	args := []any{nullableUUIDs(q.OfficeIDs), q.AssigneeID, q.ListID, kinds, strings.TrimSpace(q.Search)}
	return taskFeedSelectSQL(`
		($1::uuid[] is null or office_id = any($1))
		and ($2::uuid is null or manager_id = $2)
		and ($3::uuid is null or list_id = $3)
		and ($4::text[] is null or kind = any($4))
		and ($5::text = '' or position(lower($5) in lower(concat_ws(' ', title, lead_name, lead_code, list_name))) > 0)
		and (`+taskFeedViewPredicate(q.View)+`)`, taskFeedOrder(q.View), taskFeedLimit+1), args
}

func taskFeedSelectSQL(where, order string, limit int) string {
	return taskFeedSourceSQL + `, joined as (
	select r.*, lst.name list_name, lst.color list_color, lst.owner_id list_owner_id,
	 nullif(ap.display_name,'') assignee_name, nullif(cp.display_name,'') created_by_name
	from resolved r
	left join public.task_lists lst on lst.id = r.list_id
	left join public.profiles ap on ap.id = r.manager_id
	left join public.profiles cp on cp.id = r.created_by
)
select id, source_id, source, kind, sub_kind, title, note, status, in_progress, local_date::text, local_time, due_at,
 case when status='open' and local_date < local_today then local_today - local_date else 0 end overdue_days,
 office_id::text, office_code, name_uk, name_pl, timezone_name,
 manager_id, assignee_name, created_by, created_by_name,
 lead_id, lead_name, lead_code, phone, link_project_id, link_client_id,
 list_id, list_name, list_color, list_owner_id, version, updated_at
from joined where ` + where + ` order by ` + order + ` limit ` + strconv.Itoa(limit)
}

// parseTaskFeedKinds validates the comma-separated source chips filter.
func parseTaskFeedKinds(raw string) ([]string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, true
	}
	seen := map[string]struct{}{}
	kinds := []string{}
	for _, part := range strings.Split(raw, ",") {
		kind := strings.TrimSpace(part)
		if _, ok := taskFeedKinds[kind]; !ok {
			return nil, false
		}
		if _, dup := seen[kind]; dup {
			continue
		}
		seen[kind] = struct{}{}
		kinds = append(kinds, kind)
	}
	return kinds, true
}

// canViewAllTasks: the "All tasks" view is for super admins, office admins and curators (owner
// decision 2026-09-29); each sees the offices they belong to, a super admin every office.
func canViewAllTasks(actor Actor) bool {
	return actor.IsSuperAdmin() || actor.Role == "office_admin" || actor.Role == "curator"
}

type taskFeedScanner interface {
	Scan(dest ...any) error
}

func scanTaskFeedItem(rows taskFeedScanner, actor Actor) (taskFeedItem, error) {
	var item taskFeedItem
	var managerID, createdBy, leadID, projectID, clientID, listID, listOwnerID *uuid.UUID
	var leadName, leadCode, phone, listName, listColor *string
	if err := rows.Scan(&item.ID, &item.SourceID, &item.Source, &item.Kind, &item.SubKind, &item.Title, &item.Note, &item.Status, &item.InProgress,
		&item.DueDate, &item.DueTime, &item.DueAt, &item.OverdueDays,
		&item.Office.ID, &item.Office.Code, &item.Office.NameUK, &item.Office.NamePL, &item.Office.TimezoneName,
		&managerID, &item.AssigneeName, &createdBy, &item.CreatedByName,
		&leadID, &leadName, &leadCode, &phone, &projectID, &clientID,
		&listID, &listName, &listColor, &listOwnerID, &item.Version, &item.UpdatedAt); err != nil {
		return item, err
	}
	item.AssigneeID = managerID
	item.CreatedByID = createdBy
	switch {
	case leadID != nil:
		item.Link = &taskFeedLink{Type: "lead", ID: *leadID, Name: leadName, Code: leadCode, Phone: phone}
	case projectID != nil:
		item.Link = &taskFeedLink{Type: "project", ID: *projectID}
	case clientID != nil:
		item.Link = &taskFeedLink{Type: "client", ID: *clientID}
	}
	if listID != nil && listName != nil && listColor != nil {
		item.List = &taskFeedList{ID: *listID, Name: *listName, Color: *listColor}
	}
	if item.Source == "manual" {
		officeID, _ := uuid.Parse(item.Office.ID)
		access := taskAccess{OfficeID: officeID, CreatedBy: createdBy, AssigneeID: managerID, ListOwnerID: listOwnerID}
		item.CanManage = canManageTask(actor, access)
		item.CanEdit = item.CanManage && canEditTaskContent(actor, access)
	}
	return item, nil
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	query := r.URL.Query()
	view := strings.TrimSpace(query.Get("view"))
	if _, ok := taskFeedViews[view]; !ok {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid task criteria", map[string]string{"view": "Required; must be my_day, upcoming, all, done, or list"})
		return
	}
	kinds, ok := parseTaskFeedKinds(query.Get("kinds"))
	if !ok {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid task criteria", map[string]string{"kinds": "Use call, visit, comment, nonext, list, personal"})
		return
	}
	feed := taskFeedQuery{View: view, Kinds: kinds, Search: query.Get("q")}
	if len([]rune(feed.Search)) > 200 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid task criteria", map[string]string{"q": "Use at most 200 characters"})
		return
	}
	switch view {
	case "list":
		listID, err := uuid.Parse(strings.TrimSpace(query.Get("listId")))
		if err != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid task criteria", map[string]string{"listId": "Required for the list view"})
			return
		}
		// Task lists are visible to everyone: no office or assignee scope.
		feed.ListID = &listID
	case "all":
		if !canViewAllTasks(actor) {
			s.writeError(w, r, http.StatusForbidden, "forbidden", "Only admins and curators can see all tasks", nil)
			return
		}
		officeIDs, ok := s.reportOfficeFilter(w, r, actor)
		if !ok {
			return
		}
		feed.OfficeIDs = officeIDs
		if raw := strings.TrimSpace(query.Get("assigneeId")); raw != "" {
			id, err := uuid.Parse(raw)
			if err != nil {
				s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid task criteria", map[string]string{"assigneeId": "Must be a UUID"})
				return
			}
			feed.AssigneeID = &id
		}
	default: // my_day, upcoming, done: the actor's own tasks
		officeIDs, ok := s.reportOfficeFilter(w, r, actor)
		if !ok {
			return
		}
		feed.OfficeIDs = officeIDs
		feed.AssigneeID = &actor.ID
	}

	sql, args := buildTaskFeedQuery(feed)
	rows, err := s.pool.Query(r.Context(), sql, args...)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "tasks_load_failed", "Could not load tasks", nil)
		return
	}
	defer rows.Close()
	items := make([]taskFeedItem, 0, 64)
	truncated := false
	for rows.Next() {
		item, err := scanTaskFeedItem(rows, actor)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "tasks_load_failed", "Could not load tasks", nil)
			return
		}
		if len(items) == taskFeedLimit {
			truncated = true
			break
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "tasks_load_failed", "Could not load tasks", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "truncated": truncated})
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	taskID, err := uuid.Parse(r.PathValue("taskId"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid task id", nil)
		return
	}
	sql := taskFeedSelectSQL(`id = 'manual:' || $1::text`, "id", 1)
	row := s.pool.QueryRow(r.Context(), sql, taskID)
	item, err := scanTaskFeedItem(row, actor)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "task_not_found", "Task not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_load_failed", "Could not load task", nil)
		return
	}
	// Same reach as the write rules, plus everyone can read tasks of a list.
	if !item.CanManage && item.List == nil && (item.AssigneeID == nil || *item.AssigneeID != actor.ID) {
		s.writeError(w, r, http.StatusNotFound, "task_not_found", "Task not found", nil)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

type taskCountsResponse struct {
	MyDay    int  `json:"myDay"`
	Upcoming int  `json:"upcoming"`
	Overdue  *int `json:"overdue"`
}

// handleTaskCounts returns the sidebar badges: the actor's My day and Upcoming, and (admins only)
// everyone's overdue tasks in the offices they can see.
func (s *Server) handleTaskCounts(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	officeIDs, ok := s.reportOfficeFilter(w, r, actor)
	if !ok {
		return
	}
	var counts taskCountsResponse
	var overdue int
	err := s.pool.QueryRow(r.Context(), taskFeedSourceSQL+`
		select
		 count(*) filter (where manager_id = $2 and status='open' and local_date <= local_today),
		 count(*) filter (where manager_id = $2 and status='open' and (local_date is null or local_date > local_today)),
		 count(*) filter (where status='open' and local_date < local_today)
		from resolved where ($1::uuid[] is null or office_id = any($1))`, nullableUUIDs(officeIDs), actor.ID).Scan(&counts.MyDay, &counts.Upcoming, &overdue)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_counts_failed", "Could not load task counts", nil)
		return
	}
	if canViewAllTasks(actor) {
		counts.Overdue = &overdue
	}
	writeJSON(w, http.StatusOK, counts)
}
