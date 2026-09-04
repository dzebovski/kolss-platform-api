package crmapi

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	currencyEUR = "EUR"
	currencyPLN = "PLN"
	currencyUAH = "UAH"
	currencyUSD = "USD"
)

type currencyRateSet struct {
	ID          uuid.UUID `json:"-"`
	Version     int64     `json:"version"`
	EffectiveAt time.Time `json:"effectiveFrom"`
	PLNPerEUR   float64   `json:"plnPerEur"`
	UAHPerEUR   float64   `json:"uahPerEur"`
	UAHPerUSD   float64   `json:"uahPerUsd"`
}

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func normalizeCurrency(value string) (string, bool) {
	currency := strings.ToUpper(strings.TrimSpace(value))
	switch currency {
	case currencyEUR, currencyPLN, currencyUAH, currencyUSD:
		return currency, true
	default:
		return "", false
	}
}

func validCurrencyRates(plnPerEUR, uahPerEUR, uahPerUSD float64) bool {
	return finitePositive(plnPerEUR) && finitePositive(uahPerEUR) && finitePositive(uahPerUSD)
}

func finitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func loadCurrencyRateSetAt(ctx context.Context, db queryRower, at time.Time) (currencyRateSet, error) {
	var rates currencyRateSet
	err := db.QueryRow(ctx, `
		select id, version, effective_from, pln_per_eur, uah_per_eur, uah_per_usd
		from public.currency_rate_sets
		where effective_from <= $1
		order by effective_from desc
		limit 1
	`, at).Scan(
		&rates.ID,
		&rates.Version,
		&rates.EffectiveAt,
		&rates.PLNPerEUR,
		&rates.UAHPerEUR,
		&rates.UAHPerUSD,
	)
	return rates, err
}

func amountInEUR(amount float64, currency string, rates currencyRateSet) (float64, bool) {
	switch currency {
	case currencyEUR:
		return amount, true
	case currencyPLN:
		return amount / rates.PLNPerEUR, rates.PLNPerEUR > 0
	case currencyUAH:
		return amount / rates.UAHPerEUR, rates.UAHPerEUR > 0
	case currencyUSD:
		return amount * rates.UAHPerUSD / rates.UAHPerEUR, rates.UAHPerUSD > 0 && rates.UAHPerEUR > 0
	default:
		return 0, false
	}
}

func convertAmount(amount float64, sourceCurrency, targetCurrency string, rates currencyRateSet) (float64, bool) {
	eur, ok := amountInEUR(amount, sourceCurrency, rates)
	if !ok {
		return 0, false
	}
	switch targetCurrency {
	case currencyEUR:
		return eur, true
	case currencyPLN:
		return eur * rates.PLNPerEUR, rates.PLNPerEUR > 0
	case currencyUAH:
		return eur * rates.UAHPerEUR, rates.UAHPerEUR > 0
	case currencyUSD:
		return eur * rates.UAHPerEUR / rates.UAHPerUSD, rates.UAHPerEUR > 0 && rates.UAHPerUSD > 0
	default:
		return 0, false
	}
}

func reportingCurrencyForOffice(code string) string {
	if code == "warsaw" {
		return currencyPLN
	}
	return currencyUAH
}
