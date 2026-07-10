package validation

import (
	"net/mail"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	MaxFiles           = 5
	MinFileBytes       = 1
	MaxFileBytes       = 5 * 1024 * 1024
	MaxTotalFileBytes  = 25 * 1024 * 1024
	MaxBotTokenLength  = 2048
	MaxFilenameLength  = 255
)

type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

type FileInput struct {
	ClientFileID string
	Filename     string
	ContentType  string
	SizeBytes    int64
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
	Files                []FileInput
	RequireBotToken      bool
}

type ValidatedFile struct {
	ClientFileID  uuid.UUID
	Filename      string
	ContentType   string
	SizeBytes     int64
	Extension     string
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
	Files                []ValidatedFile
}

type CompleteFileInput struct {
	FileID string
	ETag   *string
}

type ValidatedCompleteFile struct {
	FileID uuid.UUID
	ETag   *string
}

var allowedContentTypes = map[string]string{
	".pdf":  "application/pdf",
	".txt":  "text/plain",
	".csv":  "text/csv",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
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

	files, fileErrs := validateFiles(in.Files)
	errs = append(errs, fileErrs...)

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
		Files:                files,
	}, nil, ""
}

func ValidateCompleteFiles(files []CompleteFileInput) ([]ValidatedCompleteFile, []FieldError, string) {
	if len(files) > MaxFiles {
		return nil, []FieldError{{Field: "files", Message: "must contain at most 5 items"}}, "validation_error"
	}
	seen := make(map[uuid.UUID]struct{}, len(files))
	out := make([]ValidatedCompleteFile, 0, len(files))
	var errs []FieldError
	for i, f := range files {
		prefix := "files[" + itoa(i) + "]"
		id, err := uuid.Parse(strings.TrimSpace(f.FileID))
		if err != nil {
			errs = append(errs, FieldError{Field: prefix + ".file_id", Message: "must be a valid UUID"})
			continue
		}
		if _, ok := seen[id]; ok {
			errs = append(errs, FieldError{Field: prefix + ".file_id", Message: "must be unique"})
			continue
		}
		seen[id] = struct{}{}
		var etag *string
		if f.ETag != nil {
			v := strings.TrimSpace(*f.ETag)
			if utf8.RuneCountInString(v) > 256 {
				errs = append(errs, FieldError{Field: prefix + ".etag", Message: "exceeds maximum length"})
				continue
			}
			if v != "" {
				etag = &v
			}
		}
		out = append(out, ValidatedCompleteFile{FileID: id, ETag: etag})
	}
	if len(errs) > 0 {
		return nil, errs, "validation_error"
	}
	return out, nil, ""
}

func validateFiles(files []FileInput) ([]ValidatedFile, []FieldError) {
	if len(files) == 0 {
		return nil, nil
	}
	if len(files) > MaxFiles {
		return nil, []FieldError{{Field: "files", Message: "must contain at most 5 items"}}
	}

	var errs []FieldError
	seen := make(map[uuid.UUID]struct{}, len(files))
	out := make([]ValidatedFile, 0, len(files))
	var total int64

	for i, f := range files {
		prefix := "files[" + itoa(i) + "]"
		clientID, err := uuid.Parse(strings.TrimSpace(f.ClientFileID))
		if err != nil {
			errs = append(errs, FieldError{Field: prefix + ".client_file_id", Message: "must be a valid UUID"})
			continue
		}
		if _, ok := seen[clientID]; ok {
			errs = append(errs, FieldError{Field: prefix + ".client_file_id", Message: "must be unique"})
			continue
		}
		seen[clientID] = struct{}{}

		filename := path.Base(strings.TrimSpace(f.Filename))
		filename = strings.ReplaceAll(filename, "\x00", "")
		if filename == "" || filename == "." || filename == ".." {
			errs = append(errs, FieldError{Field: prefix + ".filename", Message: "is required"})
			continue
		}
		if utf8.RuneCountInString(filename) > MaxFilenameLength {
			errs = append(errs, FieldError{Field: prefix + ".filename", Message: "exceeds maximum length"})
			continue
		}

		ext := strings.ToLower(path.Ext(filename))
		expectedType, ok := allowedContentTypes[ext]
		if !ok {
			errs = append(errs, FieldError{Field: prefix + ".filename", Message: "unsupported file type"})
			continue
		}

		declaredType := strings.ToLower(strings.TrimSpace(f.ContentType))
		if declaredType == "" {
			errs = append(errs, FieldError{Field: prefix + ".content_type", Message: "is required"})
			continue
		}
		if declaredType != expectedType {
			errs = append(errs, FieldError{Field: prefix + ".content_type", Message: "does not match file extension"})
			continue
		}

		if f.SizeBytes < MinFileBytes || f.SizeBytes > MaxFileBytes {
			errs = append(errs, FieldError{Field: prefix + ".size_bytes", Message: "must be between 1 and 5242880 bytes"})
			continue
		}
		total += f.SizeBytes

		out = append(out, ValidatedFile{
			ClientFileID: clientID,
			Filename:     filename,
			ContentType:  expectedType,
			SizeBytes:    f.SizeBytes,
			Extension:    ext,
		})
	}

	if total > MaxTotalFileBytes {
		errs = append(errs, FieldError{Field: "files", Message: "total declared size exceeds 25 MiB"})
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return out, nil
}

func NormalizedContentType(ext string) (string, bool) {
	ct, ok := allowedContentTypes[strings.ToLower(ext)]
	return ct, ok
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

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
