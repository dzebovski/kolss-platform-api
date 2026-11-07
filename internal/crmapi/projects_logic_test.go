package crmapi

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestProjectFirstStatus(t *testing.T) {
	if got := projectFirstStatus(); got != "estimate" {
		t.Errorf("first status: got %q", got)
	}
}

func TestStatusAfterContract(t *testing.T) {
	for _, tc := range []struct{ current, kind, want string }{
		{"estimate", "intro", "intro_contract"},
		{"estimate", "main", "main_contract"},
		{"design", "main", "main_contract"},
		{"intro_contract", "main", "main_contract"},
		{"production", "main", "production"}, // a late entry never drags the project back
		{"design", "intro", "design"},        // intro after design: keep
		{"main_contract", "main", "main_contract"},
	} {
		if got := statusAfterContract(tc.current, tc.kind); got != tc.want {
			t.Errorf("%s + %s contract: got %q want %q", tc.current, tc.kind, got, tc.want)
		}
	}
}

func TestValidateProjectStatus(t *testing.T) {
	manager := uuid.NewString()
	yes := true
	cases := []struct {
		name   string
		req    projectStatusRequest
		fields []string
	}{
		{"intro contract is not pickable", projectStatusRequest{Status: "intro_contract"}, []string{"status"}},
		{"main contract is not pickable", projectStatusRequest{Status: "main_contract"}, []string{"status"}},
		{"cancelled is not pickable", projectStatusRequest{Status: "cancelled"}, []string{"status"}},
		{"express is gone", projectStatusRequest{Status: "express"}, []string{"status"}},
		{"estimate ok", projectStatusRequest{Status: "estimate"}, nil},
		{"2.1 stages are gone", projectStatusRequest{Status: "measure"}, []string{"status"}},
		{"prepayment is gone", projectStatusRequest{Status: "prepayment"}, []string{"status"}},
		{"legacy measure fields are ignored", projectStatusRequest{Status: "design", ResponsibleID: &manager, Address: ptr("Legionowo"), Done: &yes}, nil},
		{"showroom meeting event is optional", projectStatusRequest{Status: "showroom_meeting", EventDate: ptr("2026-10-02"), EventTime: ptr("10:30")}, nil},
		{"factory review ok", projectStatusRequest{Status: "factory_review"}, nil},
		{"presentation ok", projectStatusRequest{Status: "presentation", EventDate: ptr("2026-10-02")}, nil},
		{"time needs a date", projectStatusRequest{Status: "production", EventTime: ptr("09:00")}, []string{"eventDate"}},
		{"completed takes no event", projectStatusRequest{Status: "completed", EventDate: ptr("2026-10-02")}, []string{"eventDate"}},
		{"completed ok", projectStatusRequest{Status: "completed", Comment: ptr("Done")}, nil},
	}
	for _, tc := range cases {
		_, fields := validateProjectStatus(tc.req)
		if len(fields) != len(tc.fields) {
			t.Errorf("%s: got %v want %v", tc.name, fields, tc.fields)
			continue
		}
		for _, name := range tc.fields {
			if _, ok := fields[name]; !ok {
				t.Errorf("%s: missing field error %q in %v", tc.name, name, fields)
			}
		}
	}
}

func TestProjectMoney(t *testing.T) {
	// Intro 500 PLN fully paid, main 80 000 PLN with 39 500 paid on it: the intro is credited.
	m := moneyFromContracts([]projectContractRow{
		{Kind: "intro", TotalCents: 50_000, Currency: "PLN", PaidCents: 50_000},
		{Kind: "main", TotalCents: 8_000_000, Currency: "PLN", PaidCents: 3_950_000},
	})
	if m.credited() != 50_000 || m.mainPaidTotal() != 4_000_000 || m.mainRemaining() != 4_000_000 || m.target50() != 4_000_000 {
		t.Errorf("credit: %+v credited=%d remaining=%d", m, m.credited(), m.mainRemaining())
	}
	if m.maxPayment("main") != 4_000_000 || m.maxPayment("intro") != 0 {
		t.Errorf("max payments: main %d intro %d", m.maxPayment("main"), m.maxPayment("intro"))
	}
	// Different currencies: nothing is credited.
	m = moneyFromContracts([]projectContractRow{
		{Kind: "intro", TotalCents: 50_000, Currency: "EUR", PaidCents: 50_000},
		{Kind: "main", TotalCents: 8_000_000, Currency: "PLN"},
	})
	if m.credited() != 0 || m.mainRemaining() != 8_000_000 {
		t.Errorf("mixed currency: credited %d remaining %d", m.credited(), m.mainRemaining())
	}
	// The target is half of the total, rounded up to the cent.
	if got := (projectMoney{MainTotal: 10_001}).target50(); got != 5_001 {
		t.Errorf("target50 rounding: %d", got)
	}
	// A main total below what is already paid (intro counts) is refused.
	m = moneyFromContracts([]projectContractRow{
		{Kind: "intro", TotalCents: 50_000, Currency: "PLN", PaidCents: 50_000},
		{Kind: "main", TotalCents: 60_000, Currency: "PLN", PaidCents: 20_000},
	})
	if field, _ := m.overpaid(); field != "totalAmount" {
		t.Error("overpaid main must be reported")
	}
}

