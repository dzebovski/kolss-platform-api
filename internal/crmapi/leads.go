package crmapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dzebovski/kolss-platform-api/internal/notifications"
)

const leadJSONExpression = `
	to_jsonb(l) || jsonb_build_object(
		'offices', to_jsonb(o),
		'profiles', case when p.id is null then null else jsonb_build_object('display_name', p.display_name) end,
		'first_contact_attempt', (
			select jsonb_build_object(
				'result', a.result,
				'comment', a.comment,
				'created_at', a.created_at,
				'manager_id', a.manager_id
			)
			from public.lead_contact_attempts a
			where a.lead_id = l.id
			order by a.created_at asc
			limit 1
		),
		'reactivated_at', (
			select e.created_at
			from public.lead_events e
			where e.lead_id = l.id and e.event_type in ('activated', 'reopened', 'lead_reopened')
			order by e.created_at desc
			limit 1
		),
		'contract', coalesce((
			select jsonb_build_object(
				'contract_number', c.contract_number,
				'amount', c.amount,
				'currency', c.currency,
				'signed_at', c.signed_at
			)
			from public.lead_contracts c
			where c.lead_id = l.id
				and c.status = 'signed'
				and c.contract_number is not null
				and c.amount is not null
				and c.currency is not null
			order by c.signed_at desc nulls last, c.created_at desc
			limit 1
		), (
			select jsonb_build_object(
				'contract_number', e.new_value->>'contract_number',
				'amount', (e.new_value->>'amount')::numeric,
				'currency', e.new_value->>'currency',
				'signed_at', coalesce(e.new_value->>'signed_at', e.created_at::text)
			)
			from public.lead_events e
			where e.lead_id = l.id
				and e.event_type in ('successful', 'contract_signed')
				and e.new_value ? 'amount'
			order by e.created_at desc
			limit 1
		)),
		'latest_timeline_comment', (
			select jsonb_build_object(
				'comment', e.comment,
				'created_at', e.created_at,
				'event_type', e.event_type,
				'event_category', e.event_category,
				'status_code', e.status_code,
				'new_value', e.new_value
			)
			from public.lead_events e
			where e.lead_id = l.id
				and e.comment is not null
				and btrim(e.comment) <> ''
			order by e.created_at desc
			limit 1
		),
		'markers', coalesce((
			select jsonb_agg(jsonb_build_object(
				'kind', m.kind,
				'actor_id', m.actor_id,
				'actor_name', coalesce(mp.display_name, ''),
				'marked_at', m.marked_at
			) order by m.kind)
			from public.lead_markers m
			left join public.profiles mp on mp.id = m.actor_id
			where m.lead_id = l.id
		), '[]'::jsonb)
	)
`

type leadCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

func encodeLeadCursor(cursor leadCursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursor.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + cursor.ID.String()))
}

func decodeLeadCursor(raw string) (leadCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return leadCursor{}, err
	}
	parts := strings.Split(string(decoded), "|")
	if len(parts) != 2 {
		return leadCursor{}, errors.New("invalid cursor")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return leadCursor{}, err
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return leadCursor{}, err
	}
	return leadCursor{CreatedAt: createdAt, ID: id}, nil
}

