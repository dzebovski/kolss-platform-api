package crmapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type leadMarkerKind string

const (
	leadMarkerReviewed     leadMarkerKind = "reviewed"
	leadMarkerManagerAware leadMarkerKind = "manager_aware"
)

func parseLeadMarkerKind(value string) (leadMarkerKind, bool) {
	kind := leadMarkerKind(strings.TrimSpace(value))
	return kind, kind == leadMarkerReviewed || kind == leadMarkerManagerAware
}

type leadMarkerResponse struct {
	Kind      leadMarkerKind `json:"kind"`
	ActorID   uuid.UUID      `json:"actor_id"`
	ActorName string         `json:"actor_name"`
	MarkedAt  time.Time      `json:"marked_at"`
}

func (s *Server) markerLeadAccess(w http.ResponseWriter, r *http.Request) (uuid.UUID, leadMarkerKind, bool) {
	actor, _ := actorFromContext(r.Context())
	leadID, leadErr := uuid.Parse(r.PathValue("leadId"))
	kind, kindOK := parseLeadMarkerKind(r.PathValue("kind"))
	if leadErr != nil || !kindOK {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid lead marker", nil)
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
		s.writeError(w, r, http.StatusInternalServerError, "lead_marker_failed", "Could not load lead marker", nil)
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

func (s *Server) handleSetLeadMarker(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	leadID, kind, ok := s.markerLeadAccess(w, r)
	if !ok {
		return
	}

	var response leadMarkerResponse
	err := s.pool.QueryRow(r.Context(), `
		insert into public.lead_markers (lead_id, kind, actor_id, marked_at)
		values ($1, $2, $3, now())
		on conflict (lead_id, kind) do update
		set actor_id = excluded.actor_id, marked_at = excluded.marked_at
		returning kind, actor_id, marked_at
	`, leadID, kind, actor.ID).Scan(&response.Kind, &response.ActorID, &response.MarkedAt)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "lead_marker_failed", "Could not save lead marker", nil)
		return
	}
	if actor.DisplayName != nil {
		response.ActorName = strings.TrimSpace(*actor.DisplayName)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleDeleteLeadMarker(w http.ResponseWriter, r *http.Request) {
	leadID, kind, ok := s.markerLeadAccess(w, r)
	if !ok {
		return
	}
	if _, err := s.pool.Exec(r.Context(), `delete from public.lead_markers where lead_id=$1 and kind=$2`, leadID, kind); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "lead_marker_failed", "Could not remove lead marker", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