func TestPickPaymentContract(t *testing.T) {
	intro, main := projectContractRow{ID: uuid.New(), Kind: "intro"}, projectContractRow{ID: uuid.New(), Kind: "main"}
	if got, ok := pickPaymentContract([]projectContractRow{intro}, nil); !ok || got.ID != intro.ID {
		t.Error("only an intro contract: payments go there")
	}
	if got, ok := pickPaymentContract([]projectContractRow{intro, main}, nil); !ok || got.ID != main.ID {
		t.Error("without an id the main contract wins")
	}
	if got, ok := pickPaymentContract([]projectContractRow{intro, main}, &intro.ID); !ok || got.ID != intro.ID {
		t.Error("an explicit id wins")
	}
	other := uuid.New()
	if _, ok := pickPaymentContract([]projectContractRow{intro, main}, &other); ok {
		t.Error("a foreign contract id is refused")
	}
	if _, ok := pickPaymentContract(nil, nil); ok {
		t.Error("no contract, no payment")
	}
}

func TestValidateEstimate(t *testing.T) {
	parse := func(body string) (map[string]*estimateLine, map[string]string) {
		var req projectEstimateRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
		return validateEstimate(req.Lines)
	}
	lines, fields := parse(`{"lines":{"kitchen":{"amount":1200.5,"currency":"PLN","note":" oak "},"countertop":null}}`)
	if len(fields) != 0 || lines["kitchen"].AmountCents != 120_050 || *lines["kitchen"].Note != "oak" || lines["countertop"] != nil {
		t.Errorf("valid estimate: %v %v", lines, fields)
	}
	for body, field := range map[string]string{
		`{"lines":{"garage":null}}`:                                 "lines.garage",
		`{"lines":{"kitchen":{"amount":0,"currency":"PLN"}}}`:       "lines.kitchen.amount",
		`{"lines":{"kitchen":{"amount":5,"currency":"GBP"}}}`:       "lines.kitchen.currency",
		`{"lines":{"kitchen":{"amount":5,"currency":"PLN","x":1}}}`: "lines.kitchen",
		`{"lines":{}}`: "lines",
	} {
		if _, fields := parse(body); fields[field] == "" {
			t.Errorf("%s: expected an error for %s, got %v", body, field, fields)
		}
	}
	a, b := &estimateLine{AmountCents: 5, Currency: "PLN"}, &estimateLine{AmountCents: 5, Currency: "PLN"}
	if !equalEstimateLine(a, b) || equalEstimateLine(a, nil) || !equalEstimateLine(nil, nil) {
		t.Error("estimate line equality")
	}
}

func TestValidateContractAndPaymentPatch(t *testing.T) {
	if change, fields := validateProjectContractPatch(projectContractPatch{TotalAmount: ptr(-1.0)}); len(fields) == 0 || !change.empty() {
		t.Errorf("negative total: %v", fields)
	}
	if change, fields := validateProjectContractPatch(projectContractPatch{Number: ptr(" A/1 "), Currency: ptr("EUR")}); len(fields) != 0 || *change.Number != "A/1" {
		t.Errorf("valid contract patch: %v", fields)
	}
	if _, fields := validateProjectPaymentPatch(projectPaymentPatch{PaidOn: ptr("2026-13-40")}); fields["paidOn"] == "" {
		t.Error("bad payment date")
	}
	if change, fields := validateProjectPaymentPatch(projectPaymentPatch{Note: ptr("  ")}); len(fields) != 0 || change.Note == nil || *change.Note != "" {
		t.Errorf("an empty note clears it: %v", fields)
	}
	if kind := ptr("final"); true {
		if _, fields := validateProjectContract(projectContractRequest{Kind: kind, Number: "1", SignedOn: "2026-10-01", TotalAmount: 1, Currency: "PLN"}); fields["kind"] == "" {
			t.Error("unknown contract kind")
		}
	}
	if in, fields := validateProjectContract(projectContractRequest{Number: "1", SignedOn: "2026-10-01", TotalAmount: 1, Currency: "PLN"}); len(fields) != 0 || in.Kind != "main" {
		t.Errorf("pre-2.36 clients send no kind and get main: %v %v", in.Kind, fields)
	}
}

