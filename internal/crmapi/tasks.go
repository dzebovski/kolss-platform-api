package crmapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const taskDateLayout = "2006-01-02"

// taskLinkRequest links a new task to one CRM entity. Only "lead" is accepted for now: projects
// and clients are validated once P1 / CL1 expose them (see taskLinkTypeAvailable).
type taskLinkRequest struct {
	Type string    `json:"type"`
	ID   uuid.UUID `json:"id"`
}

type createTaskRequest struct {
	OfficeID   uuid.UUID        `json:"officeId"`
	AssigneeID uuid.UUID        `json:"assigneeId"`
	Title      string           `json:"title"`
	DueDate    *string          `json:"dueDate"`
	DueTime    *string          `json:"dueTime"`
	Note       *string          `json:"note"`
	ListID     *uuid.UUID       `json:"listId"`
	Link       *taskLinkRequest `json:"link"`
}

// updateTaskRequest is a partial update. An empty Status means "unchanged" (v1 always sent it).
// Note, DueDate and DueTime distinguish an absent field from an explicit null (clear).
type updateTaskRequest struct {
	Status     string           `json:"status"`
	InProgress *bool            `json:"inProgress"`
	AssigneeID *uuid.UUID       `json:"assigneeId"`
	Title      *string          `json:"title"`
	Note       optional[string] `json:"note"`
	DueDate    optional[string] `json:"dueDate"`
	DueTime    optional[string] `json:"dueTime"`
}

type taskMutationResponse struct {
	ID         uuid.UUID `json:"id"`
	Version    int64     `json:"version"`
	Status     string    `json:"status"`
	InProgress bool      `json:"inProgress"`
}

var (
	errTaskNotFound        = errors.New("task not found")
	errTaskOfficeDenied    = errors.New("task office denied")
	errTaskOfficeInvalid   = errors.New("task office invalid")
	errTaskAssigneeInvalid = errors.New("task assignee invalid")
	errTaskVersion         = errors.New("task version conflict")
	errTaskListInvalid     = errors.New("task list invalid")
	errTaskLinkInvalid     = errors.New("task link invalid")
	errTaskContentDenied   = errors.New("task content denied")
)

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		s.writeError(w, r, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required", nil)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid task body", nil)
		return
	}
	var req createTaskRequest
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid task body", nil)
		return
	}
	if fields := validateCreateTask(req); len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Task validation failed", fields)
		return
	}
	if !actor.CanManageTasks(req.OfficeID) {
		s.writeError(w, r, http.StatusForbidden, "office_forbidden", "Office access denied", nil)
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_create_failed", "Could not create task", nil)
		return
	}
	defer tx.Rollback(r.Context())
	hash := sha256.Sum256(body)
	cached, status, cachedBody, err := claimIdempotency(r, tx, actor.ID, "task.create", key, hex.EncodeToString(hash[:]))
	if err != nil {
		s.writeError(w, r, http.StatusConflict, "idempotency_conflict", "Idempotency key was already used for another request", nil)
		return
	}
	if cached {
		if err := tx.Commit(r.Context()); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "task_create_failed", "Could not create task", nil)
			return
		}
		writeJSON(w, status, cachedBody)
		return
	}
	response, err := createManualTask(r, tx, actor, req)
	if err != nil {
		s.writeTaskMutationError(w, r, err, "task_create_failed")
		return
	}
	rawResponse, _ := json.Marshal(response)
	if _, err := tx.Exec(r.Context(), `update public.api_idempotency_keys set response_status=$4, response_body=$5::jsonb where actor_id=$1 and operation=$2 and idempotency_key=$3`, actor.ID, "task.create", key, http.StatusCreated, rawResponse); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_create_failed", "Could not create task", nil)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_create_failed", "Could not create task", nil)
		return
	}
	writeJSON(w, http.StatusCreated, response)
}