func (s *Server) handleListLeads(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > 100 {
		limit = 100
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
		where = append(where, "l.office_id = any("+addArg(ids)+"::uuid[])")
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("officeId")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil || !actor.CanAccessOffice(id) {
			s.writeError(w, r, http.StatusForbidden, "office_forbidden", "Office access denied", nil)
			return
		}
		where = append(where, "l.office_id = "+addArg(id))
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("assignedTo")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid assigned manager", nil)
			return
		}
		where = append(where, "l.assigned_to = "+addArg(id))
	}
	for _, filter := range []struct {
		query  string
		column string
	}{
		{"source", "l.source_system"},
		{"workflow", "l.workflow_status"},
	} {
		if raw := strings.TrimSpace(r.URL.Query().Get(filter.query)); raw != "" {
			where = append(where, filter.column+" = "+addArg(raw))
		}
	}
	statusFilters := []struct {
		query   string
		column  string
		allowed map[string]bool
	}{
		{"callStatus", "l.call_status", map[string]bool{"reached": true, "no_answer": true, "callback_requested": true}},
		{"clientStatus", "l.client_status", map[string]bool{"new_lead": true, "showroom_invited": true, "calculation_in_progress": true, "thinking": true, "closed_lost": true, "contract_signed": true}},
	}
	for _, filter := range statusFilters {
		if raw := strings.TrimSpace(r.URL.Query().Get(filter.query)); raw != "" {
			if !filter.allowed[raw] {
				s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid status filter", map[string]string{filter.query: "Unknown status"})
				return
			}
			where = append(where, filter.column+" = "+addArg(raw))
		}
	}
	if search := strings.TrimSpace(r.URL.Query().Get("search")); search != "" {
		value := "%" + search + "%"
		placeholder := addArg(value)
		where = append(where, `(coalesce(l.name, '') ilike `+placeholder+` or coalesce(l.phone, '') ilike `+placeholder+` or coalesce(l.email, '') ilike `+placeholder+`)`)
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("days")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 3660 {
			s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid days filter", map[string]string{"days": "Must be an integer from 1 to 3660"})
			return
		}
		where = append(where, "coalesce(l.source_created_at, l.created_at) >= now() - make_interval(days => "+addArg(parsed)+")")
	}
	archived := strings.TrimSpace(r.URL.Query().Get("archived"))
	switch archived {
	case "only":
		if !actor.IsSuperAdmin() {
			s.writeError(w, r, http.StatusForbidden, "archive_forbidden", "Archived leads are restricted", nil)
			return
		}
		where = append(where, "l.archived_at is not null")
	case "all":
		if !actor.IsSuperAdmin() {
			where = append(where, "l.archived_at is null")
		}
	default:
		where = append(where, "l.archived_at is null")
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("cursor")); raw != "" {
		cursor, err := decodeLeadCursor(raw)
		if err != nil {
			s.writeError(w, r, http.StatusBadRequest, "invalid_cursor", "Invalid cursor", nil)
			return
		}
		createdArg := addArg(cursor.CreatedAt)
		idArg := addArg(cursor.ID)
		where = append(where, "(l.created_at, l.id) < ("+createdArg+", "+idArg+")")
	}
	args = append(args, limit+1)
	query := `select ` + leadJSONExpression + `, l.created_at, l.id
		from public.leads l
		join public.offices o on o.id = l.office_id
		left join public.profiles p on p.id = l.assigned_to
		where ` + strings.Join(where, " and ") + `
		order by l.created_at desc, l.id desc
		limit $` + strconv.Itoa(len(args))
	rows, err := s.pool.Query(r.Context(), query, args...)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "leads_load_failed", "Could not load leads", nil)
		return
	}
	defer rows.Close()
	items := make([]json.RawMessage, 0, limit)
	var nextCursor string
	var lastIncluded leadCursor
	for rows.Next() {
		var raw []byte
		var createdAt time.Time
		var id uuid.UUID
		if err := rows.Scan(&raw, &createdAt, &id); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "leads_load_failed", "Could not load leads", nil)
			return
		}
		if len(items) == limit {
			nextCursor = encodeLeadCursor(lastIncluded)
			break
		}
		items = append(items, json.RawMessage(raw))
		lastIncluded = leadCursor{CreatedAt: createdAt, ID: id}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": nextCursor})
}

func (s *Server) leadJSON(r *http.Request, leadID uuid.UUID, includeArchived bool) (json.RawMessage, uuid.UUID, error) {
	actor, _ := actorFromContext(r.Context())
	where := "l.id = $1"
	args := []any{leadID}
	if !includeArchived || !actor.IsSuperAdmin() {
		where += " and l.archived_at is null"
	}
	if !actor.IsSuperAdmin() {
		ids := make([]uuid.UUID, 0, len(actor.OfficeIDs))
		for id := range actor.OfficeIDs {
			ids = append(ids, id)
		}
		where += " and l.office_id = any($2::uuid[])"
		args = append(args, ids)
	}
	var raw []byte
	var officeID uuid.UUID
	err := s.pool.QueryRow(r.Context(), `select `+leadJSONExpression+`, l.office_id
		from public.leads l join public.offices o on o.id=l.office_id
		left join public.profiles p on p.id=l.assigned_to where `+where, args...).Scan(&raw, &officeID)
	return json.RawMessage(raw), officeID, err
}

