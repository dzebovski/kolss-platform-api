package validation

import (
	"net/mail"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	MaxBotTokenLength = 2048
)

type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

type LeadSubmissionInput struct {
	IdempotencyKey       string
	Name                 string
	Phone                string
	Email                *string
	City                 *string
	ProjectDescription   *string
	PrivacyAccepted      bool
	PrivacyPolicyVersion string
	PageURL              *string
	BotToken             string
	Website              *string
	RequireBotToken      bool
}

type ValidatedLeadSubmission struct {
	IdempotencyKey       uuid.UUID
	Name                 string
	Phone                string
	Email                *string
	City                 *string
	ProjectDescription   *string
	PrivacyPolicyVersion string
	PageURL              *string
	BotToken             string
	HoneypotTriggered    bool
}

func ValidateLeadSubmission(in LeadSubmissionInput) (ValidatedLeadSubmission, []FieldError, string) {
	var errs []FieldError

	id, err := uuid.Parse(strings.TrimSpace(in.IdempotencyKey))
	if err != nil {
		errs = append(errs, FieldError{Field: "idempotency_key", Message: "must be a valid UUID"})
	}

	name := strings.TrimSpace(in.Name)
	if l := utf8.RuneCountInString(name); l < 2 || l > 200 {
		errs = append(errs, FieldError{Field: "name", Message: "must be between 2 and 200 characters"})
	}

	phone := strings.TrimSpace(in.Phone)
	if l := utf8.RuneCountInString(phone); l < 7 || l > 50 {
		errs = append(errs, FieldError{Field: "phone", Message: "must be between 7 and 50 characters"})
	}

	email, emailErr := optionalTrimmed(in.Email, 254)
	if emailErr != "" {
		errs = append(errs, FieldError{Field: "email", Message: emailErr})
	} else if email != nil {
		if _, err := mail.ParseAddress(*email); err != nil {
			errs = append(errs, FieldError{Field: "email", Message: "must be a valid email address"})
		}
	}

	city, cityErr := optionalTrimmed(in.City, 200)
	if cityErr != "" {
		errs = append(errs, FieldError{Field: "city", Message: cityErr})
	}

	project, projectErr := optionalTrimmed(in.ProjectDescription, 5000)
	if projectErr != "" {
		errs = append(errs, FieldError{Field: "project_description", Message: projectErr})
	}

	privacyVersion := strings.TrimSpace(in.PrivacyPolicyVersion)
	if privacyVersion == "" || utf8.RuneCountInString(privacyVersion) > 32 {
		errs = append(errs, FieldError{Field: "privacy_policy_version", Message: "must be between 1 and 32 characters"})
	}

	pageURL, pageErr := optionalTrimmed(in.PageURL, 2048)
	if pageErr != "" {
		errs = append(errs, FieldError{Field: "page_url", Message: pageErr})
	} else if pageURL != nil {
		u, err := url.ParseRequestURI(*pageURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			errs = append(errs, FieldError{Field: "page_url", Message: "must be a valid http(s) URL"})
		}
	}

	botToken := strings.TrimSpace(in.BotToken)
	if utf8.RuneCountInString(botToken) > MaxBotTokenLength {
		errs = append(errs, FieldError{Field: "bot_token", Message: "exceeds maximum length"})
	} else if in.RequireBotToken && botToken == "" {
		errs = append(errs, FieldError{Field: "bot_token", Message: "is required"})
	}

	website := ""
	if in.Website != nil {
		website = strings.TrimSpace(*in.Website)
		if utf8.RuneCountInString(website) > 200 {
			errs = append(errs, FieldError{Field: "website", Message: "must be at most 200 characters"})
		}
	}

	if !in.PrivacyAccepted {
		return ValidatedLeadSubmission{}, errs, "consent_required"
	}

	if len(errs) > 0 {
		return ValidatedLeadSubmission{}, errs, "validation_error"
	}

	return ValidatedLeadSubmission{
		IdempotencyKey:       id,
		Name:                 name,
		Phone:                phone,
		Email:                email,
		City:                 city,
		ProjectDescription:   project,
		PrivacyPolicyVersion: privacyVersion,
		PageURL:              pageURL,
		BotToken:             botToken,
		HoneypotTriggered:    website != "",
	}, nil, ""
}

func optionalTrimmed(raw *string, max int) (*string, string) {
	if raw == nil {
		return nil, ""
	}
	v := strings.TrimSpace(*raw)
	if v == "" {
		return nil, ""
	}
	if utf8.RuneCountInString(v) > max {
		return nil, "exceeds maximum length"
	}
	return &v, ""
}

func IsAllowedSiteCode(siteCode string) bool {
	switch siteCode {
	case "kolss-pl", "kolss-ua":
		return true
	default:
		return false
	}
}

func ExternalLeadID(siteCode string, idempotencyKey uuid.UUID) string {
	return "site:" + siteCode + ":" + idempotencyKey.String()
}
