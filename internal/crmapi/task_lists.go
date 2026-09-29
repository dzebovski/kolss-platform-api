package crmapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Task lists (CRM v2, T1). A list groups tasks around a project such as an exhibition. Lists are
// visible to every signed-in CRM user; only the owner (or a super admin) edits one. Members are
// not stored: they are the assignees of the list's tasks.

const (
	taskListNameMax        = 200
	taskListDescriptionMax = 2000
	taskListDatesMax       = 100
)

// taskListPalette holds the colours the Tasks boards use for lists. The API assigns them in turn
// when a list is created without a colour.
var taskListPalette = []string{"#6d4ab8", "#0e6b64", "#a15c07", "#2563a8"}

var taskListColorPattern = regexp.MustCompile(`^#[0-9a-f]{6}$`)

// optional keeps the difference between an absent JSON field and an explicit null.
type optional[T any] struct {
	Set   bool
	Value *T
}

func (o *optional[T]) UnmarshalJSON(data []byte) error {
	o.Set = true
	if string(data) == "null" {
		o.Value = nil
		return nil
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	o.Value = &value
	return nil
}

type createTaskListRequest struct {
	Name        string     `json:"name"`
	Description *string    `json:"description"`
	DatesText   *string    `json:"datesText"`
	OwnerID     *uuid.UUID `json:"ownerId"`
	Color       *string    `json:"color"`
}

type updateTaskListRequest struct {
	Name        *string          `json:"name"`
	Description optional[string] `json:"description"`
	DatesText   optional[string] `json:"datesText"`
	OwnerID     *uuid.UUID       `json:"ownerId"`
	Color       *string          `json:"color"`
}

type taskListMember struct {
	ID          uuid.UUID `json:"id"`
	DisplayName string    `json:"displayName"`
}

type taskListResponse struct {
	ID           uuid.UUID        `json:"id"`
	Name         string           `json:"name"`
	Description  *string          `json:"description"`
	DatesText    *string          `json:"datesText"`
	OwnerID      *uuid.UUID       `json:"ownerId"`
	OwnerName    *string          `json:"ownerName"`
	Color        string           `json:"color"`
	Version      int64            `json:"version"`
	TaskCount    int              `json:"taskCount"`
	DoneCount    int              `json:"doneCount"`
	OverdueCount int              `json:"overdueCount"`
	Members      []taskListMember `json:"members"`
	CreatedAt    time.Time        `json:"createdAt"`
	UpdatedAt    time.Time        `json:"updatedAt"`
}

var (
	errTaskListNotFound     = errors.New("task list not found")
	errTaskListForbidden    = errors.New("task list forbidden")
	errTaskListOwnerInvalid = errors.New("task list owner invalid")
	errTaskListVersion      = errors.New("task list version conflict")
)

// nextTaskListColor picks the palette colour for the n-th list (0-based).
func nextTaskListColor(existing int) string {
	if existing < 0 {
		existing = 0
	}
	return taskListPalette[existing%len(taskListPalette)]
}

// canEditTaskList: only the list owner, or a super admin, edits a list (Q-T1).
func canEditTaskList(actor Actor, ownerID *uuid.UUID) bool {
	return actor.IsSuperAdmin() || (ownerID != nil && *ownerID == actor.ID)
}

func validateCreateTaskList(req createTaskListRequest) map[string]string {
	fields := map[string]string{}
	if name := strings.TrimSpace(req.Name); name == "" || len([]rune(name)) > taskListNameMax {
		fields["name"] = "Use 1–200 characters"
	}
	if req.Description != nil && len([]rune(*req.Description)) > taskListDescriptionMax {
		fields["description"] = "Use at most 2000 characters"
	}
	if req.DatesText != nil && len([]rune(*req.DatesText)) > taskListDatesMax {
		fields["datesText"] = "Use at most 100 characters"
	}
	if req.OwnerID != nil && *req.OwnerID == uuid.Nil {
		fields["ownerId"] = "Invalid owner"
	}
	if req.Color != nil && !taskListColorPattern.MatchString(*req.Color) {
		fields["color"] = "Use a lowercase #rrggbb colour"
	}
	return fields
}

func validateUpdateTaskList(req updateTaskListRequest) map[string]string {
	fields := map[string]string{}
	if req.Name == nil && !req.Description.Set && !req.DatesText.Set && req.OwnerID == nil && req.Color == nil {
		fields["body"] = "Send at least one field"
		return fields
	}
	if req.Name != nil {
		if name := strings.TrimSpace(*req.Name); name == "" || len([]rune(name)) > taskListNameMax {
			fields["name"] = "Use 1–200 characters"
		}
	}
	if req.Description.Value != nil && len([]rune(*req.Description.Value)) > taskListDescriptionMax {
		fields["description"] = "Use at most 2000 characters"
	}
	if req.DatesText.Value != nil && len([]rune(*req.DatesText.Value)) > taskListDatesMax {
		fields["datesText"] = "Use at most 100 characters"
	}
	if req.OwnerID != nil && *req.OwnerID == uuid.Nil {
		fields["ownerId"] = "Invalid owner"
	}
	if req.Color != nil && !taskListColorPattern.MatchString(*req.Color) {
		fields["color"] = "Use a lowercase #rrggbb colour"
	}
	return fields
}

// blankToNil stores an empty or whitespace-only optional text as null.
func blankToNil(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

const taskListSelectSQL = `
	select l.id, l.name, l.description, l.dates_text, l.owner_id, nullif(op.display_name,''), l.color, l.version,
	  coalesce(s.total,0), coalesce(s.done,0), coalesce(s.overdue,0), coalesce(m.members,'[]'::jsonb),
	  l.created_at, l.updated_at
	from public.task_lists l
	left join public.profiles op on op.id = l.owner_id
	left join lateral (
	  select count(*) total,
	    count(*) filter (where t.status='done') done,
	    count(*) filter (where t.status='open' and t.due_at is not null
	      and (t.due_at at time zone o.timezone_name)::date < (now() at time zone o.timezone_name)::date) overdue
	  from public.tasks t join public.offices o on o.id = t.office_id
	  where t.list_id = l.id and t.status <> 'canceled'
	) s on true
	left join lateral (
	  select jsonb_agg(jsonb_build_object('id', p.id, 'displayName', coalesce(p.display_name,'')) order by p.display_name nulls last, p.id) members
	  from public.profiles p
	  where p.id in (select t.assignee_id from public.tasks t where t.list_id = l.id and t.status <> 'canceled' and t.assignee_id is not null)
	) m on true
	where ($1::uuid is null or l.id = $1)
	order by l.created_at, l.id`

func scanTaskLists(rows pgx.Rows) ([]taskListResponse, error) {
	defer rows.Close()
	items := []taskListResponse{}
	for rows.Next() {
		var item taskListResponse
		var members []byte
		if err := rows.Scan(&item.ID, &item.Name, &item.Description, &item.DatesText, &item.OwnerID, &item.OwnerName, &item.Color, &item.Version,
			&item.TaskCount, &item.DoneCount, &item.OverdueCount, &members, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.Members = []taskListMember{}
		if err := json.Unmarshal(members, &item.Members); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Server) handleListTaskLists(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), taskListSelectSQL, nil)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_lists_load_failed", "Could not load task lists", nil)
		return
	}
	items, err := scanTaskLists(rows)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_lists_load_failed", "Could not load task lists", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleGetTaskList(w http.ResponseWriter, r *http.Request) {
	listID, err := uuid.Parse(r.PathValue("listId"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid task list id", nil)
		return
	}
	rows, err := s.pool.Query(r.Context(), taskListSelectSQL, listID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_list_load_failed", "Could not load task list", nil)
		return
	}
	items, err := scanTaskLists(rows)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_list_load_failed", "Could not load task list", nil)
		return
	}
	if len(items) == 0 {
		s.writeError(w, r, http.StatusNotFound, "task_list_not_found", "Task list not found", nil)
		return
	}
	writeJSON(w, http.StatusOK, items[0])
}

func (s *Server) handleCreateTaskList(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		s.writeError(w, r, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required", nil)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid task list body", nil)
		return
	}
	var req createTaskListRequest
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid task list body", nil)
		return
	}
	if fields := validateCreateTaskList(req); len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Task list validation failed", fields)
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_list_create_failed", "Could not create task list", nil)
		return
	}
	defer tx.Rollback(r.Context())
	hash := sha256.Sum256(body)
	cached, status, cachedBody, err := claimIdempotency(r, tx, actor.ID, "task_list.create", key, hex.EncodeToString(hash[:]))
	if err != nil {
		s.writeError(w, r, http.StatusConflict, "idempotency_conflict", "Idempotency key was already used for another request", nil)
		return
	}
	if cached {
		if err := tx.Commit(r.Context()); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "task_list_create_failed", "Could not create task list", nil)
			return
		}
		writeJSON(w, status, cachedBody)
		return
	}
	ownerID := actor.ID
	if req.OwnerID != nil {
		ownerID = *req.OwnerID
	}
	if err := requireTaskListOwner(r, tx, ownerID); err != nil {
		s.writeTaskListError(w, r, err, "task_list_create_failed")
		return
	}
	var existing int
	if err := tx.QueryRow(r.Context(), `select count(*) from public.task_lists`).Scan(&existing); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_list_create_failed", "Could not create task list", nil)
		return
	}
	color := nextTaskListColor(existing)
	if req.Color != nil {
		color = *req.Color
	}
	var listID uuid.UUID
	if err := tx.QueryRow(r.Context(), `
		insert into public.task_lists (name, description, dates_text, owner_id, color, created_by)
		values ($1,$2,$3,$4,$5,$6) returning id
	`, strings.TrimSpace(req.Name), blankToNil(req.Description), blankToNil(req.DatesText), ownerID, color, actor.ID).Scan(&listID); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_list_create_failed", "Could not create task list", nil)
		return
	}
	rows, err := tx.Query(r.Context(), taskListSelectSQL, listID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_list_create_failed", "Could not create task list", nil)
		return
	}
	items, err := scanTaskLists(rows)
	if err != nil || len(items) != 1 {
		s.writeError(w, r, http.StatusInternalServerError, "task_list_create_failed", "Could not create task list", nil)
		return
	}
	raw, _ := json.Marshal(items[0])
	if _, err := tx.Exec(r.Context(), `update public.api_idempotency_keys set response_status=$4, response_body=$5::jsonb where actor_id=$1 and operation=$2 and idempotency_key=$3`, actor.ID, "task_list.create", key, http.StatusCreated, raw); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_list_create_failed", "Could not create task list", nil)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_list_create_failed", "Could not create task list", nil)
		return
	}
	writeJSON(w, http.StatusCreated, items[0])
}

