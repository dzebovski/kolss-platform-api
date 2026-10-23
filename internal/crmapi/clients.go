package crmapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CRM v2 clients (task CL1). A client is the contact that links leads and projects; its code is
// the code of its first lead. Clients are created by the database when a lead arrives (see
// migration 20261023120000_clients.sql), never by an endpoint.
//
//	GET   /v1/clients                 list (q, officeId, filter all|with_projects|leads_only; keyset paging)
//	GET   /v1/clients/facets          chip counts for the list
//	GET   /v1/clients/match           returning-client hint for Create lead (phone, then email)
//	GET   /v1/clients/{clientId}      card: contacts, stats, leads, projects, duplicate candidates
//	PATCH /v1/clients/{clientId}      Edit client (contact fields; optional If-Match version)
//	POST  /v1/clients/{clientId}/merge  merge a duplicate into the older client (admins only)

type clientHeader struct {
	ID         uuid.UUID
	OfficeID   uuid.UUID
	Code       string
	MergedInto *uuid.UUID
}

const clientHeaderSelect = `
	select c.id, c.office_id, c.code, c.merged_into_id
	from public.clients c
	where c.id = $1`

func scanClientHeader(row pgx.Row) (clientHeader, error) {
	var h clientHeader
	err := row.Scan(&h.ID, &h.OfficeID, &h.Code, &h.MergedInto)
	return h, err
}

// clientManagerExpr is the manager column of the list (Q-CL6): the manager of the latest project,
// otherwise of the latest lead. @c is the clients alias.
const clientManagerExpr = `coalesce(
	(select p.assigned_to from public.projects p where p.client_id = @c.id order by p.created_at desc, p.id desc limit 1),
	(select l.assigned_to from public.leads l where l.client_id = @c.id and l.archived_at is null
		order by l.created_at desc, l.id desc limit 1))`

// clientSummaryExpr is the row shape shared by the list, the match hint and duplicate candidates.
const clientSummaryExpr = `jsonb_build_object(
	'id', @c.id,
	'code', @c.code,
	'name', @c.name,
	'phone', @c.phone,
	'secondPhone', @c.second_phone,
	'email', @c.email,
	'officeId', @c.office_id,
	'office', (select jsonb_build_object('id', o.id, 'code', o.code, 'nameUk', o.name_uk, 'namePl', o.name_pl)
		from public.offices o where o.id = @c.office_id),
	'createdAt', @c.created_at,
	'firstChannel', (select l.channel from public.leads l where l.client_id = @c.id order by l.created_at, l.id limit 1),
	'leadsCount', (select count(*) from public.leads l where l.client_id = @c.id and l.archived_at is null),
	'projectsCount', (select count(*) from public.projects p where p.client_id = @c.id),
	'manager', (select jsonb_build_object('id', m.id, 'name', coalesce(m.display_name, ''))
		from public.profiles m where m.id = ` + clientManagerExpr + `))`

func clientSQL(template, alias string) string { return strings.ReplaceAll(template, "@c", alias) }

// clientDuplicateMatch is true when candidate d shares a phone (either number) or the email with
// target c. phone / email are the individual conditions, used to name the reason.
const (
	clientDupPhone = `(d.phone_key in (c.phone_key, c.second_phone_key) or d.second_phone_key in (c.phone_key, c.second_phone_key))`
	clientDupEmail = `(d.email_key is not null and d.email_key = c.email_key)`
)

