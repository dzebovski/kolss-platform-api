package crmapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestClientsDB runs the CL1 flow against a disposable database (KOLSS_TEST_DATABASE_URL): the
// attach trigger (phone, email, office scope, replay), match, card, edit, list and merge.
func TestClientsDB(t *testing.T) {
	dsn := os.Getenv("KOLSS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("KOLSS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	server := &Server{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	var officeID, otherOfficeID uuid.UUID
	if err := pool.QueryRow(ctx, `select id from public.offices order by code limit 1`).Scan(&officeID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select id from public.offices order by code desc limit 1`).Scan(&otherOfficeID); err != nil || otherOfficeID == officeID {
		t.Fatalf("two offices are required: %v", err)
	}
	managerID := uuid.New()
	mustExec(t, pool, `insert into auth.users (id, email) values ($1, $2)`, managerID, managerID.String()+"@test.local")
	mustExec(t, pool, `insert into public.profiles (id, display_name) values ($1, 'Client Manager') on conflict (id) do nothing`, managerID)
	mustExec(t, pool, `insert into public.user_office_memberships (user_id, office_id) values ($1, $2)`, managerID, officeID)
	admin := Actor{ID: managerID, Role: "super_admin", IsActive: true}
	manager := Actor{ID: managerID, Role: "manager", IsActive: true, OfficeIDs: map[uuid.UUID]struct{}{officeID: {}}}

	// Random numbers per run keep rows of earlier runs out of the matches.
	newPhone := func() string { return fmt.Sprintf("+48%09d", 100000000+rand.IntN(899999999)) }
	phoneA, phoneB, phoneC := newPhone(), newPhone(), newPhone()
	seed := strings.ReplaceAll(uuid.NewString(), "-", "")
	emailA := "client-" + seed[:12] + "@example.com"

	var leadIDs, clientIDs []uuid.UUID
	insertLead := func(office uuid.UUID, name, phone, email string) (leadID, clientID uuid.UUID, code string) {
		leadID = uuid.New()
		leadIDs = append(leadIDs, leadID)
		var emailArg any
		if email != "" {
			emailArg = email
		}
		defer func() { clientIDs = append(clientIDs, clientID) }()
		if err := pool.QueryRow(ctx, `
			insert into public.leads (id, office_id, source_system, external_lead_id, name, phone, email, assigned_to)
			values ($1,$2,'manual',$3,$4,$5,$6,$7) returning client_id, reference_id
		`, leadID, office, leadID.String(), name, phone, emailArg, managerID).Scan(&clientID, &code); err != nil {
			t.Fatalf("insert lead: %v", err)
		}
		return leadID, clientID, code
	}
	t.Cleanup(func() {
		mustExec(t, pool, `delete from public.leads where id = any($1::uuid[])`, leadIDs)
		mustExec(t, pool, `update public.clients set merged_into_id = null, merged_at = null where id = any($1::uuid[])`, clientIDs)
		mustExec(t, pool, `delete from public.clients where id = any($1::uuid[])`, clientIDs)
	})
	clientCount := func() int {
		var n int
		if err := pool.QueryRow(ctx, `select count(*) from public.clients`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	call := func(actor Actor, handler http.HandlerFunc, method, target string, path map[string]string, body string, headers map[string]string) (int, map[string]any) {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		for k, v := range path {
			req.SetPathValue(k, v)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		req = req.WithContext(context.WithValue(req.Context(), contextKey{}, actor))
		rec := httptest.NewRecorder()
		handler(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	idem := func() map[string]string { return map[string]string{"Idempotency-Key": uuid.NewString()} }

	// Attach rule: a new phone creates the client with the lead's code …
	leadA, clientA, codeA := insertLead(officeID, "Marta Zielinska", phoneA, emailA)
	var clientCode string
	if err := pool.QueryRow(ctx, `select code from public.clients where id = $1`, clientA).Scan(&clientCode); err != nil || clientCode != codeA {
		t.Fatalf("client code must be the first lead's code: %q vs %q (%v)", clientCode, codeA, err)
	}
	// … the same phone in another format attaches to it …
	formatted := phoneA[:3] + " " + phoneA[3:6] + " " + phoneA[6:9] + " " + phoneA[9:]
	before := clientCount()
	if _, c, _ := insertLead(officeID, "Marta Z.", formatted, ""); c != clientA || clientCount() != before {
		t.Fatalf("same phone must attach to client %s, got %s", clientA, c)
	}
	// … so does a new phone with a known email …
	if _, c, _ := insertLead(officeID, "Marta Z.", phoneB, emailA); c != clientA {
		t.Fatalf("known email must attach to client %s, got %s", clientA, c)
	}
	// … the other office gets its own client …
	if _, c, _ := insertLead(otherOfficeID, "Marta Z.", phoneA, emailA); c == clientA {
		t.Fatal("clients are office-scoped")
	}
	// … and a skipped insert (`on conflict do nothing`) creates nothing.
	before = clientCount()
	tag, err := pool.Exec(ctx, `
		insert into public.leads (office_id, source_system, external_lead_id, name, phone)
		select office_id, source_system, external_lead_id, name, '+48600999111' from public.leads where id = $1
		on conflict (source_system, external_lead_id) do nothing`, leadA)
	if err != nil || tag.RowsAffected() != 0 || clientCount() != before {
		t.Fatalf("replay must not create a client: %v %v (%d → %d)", tag, err, before, clientCount())
	}

	// Match: phone first, then email, within the office.
	match := func(actor Actor, office uuid.UUID, phone, email string) (int, map[string]any) {
		target := "/?" + url.Values{"officeId": {office.String()}, "phone": {phone}, "email": {email}}.Encode()
		return call(actor, server.handleMatchClient, http.MethodGet, target, nil, ``, nil)
	}
	code, res := match(manager, officeID, formatted, "")
	m, _ := res["match"].(map[string]any)
	if cl, _ := m["client"].(map[string]any); code != http.StatusOK || m["matchedBy"] != "phone" || cl["id"] != clientA.String() || cl["leadsCount"] != float64(3) {
		t.Fatalf("phone match: %d %v", code, res)
	}
	if _, res := match(manager, officeID, "+48 000", emailA); res["match"].(map[string]any)["matchedBy"] != "email" {
		t.Fatalf("email match: %v", res)
	}
	if _, res := match(manager, officeID, "+48 600 000 000", "nobody@example.com"); res["match"] != nil {
		t.Fatalf("unknown contact must not match: %v", res)
	}
	if code, _ := match(manager, otherOfficeID, phoneA, ""); code != http.StatusForbidden {
		t.Fatalf("another office: %d", code)
	}

	// Card: contacts, stats, leads, no duplicates yet.
	cid := map[string]string{"clientId": clientA.String()}
	code, card := call(manager, server.handleGetClient, http.MethodGet, "/", cid, ``, nil)
	if stats, _ := card["stats"].(map[string]any); code != http.StatusOK || card["code"] != codeA || stats["leadsCount"] != float64(3) || stats["projectsCount"] != float64(0) {
		t.Fatalf("card: %d %v", code, card)
	}
	if leads, _ := card["leads"].([]any); len(leads) != 3 {
		t.Fatalf("card leads: %v", card["leads"])
	}
	if perms, _ := card["permissions"].(map[string]any); perms["canEdit"] != true || perms["canMerge"] != false {
		t.Fatalf("permissions: %v", card["permissions"])
	}

	// Edit client.
	if code, res := call(manager, server.handleUpdateClient, http.MethodPatch, "/", cid, `{"phone":"+48 609 14"}`, nil); code != http.StatusBadRequest || res["code"] != "validation_error" {
		t.Fatalf("bad phone: %d %v", code, res)
	}
	code, card = call(manager, server.handleUpdateClient, http.MethodPatch, "/", cid,
		`{"name":"Marta Zielinska-Nowak","secondPhone":"+380 50 111 00 01","language":"pl","about":"Calls after 17:00"}`, nil)
	if code != http.StatusOK || card["name"] != "Marta Zielinska-Nowak" || card["secondPhone"] != "+380501110001" || card["version"] != float64(2) {
		t.Fatalf("edit: %d %v", code, card)
	}
	if code, res := call(manager, server.handleUpdateClient, http.MethodPatch, "/", cid, `{"about":"x"}`, map[string]string{"If-Match": "1"}); code != http.StatusConflict || res["code"] != "version_conflict" {
		t.Fatalf("stale version: %d %v", code, res)
	}

	// List: search by code, filters and facets.
	list := func(actor Actor, query string) []any {
		_, res := call(actor, server.handleListClients, http.MethodGet, "/?"+query, nil, ``, nil)
		items, _ := res["items"].([]any)
		return items
	}
	if items := list(manager, "q="+codeA); len(items) != 1 || items[0].(map[string]any)["id"] != clientA.String() {
		t.Fatalf("list by code: %v", items)
	}
	if items := list(manager, "q="+codeA+"&filter=with_projects"); len(items) != 0 {
		t.Fatalf("with_projects: %v", items)
	}
	if items := list(manager, "q="+codeA+"&filter=leads_only"); len(items) != 1 {
		t.Fatalf("leads_only: %v", items)
	}
	_, facets := call(manager, server.handleClientFacets, http.MethodGet, "/?q="+codeA, nil, ``, nil)
	if f, _ := facets["filter"].(map[string]any); f["all"] != float64(1) || f["leads_only"] != float64(1) || f["with_projects"] != float64(0) {
		t.Fatalf("facets: %v", facets)
	}

	// Merge: a second client with a different phone, then it is merged into the older one.
	_, clientB, codeB := insertLead(officeID, "Marta Duplicate", phoneC, "")
	if clientB == clientA {
		t.Fatal("different phone must create a separate client")
	}
	bid := map[string]string{"clientId": clientB.String()}
	mergeBody := fmt.Sprintf(`{"duplicateId":%q,"keep":{"about":"newer"}}`, clientB)
	if code, res := call(manager, server.handleMergeClients, http.MethodPost, "/", cid, mergeBody, idem()); code != http.StatusForbidden || res["code"] != "client_merge_forbidden" {
		t.Fatalf("manager merge: %d %v", code, res)
	}
	if code, _ := call(admin, server.handleMergeClients, http.MethodPost, "/", cid, mergeBody, nil); code != http.StatusBadRequest {
		t.Fatalf("merge needs an Idempotency-Key: %d", code)
	}
	mergeKey := idem()
	code, card = call(admin, server.handleMergeClients, http.MethodPost, "/", cid, mergeBody, mergeKey)
	if stats, _ := card["stats"].(map[string]any); code != http.StatusOK || card["id"] != clientA.String() || stats["leadsCount"] != float64(4) {
		t.Fatalf("merge: %d %v", code, card)
	}
	if card["secondPhone"] != "+380501110001" {
		t.Fatalf("an existing second phone is never overwritten: %v", card["secondPhone"])
	}
	if code, replay := call(admin, server.handleMergeClients, http.MethodPost, "/", cid, mergeBody, mergeKey); code != http.StatusOK || replay["id"] != clientA.String() {
		t.Fatalf("merge replay: %d %v", code, replay)
	}
	// The merged-away code resolves to the survivor.
	code, card = call(admin, server.handleGetClient, http.MethodGet, "/", bid, ``, nil)
	if rf, _ := card["redirectedFrom"].(map[string]any); code != http.StatusOK || card["id"] != clientA.String() || rf["code"] != codeB {
		t.Fatalf("redirect: %d %v", code, card)
	}
	if code, res := call(admin, server.handleUpdateClient, http.MethodPatch, "/", bid, `{"about":"x"}`, nil); code != http.StatusConflict || res["code"] != "client_merged" {
		t.Fatalf("edit of a merged client: %d %v", code, res)
	}
	var events int
	if err := pool.QueryRow(ctx, `select count(*) from public.client_events where client_id = $1 and event_type = 'merged'`, clientA).Scan(&events); err != nil || events != 1 {
		t.Fatalf("merge must be logged once: %d %v", events, err)
	}
}