func (s *Server) handleUpdateTaskList(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	listID, err := uuid.Parse(r.PathValue("listId"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid task list id", nil)
		return
	}
	version, ok := parseIfMatch(r)
	if !ok {
		s.writeError(w, r, http.StatusPreconditionRequired, "version_required", "If-Match task list version is required", nil)
		return
	}
	var req updateTaskListRequest
	if err := decodeJSON(w, r, 64*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid task list body", nil)
		return
	}
	if fields := validateUpdateTaskList(req); len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Task list validation failed", fields)
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_list_update_failed", "Could not update task list", nil)
		return
	}
	defer tx.Rollback(r.Context())
	var ownerID *uuid.UUID
	var currentVersion int64
	err = tx.QueryRow(r.Context(), `select owner_id, version from public.task_lists where id=$1 for update`, listID).Scan(&ownerID, &currentVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeTaskListError(w, r, errTaskListNotFound, "task_list_update_failed")
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_list_update_failed", "Could not update task list", nil)
		return
	}
	if !canEditTaskList(actor, ownerID) {
		s.writeTaskListError(w, r, errTaskListForbidden, "task_list_update_failed")
		return
	}
	if currentVersion != version {
		s.writeTaskListError(w, r, errTaskListVersion, "task_list_update_failed")
		return
	}
	if req.OwnerID != nil {
		if err := requireTaskListOwner(r, tx, *req.OwnerID); err != nil {
			s.writeTaskListError(w, r, err, "task_list_update_failed")
			return
		}
	}
	var name, color *string
	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		name = &trimmed
	}
	color = req.Color
	if _, err := tx.Exec(r.Context(), `
		update public.task_lists set
		  name = coalesce($2, name),
		  description = case when $3::boolean then $4 else description end,
		  dates_text = case when $5::boolean then $6 else dates_text end,
		  owner_id = coalesce($7, owner_id),
		  color = coalesce($8, color),
		  version = version + 1
		where id = $1
	`, listID, name, req.Description.Set, blankToNil(req.Description.Value), req.DatesText.Set, blankToNil(req.DatesText.Value), req.OwnerID, color); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_list_update_failed", "Could not update task list", nil)
		return
	}
	rows, err := tx.Query(r.Context(), taskListSelectSQL, listID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_list_update_failed", "Could not update task list", nil)
		return
	}
	items, err := scanTaskLists(rows)
	if err != nil || len(items) != 1 {
		s.writeError(w, r, http.StatusInternalServerError, "task_list_update_failed", "Could not update task list", nil)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "task_list_update_failed", "Could not update task list", nil)
		return
	}
	writeJSON(w, http.StatusOK, items[0])
}