func (s *Server) handleGetLead(w http.ResponseWriter, r *http.Request) {
	leadID, err := uuid.Parse(r.PathValue("leadId"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid lead id", nil)
		return
	}
	lead, _, err := s.leadJSON(r, leadID, true)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "lead_not_found", "Lead not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "lead_load_failed", "Could not load lead", nil)
		return
	}
	relations, err := s.loadLeadRelations(r, leadID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "lead_relations_load_failed", "Could not load lead history", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lead": lead, "relations": relations})
}

func (s *Server) loadLeadRelations(r *http.Request, leadID uuid.UUID) (map[string][]json.RawMessage, error) {
	queries := []struct {
		key string
		sql string
	}{
		{"contactAttempts", `select to_jsonb(a) || jsonb_build_object('profiles', jsonb_build_object('display_name', p.display_name)) from public.lead_contact_attempts a left join public.profiles p on p.id=a.manager_id where a.lead_id=$1 order by a.created_at desc`},
		{"showroomVisits", `select to_jsonb(v) from public.lead_showroom_visits v where v.lead_id=$1 order by v.created_at desc`},
		{"contracts", `select to_jsonb(c) from public.lead_contracts c where c.lead_id=$1 order by c.created_at desc`},
		{"events", `select to_jsonb(e) || jsonb_build_object('profiles', case when p.id is null then null else jsonb_build_object('display_name', p.display_name) end) from public.lead_events e left join public.profiles p on p.id=e.actor_id where e.lead_id=$1 order by e.created_at desc`},
	}
	out := make(map[string][]json.RawMessage, len(queries))
	for _, item := range queries {
		rows, err := s.pool.Query(r.Context(), item.sql, leadID)
		if err != nil {
			return nil, err
		}
		values := []json.RawMessage{}
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return nil, err
			}
			values = append(values, json.RawMessage(raw))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		out[item.key] = values
	}
	return out, nil
}

type createLeadRequest struct {
	OfficeID        uuid.UUID `json:"officeId"`
	Source          string    `json:"source"`
	Name            string    `json:"name"`
	Phone           string    `json:"phone"`
	Email           *string   `json:"email"`
	CityRegion      string    `json:"cityRegion"`
	ProductInterest string    `json:"productInterest"`
	EstimatedBudget *float64  `json:"estimatedBudget"`
	InitialMessage  string    `json:"initialMessage"`
}

