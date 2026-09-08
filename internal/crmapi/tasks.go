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

type createTaskRequest struct {
	OfficeID   uuid.UUID `json:"officeId"`
	AssigneeID uuid.UUID `json:"assigneeId"`
	Title      string    `json:"title"`
	DueDate    *string   `json:"dueDate"`
}

type updateTaskRequest struct {
	Status string `json:"status"`
}

type taskMutationResponse struct {
	ID      uuid.UUID `json:"id"`
	Version int64     `json:"version"`
	Status  string    `json:"status"`
}

var (
	errTaskNotFound        = errors.New("task not found")
	errTaskOfficeDenied    = errors.New("task office denied")
	errTaskOfficeInvalid   = errors.New("task office invalid")
	errTaskAssigneeInvalid = errors.New("task assignee invalid")
	errTaskVersion         = errors.New("task version conflict")
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
	dueAt, err := parseTaskDueDate(req.DueDate, location)
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
	var response taskMutationResponse
	err = tx.QueryRow(r.Context(), `
		insert into public.tasks (entity_type,entity_id,office_id,assignee_id,title,due_at,priority,source,task_type,status,created_by,updated_by)
		values (null,null,$1,$2,$3,$4,'high'::public.task_priority,'manual'::public.task_source,'manual'::public.task_type,'open'::public.task_status,$5,$5)
		returning id,version,status::text
	`, req.OfficeID, req.AssigneeID, strings.TrimSpace(req.Title), dueAt, actor.ID).Scan(&response.ID, &response.Version, &response.Status)
	return response, err
}

func updateManualTask(r *http.Request, tx pgx.Tx, actor Actor, taskID uuid.UUID, version int64, req updateTaskRequest) (taskMutationResponse, error) {
	var officeID uuid.UUID
	err := tx.QueryRow(r.Context(), `select office_id from public.tasks where id=$1 and entity_type is null and entity_id is null and source='manual' and task_type='manual'`, taskID).Scan(&officeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return taskMutationResponse{}, errTaskNotFound
	}
	if err != nil {
		return taskMutationResponse{}, err
	}
	if !actor.CanManageTasks(officeID) {
		return taskMutationResponse{}, errTaskOfficeDenied
	}
	var response taskMutationResponse
	err = tx.QueryRow(r.Context(), `
		update public.tasks set status=$2::public.task_status,updated_at=now(),updated_by=$3,
		completed_at=case when $2='done' then now() else null end,completed_by=case when $2='done' then $3::uuid else null::uuid end,
		canceled_at=case when $2='canceled' then now() else null end,canceled_by=case when $2='canceled' then $3::uuid else null::uuid end,version=version+1
		where id=$1 and version=$4 and entity_type is null and entity_id is null and source='manual' and task_type='manual' returning id,version,status::text
	`, taskID, req.Status, actor.ID, version).Scan(&response.ID, &response.Version, &response.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return taskMutationResponse{}, errTaskVersion
	}
	return response, err
}

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
	return fields
}

func validateUpdateTask(req updateTaskRequest) map[string]string {
	if isTaskStatus(req.Status) {
		return nil
	}
	return map[string]string{"status": "Use open, done, or canceled"}
}
func isTaskStatus(value string) bool {
	return value == "open" || value == "done" || value == "canceled"
}
func parseTaskDueDate(value *string, location *time.Location) (*time.Time, error) {
	if value == nil {
		return nil, nil
	}
	date, err := time.ParseInLocation(taskDateLayout, strings.TrimSpace(*value), location)
	if err != nil {
		return nil, err
	}
	utc := date.UTC()
	return &utc, nil
}

func (s *Server) writeTaskMutationError(w http.ResponseWriter, r *http.Request, err error, fallbackCode string) {
	switch {
	case errors.Is(err, errTaskNotFound):
		s.writeError(w, r, http.StatusNotFound, "task_not_found", "Task not found", nil)
	case errors.Is(err, errTaskOfficeDenied):
		s.writeError(w, r, http.StatusForbidden, "office_forbidden", "Office access denied", nil)
	case errors.Is(err, errTaskOfficeInvalid):
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid office", map[string]string{"officeId": "Office must be active"})
	case errors.Is(err, errTaskAssigneeInvalid):
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid assignee", map[string]string{"assigneeId": "Assignee must be active in this office"})
	case errors.Is(err, errTaskVersion):
		s.writeError(w, r, http.StatusConflict, "version_conflict", "Task was changed by another user", nil)
	default:
		s.logger.Error("task mutation failed", "error", err, "code", fallbackCode, "request_id", requestID(r.Context()))
		s.writeError(w, r, http.StatusInternalServerError, fallbackCode, "Could not save task", nil)
	}
}
