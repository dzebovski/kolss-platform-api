package crmapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CRM v2 projects (task P1): read side and creation. Mutations live in projects_mutations.go.
//
//	POST /v1/leads/{leadId}/project   create a project from a lead (lead → v2 status `project`)
//	GET  /v1/projects                 list (filters q, managerId, officeId, status; keyset paging)
//	GET  /v1/projects/facets          total + per-status counts for the list chips
//	GET  /v1/projects/{projectId}     card: project, contract, payments, permissions
//	GET  /v1/projects/{projectId}/timeline

type projectHeader struct {
	ID                 uuid.UUID
	LeadID             uuid.UUID
	OfficeID           uuid.UUID
	ManagerID          *uuid.UUID
	Status             string
	StatusBeforeCancel *string
}

const projectHeaderSelect = `
	select p.id, p.lead_id, p.office_id, p.assigned_to, p.status, p.status_before_cancel
	from public.projects p
	where p.id = $1`

func scanProjectHeader(row pgx.Row) (projectHeader, error) {
	var h projectHeader
	err := row.Scan(&h.ID, &h.LeadID, &h.OfficeID, &h.ManagerID, &h.Status, &h.StatusBeforeCancel)
	return h, err
}

// projectDetailSQL builds the card JSON. $1 = project id, $2 = whether the viewer may edit it.
// Money stays numeric all the way (jsonb keeps 2 decimals exactly).
const projectDetailSQL = `
	select jsonb_build_object(
		'id', p.id,
		'leadId', p.lead_id,
		'officeId', p.office_id,
		'code', l.reference_id,
		'status', p.status,
		'projectType', p.project_type,
		'eventDate', p.event_date,
		'eventTime', to_char(p.event_time, 'HH24:MI'),
		'statusDetails', p.status_details,
		'manager', case when p.assigned_to is null then null
			else jsonb_build_object('id', p.assigned_to, 'name', coalesce(m.display_name, '')) end,
		'client', jsonb_build_object(
			'name', coalesce(l.name, ''), 'phone', l.phone, 'email', l.email, 'cityRegion', l.city_region),
		'createdAt', p.created_at,
		'createdByName', coalesce(cb.display_name, ''),
		'statusChangedAt', p.status_changed_at,
		'lastActivityAt', p.last_activity_at,
		'cancellation', case when p.cancelled_at is null then null else jsonb_build_object(
			'cancelledAt', p.cancelled_at,
			'cancelledBy', p.cancelled_by,
			'cancelledByName', coalesce(xb.display_name, ''),
			'reasons', to_jsonb(p.cancel_reasons),
			'comment', p.cancel_comment,
			'statusBeforeCancel', p.status_before_cancel) end,
		'contract', (
			select jsonb_build_object(
				'id', c.id,
				'number', c.number,
				'signedOn', c.signed_on,
				'totalAmount', c.total_amount,
				'currency', c.currency,
				'paidAmount', paid.total,
				'remainingAmount', c.total_amount - paid.total,
				'file', (select jsonb_build_object('id', f.id, 'fileName', f.file_name, 'sizeBytes', f.size_bytes)
					from public.project_files f where f.id = c.file_id and f.status = 'ready'))
			from public.project_contracts c
			cross join lateral (
				select coalesce(sum(pp.amount), 0) as total from public.project_payments pp where pp.project_id = p.id
			) paid
			where c.project_id = p.id),
		'payments', coalesce((
			select jsonb_agg(jsonb_build_object(
				'id', pp.id,
				'amount', pp.amount,
				'paidOn', pp.paid_on,
				'note', pp.note,
				'createdAt', pp.created_at,
				'createdByName', coalesce(pb.display_name, ''),
				'file', (select jsonb_build_object('id', f.id, 'fileName', f.file_name, 'sizeBytes', f.size_bytes)
					from public.project_files f where f.id = pp.file_id and f.status = 'ready'))
				order by pp.paid_on, pp.created_at)
			from public.project_payments pp
			left join public.profiles pb on pb.id = pp.created_by
			where pp.project_id = p.id), '[]'::jsonb),
		'permissions', jsonb_build_object('canEdit', $2::boolean)
	)
	from public.projects p
	join public.leads l on l.id = p.lead_id
	left join public.profiles m on m.id = p.assigned_to
	left join public.profiles cb on cb.id = p.created_by
	left join public.profiles xb on xb.id = p.cancelled_by
	where p.id = $1`