func (s *Server) handleCreateLead(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !requireIdempotencyKey(s, w, r) {
		return
	}
	var req createLeadRequest
	if err := decodeJSON(w, r, 64*1024, &req); err != nil || req.OfficeID == uuid.Nil || strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Phone) == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid lead data", nil)
		return
	}
	if !actor.CanAccessOffice(req.OfficeID) {
		s.writeError(w, r, http.StatusForbidden, "office_forbidden", "Office access denied", nil)
		return
	}
	sourceSystem, sourceChannel := createSource(req.Source)
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "lead_create_failed", "Could not create lead", nil)
		return
	}
	defer tx.Rollback(r.Context())
	var leadID uuid.UUID
	var raw []byte
	externalID := "crm:" + uuid.NewSHA1(uuid.NameSpaceURL, []byte(actor.ID.String()+"|"+idempotencyKey)).String()
	inserted := true
	err = tx.QueryRow(r.Context(), `
		with inserted as (
			insert into public.leads (
				office_id, source_system, source_channel, external_lead_id,
				source_created_at,
				name, phone, email, city_region, product_interest, estimated_budget, order_comment
			) values ($1,$2,$3,$4,now(),$5,$6,$7,$8,$9,$10,$11)
			on conflict (source_system,external_lead_id) do nothing
			returning *
		)
		select id, to_jsonb(inserted) from inserted
	`, req.OfficeID, sourceSystem, sourceChannel, externalID, strings.TrimSpace(req.Name), strings.TrimSpace(req.Phone), cleanPtr(req.Email), clean(req.CityRegion), clean(req.ProductInterest), req.EstimatedBudget, clean(req.InitialMessage)).Scan(&leadID, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		inserted = false
		err = tx.QueryRow(r.Context(), `select id,to_jsonb(l) from public.leads l where source_system=$1 and external_lead_id=$2`, sourceSystem, externalID).Scan(&leadID, &raw)
	}
	if err == nil && inserted {
		_, err = tx.Exec(r.Context(), `insert into public.lead_events (lead_id, actor_id, event_type, new_value) values ($1,$2,'created',$3)`, leadID, actor.ID, map[string]any{"source": req.Source, "source_system": sourceSystem, "source_channel": sourceChannel})
		var officeCode string
		if err == nil {
			err = tx.QueryRow(r.Context(), `select code from public.offices where id = $1`, req.OfficeID).Scan(&officeCode)
		}
		if err == nil {
			name := strings.TrimSpace(req.Name)
			phone := strings.TrimSpace(req.Phone)
			if err := s.outbox.Enqueue(r.Context(), tx, notifications.LeadInfo{
				ID:              leadID,
				Name:            &name,
				Phone:           &phone,
				Email:           cleanPtr(req.Email),
				ClientInfo:      clean(req.InitialMessage),
				ProductInterest: clean(req.ProductInterest),
				OfficeCode:      officeCode,
				SourceSystem:    sourceSystem,
			}); err != nil {
				s.writeError(w, r, http.StatusInternalServerError, "notification_enqueue_failed", "Could not enqueue lead notification", nil)
				return
			}
		}
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "lead_create_failed", "Could not create lead", nil)
		return
	}
	if inserted && s.notificationWaker != nil {
		s.notificationWaker.Wake()
	}
	status := http.StatusCreated
	if !inserted {
		status = http.StatusOK
	}
	writeJSON(w, status, json.RawMessage(raw))
}

type updateLeadRequest struct {
	Name            string   `json:"name"`
	Phone           string   `json:"phone"`
	Email           *string  `json:"email"`
	CityRegion      string   `json:"cityRegion"`
	ProductInterest string   `json:"productInterest"`
	EstimatedBudget *float64 `json:"estimatedBudget"`
	InitialMessage  string   `json:"initialMessage"`
	AssignedToID    *string  `json:"assignedToId"`
	EditedFields    []string `json:"editedFields"`
}

func (s *Server) handleUpdateLead(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	leadID, err := uuid.Parse(r.PathValue("leadId"))
	version, hasVersion := parseIfMatch(r)
	if err != nil || !hasVersion {
		s.writeError(w, r, http.StatusPreconditionRequired, "version_required", "If-Match lead version is required", nil)
		return
	}
	var req updateLeadRequest
	if err := decodeJSON(w, r, 64*1024, &req); err != nil || strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Phone) == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid lead data", nil)
		return
	}
	var officeID uuid.UUID
	if err := s.pool.QueryRow(r.Context(), `select office_id from public.leads where id=$1 and archived_at is null`, leadID).Scan(&officeID); err != nil {
		s.writeError(w, r, http.StatusNotFound, "lead_not_found", "Lead not found", nil)
		return
	}
	if !actor.CanEditLead(officeID) {
		s.writeError(w, r, http.StatusForbidden, "lead_edit_forbidden", "Lead editing is not allowed", nil)
		return
	}
	var assignedTo *uuid.UUID
	if req.AssignedToID != nil && strings.TrimSpace(*req.AssignedToID) != "" {
		id, parseErr := uuid.Parse(*req.AssignedToID)
		if parseErr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid assigned manager", nil)
			return
		}
		assignedTo = &id
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "lead_update_failed", "Could not update lead", nil)
		return
	}
	defer tx.Rollback(r.Context())
	var nextVersion int64
	err = tx.QueryRow(r.Context(), `
		update public.leads set name=$3, phone=$4, email=$5, city_region=$6,
		product_interest=$7, estimated_budget=$8, order_comment=$9, assigned_to=$10,
		updated_at=now(), version=version+1
		where id=$1 and version=$2 and archived_at is null
		returning version
	`, leadID, version, strings.TrimSpace(req.Name), strings.TrimSpace(req.Phone), cleanPtr(req.Email), clean(req.CityRegion), clean(req.ProductInterest), req.EstimatedBudget, clean(req.InitialMessage), assignedTo).Scan(&nextVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusConflict, "version_conflict", "Lead was changed by another user", nil)
		return
	}
	if err == nil && len(req.EditedFields) > 0 {
		_, err = tx.Exec(r.Context(), `insert into public.lead_events (lead_id,actor_id,event_type,new_value) values ($1,$2,'lead_edited',$3)`, leadID, actor.ID, map[string]any{"fields": req.EditedFields, "edited_by_name": actor.DisplayName})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "lead_update_failed", "Could not update lead", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": nextVersion})
}