// clientCardSQL builds the card JSON. $1 client id, $2 canEdit, $3 canMerge, $4 the merged-away
// client id this card was reached through (null when requested directly).
var clientCardSQL = `
	select ` + clientSQL(clientSummaryExpr, "c") + ` || jsonb_build_object(
		'address', c.address,
		'language', c.language,
		'about', c.about,
		'version', c.version,
		'firstLeadId', (select l.id from public.leads l where l.client_id = c.id order by l.created_at, l.id limit 1),
		'stats', jsonb_build_object(
			'leadsCount', (select count(*) from public.leads l where l.client_id = c.id and l.archived_at is null),
			'projectsCount', (select count(*) from public.projects p where p.client_id = c.id),
			'contractValue', coalesce((
				select jsonb_agg(jsonb_build_object(
					'currency', t.currency, 'total', t.total, 'paid', t.paid, 'remaining', t.total - t.paid)
					order by t.currency)
				from (
					select ct.currency, sum(ct.total_amount) as total, sum(coalesce(pay.paid, 0)) as paid
					from public.projects p
					join public.project_contracts ct on ct.project_id = p.id
					left join lateral (
						select sum(pp.amount) as paid from public.project_payments pp where pp.project_id = p.id
					) pay on true
					where p.client_id = c.id and p.status <> 'cancelled'
					group by ct.currency
				) t), '[]'::jsonb)),
		'leads', coalesce((
			select jsonb_agg(jsonb_build_object(
				'id', x.id,
				'code', x.reference_id,
				'name', coalesce(x.name, ''),
				'createdAt', x.created_at,
				'channel', x.channel,
				'v2Status', x.v2_status,
				'rating', x.rating,
				'products', to_jsonb(x.products),
				'manager', case when x.assigned_to is null then null
					else jsonb_build_object('id', x.assigned_to, 'name', coalesce(xm.display_name, '')) end,
				'projectId', x.converted_project_id)
				order by x.created_at desc, x.id desc)
			from (
				select * from public.leads l
				where l.client_id = c.id and l.archived_at is null
				order by l.created_at desc, l.id desc limit 200
			) x
			left join public.profiles xm on xm.id = x.assigned_to), '[]'::jsonb),
		'projects', coalesce((
			select jsonb_agg(jsonb_build_object(
				'id', y.id,
				'leadId', y.lead_id,
				'code', yl.reference_id,
				'title', to_jsonb(yl.products),
				'status', y.status,
				'projectType', y.project_type,
				'createdAt', y.created_at,
				'manager', case when y.assigned_to is null then null
					else jsonb_build_object('id', y.assigned_to, 'name', coalesce(ym.display_name, '')) end,
				'contract', (
					select jsonb_build_object(
						'currency', ct.currency,
						'totalAmount', ct.total_amount,
						'paidAmount', coalesce((select sum(pp.amount) from public.project_payments pp where pp.project_id = y.id), 0))
					from public.project_contracts ct where ct.project_id = y.id))
				order by y.created_at desc, y.id desc)
			from (
				select * from public.projects p
				where p.client_id = c.id
				order by p.created_at desc, p.id desc limit 200
			) y
			join public.leads yl on yl.id = y.lead_id
			left join public.profiles ym on ym.id = y.assigned_to), '[]'::jsonb),
		'duplicates', coalesce((
			select jsonb_agg(` + clientSQL(clientSummaryExpr, "d") + ` || jsonb_build_object(
				'matchedBy', to_jsonb(array_remove(array[
					case when ` + clientDupPhone + ` then 'phone'::text end,
					case when ` + clientDupEmail + ` then 'email'::text end], null)))
				order by d.created_at, d.id)
			from public.clients d
			where d.office_id = c.office_id and d.id <> c.id and d.merged_into_id is null
				and (` + clientDupPhone + ` or ` + clientDupEmail + `)), '[]'::jsonb),
		'redirectedFrom', (select jsonb_build_object('id', rf.id, 'code', rf.code) from public.clients rf where rf.id = $4::uuid),
		'permissions', jsonb_build_object('canEdit', $2::boolean, 'canMerge', $3::boolean)
	)
	from public.clients c
	where c.id = $1`

func (s *Server) clientCardJSON(ctx context.Context, q appointmentQuerier, actor Actor, h clientHeader, redirectedFrom *uuid.UUID) (json.RawMessage, error) {
	var raw []byte
	if err := q.QueryRow(ctx, clientCardSQL, h.ID, actor.CanEditClient(h.OfficeID), actor.CanMergeClients(h.OfficeID), redirectedFrom).Scan(&raw); err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// loadReadableClient loads the header of the {clientId} client and enforces office access. A
// client that was merged away resolves to the client it was merged into (its code "redirects"),
// reported through redirectedFrom. It writes the error response itself.
func (s *Server) loadReadableClient(w http.ResponseWriter, r *http.Request, actor Actor) (h clientHeader, redirectedFrom *uuid.UUID, ok bool) {
	id, err := uuid.Parse(r.PathValue("clientId"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid client id", nil)
		return clientHeader{}, nil, false
	}
	requested := id
	for hops := 0; ; hops++ {
		h, err = scanClientHeader(s.pool.QueryRow(r.Context(), clientHeaderSelect, id))
		if errors.Is(err, pgx.ErrNoRows) {
			s.writeError(w, r, http.StatusNotFound, "client_not_found", "Client not found", nil)
			return clientHeader{}, nil, false
		}
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "client_load_failed", "Could not load client", nil)
			return clientHeader{}, nil, false
		}
		if !actor.CanAccessOffice(h.OfficeID) {
			s.writeError(w, r, http.StatusForbidden, "office_forbidden", "Office access denied", nil)
			return clientHeader{}, nil, false
		}
		if h.MergedInto == nil {
			break
		}
		if hops >= 10 {
			s.writeError(w, r, http.StatusInternalServerError, "client_load_failed", "Could not load client", nil)
			return clientHeader{}, nil, false
		}
		id = *h.MergedInto
	}
	if id != requested {
		redirectedFrom = &requested
	}
	return h, redirectedFrom, true
}

