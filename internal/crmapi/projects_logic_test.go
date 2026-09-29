package crmapi

import (
	"testing"

	"github.com/google/uuid"
)

func TestProjectFirstStatus(t *testing.T) {
	for typ, want := range map[string]string{"express": "express", "measure": "measure", "contract": "design", "bogus": "none"} {
		typ := typ
		if got := projectFirstStatus(&typ); got != want {
			t.Errorf("type %q: got %q want %q", typ, got, want)
		}
	}
	if got := projectFirstStatus(nil); got != "none" {
		t.Errorf("no type: got %q", got)
	}
}

func TestValidateProjectStatus(t *testing.T) {
	manager := uuid.NewString()
	yes, no := true, false
	cases := []struct {
		name   string
		req    projectStatusRequest
		fields []string
	}{
		{"contract is not pickable", projectStatusRequest{Status: "contract"}, []string{"status"}},
		{"cancelled is not pickable", projectStatusRequest{Status: "cancelled"}, []string{"status"}},
		{"express needs responsible, what, deadline", projectStatusRequest{Status: "express"}, []string{"responsibleId", "eventDate", "what"}},
		{"express done needs a result", projectStatusRequest{Status: "express", ResponsibleID: &manager, What: ptr("Estimate"), EventDate: ptr("2026-10-02"), Done: &yes}, []string{"result"}},
		{"express ok", projectStatusRequest{Status: "express", ResponsibleID: &manager, What: ptr("Estimate"), EventDate: ptr("2026-10-02"), Done: &no}, nil},
		{"measure needs time and address", projectStatusRequest{Status: "measure", ResponsibleID: &manager, EventDate: ptr("2026-10-02")}, []string{"eventTime", "address"}},
		{"measure ok", projectStatusRequest{Status: "measure", ResponsibleID: &manager, EventDate: ptr("2026-10-02"), EventTime: ptr("10:30"), Address: ptr("Legionowo")}, nil},
		{"design event is optional", projectStatusRequest{Status: "design"}, nil},
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

func TestValidateProjectCancel(t *testing.T) {
	if _, _, fields := validateProjectCancel(projectCancelRequest{}); fields["reasons"] == "" {
		t.Error("at least one reason is required")
	}
	if _, _, fields := validateProjectCancel(projectCancelRequest{Reasons: []string{"other"}}); fields["comment"] == "" {
		t.Error("other requires a comment")
	}
	reasons, comment, fields := validateProjectCancel(projectCancelRequest{Reasons: []string{"price_too_high", "price_too_high", "not_relevant"}})
	if len(fields) != 0 || len(reasons) != 2 || comment != nil {
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
