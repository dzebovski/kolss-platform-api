package crmapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type leadReactionKind string

const (
	leadReactionLike     leadReactionKind = "like"
	leadReactionStar     leadReactionKind = "star"
	leadReactionQuestion leadReactionKind = "question"
)

func parseLeadReactionKind(value string) (leadReactionKind, bool) {
	kind := leadReactionKind(strings.TrimSpace(value))
	return kind, kind == leadReactionLike || kind == leadReactionStar || kind == leadReactionQuestion
}

type leadReactionResponse struct {
	Kind      leadReactionKind `json:"kind"`
	Count     int              `json:"count"`
	UserIDs   []uuid.UUID      `json:"user_ids"`
	UserNames []string         `json:"user_names"`
}

// reactionLeadAccess mirrors markerLeadAccess: any office member may react to
// an active lead of their office.
func (s *Server) reactionLeadAccess(w http.ResponseWriter, r *http.Request) (uuid.UUID, leadReactionKind, bool) {
	actor, _ := actorFromContext(r.Context())
	leadID, leadErr := uuid.Parse(r.PathValue("leadId"))
	kind, kindOK := parseLeadReactionKind(r.PathValue("kind"))
	if leadErr != nil || !kindOK {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid lead reaction", nil)
		return uuid.Nil, "", false
	}

	var officeID uuid.UUID
	var archivedAt *time.Time
	err := s.pool.QueryRow(r.Context(), `select office_id, archived_at from public.leads where id=$1`, leadID).Scan(&officeID, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "lead_not_found", "Lead not found", nil)
		return uuid.Nil, "", false
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "lead_reaction_failed", "Could not load lead reaction", nil)
		return uuid.Nil, "", false
	}
	if !actor.CanAccessOffice(officeID) {
		s.writeError(w, r, http.StatusForbidden, "office_forbidden", "Office access denied", nil)
		return uuid.Nil, "", false
	}
	if archivedAt != nil {
		s.writeError(w, r, http.StatusConflict, "lead_archived", "Archived leads are read-only", nil)
		return uuid.Nil, "", false
	}
	return leadID, kind, true
}

func (s *Server) handleSetLeadReaction(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	leadID, kind, ok := s.reactionLeadAccess(w, r)
	if !ok {
		return
	}
	if _, err := s.pool.Exec(r.Context(), `
		insert into public.lead_reactions (lead_id, kind, user_id)
		values ($1, $2, $3)
		on conflict (lead_id, kind, user_id) do nothing
	`, leadID, kind, actor.ID); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "lead_reaction_failed", "Could not save lead reaction", nil)
		return
	}

	response := leadReactionResponse{Kind: kind, UserIDs: []uuid.UUID{}, UserNames: []string{}}
	rows, err := s.pool.Query(r.Context(), `
		select lr.user_id, coalesce(p.display_name, '')
		from public.lead_reactions lr
		left join public.profiles p on p.id = lr.user_id
		where lr.lead_id = $1 and lr.kind = $2
		order by lr.created_at, lr.user_id
	`, leadID, kind)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "lead_reaction_failed", "Could not load lead reaction", nil)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var userID uuid.UUID
		var name string
		if err := rows.Scan(&userID, &name); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "lead_reaction_failed", "Could not load lead reaction", nil)
			return
		}
		response.UserIDs = append(response.UserIDs, userID)
		response.UserNames = append(response.UserNames, strings.TrimSpace(name))
	}
	if err := rows.Err(); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "lead_reaction_failed", "Could not load lead reaction", nil)
		return
	}
	response.Count = len(response.UserIDs)
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleDeleteLeadReaction(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	leadID, kind, ok := s.reactionLeadAccess(w, r)
	if !ok {
		return
	}
	if _, err := s.pool.Exec(r.Context(), `delete from public.lead_reactions where lead_id=$1 and kind=$2 and user_id=$3`, leadID, kind, actor.ID); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "lead_reaction_failed", "Could not remove lead reaction", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