func (s *Server) handleUpdateTask(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	taskID, err := uuid.Parse(r.PathValue("taskId"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid task id", nil)
		return
	}
	version, ok := parseIfMatch(r)
	if !ok {
		s.writeError(w, r, http.StatusPreconditionRequired, "version_required", "If-Match task version is required", nil)
		return
	}
	var req updateTaskRequest
	if err := decodeJSON(w, r, 64*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid task body", nil)
		return
	}
	if fields := validateUpdateTask(req); len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Task validation failed", fields)
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_update_failed", "Could not update task", nil)
		return
	}
	defer tx.Rollback(r.Context())
	response, err := updateManualTask(r, tx, actor, taskID, version, req)
	if err != nil {
		s.writeTaskMutationError(w, r, err, "task_update_failed")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_update_failed", "Could not update task", nil)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// taskLinkTypeAvailable reports which link types the API accepts. Projects (P1) and clients (CL1)
// are stored in link_project_id / link_client_id without a foreign key, but they stay disabled
// until their own endpoints exist to validate the id and the office; enable each type here then.
func taskLinkTypeAvailable(linkType string) bool {
	return linkType == "lead"
}

func createManualTask(r *http.Request, tx pgx.Tx, actor Actor, req createTaskRequest) (taskMutationResponse, error) {
	var timezone string
	if err := tx.QueryRow(r.Context(), `select timezone_name from public.offices where id=$1 and is_active=true`, req.OfficeID).Scan(&timezone); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return taskMutationResponse{}, errTaskOfficeInvalid
		}
		return taskMutationResponse{}, err
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return taskMutationResponse{}, err
	}
	dueAt, err := parseTaskDue(req.DueDate, req.DueTime, location)
	if err != nil {
		return taskMutationResponse{}, err
	}
	var assigneeActive bool
	if err := tx.QueryRow(r.Context(), `select exists(select 1 from public.profiles p join public.user_office_memberships m on m.user_id=p.id where p.id=$1 and p.is_active=true and p.role<>'super_admin' and m.office_id=$2)`, req.AssigneeID, req.OfficeID).Scan(&assigneeActive); err != nil {
		return taskMutationResponse{}, err
	}
	if !assigneeActive {
		return taskMutationResponse{}, errTaskAssigneeInvalid
	}
	if req.ListID != nil {
		var exists bool
		if err := tx.QueryRow(r.Context(), `select exists(select 1 from public.task_lists where id=$1)`, *req.ListID).Scan(&exists); err != nil {
			return taskMutationResponse{}, err
		}
		if !exists {
			return taskMutationResponse{}, errTaskListInvalid
		}
	}
	var linkLeadID *uuid.UUID
	if req.Link != nil && req.Link.Type == "lead" {
		var leadOffice uuid.UUID
		err := tx.QueryRow(r.Context(), `select office_id from public.leads where id=$1 and archived_at is null`, req.Link.ID).Scan(&leadOffice)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && leadOffice != req.OfficeID) {
			return taskMutationResponse{}, errTaskLinkInvalid
		}
		if err != nil {
			return taskMutationResponse{}, err
		}
		linkLeadID = &req.Link.ID
	}
	var dueTime *string
	if dueAt != nil {
		dueTime = trimmedPtr(req.DueTime)
	}
	var response taskMutationResponse
	err = tx.QueryRow(r.Context(), `
		insert into public.tasks (entity_type,entity_id,office_id,assignee_id,title,note,due_at,due_time,list_id,link_lead_id,priority,source,task_type,status,created_by,updated_by)
		values (null,null,$1,$2,$3,$4,$5,$6::time,$7,$8,'high'::public.task_priority,'manual'::public.task_source,'manual'::public.task_type,'open'::public.task_status,$9,$9)
		returning id,version,status::text,in_progress
	`, req.OfficeID, req.AssigneeID, strings.TrimSpace(req.Title), blankToNil(req.Note), dueAt, dueTime, req.ListID, linkLeadID, actor.ID).Scan(&response.ID, &response.Version, &response.Status, &response.InProgress)
	return response, err
}

// taskState is the editable part of a manual task. Dates are office-local.
type taskState struct {
	Status     string
	InProgress bool
	Title      string
	Note       *string
	DueDate    *string
	DueTime    *string
}

type taskPatchEffect struct {
	ContentChanged bool // title or note
}