func (s *Server) handleGetClient(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	h, redirectedFrom, ok := s.loadReadableClient(w, r, actor)
	if !ok {
		return
	}
	raw, err := s.clientCardJSON(r.Context(), s.pool, actor, h, redirectedFrom)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "client_load_failed", "Could not load client", nil)
		return
	}
	writeJSON(w, http.StatusOK, raw)
}

// ---- list ----------------------------------------------------------------------------------

type clientListFilterError struct {
	status  int
	code    string
	message string
	fields  map[string]string
}

// clientListWhere builds the WHERE clause; skip names a filter to leave out (facets count each
// chip without the chip filter itself).
func clientListWhere(actor Actor, query url.Values, skip string) ([]string, []any, *clientListFilterError) {
	get := func(key string) string { return strings.TrimSpace(query.Get(key)) }
	where := []string{"c.merged_into_id is null"}
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
		where = append(where, "c.office_id = any("+addArg(ids)+"::uuid[])")
	}
	if raw := get("officeId"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil || !actor.CanAccessOffice(id) {
			return nil, nil, &clientListFilterError{http.StatusForbidden, "office_forbidden", "Office access denied", nil}
		}
		where = append(where, "c.office_id = "+addArg(id))
	}
	filter := get("filter")
	if !validClientFilter(filter) {
		return nil, nil, &clientListFilterError{http.StatusBadRequest, "validation_error", "Invalid filter",
			map[string]string{"filter": "Must be all, with_projects, or leads_only"}}
	}
	if skip != "filter" {
		switch filter {
		case clientFilterWithProjects:
			where = append(where, "exists (select 1 from public.projects p where p.client_id = c.id)")
		case clientFilterLeadsOnly:
			where = append(where, "not exists (select 1 from public.projects p where p.client_id = c.id)")
		}
	}
	if q := get("q"); q != "" {
		like := addArg("%" + escapeLike(q) + "%")
		cond := "c.name ilike " + like + " or c.code ilike " + like + " or c.phone ilike " + like + " or c.second_phone ilike " + like
		if digits := onlyDigits(q); len(digits) >= 3 {
			dl := addArg("%" + digits + "%")
			cond += " or regexp_replace(coalesce(c.phone, ''), '\\D', '', 'g') like " + dl +
				" or regexp_replace(coalesce(c.second_phone, ''), '\\D', '', 'g') like " + dl
		}
		where = append(where, "("+cond+")")
	}
	return where, args, nil
}

