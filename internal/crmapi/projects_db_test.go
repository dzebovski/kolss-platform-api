package crmapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestProjectsDB runs the P1 flow against a disposable database (KOLSS_TEST_DATABASE_URL):
// create from a lead, change status, contract, payments (balance rule), permissions, cancel and
// restore, list/facets/timeline and the project-file download.
func TestProjectsDB(t *testing.T) {
	dsn := os.Getenv("KOLSS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("KOLSS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close) // registered first, so it runs after the row cleanup below
	server := &Server{pool: pool, storage: fakeDocumentStorage{size: 1200}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	var officeID uuid.UUID
	if err := pool.QueryRow(ctx, `select id from public.offices order by code limit 1`).Scan(&officeID); err != nil {
		t.Fatal(err)
	}
	newUser := func(name string) uuid.UUID {
		id := uuid.New()
		mustExec(t, pool, `insert into auth.users (id, email) values ($1, $2)`, id, id.String()+"@test.local")
		mustExec(t, pool, `insert into public.profiles (id, display_name) values ($1, $2) on conflict (id) do nothing`, id, name)
		mustExec(t, pool, `insert into public.user_office_memberships (user_id, office_id) values ($1, $2)`, id, officeID)
		return id
	}
	adminID, managerID, otherID := newUser("Admin"), newUser("Manager"), newUser("Colleague")
	admin := Actor{ID: adminID, Role: "super_admin", IsActive: true}
	manager := Actor{ID: managerID, Role: "manager", IsActive: true, OfficeIDs: map[uuid.UUID]struct{}{officeID: {}}}
	other := Actor{ID: otherID, Role: "manager", IsActive: true, OfficeIDs: map[uuid.UUID]struct{}{officeID: {}}}

	leadID := uuid.New()
	clientName := "Jakub " + leadID.String() // unique, so list filters ignore rows from earlier runs
	mustExec(t, pool, `insert into public.leads (id, office_id, external_lead_id, name, phone, assigned_to) values ($1,$2,$3,$4,'+48600000002',$5)`,
		leadID, officeID, leadID.String(), clientName, managerID)
	t.Cleanup(func() {
		mustExec(t, pool, `update public.leads set converted_project_id = null where id = $1`, leadID)
		mustExec(t, pool, `delete from public.projects where lead_id = $1`, leadID)
		mustExec(t, pool, `delete from public.lead_events where lead_id = $1`, leadID)
		mustExec(t, pool, `delete from public.leads where id = $1`, leadID)
	})

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
	fieldErr := func(res map[string]any, field string) string {
		fields, _ := res["fieldErrors"].(map[string]any)
		text, _ := fields[field].(string)
		return text
	}

	// Create: the "Paid measurement" type starts in `measure`; the lead becomes read-only (`project`).
	createKey := uuid.NewString()
	code, project := call(manager, server.handleCreateProject, http.MethodPost, "/", map[string]string{"leadId": leadID.String()},
		`{"projectType":"measure"}`, map[string]string{"Idempotency-Key": createKey})
	if code != http.StatusCreated || project["status"] != "measure" || project["code"] == "" {
		t.Fatalf("create: %d %v", code, project)
	}
	projectID := project["id"].(string)
	pid := map[string]string{"projectId": projectID}
	if perms, _ := project["permissions"].(map[string]any); perms["canEdit"] != true {
		t.Fatalf("manager must edit: %v", project["permissions"])
	}
	if code, replay := call(manager, server.handleCreateProject, http.MethodPost, "/", map[string]string{"leadId": leadID.String()},
		`{"projectType":"measure"}`, map[string]string{"Idempotency-Key": createKey}); code != http.StatusCreated || replay["id"] != projectID {
		t.Fatalf("idempotent replay: %d %v", code, replay)
	}
	if code, res := call(manager, server.handleCreateProject, http.MethodPost, "/", map[string]string{"leadId": leadID.String()}, `{}`, idem()); code != http.StatusConflict || res["code"] != "project_exists" {
		t.Fatalf("second project: %d %v", code, res)
	}
	var v2Status string
	var linked uuid.UUID
	if err := pool.QueryRow(ctx, `select v2_status, converted_project_id from public.leads where id=$1`, leadID).Scan(&v2Status, &linked); err != nil || v2Status != "project" || linked.String() != projectID {
		t.Fatalf("lead link: %s %s %v", v2Status, linked, err)
	}
	// Activities on the lead are refused, comments are not.
	activity := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.SetPathValue("leadId", leadID.String())
		req.Header.Set("Idempotency-Key", uuid.NewString())
		req = req.WithContext(context.WithValue(req.Context(), contextKey{}, admin))
		rec := httptest.NewRecorder()
		server.handleLeadActivity(rec, req)
		return rec.Code
	}
	if code := activity(`{"type":"rating","rating":"hot"}`); code != http.StatusConflict {
		t.Fatalf("activity on a project lead: %d", code)
	}

	// Permissions: a colleague can read but not change.
	if code, _ := call(other, server.handleGetProject, http.MethodGet, "/", pid, ``, nil); code != http.StatusOK {
		t.Fatalf("colleague read: %d", code)
	}
	if code, res := call(other, server.handleChangeProjectStatus, http.MethodPost, "/", pid, `{"status":"design"}`, nil); code != http.StatusForbidden || res["code"] != "project_edit_forbidden" {
		t.Fatalf("colleague edit: %d %v", code, res)
	}

	// Change status: measure needs its details; design takes an optional event.
	if code, res := call(manager, server.handleChangeProjectStatus, http.MethodPost, "/", pid, `{"status":"measure"}`, nil); code != http.StatusBadRequest || fieldErr(res, "address") == "" {
		t.Fatalf("measure without details: %d %v", code, res)
	}
	measure := `{"status":"measure","responsibleId":"` + managerID.String() + `","eventDate":"2026-10-05","eventTime":"10:00","address":"Legionowo, Parkowa 12","done":true,"result":"Measured"}`
	if code, res := call(manager, server.handleChangeProjectStatus, http.MethodPost, "/", pid, measure, nil); code != http.StatusOK || res["eventTime"] != "10:00" {
		t.Fatalf("measure details: %d %v", code, res)
	}
	if code, res := call(manager, server.handleChangeProjectStatus, http.MethodPost, "/", pid, `{"status":"design","eventDate":"2026-10-08","comment":"Handed over to design"}`, nil); code != http.StatusOK || res["status"] != "design" {
		t.Fatalf("design: %d %v", code, res)
	}
	if code, res := call(manager, server.handleChangeProjectStatus, http.MethodPost, "/", pid, `{"status":"contract"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("contract by hand: %d %v", code, res)
	}

	// Payment before a contract is refused.
	if code, res := call(manager, server.handleAddProjectPayment, http.MethodPost, "/", pid, `{"amount":100,"paidOn":"2026-10-01"}`, idem()); code != http.StatusConflict || res["code"] != "contract_required" {
		t.Fatalf("payment without contract: %d %v", code, res)
	}

	// Contract with a file: upload → contract.
	code, upload := call(manager, server.handleCreateProjectFileUpload, http.MethodPost, "/", pid, `{"kind":"contract","fileName":"contract-KW-2026-041.pdf","sizeBytes":1200}`, nil)
	if code != http.StatusOK || upload["fileId"] == nil {
		t.Fatalf("file upload: %d %v", code, upload)
	}
	fileID := upload["fileId"].(string)
	contractBody := `{"number":"KW/2026/041","signedOn":"2026-10-09","totalAmount":80000,"currency":"PLN","fileId":"` + fileID + `"}`
	code, card := call(manager, server.handleAddProjectContract, http.MethodPost, "/", pid, contractBody, idem())
	contract, _ := card["contract"].(map[string]any)
	if code != http.StatusCreated || card["status"] != "contract" || contract["number"] != "KW/2026/041" || contract["remainingAmount"] != float64(80000) {
		t.Fatalf("contract: %d %v", code, card)
	}
	if file, _ := contract["file"].(map[string]any); file["id"] != fileID {
		t.Fatalf("contract file: %v", contract)
	}
	if code, res := call(manager, server.handleAddProjectContract, http.MethodPost, "/", pid, contractBody, idem()); code != http.StatusConflict || res["code"] != "contract_exists" {
		t.Fatalf("second contract: %d %v", code, res)
	}

	// Payments: never above the remaining balance.
	if code, res := call(manager, server.handleAddProjectPayment, http.MethodPost, "/", pid, `{"amount":40000,"paidOn":"2026-10-09","note":"50% prepayment"}`, idem()); code != http.StatusCreated {
		t.Fatalf("payment 1: %d %v", code, res)
	}
	if code, res := call(manager, server.handleAddProjectPayment, http.MethodPost, "/", pid, `{"amount":45000,"paidOn":"2026-10-10"}`, idem()); code != http.StatusBadRequest || !strings.Contains(fieldErr(res, "amount"), "remaining 40 000 PLN") {
		t.Fatalf("over balance: %d %v", code, res)
	}
	code, card = call(manager, server.handleAddProjectPayment, http.MethodPost, "/", pid, `{"amount":40000,"paidOn":"2026-10-10"}`, idem())
	contract, _ = card["contract"].(map[string]any)
	if code != http.StatusCreated || contract["remainingAmount"] != float64(0) || contract["paidAmount"] != float64(80000) {
		t.Fatalf("payment 2: %d %v", code, card)
	}

	// The contract file downloads through the shared endpoint.
	if code, res := call(manager, server.handleFileDownloadURL, http.MethodGet, "/", map[string]string{"fileId": fileID}, ``, nil); code != http.StatusOK || res["url"] == nil {
		t.Fatalf("download url: %d %v", code, res)
	}

	// Cancel needs reasons; restore returns the previous status.
	if code, res := call(manager, server.handleCancelProject, http.MethodPost, "/", pid, `{"reasons":["other"]}`, nil); code != http.StatusBadRequest || fieldErr(res, "comment") == "" {
		t.Fatalf("cancel without comment: %d %v", code, res)
	}
	code, card = call(manager, server.handleCancelProject, http.MethodPost, "/", pid, `{"reasons":["postponed_renovation"],"comment":"Moving to spring","cancelFutureEvents":true}`, nil)
	cancellation, _ := card["cancellation"].(map[string]any)
	if code != http.StatusOK || card["status"] != "cancelled" || cancellation["statusBeforeCancel"] != "contract" {
		t.Fatalf("cancel: %d %v", code, card)
	}
	if code, res := call(manager, server.handleChangeProjectStatus, http.MethodPost, "/", pid, `{"status":"design"}`, nil); code != http.StatusConflict || res["code"] != "project_cancelled" {
		t.Fatalf("status on cancelled: %d %v", code, res)
	}
	code, card = call(manager, server.handleRestoreProject, http.MethodPost, "/", pid, ``, nil)
	if code != http.StatusOK || card["status"] != "contract" || card["cancellation"] != nil {
		t.Fatalf("restore: %d %v", code, card)
	}

	// List, facets and timeline.
	code, list := call(admin, server.handleListProjects, http.MethodGet, "/?"+url.Values{"status": {"contract"}, "q": {clientName}}.Encode(), nil, ``, nil)
	items, _ := list["items"].([]any)
	if code != http.StatusOK || len(items) != 1 {
		t.Fatalf("list: %d %v", code, list)
	}
	if last, _ := items[0].(map[string]any)["lastComment"].(map[string]any); last["text"] != "Moving to spring" {
		t.Fatalf("last comment: %v", items[0])
	}
	code, facets := call(admin, server.handleProjectFacets, http.MethodGet, "/?"+url.Values{"q": {clientName}}.Encode(), nil, ``, nil)
	if statuses, _ := facets["status"].(map[string]any); code != http.StatusOK || statuses["contract"] != float64(1) || facets["total"] != float64(1) {
		t.Fatalf("facets: %d %v", code, facets)
	}
	code, timeline := call(other, server.handleListProjectTimeline, http.MethodGet, "/", pid, ``, nil)
	events, _ := timeline["items"].([]any)
	// created, measure details (status_updated), design, contract, 2 payments, cancelled, restored.
	if code != http.StatusOK || len(events) != 8 || events[0].(map[string]any)["type"] != "restored" {
		t.Fatalf("timeline: %d %v", code, timeline)
	}
}
