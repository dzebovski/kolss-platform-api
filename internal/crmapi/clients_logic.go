package crmapi

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// CRM v2 clients (task CL1). Owner decisions of 2026-09-29 (Q-CL2, Q-CL5…Q-CL7) and the behaviour
// spec of the "KOLSS CRM v2" boards (Clients-list, Client-card, Client-edit, Client-merge,
// Create-lead-existing-client). This file is the pure part: validation, matching keys and the
// merge field rules, unit-tested without a database.

const (
	maxClientNameLength    = 200
	maxClientEmailLength   = 254
	maxClientAddressLength = 300
	maxClientAboutLength   = 2000

	clientKeepOlder = "older"
	clientKeepNewer = "newer"
)

var clientLanguages = map[string]struct{}{"pl": {}, "uk": {}, "en": {}, "ru": {}}

var (
	clientPhonePattern = regexp.MustCompile(`^\+(48[0-9]{9}|380[0-9]{9})$`)
	clientEmailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// Actor permissions. Editing the contact data is open to everyone with office access (the boards
// draw no restriction); merging is irreversible, so only admins do it (board spec).

func (a Actor) CanEditClient(officeID uuid.UUID) bool { return a.CanAccessOffice(officeID) }

func (a Actor) CanMergeClients(officeID uuid.UUID) bool {
	return a.IsSuperAdmin() || (a.Role == "office_admin" && a.CanAccessOffice(officeID))
}

// normalizeClientPhone accepts the Create lead masks (+48 and +380, Q-CL5) with spaces, dashes,
// dots or brackets, and a leading 00 instead of +. It returns the compact "+48601334812" form.
func normalizeClientPhone(raw string) (string, bool) {
	var b strings.Builder
	for i, r := range strings.TrimSpace(raw) {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && i == 0:
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '.' || r == '(' || r == ')' || r == ' ':
		default:
			return "", false
		}
	}
	compact := b.String()
	if strings.HasPrefix(compact, "00") {
		compact = "+" + compact[2:]
	}
	if !clientPhonePattern.MatchString(compact) {
		return "", false
	}
	return compact, true
}

// clientPhoneKey mirrors public.client_phone_key (migration 20261023120000): the last 9 digits of
// a number with at least 9 digits, "" otherwise.
func clientPhoneKey(phone string) string {
	var digits []rune
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			digits = append(digits, r)
		}
	}
	if len(digits) < 9 {
		return ""
	}
	return string(digits[len(digits)-9:])
}

func validClientEmail(email string) bool {
	return len(email) <= maxClientEmailLength && clientEmailPattern.MatchString(email)
}

// updateClientRequest is PATCH /v1/clients/{clientId}. A field that is absent stays unchanged; an
// explicit null (or empty text) clears the optional ones.
type updateClientRequest struct {
	Name        optional[string] `json:"name"`
	Phone       optional[string] `json:"phone"`
	SecondPhone optional[string] `json:"secondPhone"`
	Email       optional[string] `json:"email"`
	Address     optional[string] `json:"address"`
	Language    optional[string] `json:"language"`
	About       optional[string] `json:"about"`
}

// clientPatchColumns is the update order; column names are fixed here, never taken from input.
var clientPatchColumns = []string{"name", "phone", "second_phone", "email", "address", "language", "about"}

// clientPatch maps a column to its new value (nil = SQL null). Only changed-by-request columns
// are present.
type clientPatch map[string]*string

func validateUpdateClient(req updateClientRequest) (clientPatch, map[string]string) {
	fields := map[string]string{}
	patch := clientPatch{}
	text := func(o optional[string]) *string {
		if !o.Set || o.Value == nil {
			return nil
		}
		value := strings.TrimSpace(*o.Value)
		if value == "" {
			return nil
		}
		return &value
	}
	tooLong := func(value *string, limit int) bool {
		return value != nil && utf8.RuneCountInString(*value) > limit
	}

	if req.Name.Set {
		name := text(req.Name)
		switch {
		case name == nil:
			fields["name"] = "Required"
		case tooLong(name, maxClientNameLength):
			fields["name"] = "Must be at most 200 characters"
		default:
			patch["name"] = name
		}
	}
	for _, item := range []struct {
		key    string
		column string
		value  optional[string]
		must   bool
	}{
		{"phone", "phone", req.Phone, true},
		{"secondPhone", "second_phone", req.SecondPhone, false},
	} {
		if !item.value.Set {
			continue
		}
		value := text(item.value)
		if value == nil {
			if item.must {
				fields[item.key] = "Required"
			} else {
				patch[item.column] = nil
			}
			continue
		}
		phone, ok := normalizeClientPhone(*value)
		if !ok {
			fields[item.key] = "Must be a +48 or +380 number with 9 digits after the country code"
			continue
		}
		patch[item.column] = &phone
	}
	if req.Email.Set {
		email := text(req.Email)
		if email != nil && !validClientEmail(*email) {
			fields["email"] = "Must be a valid email address"
		} else {
			patch["email"] = email
		}
	}
	if req.Address.Set {
		address := text(req.Address)
		if tooLong(address, maxClientAddressLength) {
			fields["address"] = "Must be at most 300 characters"
		} else {
			patch["address"] = address
		}
	}
	if req.Language.Set {
		language := text(req.Language)
		if language != nil {
			if _, ok := clientLanguages[*language]; !ok {
				fields["language"] = "Must be pl, uk, en, or ru"
			}
		}
		if fields["language"] == "" {
			patch["language"] = language
		}
	}
	if req.About.Set {
		about := text(req.About)
		if tooLong(about, maxClientAboutLength) {
			fields["about"] = "Must be at most 2000 characters"
		} else {
			patch["about"] = about
		}
	}
	if len(fields) == 0 && len(patch) == 0 {
		fields["body"] = "Nothing to change"
	}
	return patch, fields
}

