package crmapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type userMutationRequest struct {
	Email           string      `json:"email"`
	DisplayName     string      `json:"displayName"`
	Password        string      `json:"password"`
	PasswordConfirm string      `json:"passwordConfirm"`
	Role            string      `json:"role"`
	OfficeIDs       []uuid.UUID `json:"officeIds"`
	ConfirmEmail    string      `json:"confirmEmail"`
}

func (s *Server) handleListManagers(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	var officeIDs any
	if !actor.IsSuperAdmin() {
		ids := make([]uuid.UUID, 0, len(actor.OfficeIDs))
		for id := range actor.OfficeIDs {
			ids = append(ids, id)
		}
		officeIDs = ids
	}
	rows, err := s.pool.Query(r.Context(), `
		select p.id,coalesce(p.display_name,''),p.role::text,p.created_at,p.updated_at,
			array_agg(o.code order by o.code),array_agg(o.id order by o.code)
		from public.profiles p
		join public.user_office_memberships m on m.user_id=p.id
		join public.offices o on o.id=m.office_id
		where p.is_active=true and p.role<>'super_admin'
		  and ($1::uuid[] is null or m.office_id=any($1))
		group by p.id,p.display_name,p.role,p.created_at,p.updated_at
		order by p.display_name nulls last
	`, officeIDs)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "managers_load_failed", "Could not load managers", nil)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id uuid.UUID
		var displayName, role string
		var createdAt, updatedAt time.Time
		var officeCodes []string
		var officeUUIDs []uuid.UUID
		if err := rows.Scan(&id, &displayName, &role, &createdAt, &updatedAt, &officeCodes, &officeUUIDs); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "managers_load_failed", "Could not load managers", nil)
			return
		}
		items = append(items, map[string]any{
			"id": id, "email": nil, "displayName": displayName, "role": role,
			"officeIds": officeCodes, "officeUuids": officeUUIDs, "status": "active",
			"createdAt": createdAt, "lastActiveAt": updatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) requireSuperAdmin(w http.ResponseWriter, r *http.Request) bool {
	actor, ok := actorFromContext(r.Context())
	if !ok || !actor.IsSuperAdmin() {
		s.writeError(w, r, http.StatusForbidden, "super_admin_required", "Only super admin can manage users", nil)
		return false
	}
	return true
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if !s.requireSuperAdmin(w, r) {
		return
	}
	active := strings.TrimSpace(r.URL.Query().Get("active"))
	where := "true"
	if active == "true" {
		where = "p.is_active=true"
	} else if active == "false" {
		where = "p.is_active=false"
	}
	// Join auth.users for emails and aggregate offices once — avoid Auth Admin pagination (504 risk).
	rows, err := s.pool.Query(r.Context(), `
		select p.id, to_jsonb(p), coalesce(u.email, ''),
			coalesce(offices.offices, '[]'::jsonb)
		from public.profiles p
		left join auth.users u on u.id = p.id
		left join lateral (
			select jsonb_agg(to_jsonb(o) order by o.code) as offices
			from public.user_office_memberships m
			join public.offices o on o.id = m.office_id
			where m.user_id = p.id
		) offices on true
		where `+where+`
		order by p.display_name nulls last, p.created_at
	`)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "users_load_failed", "Could not load users", nil)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id uuid.UUID
		var profile, offices []byte
		var email string
		if err := rows.Scan(&id, &profile, &email, &offices); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "users_load_failed", "Could not load users", nil)
			return
		}
		var emailVal any
		if email != "" {
			emailVal = email
		}
		items = append(items, map[string]any{"id": id, "email": emailVal, "profile": json.RawMessage(profile), "offices": json.RawMessage(offices)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireSuperAdmin(w, r) {
		return
	}
	userID, err := uuid.Parse(r.PathValue("userId"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid user id", nil)
		return
	}
	// Same source as listUsers: profiles + auth.users. Avoid Auth Admin GET —
	// a missing/failed admin lookup was returning 404 even when the profile exists.
	var profile, offices []byte
	var email string
	err = s.pool.QueryRow(r.Context(), `
		select to_jsonb(p), coalesce(u.email, ''),
			coalesce((select jsonb_agg(to_jsonb(o) order by o.code)
				from public.user_office_memberships m join public.offices o on o.id=m.office_id
				where m.user_id=p.id),'[]'::jsonb)
		from public.profiles p
		left join auth.users u on u.id = p.id
		where p.id=$1
	`, userID).Scan(&profile, &email, &offices)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "user_not_found", "User not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "user_load_failed", "Could not load user", nil)
		return
	}
	var emailVal any
	if email != "" {
		emailVal = email
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": userID, "email": emailVal, "profile": json.RawMessage(profile), "offices": json.RawMessage(offices)})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireSuperAdmin(w, r) || !requireIdempotencyKey(s, w, r) {
		return
	}
	actor, _ := actorFromContext(r.Context())
	var req userMutationRequest
	if err := decodeJSON(w, r, 64*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid user data", nil)
		return
	}
	if fields := validateUserMutation(req, true); len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid user data", fields)
		return
	}
	authPayload := map[string]any{
		"email":         strings.ToLower(strings.TrimSpace(req.Email)),
		"password":      req.Password,
		"email_confirm": true,
		"user_metadata": map[string]string{"display_name": strings.TrimSpace(req.DisplayName)},
	}
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	if err := s.authAdmin(r, http.MethodPost, "/auth/v1/admin/users", authPayload, &created); err != nil || created.ID == uuid.Nil {
		s.writeError(w, r, http.StatusBadGateway, "auth_user_create_failed", "Could not create auth user", nil)
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err == nil {
		defer tx.Rollback(r.Context())
	}
	if err == nil {
		err = setLocalActor(r.Context(), tx, actor.ID)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `insert into public.profiles (id,role,display_name,is_active,deactivated_at) values ($1,$2,$3,true,null) on conflict (id) do update set role=excluded.role,display_name=excluded.display_name,is_active=true,deactivated_at=null`, created.ID, req.Role, strings.TrimSpace(req.DisplayName))
	}
	if err == nil {
		err = replaceMemberships(r.Context(), tx, created.ID, req.OfficeIDs)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	} else if tx != nil {
		_ = tx.Rollback(r.Context())
	}
	if err != nil {
		_ = s.authAdmin(r, http.MethodDelete, "/auth/v1/admin/users/"+created.ID.String(), nil, nil)
		s.writeError(w, r, http.StatusInternalServerError, "user_create_failed", "Could not create user profile", nil)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"userId": created.ID})
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireSuperAdmin(w, r) {
		return
	}
	// Bound the whole update so a stuck Auth Admin call or DB lock cannot
	// hang until the ingress gateway returns an opaque 504.
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	actor, _ := actorFromContext(r.Context())
	userID, err := uuid.Parse(r.PathValue("userId"))
	var req userMutationRequest
	if err != nil || decodeJSON(w, r, 64*1024, &req) != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid user data", nil)
		return
	}
	if fields := validateUserMutation(req, false); len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid user data", fields)
		return
	}

	writeUpdateTimeout := func(step string, stepErr error) {
		s.logger.Error("user update timed out",
			"step", step,
			"error", stepErr,
			"user_id", userID,
			"request_id", requestID(r.Context()),
		)
		s.writeError(w, r, http.StatusGatewayTimeout, "user_update_timeout", "User update timed out", nil)
	}
	isTimeout := func(err error) bool {
		if err == nil {
			return false
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return true
		}
		// Postgres lock_timeout / statement_timeout surface as SQLSTATE errors
		// rather than context cancellation when RuntimeParams are set on the pool.
		var msg = strings.ToLower(err.Error())
		return strings.Contains(msg, "lock timeout") ||
			strings.Contains(msg, "statement timeout") ||
			strings.Contains(msg, "canceling statement due to")
	}
	logStep := func(step string, started time.Time, stepErr error) {
		attrs := []any{
			"step", step,
			"duration_ms", time.Since(started).Milliseconds(),
			"user_id", userID,
			"request_id", requestID(r.Context()),
		}
		if stepErr != nil {
			attrs = append(attrs, "error", stepErr)
			s.logger.Error("user update step failed", attrs...)
			return
		}
		s.logger.Info("user update step", attrs...)
	}

	var existingRole string
	started := time.Now()
	err = s.pool.QueryRow(ctx, `select role::text from public.profiles where id=$1`, userID).Scan(&existingRole)
	logStep("precheck_select", started, err)
	if isTimeout(err) {
		writeUpdateTimeout("precheck_select", err)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "user_not_found", "User not found", nil)
		return
	}
	if existingRole == "super_admin" {
		s.writeError(w, r, http.StatusBadRequest, "super_admin_immutable", "Super admin cannot be edited here", nil)
		return
	}
	authPayload := map[string]any{"email": strings.ToLower(strings.TrimSpace(req.Email)), "user_metadata": map[string]string{"display_name": strings.TrimSpace(req.DisplayName)}}
	if req.Password != "" {
		authPayload["password"] = req.Password
	}
	started = time.Now()
	err = s.authAdmin(r, http.MethodPut, "/auth/v1/admin/users/"+userID.String(), authPayload, nil)
	logStep("auth_admin_put", started, err)
	if isTimeout(err) {
		writeUpdateTimeout("auth_admin_put", err)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusBadGateway, "auth_user_update_failed", "Could not update auth user", nil)
		return
	}
	started = time.Now()
	tx, err := s.pool.Begin(ctx)
	logStep("begin", started, err)
	if isTimeout(err) {
		writeUpdateTimeout("begin", err)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "user_update_failed", "Could not update user profile", nil)
		return
	}
	defer tx.Rollback(ctx)

	// Transaction-pooler (Supabase) may ignore connection RuntimeParams; pin
	// timeouts on this transaction so a lock cannot hang until gateway 504.
	started = time.Now()
	_, err = tx.Exec(ctx, `select set_config('lock_timeout', '5s', true), set_config('statement_timeout', '20s', true)`)
	logStep("set_tx_timeouts", started, err)
	if isTimeout(err) {
		writeUpdateTimeout("set_tx_timeouts", err)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "user_update_failed", "Could not update user profile", nil)
		return
	}

	started = time.Now()
	err = setLocalActor(ctx, tx, actor.ID)
	logStep("set_local_actor", started, err)
	if isTimeout(err) {
		writeUpdateTimeout("set_local_actor", err)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "user_update_failed", "Could not update user profile", nil)
		return
	}

	started = time.Now()
	_, err = tx.Exec(ctx, `update public.profiles set role=$2,display_name=$3,updated_at=now() where id=$1`, userID, req.Role, strings.TrimSpace(req.DisplayName))
	logStep("update_profile", started, err)
	if isTimeout(err) {
		writeUpdateTimeout("update_profile", err)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "user_update_failed", "Could not update user profile", nil)
		return
	}

	started = time.Now()
	err = replaceMemberships(ctx, tx, userID, req.OfficeIDs)
	logStep("replace_memberships", started, err)
	if isTimeout(err) {
		writeUpdateTimeout("replace_memberships", err)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "user_update_failed", "Could not update user profile", nil)
		return
	}

	started = time.Now()
	err = tx.Commit(ctx)
	logStep("commit", started, err)
	if isTimeout(err) {
		writeUpdateTimeout("commit", err)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "user_update_failed", "Could not update user profile", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDeactivateUser(w http.ResponseWriter, r *http.Request) {
	s.handleSetUserActive(w, r, false)
}

func (s *Server) handleReactivateUser(w http.ResponseWriter, r *http.Request) {
	s.handleSetUserActive(w, r, true)
}

func (s *Server) handleSetUserActive(w http.ResponseWriter, r *http.Request, active bool) {
	if !s.requireSuperAdmin(w, r) || !requireIdempotencyKey(s, w, r) {
		return
	}
	actor, _ := actorFromContext(r.Context())
	userID, err := uuid.Parse(r.PathValue("userId"))
	var req userMutationRequest
	if err != nil || decodeJSON(w, r, 16*1024, &req) != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid user request", nil)
		return
	}
	var role, email string
	err = s.pool.QueryRow(r.Context(), `
		select p.role::text, coalesce(u.email, '')
		from public.profiles p
		left join auth.users u on u.id = p.id
		where p.id=$1
	`, userID).Scan(&role, &email)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "user_not_found", "User not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "user_status_failed", "Could not load user", nil)
		return
	}
	if role == "super_admin" && !active {
		s.writeError(w, r, http.StatusBadRequest, "super_admin_immutable", "Super admin cannot be deactivated", nil)
		return
	}
	if !active && !strings.EqualFold(strings.TrimSpace(req.ConfirmEmail), email) {
		s.writeError(w, r, http.StatusBadRequest, "confirmation_mismatch", "Confirmation email does not match", map[string]string{"confirmEmail": "Email does not match"})
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "user_status_failed", "Could not update user status", nil)
		return
	}
	defer tx.Rollback(r.Context())
	if err := setLocalActor(r.Context(), tx, actor.ID); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "user_status_failed", "Could not update user status", nil)
		return
	}
	if _, err := tx.Exec(r.Context(), `update public.profiles set is_active=$2,deactivated_at=case when $2 then null else now() end,updated_at=now() where id=$1`, userID, active); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "user_status_failed", "Could not update user status", nil)
		return
	}
	if !active {
		if _, err := tx.Exec(r.Context(), `
			update public.leads
			set assigned_to = null, updated_at = now()
			where assigned_to = $1 and archived_at is null
		`, userID); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "user_status_failed", "Could not clear lead assignments", nil)
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "user_status_failed", "Could not update user status", nil)
		return
	}
	ban := "876000h"
	if active {
		ban = "none"
	}
	if err := s.authAdmin(r, http.MethodPut, "/auth/v1/admin/users/"+userID.String(), map[string]string{"ban_duration": ban}, nil); err != nil {
		rollbackTx, rollbackErr := s.pool.Begin(r.Context())
		if rollbackErr == nil {
			defer rollbackTx.Rollback(r.Context())
			rollbackErr = setLocalActor(r.Context(), rollbackTx, actor.ID)
		}
		if rollbackErr == nil {
			_, rollbackErr = rollbackTx.Exec(r.Context(), `update public.profiles set is_active=$2,deactivated_at=case when $2 then null else now() end,updated_at=now() where id=$1`, userID, !active)
		}
		if rollbackErr == nil {
			rollbackErr = rollbackTx.Commit(r.Context())
		}
		if rollbackErr != nil {
			s.logger.Error("could not roll back user status after auth update failure", "error", rollbackErr, "user_id", userID, "request_id", requestID(r.Context()))
		}
		s.writeError(w, r, http.StatusBadGateway, "auth_user_status_failed", "Could not update auth user status", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireSuperAdmin(w, r) || !requireIdempotencyKey(s, w, r) {
		return
	}
	userID, err := uuid.Parse(r.PathValue("userId"))
	var req userMutationRequest
	if err != nil || decodeJSON(w, r, 16*1024, &req) != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid user request", nil)
		return
	}
	var role, email string
	var active bool
	err = s.pool.QueryRow(r.Context(), `
		select p.role::text, p.is_active, coalesce(u.email, '')
		from public.profiles p
		left join auth.users u on u.id = p.id
		where p.id=$1
	`, userID).Scan(&role, &active, &email)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "user_not_found", "User not found", nil)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "user_delete_failed", "Could not load user", nil)
		return
	}
	if active || role == "super_admin" || !strings.EqualFold(strings.TrimSpace(req.ConfirmEmail), email) {
		s.writeError(w, r, http.StatusBadRequest, "delete_not_allowed", "Deactivate the user and confirm the exact email first", nil)
		return
	}
	if err := s.authAdmin(r, http.MethodDelete, "/auth/v1/admin/users/"+userID.String(), nil, nil); err != nil {
		s.writeError(w, r, http.StatusBadGateway, "auth_user_delete_failed", "Could not delete auth user", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func validateUserMutation(req userMutationRequest, requirePassword bool) map[string]string {
	fields := map[string]string{}
	if !strings.Contains(req.Email, "@") {
		fields["email"] = "Valid email required"
	}
	if strings.TrimSpace(req.DisplayName) == "" {
		fields["displayName"] = "Required"
	}
	validRole := req.Role == "super_admin" || req.Role == "curator" || req.Role == "office_admin" || req.Role == "office_member"
	if !validRole {
		fields["role"] = "Invalid role"
	}
	if req.Role != "super_admin" && len(req.OfficeIDs) == 0 {
		fields["officeIds"] = "At least one office is required"
	}
	if requirePassword && len(req.Password) < 8 {
		fields["password"] = "At least 8 characters required"
	}
	if req.Password != req.PasswordConfirm {
		fields["passwordConfirm"] = "Passwords do not match"
	}
	return fields
}

func replaceMemberships(ctx context.Context, tx pgx.Tx, userID uuid.UUID, officeIDs []uuid.UUID) error {
	if _, err := tx.Exec(ctx, `delete from public.user_office_memberships where user_id=$1`, userID); err != nil {
		return err
	}
	for _, officeID := range officeIDs {
		if _, err := tx.Exec(ctx, `insert into public.user_office_memberships (user_id,office_id) values ($1,$2)`, userID, officeID); err != nil {
			return err
		}
	}
	return nil
}

// setLocalActor propagates the already authenticated request actor into the
// current database transaction. Supabase auth helpers such as auth.uid() read
// this transaction-local setting, allowing the profiles guard trigger to
// enforce that sensitive fields are changed only by an active super admin.
func setLocalActor(ctx context.Context, tx pgx.Tx, actorID uuid.UUID) error {
	_, err := tx.Exec(ctx, `select set_config('request.jwt.claim.sub', $1, true)`, actorID.String())
	return err
}

func (s *Server) authAdmin(r *http.Request, method, path string, payload any, out any) error {
	if s.supabaseURL == "" || s.supabaseSecretKey == "" {
		return errors.New("supabase auth admin is not configured")
	}
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	// Honor the caller's context deadline (e.g. handleUpdateUser's 25s budget)
	// and keep a hard ceiling so Auth Admin cannot hang unbounded.
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, s.supabaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.supabaseSecretKey)
	req.Header.Set("apikey", s.supabaseSecretKey)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("supabase auth admin status %d: %s", res.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.NewDecoder(res.Body).Decode(out)
	}
	return nil
}
