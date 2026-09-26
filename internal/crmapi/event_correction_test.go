package crmapi

import (
	"testing"
	"time"
)

func ptr[T any](value T) *T { return &value }

func TestCorrectionKindOfEvent(t *testing.T) {
	cases := []struct {
		eventType string
		code      *string
		want      string
		ok        bool
	}{
		{"call_status_changed", ptr("reached"), v2StatusSuccess, true},
		{"call_status_changed", ptr("callback_requested"), v2StatusLater, true},
		{"call_status_changed", ptr("no_answer"), v2StatusNoAnswer, true},
		{"client_status_changed", ptr("thinking"), v2StatusThinking, true},
		{"comment_added", nil, correctionKindComment, true},
		{"client_status_changed", ptr("closed_lost"), "", false},
		{"client_status_changed", ptr("showroom_invited"), "", false},
		{"rating_changed", nil, "", false},
	}
	for _, tc := range cases {
		got, ok := correctionKindOfEvent(tc.eventType, tc.code)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s/%v: got %q %v, want %q %v", tc.eventType, tc.code, got, ok, tc.want, tc.ok)
		}
	}
}

func TestValidateEventCorrection(t *testing.T) {
	due := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	note := ptr("Client asked to call back")

	kind, _, fields := validateEventCorrection(eventCorrectionRequest{Type: ptr("later"), DueAt: &due, Reason: "Wrong button"}, v2StatusNoAnswer, nil)
	if len(fields) != 0 || kind != v2StatusLater {
		t.Fatalf("type change: kind %q fields %v", kind, fields)
	}

	_, _, fields = validateEventCorrection(eventCorrectionRequest{Type: ptr("later"), Reason: "x"}, v2StatusNoAnswer, nil)
	if fields["dueAt"] == "" {
		t.Error("later without a date must fail")
	}

	_, _, fields = validateEventCorrection(eventCorrectionRequest{Type: ptr("noanswer"), DueAt: &due, Reason: "x"}, v2StatusNoAnswer, nil)
	if fields["type"] == "" {
		t.Error("the current type must be rejected")
	}

	_, _, fields = validateEventCorrection(eventCorrectionRequest{Type: ptr("lost"), Reason: "x"}, v2StatusNoAnswer, nil)
	if fields["type"] == "" {
		t.Error("types outside the board must be rejected")
	}

	_, _, fields = validateEventCorrection(eventCorrectionRequest{Type: ptr("success"), Reason: ""}, v2StatusNoAnswer, nil)
	if fields["reason"] == "" {
		t.Error("the reason is required")
	}

	_, _, fields = validateEventCorrection(eventCorrectionRequest{Reason: "x"}, v2StatusNoAnswer, nil)
	if fields["type"] == "" {
		t.Error("a correction must change something")
	}

	_, _, fields = validateEventCorrection(eventCorrectionRequest{Type: ptr("comment"), Reason: "x"}, v2StatusNoAnswer, nil)
	if fields["comment"] == "" {
		t.Error("a comment entry needs a comment")
	}

	kind, comment, fields := validateEventCorrection(eventCorrectionRequest{Type: ptr("comment"), Reason: "x"}, v2StatusNoAnswer, note)
	if len(fields) != 0 || kind != correctionKindComment || comment == nil || *comment != *note {
		t.Errorf("status to comment keeps the existing comment: %q %v %v", kind, comment, fields)
	}

	_, _, fields = validateEventCorrection(eventCorrectionRequest{Type: ptr("success"), DueAt: &due, Reason: "x"}, v2StatusNoAnswer, nil)
	if fields["dueAt"] == "" {
		t.Error("success takes no date")
	}

	_, comment, fields = validateEventCorrection(eventCorrectionRequest{Comment: ptr("  Fixed text "), Reason: "Typo"}, correctionKindComment, note)
	if len(fields) != 0 || comment == nil || *comment != "Fixed text" {
		t.Errorf("comment-only correction: %v %v", comment, fields)
	}
}