func TestValidateProjectComment(t *testing.T) {
	if _, fields := validateProjectComment(projectCommentRequest{Text: "  "}); fields["text"] == "" {
		t.Error("empty comment")
	}
	if text, fields := validateProjectComment(projectCommentRequest{Text: " hi "}); len(fields) != 0 || text != "hi" {
		t.Errorf("comment: %q %v", text, fields)
	}
}

func TestValidateProjectCancel(t *testing.T) {
	if _, _, fields := validateProjectCancel(projectCancelRequest{}); fields["reasons"] == "" {
		t.Error("at least one reason is required")
	}
	if _, _, fields := validateProjectCancel(projectCancelRequest{Reasons: []string{"price_too_high"}}); fields["comment"] == "" {
		t.Error("every reason requires a comment (pipeline 3)")
	}
	reasons, comment, fields := validateProjectCancel(projectCancelRequest{Reasons: []string{"price_too_high", "price_too_high", "not_relevant"}, Comment: ptr("Chose a cheaper offer from IKEA")})
	if len(fields) != 0 || len(reasons) != 2 || comment == nil {
		t.Errorf("valid cancel: %v %v %v", reasons, comment, fields)
	}
}

func TestMoneyToCents(t *testing.T) {
	for amount, want := range map[float64]int64{80000: 8000000, 0.1: 10, 1234.56: 123456, 19.99: 1999} {
		if got, ok := moneyToCents(amount); !ok || got != want {
			t.Errorf("%v: got %d,%v want %d", amount, got, ok, want)
		}
	}
	for _, bad := range []float64{0, -5, 10.005, 1e12} {
		if _, ok := moneyToCents(bad); ok {
			t.Errorf("%v must be rejected", bad)
		}
	}
	if got := formatMoneyCents(4000000); got != "40 000" {
		t.Errorf("format: %q", got)
	}
	if got := formatMoneyCents(123456); got != "1 234.56" {
		t.Errorf("format: %q", got)
	}
}

func TestCanEditProject(t *testing.T) {
	office, other := uuid.New(), uuid.New()
	manager := uuid.New()
	member := Actor{ID: uuid.New(), Role: "manager", OfficeIDs: map[uuid.UUID]struct{}{office: {}}}
	owner := Actor{ID: manager, Role: "manager", OfficeIDs: map[uuid.UUID]struct{}{office: {}}}
	admin := Actor{ID: uuid.New(), Role: "office_admin", OfficeIDs: map[uuid.UUID]struct{}{office: {}}}
	super := Actor{ID: uuid.New(), Role: "super_admin"}
	for _, tc := range []struct {
		name   string
		actor  Actor
		office uuid.UUID
		want   bool
	}{
		{"other office member", member, office, false},
		{"responsible manager", owner, office, true},
		{"office admin", admin, office, true},
		{"office admin of another office", admin, other, false},
		{"super admin", super, other, true},
	} {
		if got := tc.actor.CanEditProject(tc.office, &manager); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestPrepaymentCheck(t *testing.T) {
	half := projectMoney{HasMain: true, MainTotal: 8_000_000, MainPaid: 3_950_000, HasIntro: true, IntroPaid: 50_000, IntroCurrency: "PLN", MainCurrency: "PLN"}
	low := projectMoney{HasMain: true, MainTotal: 8_000_000, MainPaid: 800_000}
	cases := []struct {
		name           string
		current, next  string
		money          projectMoney
		admin, comment bool
		want           string
	}{
		{"before production is free", "design", "presentation", projectMoney{}, false, false, prepaymentOK},
		{"credited intro counts toward 50%", "main_contract", "production", half, false, false, prepaymentOK},
		{"10% blocks a manager", "main_contract", "production", low, false, false, prepaymentBlocked},
		{"skipping to installation is checked too", "presentation", "installation", low, false, false, prepaymentBlocked},
		{"no main contract blocks", "factory_review", "production", projectMoney{}, false, false, prepaymentBlocked},
		{"admin needs a comment", "main_contract", "production", low, true, false, prepaymentNeedsComment},
		{"admin override with a comment", "main_contract", "production", low, true, true, prepaymentOverrideAllowed},
		{"already in production", "production", "installation", low, false, false, prepaymentOK},
	}
	for _, tc := range cases {
		if got := prepaymentCheck(tc.current, tc.next, tc.money, tc.admin, tc.comment); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}
