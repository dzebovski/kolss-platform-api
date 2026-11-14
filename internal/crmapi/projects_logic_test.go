package crmapi

import (
	"testing"

	"github.com/google/uuid"
)

func TestProjectFirstStatus(t *testing.T) {
	if got := projectFirstStatus(); got != "preliminary_estimate" {
		t.Errorf("first status: got %q", got)
	}
}

func TestProjectStagesPipeline4(t *testing.T) {
	want := []string{"preliminary_estimate", "measurement_design", "factory_approval", "contract_signing", "production_delivery", "installation"}
	if len(projectBoardStatuses) != len(want) {
		t.Fatalf("board columns: %v", projectBoardStatuses)
	}
	for i, stage := range want {
		if projectBoardStatuses[i] != stage {
			t.Errorf("column %d: got %q want %q", i, projectBoardStatuses[i], stage)
		}
	}
	if len(projectStatuses) != 8 || projectStatuses[6] != "completed" || projectStatuses[7] != "cancelled" {
		t.Errorf("statuses: %v", projectStatuses)
	}
}

func TestValidateProjectStatus(t *testing.T) {
	for _, stage := range projectStages {
		if _, fields := validateProjectStatus(projectStatusRequest{Status: stage}); len(fields) != 0 {
			t.Errorf("%s: %v", stage, fields)
		}
	}
	for status, want := range map[string]string{
		"completed": "Use Project finished", "cancelled": "Use Close project", "estimate": "",
	} {
		_, fields := validateProjectStatus(projectStatusRequest{Status: status})
		if fields["status"] == "" || (want != "" && fields["status"] != want) {
			t.Errorf("%s: %v", status, fields)
		}
	}
	in, fields := validateProjectStatus(projectStatusRequest{Status: "installation", EventDate: ptr("2026-12-01"), EventTime: ptr("9:30"), Comment: ptr(" ok ")})
	if len(fields) != 0 || *in.EventDate != "2026-12-01" || *in.EventTime != "09:30" || *in.Comment != "ok" {
		t.Errorf("event: %+v %v", in, fields)
	}
	if _, fields := validateProjectStatus(projectStatusRequest{Status: "installation", EventTime: ptr("10:00")}); fields["eventDate"] == "" {
		t.Error("a time needs a date")
	}
}

func TestFinishAndRestore(t *testing.T) {
	if !canFinishProject("installation") || canFinishProject("production_delivery") || canFinishProject("completed") {
		t.Error("finish only from installation")
	}
	for _, tc := range []struct {
		status string
		before *string
		want   string
		ok     bool
	}{
		{"completed", nil, "installation", true},
		{"cancelled", ptr("factory_approval"), "factory_approval", true},
		{"cancelled", ptr("completed"), "preliminary_estimate", true},
		{"cancelled", nil, "preliminary_estimate", true},
		{"installation", nil, "", false},
	} {
		got, ok := restoreTarget(tc.status, tc.before)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: got %q,%v want %q,%v", tc.status, got, ok, tc.want, tc.ok)
		}
	}
	if !isProjectFinal("completed") || !isProjectFinal("cancelled") || isProjectFinal("installation") {
		t.Error("finals")
	}
}

func TestValidateProjectPayment(t *testing.T) {
	_, fields := validateProjectPayment(projectPaymentRequest{})
	for _, key := range []string{"amount", "currency", "paidOn", "basis"} {
		if fields[key] == "" {
			t.Errorf("%s required: %v", key, fields)
		}
	}
	in, fields := validateProjectPayment(projectPaymentRequest{Amount: 500, Currency: "PLN", PaidOn: "2026-11-14", Basis: "receipt", Note: ptr(" measurement ")})
	if len(fields) != 0 || in.AmountCents != 50000 || in.Basis != "receipt" || *in.Note != "measurement" {
		t.Errorf("valid payment: %+v %v", in, fields)
	}
	if _, fields := validateProjectPayment(projectPaymentRequest{Amount: 1, Currency: "PLN", PaidOn: "2026-11-14", Basis: "cash"}); fields["basis"] == "" {
		t.Error("unknown basis")
	}
}

func TestValidateProjectPaymentPatch(t *testing.T) {
	change, fields := validateProjectPaymentPatch(projectPaymentPatch{})
	if len(fields) != 0 || !change.empty() {
		t.Errorf("empty patch: %+v %v", change, fields)
	}
	change, fields = validateProjectPaymentPatch(projectPaymentPatch{Basis: ptr("invoice"), Currency: ptr("EUR"), Note: ptr("")})
	if len(fields) != 0 || *change.Basis != "invoice" || *change.Currency != "EUR" || *change.Note != "" {
		t.Errorf("patch: %+v %v", change, fields)
	}
	if _, fields := validateProjectPaymentPatch(projectPaymentPatch{Amount: ptr[float64](-1), Basis: ptr("x")}); fields["amount"] == "" || fields["basis"] == "" {
		t.Errorf("bad patch: %v", fields)
	}
}

func TestValidateProjectEstimate(t *testing.T) {
	if got, fields := validateProjectEstimate(projectEstimateRequest{}); got != nil || len(fields) != 0 {
		t.Errorf("clear: %v %v", got, fields)
	}
	got, fields := validateProjectEstimate(projectEstimateRequest{Amount: ptr[float64](42000.5), Currency: ptr("PLN")})
	if len(fields) != 0 || got.AmountCents != 4200050 || got.Currency != "PLN" {
		t.Errorf("set: %+v %v", got, fields)
	}
	if _, fields := validateProjectEstimate(projectEstimateRequest{Amount: ptr[float64](0), Currency: ptr("GBP")}); fields["amount"] == "" || fields["currency"] == "" {
		t.Errorf("bad: %v", fields)
	}
	if !equalProjectEstimate(nil, nil) || equalProjectEstimate(got, nil) {
		t.Error("equal")
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
		t.Error("every reason requires a comment")
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