type eventUpdateRequest struct {
	Comment string `json:"comment"`
}

func (s *Server) handleUpdateEvent(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	if !s.requireSuperAdmin(w, r) {
		return
	}
	leadID, leadErr := uuid.Parse(r.PathValue("leadId"))
	eventID, eventErr := uuid.Parse(r.PathValue("eventId"))
	var req eventUpdateRequest
	if leadErr != nil || eventErr != nil || decodeJSON(w, r, 32*1024, &req) != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid history event", nil)
		return
	}
	var exists bool
	if err := s.pool.QueryRow(r.Context(), `select exists(select 1 from public.leads where id=$1 and archived_at is null)`, leadID).Scan(&exists); err != nil || !exists {
		s.writeError(w, r, http.StatusNotFound, "lead_not_found", "Lead not found", nil)
		return
	}
	editedByName := ""
	if actor.DisplayName != nil {
		editedByName = *actor.DisplayName
	}
	command, err := s.pool.Exec(r.Context(), `
		update public.lead_events
		set comment=nullif(trim($3),''),
		    new_value = coalesce(new_value, '{}'::jsonb) || jsonb_build_object(
		      'edit_audit', jsonb_build_object(
		        'fields', jsonb_build_array('message'),
		        'edited_at', now(),
		        'edited_by', $4::text,
		        'edited_by_name', $5::text))
		where id=$1 and lead_id=$2
	`, eventID, leadID, req.Comment, actor.ID.String(), editedByName)
	if err != nil || command.RowsAffected() == 0 {
		s.writeError(w, r, http.StatusNotFound, "event_not_found", "History event not found", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changedFields": []string{"message"}})
}

func (s *Server) handleDeleteEvent(w http.ResponseWriter, r *http.Request) {
	if !s.requireSuperAdmin(w, r) {
		return
	}
	leadID, leadErr := uuid.Parse(r.PathValue("leadId"))
	eventID, eventErr := uuid.Parse(r.PathValue("eventId"))
	if leadErr != nil || eventErr != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid history event", nil)
		return
	}
	command, err := s.pool.Exec(r.Context(), `
		delete from public.lead_events where id=$1 and lead_id=$2
	`, eventID, leadID)
	if err != nil || command.RowsAffected() == 0 {
		s.writeError(w, r, http.StatusNotFound, "event_not_found", "History event not found", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleArchiveLead(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	leadID, err := uuid.Parse(r.PathValue("leadId"))
	if err != nil || !requireIdempotencyKey(s, w, r) {
		if err != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid lead id", nil)
		}
		return
	}
	var officeID uuid.UUID
	if err := s.pool.QueryRow(r.Context(), `select office_id from public.leads where id=$1`, leadID).Scan(&officeID); err != nil {
		s.writeError(w, r, http.StatusNotFound, "lead_not_found", "Lead not found", nil)
		return
	}
	if !actor.CanEditLead(officeID) {
		s.writeError(w, r, http.StatusForbidden, "archive_forbidden", "Lead archiving is not allowed", nil)
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "archive_failed", "Could not archive lead", nil)
		return
	}
	defer tx.Rollback(r.Context())
	var changed bool
	err = tx.QueryRow(r.Context(), `with changed as (update public.leads set archived_at=now(),archived_by=$2,version=version+1,updated_at=now() where id=$1 and archived_at is null returning 1) select exists(select 1 from changed)`, leadID, actor.ID).Scan(&changed)
	if err == nil && changed {
		_, err = tx.Exec(r.Context(), `insert into public.lead_events (lead_id,actor_id,event_type,new_value) values ($1,$2,'archived',jsonb_build_object('archived',true))`, leadID, actor.ID)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "archive_failed", "Could not archive lead", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"archived": true})
}