// applyTaskPatch applies a validated partial update to the current state and enforces the rules
// that link fields together: only an open task is in progress, and a time needs a date.
func applyTaskPatch(cur taskState, req updateTaskRequest) (taskState, taskPatchEffect, map[string]string) {
	next := cur
	effect := taskPatchEffect{}
	fields := map[string]string{}
	if req.Status != "" {
		next.Status = req.Status
		if req.Status != "open" || cur.Status != "open" {
			// Done and canceled tasks are not in progress; reopening starts at "To do".
			next.InProgress = false
		}
	}
	if req.InProgress != nil {
		if *req.InProgress && next.Status != "open" {
			fields["inProgress"] = "Only an open task can be in progress"
		} else {
			next.InProgress = *req.InProgress
		}
	}
	if req.Title != nil {
		title := strings.TrimSpace(*req.Title)
		if title != next.Title {
			effect.ContentChanged = true
		}
		next.Title = title
	}
	if req.Note.Set {
		note := blankToNil(req.Note.Value)
		if !equalStringPtr(note, next.Note) {
			effect.ContentChanged = true
		}
		next.Note = note
	}
	if req.DueDate.Set {
		next.DueDate = trimmedPtr(req.DueDate.Value)
		if next.DueDate == nil {
			next.DueTime = nil
		}
	}
	if req.DueTime.Set {
		next.DueTime = trimmedPtr(req.DueTime.Value)
	}
	if next.DueTime != nil && next.DueDate == nil {
		fields["dueTime"] = "Set a date before the time"
	}
	return next, effect, fields
}

func trimmedPtr(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	return &trimmed
}

// taskAccess is what the permission rules need to know about a stored task.
type taskAccess struct {
	OfficeID    uuid.UUID
	CreatedBy   *uuid.UUID
	AssigneeID  *uuid.UUID
	ListOwnerID *uuid.UUID
}

func isSameUser(id *uuid.UUID, actor Actor) bool {
	return id != nil && *id == actor.ID
}

// canManageTask: any member of the task's office (the v1 rule) or the owner of the task's list.
func canManageTask(actor Actor, task taskAccess) bool {
	return actor.CanManageTasks(task.OfficeID) || isSameUser(task.ListOwnerID, actor)
}

// canEditTaskContent limits title and note edits (Q-T2): the creator, the assignee, the list owner,
// an office admin of the task's office and a super admin.
func canEditTaskContent(actor Actor, task taskAccess) bool {
	if actor.IsSuperAdmin() {
		return true
	}
	if actor.Role == "office_admin" && actor.CanAccessOffice(task.OfficeID) {
		return true
	}
	return isSameUser(task.CreatedBy, actor) || isSameUser(task.AssigneeID, actor) || isSameUser(task.ListOwnerID, actor)
}

