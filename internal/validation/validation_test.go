package validation_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/dzebovski/kolss-platform-api/internal/validation"
)

func strPtr(v string) *string { return &v }

func TestValidateLeadSubmission_Valid(t *testing.T) {
	id := uuid.NewString()
	got, errs, code := validation.ValidateLeadSubmission(validation.LeadSubmissionInput{
		IdempotencyKey:       id,
		Name:                 "Anna Kowalska",
		Phone:                "+48123456789",
		Email:                strPtr("anna@example.com"),
		City:                 strPtr("Warsaw"),
		ProjectDescription:   strPtr("Kitchen remodel"),
		PrivacyAccepted:      true,
		PrivacyPolicyVersion: "pl-v1",
		PageURL:              strPtr("http://localhost:4200/"),
		Website:              strPtr(""),
	})
	if code != "" || len(errs) != 0 {
		t.Fatalf("unexpected validation failure code=%s errs=%v", code, errs)
	}
	if got.HoneypotTriggered {
		t.Fatal("honeypot should not trigger for empty website")
	}
	if got.Name != "Anna Kowalska" {
		t.Fatalf("name=%q", got.Name)
	}
}

func TestValidateLeadSubmission_ConsentRequired(t *testing.T) {
	_, _, code := validation.ValidateLeadSubmission(validation.LeadSubmissionInput{
		IdempotencyKey:       uuid.NewString(),
		Name:                 "Anna",
		Phone:                "+48123456789",
		PrivacyAccepted:      false,
		PrivacyPolicyVersion: "pl-v1",
	})
	if code != "consent_required" {
		t.Fatalf("code=%s", code)
	}
}

func TestValidateLeadSubmission_Honeypot(t *testing.T) {
	got, errs, code := validation.ValidateLeadSubmission(validation.LeadSubmissionInput{
		IdempotencyKey:       uuid.NewString(),
		Name:                 "Bot",
		Phone:                "+48123456789",
		PrivacyAccepted:      true,
		PrivacyPolicyVersion: "pl-v1",
		Website:              strPtr("http://spam.example"),
	})
	if code != "" || len(errs) != 0 {
		t.Fatalf("unexpected failure code=%s errs=%v", code, errs)
	}
	if !got.HoneypotTriggered {
		t.Fatal("expected honeypot trigger")
	}
}

func TestValidateLeadSubmission_InvalidFields(t *testing.T) {
	_, errs, code := validation.ValidateLeadSubmission(validation.LeadSubmissionInput{
		IdempotencyKey:       "not-a-uuid",
		Name:                 "A",
		Phone:                "123",
		Email:                strPtr("bad"),
		PrivacyAccepted:      true,
		PrivacyPolicyVersion: "",
	})
	if code != "validation_error" {
		t.Fatalf("code=%s", code)
	}
	if len(errs) < 4 {
		t.Fatalf("expected multiple field errors, got %v", errs)
	}
}

func TestExternalLeadIDAndSiteCodes(t *testing.T) {
	id := uuid.MustParse("7c9e6679-7425-40de-944b-e07fc1f90ae7")
	got := validation.ExternalLeadID("kolss-pl", id)
	want := "site:kolss-pl:7c9e6679-7425-40de-944b-e07fc1f90ae7"
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if !validation.IsAllowedSiteCode("kolss-pl") || !validation.IsAllowedSiteCode("kolss-ua") {
		t.Fatal("expected allowed site codes")
	}
	if validation.IsAllowedSiteCode("kolss-uk") {
		t.Fatal("unexpected site code allowed")
	}
}