// requireTaskListOwner accepts an active profile that works in at least one office, or a super
// admin (who has no office memberships).
func requireTaskListOwner(r *http.Request, tx pgx.Tx, ownerID uuid.UUID) error {
	var valid bool
	if err := tx.QueryRow(r.Context(), `
		select exists(
		  select 1 from public.profiles p
		  where p.id = $1 and p.is_active = true
		    and (p.role = 'super_admin' or exists (select 1 from public.user_office_memberships m where m.user_id = p.id))
		)
	`, ownerID).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return errTaskListOwnerInvalid
	}
	return nil
}

func (s *Server) writeTaskListError(w http.ResponseWriter, r *http.Request, err error, fallbackCode string) {
	switch {
	case errors.Is(err, errTaskListNotFound):
		s.writeError(w, r, http.StatusNotFound, "task_list_not_found", "Task list not found", nil)
	case errors.Is(err, errTaskListForbidden):
		s.writeError(w, r, http.StatusForbidden, "task_list_forbidden", "Only the list owner can edit this list", nil)
	case errors.Is(err, errTaskListOwnerInvalid):
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid owner", map[string]string{"ownerId": "Owner must be an active CRM user"})
	case errors.Is(err, errTaskListVersion):
		s.writeError(w, r, http.StatusConflict, "version_conflict", "Task list was changed by another user", nil)
	default:
		s.logger.Error("task list mutation failed", "error", err, "code", fallbackCode, "request_id", requestID(r.Context()))
		s.writeError(w, r, http.StatusInternalServerError, fallbackCode, "Could not save task list", nil)
	}
}