func updateManualTask(r *http.Request, tx pgx.Tx, actor Actor, taskID uuid.UUID, version int64, req updateTaskRequest) (taskMutationResponse, error) {
	var access taskAccess
	var timezone string
	var cur taskState
	var dueAt *time.Time
	var currentVersion int64
	err := tx.QueryRow(r.Context(), `
		select t.office_id, o.timezone_name, t.assignee_id, t.created_by, l.owner_id, t.title, t.note, t.status::text, t.in_progress,
		  t.due_at, to_char(t.due_time,'HH24:MI'), t.version
		from public.tasks t
		join public.offices o on o.id = t.office_id
		left join public.task_lists l on l.id = t.list_id
		where t.id=$1 and t.entity_type is null and t.entity_id is null and t.source='manual' and t.task_type='manual'
		for update of t
	`, taskID).Scan(&access.OfficeID, &timezone, &access.AssigneeID, &access.CreatedBy, &access.ListOwnerID, &cur.Title, &cur.Note, &cur.Status, &cur.InProgress,
		&dueAt, &cur.DueTime, &currentVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return taskMutationResponse{}, errTaskNotFound
	}
	if err != nil {
		return taskMutationResponse{}, err
	}
	if !canManageTask(actor, access) {
		return taskMutationResponse{}, errTaskOfficeDenied
	}
	if currentVersion != version {
		return taskMutationResponse{}, errTaskVersion
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return taskMutationResponse{}, err
	}
	if dueAt != nil {
		date := dueAt.In(location).Format(taskDateLayout)
		cur.DueDate = &date
	}
	next, effect, fields := applyTaskPatch(cur, req)
	if len(fields) > 0 {
		return taskMutationResponse{}, &taskFieldsError{fields: fields}
	}
	if effect.ContentChanged && !canEditTaskContent(actor, access) {
		return taskMutationResponse{}, errTaskContentDenied
	}
	var assigneeID *uuid.UUID
	if req.AssigneeID != nil && (access.AssigneeID == nil || *access.AssigneeID != *req.AssigneeID) {
		var active bool
		if err := tx.QueryRow(r.Context(), `select exists(select 1 from public.profiles p join public.user_office_memberships m on m.user_id=p.id where p.id=$1 and p.is_active=true and p.role<>'super_admin' and m.office_id=$2)`, *req.AssigneeID, access.OfficeID).Scan(&active); err != nil {
			return taskMutationResponse{}, err
		}
		if !active {
			return taskMutationResponse{}, errTaskAssigneeInvalid
		}
		assigneeID = req.AssigneeID
	}
	nextDue, err := parseTaskDue(next.DueDate, next.DueTime, location)
	if err != nil {
		return taskMutationResponse{}, err
	}
	var nextTime *string
	if nextDue != nil {
		nextTime = next.DueTime
	}
	var response taskMutationResponse
	err = tx.QueryRow(r.Context(), `
		update public.tasks set
		  status=$2::public.task_status, in_progress=$3, title=$4, note=$5, due_at=$6, due_time=$7::time,
		  assignee_id=coalesce($8::uuid, assignee_id),
		  completed_at=case when $2<>'done' then null when status='done' then completed_at else now() end,
		  completed_by=case when $2<>'done' then null::uuid when status='done' then completed_by else $9::uuid end,
		  canceled_at=case when $2<>'canceled' then null when status='canceled' then canceled_at else now() end,
		  canceled_by=case when $2<>'canceled' then null::uuid when status='canceled' then canceled_by else $9::uuid end,
		  updated_at=now(), updated_by=$9, version=version+1
		where id=$1 and version=$10 and entity_type is null and entity_id is null and source='manual' and task_type='manual'
		returning id,version,status::text,in_progress
	`, taskID, next.Status, next.InProgress, next.Title, next.Note, nextDue, nextTime, assigneeID, actor.ID, version).Scan(&response.ID, &response.Version, &response.Status, &response.InProgress)
	if errors.Is(err, pgx.ErrNoRows) {
		return taskMutationResponse{}, errTaskVersion
	}
	return response, err
}

type taskFieldsError struct{ fields map[string]string }

func (e *taskFieldsError) Error() string { return "task fields invalid" }

func validateCreateTask(req createTaskRequest) map[string]string {
	fields := map[string]string{}
	if req.OfficeID == uuid.Nil {
		fields["officeId"] = "Required"
	}
	if req.AssigneeID == uuid.Nil {
		fields["assigneeId"] = "Required"
	}
	title := strings.TrimSpace(req.Title)
	if title == "" || len([]rune(title)) > 1000 {
		fields["title"] = "Use 1–1000 characters"
	}
	if req.DueDate != nil {
		if value := strings.TrimSpace(*req.DueDate); value == "" {
			fields["dueDate"] = "Use YYYY-MM-DD or null"
		} else if _, err := time.Parse(taskDateLayout, value); err != nil {
			fields["dueDate"] = "Use YYYY-MM-DD or null"
		}
	}
	if req.DueTime != nil {
		if !validTaskTime(*req.DueTime) {
			fields["dueTime"] = "Use HH:MM or null"
		} else if req.DueDate == nil {
			fields["dueTime"] = "Set a date before the time"
		}
	}
	if req.Note != nil && len([]rune(*req.Note)) > taskNoteMax {
		fields["note"] = "Use at most 5000 characters"
	}
	if req.ListID != nil && *req.ListID == uuid.Nil {
		fields["listId"] = "Invalid task list"
	}
	if req.Link != nil {
		switch {
		case req.ListID != nil:
			fields["link"] = "A task is linked to a list or to one entity, not both"
		case req.Link.ID == uuid.Nil:
			fields["link"] = "Required"
		case req.Link.Type != "lead" && req.Link.Type != "project" && req.Link.Type != "client":
			fields["link"] = "Use lead, project or client"
		case !taskLinkTypeAvailable(req.Link.Type):
			fields["link"] = "Links to " + req.Link.Type + "s are not available yet"
		}
	}
	return fields
}

const taskNoteMax = 5000

func validTaskTime(value string) bool {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) != 5 {
		return false
	}
	_, err := time.Parse("15:04", trimmed)
	return err == nil
}