func (s *Server) handleListClients(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	limit := 30
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = min(parsed, 100)
		}
	}
	where, args, filterErr := clientListWhere(actor, r.URL.Query(), "")
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
		where = append(where, "(c.created_at, c.id) < ("+createdArg+", "+idArg+")")
	}
	args = append(args, limit+1)
	rows, err := s.pool.Query(r.Context(), `
		select `+clientSQL(clientSummaryExpr, "c")+`, c.created_at, c.id
		from public.clients c
		where `+strings.Join(where, " and ")+`
		order by c.created_at desc, c.id desc
		limit $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "clients_load_failed", "Could not load clients", nil)
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
			s.writeError(w, r, http.StatusInternalServerError, "clients_load_failed", "Could not load clients", nil)
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
		s.writeError(w, r, http.StatusInternalServerError, "clients_load_failed", "Could not load clients", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": nextCursor})
}

// handleClientFacets returns the chip counts of the Clients list: the total with every filter and
// the All / With projects / Leads only counts under the current search and office.
func (s *Server) handleClientFacets(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	query := r.URL.Query()
	where, args, filterErr := clientListWhere(actor, query, "")
	if filterErr != nil {
		s.writeError(w, r, filterErr.status, filterErr.code, filterErr.message, filterErr.fields)
		return
	}
	var total int
	if err := s.pool.QueryRow(r.Context(), `select count(*) from public.clients c where `+strings.Join(where, " and "), args...).Scan(&total); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "clients_load_failed", "Could not load client counts", nil)
		return
	}
	where, args, filterErr = clientListWhere(actor, query, "filter")
	if filterErr != nil {
		s.writeError(w, r, filterErr.status, filterErr.code, filterErr.message, filterErr.fields)
		return
	}
	var all, withProjects int
	if err := s.pool.QueryRow(r.Context(), `
		select count(*), count(*) filter (where exists (select 1 from public.projects p where p.client_id = c.id))
		from public.clients c where `+strings.Join(where, " and "), args...).Scan(&all, &withProjects); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "clients_load_failed", "Could not load client counts", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total": total,
		"filter": map[string]int{
			clientFilterAll:          all,
			clientFilterWithProjects: withProjects,
			clientFilterLeadsOnly:    all - withProjects,
		},
	})
}

// ---- match ---------------------------------------------------------------------------------

// handleMatchClient answers "is this phone / email a returning client?" for Create lead. The
// phone is tried first, then the email, the same order the database uses when a lead is created.
// officeId may be omitted only for a user with exactly one office.
func (s *Server) handleMatchClient(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	query := r.URL.Query()
	var officeID uuid.UUID
	if raw := strings.TrimSpace(query.Get("officeId")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid office", map[string]string{"officeId": "Must be a valid id"})
			return
		}
		officeID = id
	} else if !actor.IsSuperAdmin() && len(actor.OfficeIDs) == 1 {
		for id := range actor.OfficeIDs {
			officeID = id
		}
	} else {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Office is required", map[string]string{"officeId": "Required"})
		return
	}
	if !actor.CanAccessOffice(officeID) {
		s.writeError(w, r, http.StatusForbidden, "office_forbidden", "Office access denied", nil)
		return
	}
	phone := strings.TrimSpace(query.Get("phone"))
	email := strings.TrimSpace(query.Get("email"))

	find := func(condition string, value string) (json.RawMessage, error) {
		var raw []byte
		err := s.pool.QueryRow(r.Context(), `
			select `+clientSQL(clientSummaryExpr, "c")+`
			from public.clients c
			where c.office_id = $1 and c.merged_into_id is null and `+condition+`
			order by c.created_at, c.id
			limit 1`, officeID, value).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return json.RawMessage(raw), err
	}
	fail := func() {
		s.writeError(w, r, http.StatusInternalServerError, "client_match_failed", "Could not look up the client", nil)
	}
	if clientPhoneKey(phone) != "" {
		raw, err := find("(c.phone_key = public.client_phone_key($2) or c.second_phone_key = public.client_phone_key($2))", phone)
		if err != nil {
			fail()
			return
		}
		if raw != nil {
			writeJSON(w, http.StatusOK, map[string]any{"match": map[string]any{"matchedBy": "phone", "client": raw}})
			return
		}
	}
	if validClientEmail(email) {
		raw, err := find("c.email_key = public.client_email_key($2)", email)
		if err != nil {
			fail()
			return
		}
		if raw != nil {
			writeJSON(w, http.StatusOK, map[string]any{"match": map[string]any{"matchedBy": "email", "client": raw}})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"match": nil})
}

// ---- edit ----------------------------------------------------------------------------------

type clientRow struct {
	Name        string
	Phone       *string
	SecondPhone *string
	Email       *string
	Address     *string
	Language    *string
	About       *string
	Version     int64
}

func (c clientRow) contact() clientContact {
	return clientContact{Name: c.Name, Phone: c.Phone, SecondPhone: c.SecondPhone, Email: c.Email,
		Address: c.Address, Language: c.Language, About: c.About}
}

func (c clientRow) column(name string) *string {
	switch name {
	case "name":
		return &c.Name
	case "phone":
		return c.Phone
	case "second_phone":
		return c.SecondPhone
	case "email":
		return c.Email
	case "address":
		return c.Address
	case "language":
		return c.Language
	case "about":
		return c.About
	}
	return nil
}

const clientRowSelect = `
	select name, phone, second_phone, email, address, language, about, version
	from public.clients where id = $1 for update`

func scanClientRow(row pgx.Row) (clientRow, error) {
	var c clientRow
	err := row.Scan(&c.Name, &c.Phone, &c.SecondPhone, &c.Email, &c.Address, &c.Language, &c.About, &c.Version)
	return c, err
}

func jsonValue(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func (s *Server) insertClientEvent(ctx context.Context, tx pgx.Tx, clientID, actorID uuid.UUID, eventType string, oldValue, newValue map[string]any) error {
	_, err := tx.Exec(ctx, `
		insert into public.client_events (client_id, actor_id, event_type, old_value, new_value)
		values ($1,$2,$3,$4,$5)
	`, clientID, actorID, eventType, oldValue, newValue)
	return err
}

func (s *Server) handleUpdateClient(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	h, redirectedFrom, ok := s.loadReadableClient(w, r, actor)
	if !ok {
		return
	}
	if redirectedFrom != nil {
		s.writeError(w, r, http.StatusConflict, "client_merged", "This client was merged into "+h.Code, nil)
		return
	}
	if !actor.CanEditClient(h.OfficeID) {
		s.writeError(w, r, http.StatusForbidden, "client_edit_forbidden", "Client editing is not allowed", nil)
		return
	}
	var req updateClientRequest
	if err := decodeJSON(w, r, 16*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid client", nil)
		return
	}
	patch, fields := validateUpdateClient(req)
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid client", fields)
		return
	}
	ifMatch, hasIfMatch := parseIfMatch(r)

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "client_update_failed", "Could not update the client", nil)
		return
	}
	defer tx.Rollback(r.Context())
	current, err := scanClientRow(tx.QueryRow(r.Context(), clientRowSelect, h.ID))
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "client_update_failed", "Could not update the client", nil)
		return
	}
	if hasIfMatch && ifMatch != current.Version {
		s.writeError(w, r, http.StatusConflict, "version_conflict", "Client was changed by another user", nil)
		return
	}

	sets := []string{}
	args := []any{h.ID}
	oldValue, newValue := map[string]any{}, map[string]any{}
	for _, column := range clientPatchColumns {
		next, requested := patch[column]
		if !requested {
			continue
		}
		prev := current.column(column)
		if (prev == nil && next == nil) || (prev != nil && next != nil && *prev == *next) {
			continue
		}
		args = append(args, next)
		sets = append(sets, fmt.Sprintf("%s = $%d", column, len(args)))
		oldValue[column] = jsonValue(prev)
		newValue[column] = jsonValue(next)
	}
	if len(sets) > 0 {
		if _, err := tx.Exec(r.Context(), `update public.clients set `+strings.Join(sets, ", ")+`, version = version + 1 where id = $1`, args...); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "client_update_failed", "Could not update the client", nil)
			return
		}
		if err := s.insertClientEvent(r.Context(), tx, h.ID, actor.ID, "updated", oldValue, newValue); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "client_update_failed", "Could not update the client", nil)
			return
		}
	}
	raw, err := s.clientCardJSON(r.Context(), tx, actor, h, nil)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "client_update_failed", "Could not update the client", nil)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "client_update_failed", "Could not update the client", nil)
		return
	}
	writeJSON(w, http.StatusOK, raw)
}

// ---- merge ---------------------------------------------------------------------------------

// handleMergeClients merges the duplicate named in the body into the older of the two clients.
// Irreversible: the duplicate stays as a tombstone (merged_into_id) so its code keeps resolving
// to the survivor. Leads, projects and linked tasks move; documents and timelines follow their
// lead or project. Needs an Idempotency-Key.
func (s *Server) handleMergeClients(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	pathID, err := uuid.Parse(r.PathValue("clientId"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid client id", nil)
		return
	}
	var req mergeClientsRequest
	if err := decodeJSON(w, r, 4*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid merge", nil)
		return
	}
	keep, fields := validateMergeClients(req, pathID)
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid merge", fields)
		return
	}

	const operation = "client.merge"
	tx, key, ok := s.beginIdempotentTx(w, r, actor, operation,
		pathID.String(), req.DuplicateID.String(), keep.Email, keep.Address, keep.About)
	if !ok {
		return
	}
	defer tx.Rollback(r.Context())
	fail := func() {
		s.writeError(w, r, http.StatusInternalServerError, "client_merge_failed", "Could not merge the clients", nil)
	}

	type lockedClient struct {
		ID        uuid.UUID
		OfficeID  uuid.UUID
		Code      string
		CreatedAt time.Time
		Merged    bool
		Row       clientRow
	}
	rows, err := tx.Query(r.Context(), `
		select id, office_id, code, created_at, merged_into_id is not null,
		  name, phone, second_phone, email, address, language, about, version
		from public.clients where id = any($1::uuid[]) order by id for update
	`, []uuid.UUID{pathID, req.DuplicateID})
	if err != nil {
		fail()
		return
	}
	locked := map[uuid.UUID]lockedClient{}
	for rows.Next() {
		var c lockedClient
		if err := rows.Scan(&c.ID, &c.OfficeID, &c.Code, &c.CreatedAt, &c.Merged,
			&c.Row.Name, &c.Row.Phone, &c.Row.SecondPhone, &c.Row.Email, &c.Row.Address, &c.Row.Language, &c.Row.About, &c.Row.Version); err != nil {
			rows.Close()
			fail()
			return
		}
		locked[c.ID] = c
	}
	rows.Close()
	if rows.Err() != nil {
		fail()
		return
	}
	first, okFirst := locked[pathID]
	second, okSecond := locked[req.DuplicateID]
	if !okFirst || !okSecond {
		s.writeError(w, r, http.StatusNotFound, "client_not_found", "Client not found", nil)
		return
	}
	if !actor.CanAccessOffice(first.OfficeID) || !actor.CanAccessOffice(second.OfficeID) {
		s.writeError(w, r, http.StatusForbidden, "office_forbidden", "Office access denied", nil)
		return
	}
	if first.OfficeID != second.OfficeID {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid merge",
			map[string]string{"duplicateId": "Must be a client of the same office"})
		return
	}
	if !actor.CanMergeClients(first.OfficeID) {
		s.writeError(w, r, http.StatusForbidden, "client_merge_forbidden", "Only admins can merge clients", nil)
		return
	}
	if first.Merged || second.Merged {
		s.writeError(w, r, http.StatusConflict, "client_merged", "One of the clients was already merged", nil)
		return
	}

	older, newer := first, second
	if second.CreatedAt.Before(first.CreatedAt) || (second.CreatedAt.Equal(first.CreatedAt) && second.ID.String() < first.ID.String()) {
		older, newer = second, first
	}
	merged := mergeClientContacts(older.Row.contact(), newer.Row.contact(), keep)

	leads, err := tx.Exec(r.Context(), `update public.leads set client_id = $1 where client_id = $2`, older.ID, newer.ID)
	if err != nil {
		fail()
		return
	}
	projects, err := tx.Exec(r.Context(), `update public.projects set client_id = $1 where client_id = $2`, older.ID, newer.ID)
	if err != nil {
		fail()
		return
	}
	if _, err := tx.Exec(r.Context(), `update public.tasks set link_client_id = $1 where link_client_id = $2`, older.ID, newer.ID); err != nil {
		fail()
		return
	}
	if _, err := tx.Exec(r.Context(), `update public.clients set merged_into_id = $1, merged_at = now(), version = version + 1 where id = $2`, older.ID, newer.ID); err != nil {
		fail()
		return
	}
	if _, err := tx.Exec(r.Context(), `
		update public.clients
		set second_phone = $2, email = $3, address = $4, language = $5, about = $6, version = version + 1
		where id = $1
	`, older.ID, merged.SecondPhone, merged.Email, merged.Address, merged.Language, merged.About); err != nil {
		fail()
		return
	}

	snapshot := func(c clientContact) map[string]any {
		return map[string]any{
			"name": c.Name, "phone": jsonValue(c.Phone), "secondPhone": jsonValue(c.SecondPhone), "email": jsonValue(c.Email),
			"address": jsonValue(c.Address), "language": jsonValue(c.Language), "about": jsonValue(c.About),
		}
	}
	keepValue := map[string]any{"email": keep.Email, "address": keep.Address, "about": keep.About}
	if err := s.insertClientEvent(r.Context(), tx, older.ID, actor.ID, "merged", snapshot(older.Row.contact()), map[string]any{
		"mergedClientId": newer.ID.String(), "mergedClientCode": newer.Code,
		"leadsMoved": leads.RowsAffected(), "projectsMoved": projects.RowsAffected(),
		"keep": keepValue, "result": snapshot(merged), "mergedClient": snapshot(newer.Row.contact()),
	}); err != nil {
		fail()
		return
	}
	if err := s.insertClientEvent(r.Context(), tx, newer.ID, actor.ID, "merged", nil, map[string]any{
		"intoClientId": older.ID.String(), "intoClientCode": older.Code,
	}); err != nil {
		fail()
		return
	}

	raw, err := s.clientCardJSON(r.Context(), tx, actor, clientHeader{ID: older.ID, OfficeID: older.OfficeID, Code: older.Code}, nil)
	if err != nil {
		fail()
		return
	}
	if err := s.finishIdempotentTx(r, tx, actor, operation, key, http.StatusOK, raw); err != nil {
		fail()
		return
	}
	writeJSON(w, http.StatusOK, raw)
}