// clientContact is the part of a client the merge combines.
type clientContact struct {
	Name        string
	Phone       *string
	SecondPhone *string
	Email       *string
	Address     *string
	Language    *string
	About       *string
}

type mergeClientsRequest struct {
	DuplicateID uuid.UUID `json:"duplicateId"`
	Keep        struct {
		Email   *string `json:"email"`
		Address *string `json:"address"`
		About   *string `json:"about"`
	} `json:"keep"`
}

// mergeKeep holds, per field, which client's value survives ("older" or "newer").
type mergeKeep struct {
	Email, Address, About string
}

func validateMergeClients(req mergeClientsRequest, pathClientID uuid.UUID) (mergeKeep, map[string]string) {
	fields := map[string]string{}
	if req.DuplicateID == uuid.Nil {
		fields["duplicateId"] = "Required"
	} else if req.DuplicateID == pathClientID {
		fields["duplicateId"] = "Must be a different client"
	}
	keep := mergeKeep{Email: clientKeepOlder, Address: clientKeepOlder, About: clientKeepOlder}
	for _, item := range []struct {
		key   string
		in    *string
		field *string
	}{
		{"keep.email", req.Keep.Email, &keep.Email},
		{"keep.address", req.Keep.Address, &keep.Address},
		{"keep.about", req.Keep.About, &keep.About},
	} {
		if item.in == nil || strings.TrimSpace(*item.in) == "" {
			continue
		}
		switch value := strings.TrimSpace(*item.in); value {
		case clientKeepOlder, clientKeepNewer:
			*item.field = value
		default:
			fields[item.key] = "Must be older or newer"
		}
	}
	return keep, fields
}

// mergeClientContacts is the field rule of Merge clients (board Client-merge): the older client
// survives with its name and phone; email, address and note follow the per-field choice. Nothing
// is lost silently: the newer client's phone (or second phone) moves into an empty second phone
// when it is a different number, and an empty language is taken from the newer client.
func mergeClientContacts(older, newer clientContact, keep mergeKeep) clientContact {
	pick := func(choice string, a, b *string) *string {
		if choice == clientKeepNewer {
			return b
		}
		return a
	}
	merged := older
	merged.Email = pick(keep.Email, older.Email, newer.Email)
	merged.Address = pick(keep.Address, older.Address, newer.Address)
	merged.About = pick(keep.About, older.About, newer.About)
	if merged.Language == nil {
		merged.Language = newer.Language
	}
	if merged.SecondPhone == nil && older.Phone != nil {
		known := map[string]struct{}{}
		for _, phone := range []*string{older.Phone, older.SecondPhone} {
			if phone != nil {
				known[clientPhoneKey(*phone)] = struct{}{}
			}
		}
		for _, phone := range []*string{newer.Phone, newer.SecondPhone} {
			if phone == nil {
				continue
			}
			if _, same := known[clientPhoneKey(*phone)]; same {
				continue
			}
			merged.SecondPhone = phone
			break
		}
	}
	return merged
}

// clientFilters are the Clients list chips.
const (
	clientFilterAll          = "all"
	clientFilterWithProjects = "with_projects"
	clientFilterLeadsOnly    = "leads_only"
)

func validClientFilter(value string) bool {
	switch value {
	case "", clientFilterAll, clientFilterWithProjects, clientFilterLeadsOnly:
		return true
	default:
		return false
	}
}

// escapeLike makes user text literal inside an ILIKE pattern.
func escapeLike(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

func onlyDigits(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
