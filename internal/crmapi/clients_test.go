package crmapi

import (
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestNormalizeClientPhone(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"+48 601 334 812", "+48601334812", true},
		{"+48601334812", "+48601334812", true},
		{"0048 601-334-812", "+48601334812", true},
		{"+380 50 111 00 01", "+380501110001", true},
		{"+380 (50) 111-00-01", "+380501110001", true},
		{"601334812", "", false},        // no country code
		{"+48 609 14", "", false},       // too short (board error state)
		{"+49 601 334 812", "", false},  // only +48 and +380
		{"+48 601 334 8123", "", false}, // too long
		{"+48 abc 334 812", "", false},  // letters
		{"", "", false},
	}
	for _, tt := range tests {
		got, ok := normalizeClientPhone(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("normalizeClientPhone(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestClientPhoneKeyMatchesAcrossFormats(t *testing.T) {
	key := clientPhoneKey("+48 601 334 812")
	for _, phone := range []string{"601334812", "0048601334812", "+48601334812", "601-334-812"} {
		if clientPhoneKey(phone) != key {
			t.Errorf("%q should share the key %q, got %q", phone, key, clientPhoneKey(phone))
		}
	}
	if clientPhoneKey("+48 601 334 813") == key {
		t.Error("different numbers must not share a key")
	}
	if clientPhoneKey("12345678") != "" || clientPhoneKey("") != "" {
		t.Error("fewer than 9 digits never match")
	}
}

func TestValidateUpdateClient(t *testing.T) {
	str := func(value string) optional[string] { return optional[string]{Set: true, Value: &value} }
	null := optional[string]{Set: true}

	patch, fields := validateUpdateClient(updateClientRequest{
		Name: str("  Jakub Michalski "), Phone: str("+48 609 145 882"), SecondPhone: str("+48 511 402 667"),
		Email: str("J.Michalski@wp.pl"), Address: str("Legionowo"), Language: str("pl"), About: str("Calls after 17:00"),
	})
	if len(fields) != 0 {
		t.Fatalf("valid edit rejected: %#v", fields)
	}
	if *patch["name"] != "Jakub Michalski" || *patch["phone"] != "+48609145882" || *patch["second_phone"] != "+48511402667" {
		t.Fatalf("values must be trimmed and phones compacted: %#v", patch)
	}

	patch, fields = validateUpdateClient(updateClientRequest{SecondPhone: null, Email: str("  "), About: null})
	if len(fields) != 0 || len(patch) != 3 || patch["second_phone"] != nil || patch["email"] != nil || patch["about"] != nil {
		t.Fatalf("null / empty text clears an optional field: %#v %#v", patch, fields)
	}

	_, fields = validateUpdateClient(updateClientRequest{
		Name: str(" "), Phone: str("+48 609 14"), SecondPhone: str("123"), Email: str("jakub@wp"),
		Language: str("de"), Address: str(strings.Repeat("a", maxClientAddressLength+1)),
		About: str(strings.Repeat("a", maxClientAboutLength+1)),
	})
	for _, field := range []string{"name", "phone", "secondPhone", "email", "language", "address", "about"} {
		if fields[field] == "" {
			t.Errorf("expected an error for %s, got %#v", field, fields)
		}
	}
	if _, fields := validateUpdateClient(updateClientRequest{Phone: null}); fields["phone"] == "" {
		t.Error("phone cannot be cleared")
	}
	if _, fields := validateUpdateClient(updateClientRequest{}); fields["body"] == "" {
		t.Error("an empty edit must be rejected")
	}
}

func TestUpdateClientRequestDistinguishesAbsentFromNull(t *testing.T) {
	var req updateClientRequest
	if err := json.Unmarshal([]byte(`{"email": null, "name": "Ola"}`), &req); err != nil {
		t.Fatal(err)
	}
	if !req.Email.Set || req.Email.Value != nil || !req.Name.Set || req.Phone.Set {
		t.Fatalf("unexpected decode: %#v", req)
	}
}

func TestMergeClientContacts(t *testing.T) {
	ptr := func(value string) *string { return &value }
	older := clientContact{Name: "Jakub Michalski", Phone: ptr("+48609145882"), Email: ptr("old@wp.pl"), Address: ptr("Legionowo"), About: ptr("Calls after 17:00")}
	newer := clientContact{Name: "Jakub M.", Phone: ptr("+48 511 402 667"), Email: ptr("new@wp.pl"), Address: nil, About: ptr("Decides with his wife"), Language: ptr("pl")}

	merged := mergeClientContacts(older, newer, mergeKeep{Email: clientKeepNewer, Address: clientKeepOlder, About: clientKeepNewer})
	if merged.Name != "Jakub Michalski" || *merged.Phone != "+48609145882" {
		t.Errorf("name and phone stay with the older client: %#v", merged)
	}
	if *merged.Email != "new@wp.pl" || *merged.Address != "Legionowo" || *merged.About != "Decides with his wife" {
		t.Errorf("per-field choice not applied: %#v", merged)
	}
	if merged.SecondPhone == nil || *merged.SecondPhone != "+48 511 402 667" {
		t.Errorf("the newer client's different number must survive as second phone: %#v", merged.SecondPhone)
	}
	if merged.Language == nil || *merged.Language != "pl" {
		t.Errorf("an empty language is taken from the newer client: %#v", merged.Language)
	}

	// Same number in a different format is not a second phone.
	same := mergeClientContacts(older, clientContact{Phone: ptr("609 145 882")}, mergeKeep{})
	if same.SecondPhone != nil {
		t.Errorf("same phone must not become a second phone: %#v", same.SecondPhone)
	}
	// An existing second phone is never overwritten.
	withSecond := older
	withSecond.SecondPhone = ptr("+48500000000")
	if got := mergeClientContacts(withSecond, newer, mergeKeep{}); *got.SecondPhone != "+48500000000" {
		t.Errorf("existing second phone overwritten: %#v", got.SecondPhone)
	}
}

func TestValidateMergeClients(t *testing.T) {
	path, other := uuid.New(), uuid.New()
	keep, fields := validateMergeClients(mergeClientsRequest{DuplicateID: other}, path)
	if len(fields) != 0 || keep != (mergeKeep{Email: "older", Address: "older", About: "older"}) {
		t.Fatalf("defaults keep the older client's values: %#v %#v", keep, fields)
	}
	newer := "newer"
	var req mergeClientsRequest
	req.DuplicateID = other
	req.Keep.About = &newer
	if keep, fields := validateMergeClients(req, path); len(fields) != 0 || keep.About != "newer" {
		t.Fatalf("newer choice rejected: %#v %#v", keep, fields)
	}
	bad := "both"
	req.Keep.Email = &bad
	if _, fields := validateMergeClients(req, path); fields["keep.email"] == "" {
		t.Error("unknown choice must be rejected")
	}
	if _, fields := validateMergeClients(mergeClientsRequest{DuplicateID: path}, path); fields["duplicateId"] == "" {
		t.Error("a client cannot be merged into itself")
	}
	if _, fields := validateMergeClients(mergeClientsRequest{}, path); fields["duplicateId"] == "" {
		t.Error("duplicateId is required")
	}
}

func TestClientPermissions(t *testing.T) {
	office, otherOffice := uuid.New(), uuid.New()
	member := func(role string) Actor {
		return Actor{ID: uuid.New(), Role: role, OfficeIDs: map[uuid.UUID]struct{}{office: {}}}
	}
	if !member("manager").CanEditClient(office) || member("manager").CanEditClient(otherOffice) {
		t.Error("office members edit clients of their own office only")
	}
	if member("manager").CanMergeClients(office) || member("curator").CanMergeClients(office) {
		t.Error("only admins merge")
	}
	if !member("office_admin").CanMergeClients(office) || member("office_admin").CanMergeClients(otherOffice) {
		t.Error("an office admin merges inside the office")
	}
	if !(Actor{Role: "super_admin"}).CanMergeClients(otherOffice) {
		t.Error("a super admin merges everywhere")
	}
}

func TestClientListWhere(t *testing.T) {
	office := uuid.New()
	manager := Actor{ID: uuid.New(), Role: "manager", OfficeIDs: map[uuid.UUID]struct{}{office: {}}}

	where, args, err := clientListWhere(manager, url.Values{"filter": {"with_projects"}, "q": {"+48 601_33%"}}, "")
	if err != nil {
		t.Fatalf("unexpected error: %#v", err)
	}
	joined := strings.Join(where, " and ")
	for _, fragment := range []string{"merged_into_id is null", "c.office_id = any(", "exists (select 1 from public.projects", "ilike"} {
		if !strings.Contains(joined, fragment) {
			t.Errorf("missing %q in %s", fragment, joined)
		}
	}
	if len(args) != 3 || args[1] != `%+48 601\_33\%%` {
		t.Errorf("LIKE wildcards must be escaped, digits search added: %#v", args)
	}

	where, _, _ = clientListWhere(manager, url.Values{"filter": {"leads_only"}}, "filter")
	if strings.Contains(strings.Join(where, " "), "projects") {
		t.Error("facets count each chip without the chip filter")
	}
	if _, _, err := clientListWhere(manager, url.Values{"filter": {"bogus"}}, ""); err == nil || err.status != 400 {
		t.Error("unknown filter must be a validation error")
	}
	if _, _, err := clientListWhere(manager, url.Values{"officeId": {uuid.NewString()}}, ""); err == nil || err.status != 403 {
		t.Error("another office is forbidden")
	}
}

// The migration cannot run without a database here; this pins the rules the API relies on.
func TestClientsMigrationShape(t *testing.T) {
	content, err := os.ReadFile("../../supabase/migrations/20261023120000_clients.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(content)
	for _, fragment := range []string{
		"create table if not exists public.clients",
		"add column if not exists client_id uuid references public.clients",
		"alter table public.leads disable trigger leads_updated_at",
		"alter table public.leads enable trigger leads_updated_at",
		"where l.client_id is null",
		"order by l.created_at, l.id",
		"create trigger leads_v2_client_attach",
		"before insert on public.leads",
		"l.source_system = new.source_system and l.external_lead_id = new.external_lead_id",
		"grant select, insert, update on public.clients to kolss_api",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("clients migration missing %q", fragment)
		}
	}
	for _, forbidden := range []string{"drop table", "drop column", "alter column", "delete from public.leads", "rename"} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("clients migration must stay additive: found %q", forbidden)
		}
	}
}