func (s *Server) projectDetailJSON(ctx context.Context, q appointmentQuerier, actor Actor, h projectHeader) (json.RawMessage, error) {
	var raw []byte
	if err := q.QueryRow(ctx, projectDetailSQL, h.ID, actor.CanEditProject(h.OfficeID, h.ManagerID)).Scan(&raw); err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// loadReadableProject loads the header and enforces office access; it writes the error response
// itself and reports whether the caller may continue.
func (s *Server) loadReadableProject(w http.ResponseWriter, r *http.Request, actor Actor) (projectHeader, bool) {
	id, err := uuid.Parse(r.PathValue("projectId"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid project id", nil)
		return projectHeader{}, false
	}
	h, err := scanProjectHeader(s.pool.QueryRow(r.Context(), projectHeaderSelect, id))
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "project_not_found", "Project not found", nil)
		return projectHeader{}, false
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "project_load_failed", "Could not load project", nil)
		return projectHeader{}, false
	}
	if !actor.CanAccessOffice(h.OfficeID) {
		s.writeError(w, r, http.StatusForbidden, "office_forbidden", "Office access denied", nil)
		return projectHeader{}, false
	}
	return h, true
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	h, ok := s.loadReadableProject(w, r, actor)
	if !ok {
		return
	}
	raw, err := s.projectDetailJSON(r.Context(), s.pool, actor, h)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "project_load_failed", "Could not load project", nil)
		return
	}
	writeJSON(w, http.StatusOK, raw)
}

func (s *Server) handleListProjectTimeline(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	h, ok := s.loadReadableProject(w, r, actor)
	if !ok {
		return
	}
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = min(parsed, 200)
		}
	}
	rows, err := s.pool.Query(r.Context(), `
		select jsonb_build_object(
			'id', e.id,
			'type', e.event_type,
			'actor', case when e.actor_id is null then null
				else jsonb_build_object('id', e.actor_id, 'name', coalesce(a.display_name, '')) end,
			'oldValue', e.old_value,
			'newValue', e.new_value,
			'comment', e.comment,
			'createdAt', e.created_at)
		from public.project_events e
		left join public.profiles a on a.id = e.actor_id
		where e.project_id = $1
		order by e.created_at desc, e.id desc
		limit $2
	`, h.ID, limit)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "project_timeline_failed", "Could not load the timeline", nil)
		return
	}
	defer rows.Close()
	items := []json.RawMessage{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "project_timeline_failed", "Could not load the timeline", nil)
			return
		}
		items = append(items, json.RawMessage(raw))
	}
	if err := rows.Err(); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "project_timeline_failed", "Could not load the timeline", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// ---- list ----------------------------------------------------------------------------------

type projectListFilterError struct {
	status  int
	code    string
	message string
	fields  map[string]string
}

// projectListWhere builds the WHERE clause; skip names a filter to leave out (facets count each
// status group without the status filter itself).
func projectListWhere(actor Actor, query map[string][]string, skip string) ([]string, []any, *projectListFilterError) {
	get := func(key string) string {
		if values := query[key]; len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
		return ""
	}
	where := []string{"true"}
	args := []any{}
	addArg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if !actor.IsSuperAdmin() {
		ids := make([]uuid.UUID, 0, len(actor.OfficeIDs))
		for id := range actor.OfficeIDs {
			ids = append(ids, id)
		}
		where = append(where, "p.office_id = any("+addArg(ids)+"::uuid[])")
	}
	if raw := get("officeId"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil || !actor.CanAccessOffice(id) {
			return nil, nil, &projectListFilterError{http.StatusForbidden, "office_forbidden", "Office access denied", nil}
		}
		where = append(where, "p.office_id = "+addArg(id))
	}
	if raw := get("managerId"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, nil, &projectListFilterError{http.StatusBadRequest, "validation_error", "Invalid manager", nil}
		}
		where = append(where, "p.assigned_to = "+addArg(id))
	}
	if raw := get("status"); raw != "" && skip != "status" {
		known := map[string]struct{}{}
		for _, status := range projectStatuses {
			known[status] = struct{}{}
		}
		values := splitQueryValues(raw)
		for _, value := range values {
			if _, ok := known[value]; !ok {
				return nil, nil, &projectListFilterError{http.StatusBadRequest, "validation_error", "Invalid status filter", map[string]string{"status": "Unknown status"}}
			}
		}
		if len(values) > 0 {
			where = append(where, "p.status = any("+addArg(values)+"::text[])")
		}
	}
	if raw := get("q"); raw != "" {
		like := addArg("%" + raw + "%")
		where = append(where, "(l.name ilike "+like+" or l.phone ilike "+like+" or l.reference_id ilike "+like+")")
	}
	return where, args, nil
}