func (s *Server) handleRestoreLead(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	if !actor.IsSuperAdmin() {
		s.writeError(w, r, http.StatusForbidden, "restore_forbidden", "Only super admin can restore leads", nil)
		return
	}
	leadID, err := uuid.Parse(r.PathValue("leadId"))
	if err != nil || !requireIdempotencyKey(s, w, r) {
		if err != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid lead id", nil)
		}
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "restore_failed", "Could not restore lead", nil)
		return
	}
	defer tx.Rollback(r.Context())
	result, err := tx.Exec(r.Context(), `update public.leads set archived_at=null,archived_by=null,version=version+1,updated_at=now() where id=$1 and archived_at is not null`, leadID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "restore_failed", "Could not restore lead", nil)
		return
	}
	if result.RowsAffected() > 0 {
		_, err = tx.Exec(r.Context(), `insert into public.lead_events (lead_id,actor_id,event_type,new_value) values ($1,$2,'restored',jsonb_build_object('archived',false))`, leadID, actor.ID)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "restore_failed", "Could not restore lead", nil)
		return
	}
	if result.RowsAffected() == 0 {
		var exists bool
		_ = s.pool.QueryRow(r.Context(), `select exists(select 1 from public.leads where id=$1)`, leadID).Scan(&exists)
		if !exists {
			s.writeError(w, r, http.StatusNotFound, "lead_not_found", "Lead not found", nil)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"restored": true})
}

func (s *Server) handleDeleteLead(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	if !actor.IsSuperAdmin() {
		s.writeError(w, r, http.StatusForbidden, "delete_forbidden", "Only super admin can permanently delete leads", nil)
		return
	}
	leadID, err := uuid.Parse(r.PathValue("leadId"))
	if err != nil || !requireIdempotencyKey(s, w, r) {
		if err != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid lead id", nil)
		}
		return
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "delete_failed", "Could not delete lead", nil)
		return
	}
	defer tx.Rollback(r.Context())

	var archivedAt *time.Time
	err = tx.QueryRow(r.Context(), `select archived_at from public.leads where id=$1 for update`, leadID).Scan(&archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "lead_not_found", "Lead not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "delete_failed", "Could not delete lead", nil)
		return
	}
	if archivedAt == nil {
		s.writeError(w, r, http.StatusConflict, "not_archived", "Only archived leads can be permanently deleted", nil)
		return
	}

	if _, err := tx.Exec(r.Context(), `update public.leads set converted_project_id=null where id=$1`, leadID); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "delete_failed", "Could not delete lead", nil)
		return
	}
	if _, err := tx.Exec(r.Context(), `delete from public.projects where lead_id=$1`, leadID); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "delete_failed", "Could not delete lead", nil)
		return
	}
	result, err := tx.Exec(r.Context(), `delete from public.leads where id=$1 and archived_at is not null`, leadID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "delete_failed", "Could not delete lead", nil)
		return
	}
	if result.RowsAffected() == 0 {
		s.writeError(w, r, http.StatusConflict, "not_archived", "Only archived leads can be permanently deleted", nil)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "delete_failed", "Could not delete lead", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func requireIdempotencyKey(s *Server, w http.ResponseWriter, r *http.Request) bool {
	if strings.TrimSpace(r.Header.Get("Idempotency-Key")) == "" {
		s.writeError(w, r, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required", nil)
		return false
	}
	return true
}

func createSource(source string) (string, string) {
	switch source {
	case "facebook":
		return "meta_lead_ads", "facebook"
	case "google":
		return "google_ads", "google"
	default:
		return "manual", "manual"
	}
}

func clean(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func cleanPtr(value *string) *string {
	if value == nil {
		return nil
	}
	return clean(*value)
}
