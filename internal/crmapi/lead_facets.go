package crmapi

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// handleLeadFacets returns the leads list chip counts (contract §3.4): the total with every
// filter, v2 status counts without the v2Status filter and rating counts without the rating
// filter. Leads without a v2 status (legacy v1 statuses) or without a rating are not counted
// in their group. Takes the listLeads filters; cursor and limit are ignored.
func (s *Server) handleLeadFacets(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	query := r.URL.Query()
	failed := func(filterErr *leadListFilterError, err error) bool {
		switch {
		case filterErr != nil:
			s.writeError(w, r, filterErr.status, filterErr.code, filterErr.message, filterErr.fields)
		case err != nil:
			s.writeError(w, r, http.StatusInternalServerError, "leads_load_failed", "Could not load lead counts", nil)
		default:
			return false
		}
		return true
	}
	total, filterErr, err := s.countLeads(r.Context(), actor, query)
	if failed(filterErr, err) {
		return
	}
	statuses, filterErr, err := s.countLeadsBy(r.Context(), actor, query, "v2Status", "l.v2_status")
	if failed(filterErr, err) {
		return
	}
	ratings, filterErr, err := s.countLeadsBy(r.Context(), actor, query, "rating", "l.rating")
	if failed(filterErr, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "v2Status": statuses, "rating": ratings})
}

const leadFacetsFrom = `from public.leads l join public.offices o on o.id = l.office_id where `

func (s *Server) countLeads(ctx context.Context, actor Actor, query url.Values) (int, *leadListFilterError, error) {
	where, args, filterErr := leadListWhere(actor, query, "")
	if filterErr != nil {
		return 0, filterErr, nil
	}
	var total int
	err := s.pool.QueryRow(ctx, `select count(*) `+leadFacetsFrom+strings.Join(where, " and "), args...).Scan(&total)
	return total, nil, err
}

// countLeadsBy counts leads per value of column, without the filter named skip.
func (s *Server) countLeadsBy(ctx context.Context, actor Actor, query url.Values, skip, column string) (map[string]int, *leadListFilterError, error) {
	where, args, filterErr := leadListWhere(actor, query, skip)
	if filterErr != nil {
		return nil, filterErr, nil
	}
	rows, err := s.pool.Query(ctx, `select `+column+`, count(*) `+leadFacetsFrom+strings.Join(where, " and ")+` and `+column+` is not null group by 1`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var value string
		var count int
		if err := rows.Scan(&value, &count); err != nil {
			return nil, nil, err
		}
		counts[value] = count
	}
	return counts, nil, rows.Err()
}