// validateUpdateTask checks each supplied field on its own; the rules that tie fields together
// (in progress needs an open task, a time needs a date) live in applyTaskPatch.
func validateUpdateTask(req updateTaskRequest) map[string]string {
	fields := map[string]string{}
	if req.Status == "" && req.InProgress == nil && req.AssigneeID == nil && req.Title == nil && !req.Note.Set && !req.DueDate.Set && !req.DueTime.Set {
		fields["status"] = "Use open, done, or canceled"
		return fields
	}
	if req.Status != "" && !isTaskStatus(req.Status) {
		fields["status"] = "Use open, done, or canceled"
	}
	if req.AssigneeID != nil && *req.AssigneeID == uuid.Nil {
		fields["assigneeId"] = "Invalid assignee"
	}
	if req.Title != nil {
		if title := strings.TrimSpace(*req.Title); title == "" || len([]rune(title)) > 1000 {
			fields["title"] = "Use 1–1000 characters"
		}
	}
	if req.Note.Value != nil && len([]rune(*req.Note.Value)) > taskNoteMax {
		fields["note"] = "Use at most 5000 characters"
	}
	if req.DueDate.Value != nil {
		if _, err := time.Parse(taskDateLayout, strings.TrimSpace(*req.DueDate.Value)); err != nil {
			fields["dueDate"] = "Use YYYY-MM-DD or null"
		}
	}
	if req.DueTime.Value != nil && !validTaskTime(*req.DueTime.Value) {
		fields["dueTime"] = "Use HH:MM or null"
	}
	return fields
}

func isTaskStatus(value string) bool {
	return value == "open" || value == "done" || value == "canceled"
}

func parseTaskDueDate(value *string, location *time.Location) (*time.Time, error) {
	return parseTaskDue(value, nil, location)
}

// parseTaskDue turns an office-local date and optional time of day into the stored instant. A
// date alone is office-local midnight (the v1 convention); the time of day is stored separately
// in due_time so a task due exactly at 00:00 stays distinguishable from a date-only task.
func parseTaskDue(date *string, timeOfDay *string, location *time.Location) (*time.Time, error) {
	if date == nil {
		return nil, nil
	}
	layout, value := taskDateLayout, strings.TrimSpace(*date)
	if timeOfDay != nil {
		layout, value = taskDateLayout+" 15:04", value+" "+strings.TrimSpace(*timeOfDay)
	}
	parsed, err := time.ParseInLocation(layout, value, location)
	if err != nil {
		return nil, err
	}
	utc := parsed.UTC()
	return &utc, nil
}

func (s *Server) writeTaskMutationError(w http.ResponseWriter, r *http.Request, err error, fallbackCode string) {
	var fieldsErr *taskFieldsError
	switch {
	case errors.As(err, &fieldsErr):
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Task validation failed", fieldsErr.fields)
	case errors.Is(err, errTaskNotFound):
		s.writeError(w, r, http.StatusNotFound, "task_not_found", "Task not found", nil)
	case errors.Is(err, errTaskOfficeDenied):
		s.writeError(w, r, http.StatusForbidden, "office_forbidden", "Office access denied", nil)
	case errors.Is(err, errTaskContentDenied):
		s.writeError(w, r, http.StatusForbidden, "task_edit_forbidden", "Only the creator, the assignee, the list owner or an admin can edit the title and note", nil)
	case errors.Is(err, errTaskOfficeInvalid):
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid office", map[string]string{"officeId": "Office must be active"})
	case errors.Is(err, errTaskAssigneeInvalid):
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid assignee", map[string]string{"assigneeId": "Assignee must be active in this office"})
	case errors.Is(err, errTaskListInvalid):
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid task list", map[string]string{"listId": "Task list not found"})
	case errors.Is(err, errTaskLinkInvalid):
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid link", map[string]string{"link": "Lead not found in this office"})
	case errors.Is(err, errTaskVersion):
		s.writeError(w, r, http.StatusConflict, "version_conflict", "Task was changed by another user", nil)
	default:
		s.logger.Error("task mutation failed", "error", err, "code", fallbackCode, "request_id", requestID(r.Context()))
		s.writeError(w, r, http.StatusInternalServerError, fallbackCode, "Could not save task", nil)
	}
}
