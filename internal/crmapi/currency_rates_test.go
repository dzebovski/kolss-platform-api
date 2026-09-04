package crmapi

import "testing"

func TestConvertAmountUsesConfiguredCrossRates(t *testing.T) {
	rates := currencyRateSet{PLNPerEUR: 4.2, UAHPerEUR: 52, UAHPerUSD: 44.2}

	tests := []struct {
		name   string
		amount float64
		from   string
		to     string
		want   float64
	}{
		{name: "EUR to PLN", amount: 100, from: "EUR", to: "PLN", want: 420},
		{name: "EUR to UAH", amount: 100, from: "EUR", to: "UAH", want: 5200},
		{name: "USD to UAH", amount: 100, from: "USD", to: "UAH", want: 4420},
		{name: "UAH to EUR", amount: 5200, from: "UAH", to: "EUR", want: 100},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := convertAmount(test.amount, test.from, test.to, rates)
			if !ok || got != test.want {
				t.Fatalf("convertAmount() = %v, %v; want %v, true", got, ok, test.want)
			}
		})
	}
}

func TestCurrencyRateValidationRejectsInvalidValues(t *testing.T) {
	if !validCurrencyRates(4.2, 52, 44.2) {
		t.Fatal("expected configured rates to be valid")
	}
	if validCurrencyRates(0, 52, 44.2) || validCurrencyRates(4.2, -1, 44.2) {
		t.Fatal("expected non-positive rates to be rejected")
	}
}
