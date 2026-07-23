package crmapi

import (
	"net/http"

	"github.com/google/uuid"
)

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFromContext(r.Context())
	if !ok {
		s.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Unauthorized", nil)
		return
	}

	var response MeResponse
	response.User.ID = actor.ID
	response.User.Email = actor.Email
	if err := s.pool.QueryRow(r.Context(), `
		select id, role::text, display_name, is_active, deactivated_at, created_at, updated_at
		from public.profiles where id = $1
	`, actor.ID).Scan(
		&response.Profile.ID,
		&response.Profile.Role,
		&response.Profile.DisplayName,
		&response.Profile.IsActive,
		&response.Profile.DeactivatedAt,
		&response.Profile.CreatedAt,
		&response.Profile.UpdatedAt,
	); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "profile_load_failed", "Could not load profile", nil)
		return
	}

	offices, err := s.listActorOffices(r, actor, actor.IsSuperAdmin())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "offices_load_failed", "Could not load offices", nil)
		return
	}
	response.Offices = offices
	response.UserOffices = offices
	response.Permissions = Permissions{
		CanManageUsers:    actor.IsSuperAdmin(),
		CanEditLeadFields: actor.IsSuperAdmin() || actor.Role == "office_admin",
		CanArchiveLeads:   actor.IsSuperAdmin() || actor.Role == "office_admin",
		CanRestoreLeads:   actor.IsSuperAdmin(),
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleOffices(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFromContext(r.Context())
	if !ok {
		s.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Unauthorized", nil)
		return
	}
	offices, err := s.listActorOffices(r, actor, actor.IsSuperAdmin())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "offices_load_failed", "Could not load offices", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": offices})
}

func (s *Server) listActorOffices(r *http.Request, actor Actor, all bool) ([]Office, error) {
	query := `
		select o.id, o.code, o.name_uk, o.name_pl, o.timezone_name, o.is_active
		from public.offices o
		where o.is_active = true
	`
	args := []any{}
	if !all {
		ids := make([]uuid.UUID, 0, len(actor.OfficeIDs))
		for id := range actor.OfficeIDs {
			ids = append(ids, id)
		}
		query += ` and o.id = any($1::uuid[])`
		args = append(args, ids)
	}
	query += ` order by o.code`
	rows, err := s.pool.Query(r.Context(), query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	offices := []Office{}
	for rows.Next() {
		var office Office
		if err := rows.Scan(
			&office.ID,
			&office.Code,
			&office.NameUK,
			&office.NamePL,
			&office.TimezoneName,
			&office.IsActive,
		); err != nil {
			return nil, err
		}
		offices = append(offices, office)
	}
	return offices, rows.Err()
}

func (s *Server) handleLossReasons(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `select code, label_uk, label_pl from public.loss_reasons order by code`)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "loss_reasons_load_failed", "Could not load loss reasons", nil)
		return
	}
	defer rows.Close()
	items := make([]map[string]string, 0)
	for rows.Next() {
		var code, labelUK, labelPL string
		if err := rows.Scan(&code, &labelUK, &labelPL); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "loss_reasons_load_failed", "Could not load loss reasons", nil)
			return
		}
		items = append(items, map[string]string{"code": code, "label_uk": labelUK, "label_pl": labelPL})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
