package crmapi

import (
	"net/http"
	"strconv"
	"time"
)

type updateCurrencyRatesRequest struct {
	PLNPerEUR float64 `json:"plnPerEur"`
	UAHPerEUR float64 `json:"uahPerEur"`
	UAHPerUSD float64 `json:"uahPerUsd"`
}

func writeCurrencyRateSet(w http.ResponseWriter, status int, rates currencyRateSet) {
	w.Header().Set("ETag", `"`+formatVersion(rates.Version)+`"`)
	writeJSON(w, status, rates)
}

func formatVersion(version int64) string {
	return strconv.FormatInt(version, 10)
}

func (s *Server) handleGetCurrencyRates(w http.ResponseWriter, r *http.Request) {
	if !s.requireSuperAdmin(w, r) {
		return
	}
	rates, err := loadCurrencyRateSetAt(r.Context(), s.pool, time.Now().UTC())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "currency_rates_load_failed", "Could not load currency rates", nil)
		return
	}
	writeCurrencyRateSet(w, http.StatusOK, rates)
}

func (s *Server) handleUpdateCurrencyRates(w http.ResponseWriter, r *http.Request) {
	if !s.requireSuperAdmin(w, r) {
		return
	}
	version, ok := parseIfMatch(r)
	if !ok {
		s.writeError(w, r, http.StatusPreconditionRequired, "version_required", "If-Match currency rate version is required", nil)
		return
	}
	var req updateCurrencyRatesRequest
	if err := decodeJSON(w, r, 32*1024, &req); err != nil {
		return
	}
	if !validCurrencyRates(req.PLNPerEUR, req.UAHPerEUR, req.UAHPerUSD) {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid currency rates", map[string]string{
			"rates": "Every currency rate must be greater than zero",
		})
		return
	}

	actor, _ := actorFromContext(r.Context())
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "currency_rates_update_failed", "Could not update currency rates", nil)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `select pg_advisory_xact_lock(hashtext('currency_rate_sets'))`); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "currency_rates_update_failed", "Could not update currency rates", nil)
		return
	}

	var current currencyRateSet
	err = tx.QueryRow(r.Context(), `
		select id, version, effective_from, pln_per_eur, uah_per_eur, uah_per_usd
		from public.currency_rate_sets
		order by effective_from desc
		limit 1
		for update
	`).Scan(
		&current.ID,
		&current.Version,
		&current.EffectiveAt,
		&current.PLNPerEUR,
		&current.UAHPerEUR,
		&current.UAHPerUSD,
	)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "currency_rates_update_failed", "Could not update currency rates", nil)
		return
	}
	if current.Version != version {
		s.writeError(w, r, http.StatusConflict, "version_conflict", "Currency rates were changed by another user", nil)
		return
	}
	var next currencyRateSet
	err = tx.QueryRow(r.Context(), `
		insert into public.currency_rate_sets (
			effective_from, pln_per_eur, uah_per_eur, uah_per_usd, created_by
		) values (now(), $1, $2, $3, $4)
		returning id, version, effective_from, pln_per_eur, uah_per_eur, uah_per_usd
	`, req.PLNPerEUR, req.UAHPerEUR, req.UAHPerUSD, actor.ID).Scan(
		&next.ID,
		&next.Version,
		&next.EffectiveAt,
		&next.PLNPerEUR,
		&next.UAHPerEUR,
		&next.UAHPerUSD,
	)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "currency_rates_update_failed", "Could not update currency rates", nil)
		return
	}
	writeCurrencyRateSet(w, http.StatusOK, next)
}