const projectListFrom = `
	from public.projects p
	join public.leads l on l.id = p.lead_id
	left join public.profiles m on m.id = p.assigned_to`

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	limit := 30
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = min(parsed, 100)
		}
	}
	where, args, filterErr := projectListWhere(actor, r.URL.Query(), "")
	if filterErr != nil {
		s.writeError(w, r, filterErr.status, filterErr.code, filterErr.message, filterErr.fields)
		return
	}
	addArg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("cursor")); raw != "" {
		cursor, err := decodeLeadCursor(raw)
		if err != nil {
			s.writeError(w, r, http.StatusBadRequest, "invalid_cursor", "Invalid cursor", nil)
			return
		}
		createdArg := addArg(cursor.CreatedAt)
		idArg := addArg(cursor.ID)
		where = append(where, "(p.created_at, p.id) < ("+createdArg+", "+idArg+")")
	}
	args = append(args, limit+1)
	rows, err := s.pool.Query(r.Context(), `
		select jsonb_build_object(
			'id', p.id,
			'leadId', p.lead_id,
			'code', l.reference_id,
			'clientName', coalesce(l.name, ''),
			'products', to_jsonb(l.products),
			'status', p.status,
			'projectType', p.project_type,
			'eventDate', p.event_date,
			'eventTime', to_char(p.event_time, 'HH24:MI'),
			'manager', case when p.assigned_to is null then null
				else jsonb_build_object('id', p.assigned_to, 'name', coalesce(m.display_name, '')) end,
			'createdAt', p.created_at,
			'lastComment', (
				select jsonb_build_object('text', e.comment, 'at', e.created_at)
				from public.project_events e
				where e.project_id = p.id and e.comment is not null
				order by e.created_at desc, e.id desc limit 1)),
			p.created_at, p.id`+projectListFrom+`
		where `+strings.Join(where, " and ")+`
		order by p.created_at desc, p.id desc
		limit $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "projects_load_failed", "Could not load projects", nil)
		return
	}
	defer rows.Close()
	items := make([]json.RawMessage, 0, limit)
	var nextCursor string
	var last leadCursor
	for rows.Next() {
		var raw []byte
		var createdAt time.Time
		var id uuid.UUID
		if err := rows.Scan(&raw, &createdAt, &id); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "projects_load_failed", "Could not load projects", nil)
			return
		}
		if len(items) == limit {
			nextCursor = encodeLeadCursor(last)
			break
		}
		items = append(items, json.RawMessage(raw))
		last = leadCursor{CreatedAt: createdAt, ID: id}
	}
	if err := rows.Err(); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "projects_load_failed", "Could not load projects", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": nextCursor})
}

// handleProjectFacets returns the chip counts of the Projects list: the total with every filter
// and the per-status counts without the status filter.
func (s *Server) handleProjectFacets(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	query := r.URL.Query()
	where, args, filterErr := projectListWhere(actor, query, "")
	if filterErr != nil {
		s.writeError(w, r, filterErr.status, filterErr.code, filterErr.message, filterErr.fields)
		return
	}
	var total int
	if err := s.pool.QueryRow(r.Context(), `select count(*) `+projectListFrom+` where `+strings.Join(where, " and "), args...).Scan(&total); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "projects_load_failed", "Could not load project counts", nil)
		return
	}
	where, args, filterErr = projectListWhere(actor, query, "status")
	if filterErr != nil {
		s.writeError(w, r, filterErr.status, filterErr.code, filterErr.message, filterErr.fields)
		return
	}
	rows, err := s.pool.Query(r.Context(), `select p.status, count(*) `+projectListFrom+` where `+strings.Join(where, " and ")+` group by p.status`, args...)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "projects_load_failed", "Could not load project counts", nil)
		return
	}
	defer rows.Close()
	statuses := map[string]int{}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "projects_load_failed", "Could not load project counts", nil)
			return
		}
		statuses[status] = count
	}
	if err := rows.Err(); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "projects_load_failed", "Could not load project counts", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "status": statuses})
}

// ---- create --------------------------------------------------------------------------------

type createProjectRequest struct {
	ProjectType *string `json:"projectType"`
	ManagerID   *string `json:"managerId"`
}

// beginIdempotentTx starts a transaction that owns the request's Idempotency-Key. When the key
// was already used with the same body the stored response is replayed and the transaction is
// closed (ok = false, response written). Otherwise the caller must defer tx.Rollback.
func (s *Server) beginIdempotentTx(w http.ResponseWriter, r *http.Request, actor Actor, operation string, hashParts ...string) (tx pgx.Tx, key string, ok bool) {
	key = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		s.writeError(w, r, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required", nil)
		return nil, "", false
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "project_update_failed", "Could not update the project", nil)
		return nil, "", false
	}
	hash := sha256.Sum256([]byte(strings.Join(hashParts, "|")))
	cached, status, body, err := claimIdempotency(r, tx, actor.ID, operation, key, hex.EncodeToString(hash[:]))
	if err != nil {
		_ = tx.Rollback(r.Context())
		s.writeError(w, r, http.StatusConflict, "idempotency_conflict", "Idempotency key was already used for another request", nil)
		return nil, "", false
	}
	if cached {
		if err := tx.Commit(r.Context()); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "project_update_failed", "Could not update the project", nil)
			return nil, "", false
		}
		writeJSON(w, status, body)
		return nil, "", false
	}
	return tx, key, true
}

// finishIdempotentTx stores the response for replays and commits.
func (s *Server) finishIdempotentTx(r *http.Request, tx pgx.Tx, actor Actor, operation, key string, status int, body json.RawMessage) error {
	if _, err := tx.Exec(r.Context(), `
		update public.api_idempotency_keys
		set response_status=$4, response_body=$5::jsonb
		where actor_id=$1 and operation=$2 and idempotency_key=$3
	`, actor.ID, operation, key, status, []byte(body)); err != nil {
		return err
	}
	return tx.Commit(r.Context())
}

func (s *Server) insertProjectEvent(ctx context.Context, tx pgx.Tx, projectID, actorID uuid.UUID, eventType string, oldValue, newValue map[string]any, comment *string) error {
	if _, err := tx.Exec(ctx, `
		insert into public.project_events (project_id, actor_id, event_type, old_value, new_value, comment)
		values ($1,$2,$3,$4,$5,$6)
	`, projectID, actorID, eventType, oldValue, newValue, comment); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `update public.projects set last_activity_at = now() where id = $1`, projectID)
	return err
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	leadID, err := uuid.Parse(r.PathValue("leadId"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid lead id", nil)
		return
	}
	var req createProjectRequest
	if err := decodeJSON(w, r, 4*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid project", nil)
		return
	}
	fields := map[string]string{}
	var projectType *string
	if text := trimmedText(req.ProjectType); text != "" {
		if _, ok := projectTypeFirstStatus[text]; !ok {
			fields["projectType"] = "Must be express, measure, or contract"
		}
		projectType = &text
	}
	var managerID *uuid.UUID
	if text := trimmedText(req.ManagerID); text != "" {
		id, err := uuid.Parse(text)
		if err != nil {
			fields["managerId"] = "Must be a valid id"
		}
		managerID = &id
	}
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid project", fields)
		return
	}

	const operation = "lead.project.create"
	tx, key, ok := s.beginIdempotentTx(w, r, actor, operation,
		leadID.String(), trimmedText(req.ProjectType), trimmedText(req.ManagerID))
	if !ok {
		return
	}
	defer tx.Rollback(r.Context())

	var lead struct {
		OfficeID          uuid.UUID
		AssignedTo        *uuid.UUID
		ResponsibleID     *uuid.UUID
		LeadProjectType   *string
		ClientStatus      string
		ArchivedAt        *time.Time
		ConvertedProject  *uuid.UUID
		ExistingProjectID *uuid.UUID
	}
	err = tx.QueryRow(r.Context(), `
		select l.office_id, l.assigned_to, l.responsible_manager_id, l.project_type, l.client_status,
		  l.archived_at, l.converted_project_id,
		  (select p.id from public.projects p where p.lead_id = l.id)
		from public.leads l where l.id = $1 for update of l
	`, leadID).Scan(&lead.OfficeID, &lead.AssignedTo, &lead.ResponsibleID, &lead.LeadProjectType, &lead.ClientStatus,
		&lead.ArchivedAt, &lead.ConvertedProject, &lead.ExistingProjectID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && lead.ArchivedAt != nil) {
		s.writeError(w, r, http.StatusNotFound, "lead_not_found", "Lead not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "project_create_failed", "Could not create the project", nil)
		return
	}
	if !actor.CanEditLead(lead.OfficeID) {
		s.writeError(w, r, http.StatusForbidden, "lead_edit_forbidden", "Lead editing is not allowed", nil)
		return
	}
	if lead.ConvertedProject != nil || lead.ExistingProjectID != nil {
		s.writeError(w, r, http.StatusConflict, "project_exists", "A project was already created from this lead", nil)
		return
	}
	if lead.ClientStatus == "closed_lost" {
		s.writeError(w, r, http.StatusConflict, "lead_terminal", "Reopen the lead before creating a project", nil)
		return
	}

	if projectType == nil {
		projectType = lead.LeadProjectType
	}
	if managerID == nil {
		managerID = lead.ResponsibleID
	}
	if managerID == nil {
		managerID = lead.AssignedTo
	}
	if managerID == nil {
		managerID = &actor.ID
	}
	if err := validateAppointmentManager(r, tx, *managerID, lead.OfficeID); err != nil {
		if errors.Is(err, errAppointmentManagerInvalid) {
			s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid project", map[string]string{
				"managerId": "Must be an active member of the lead's office",
			})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "project_create_failed", "Could not create the project", nil)
		return
	}

	status := projectFirstStatus(projectType)
	var projectID uuid.UUID
	if err := tx.QueryRow(r.Context(), `
		insert into public.projects (lead_id, office_id, status, project_type, assigned_to, created_by)
		values ($1,$2,$3,$4,$5,$6)
		returning id
	`, leadID, lead.OfficeID, status, projectType, managerID, actor.ID).Scan(&projectID); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "project_create_failed", "Could not create the project", nil)
		return
	}
	// The lead keeps its data (the project card reads it) and turns read-only: v2 status `project`.
	if _, err := tx.Exec(r.Context(), `
		update public.leads
		set converted_project_id = $2,
		    v2_status = 'project',
		    v2_status_changed_at = now(),
		    project_type = $3,
		    responsible_manager_id = $4,
		    version = version + 1
		where id = $1
	`, leadID, projectID, projectType, managerID); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "project_create_failed", "Could not create the project", nil)
		return
	}
	newValue := map[string]any{"status": status, "managerId": managerID.String()}
	if projectType != nil {
		newValue["projectType"] = *projectType
	}
	if err := s.insertProjectEvent(r.Context(), tx, projectID, actor.ID, "created", nil, newValue, nil); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "project_create_failed", "Could not create the project", nil)
		return
	}

	h := projectHeader{ID: projectID, LeadID: leadID, OfficeID: lead.OfficeID, ManagerID: managerID, Status: status}
	raw, err := s.projectDetailJSON(r.Context(), tx, actor, h)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "project_create_failed", "Could not create the project", nil)
		return
	}
	if err := s.finishIdempotentTx(r, tx, actor, operation, key, http.StatusCreated, raw); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "project_create_failed", "Could not create the project", nil)
		return
	}
	writeJSON(w, http.StatusCreated, raw)
}
