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

	// Create: every project starts as `estimate` whatever the type; the lead becomes read-only (`project`).
	createKey := uuid.NewString()
	code, project := call(manager, server.handleCreateProject, http.MethodPost, "/", map[string]string{"leadId": leadID.String()},
		`{"projectType":"measure"}`, map[string]string{"Idempotency-Key": createKey})
	if code != http.StatusCreated || project["status"] != "estimate" || project["code"] == "" {
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

	money := func(card map[string]any, kind, field string) float64 {
		contracts, _ := card["contracts"].(map[string]any)
		contract, _ := contracts[kind].(map[string]any)
		value, _ := contract[field].(float64)
		return value
	}
	eventTypes := func(actor Actor) []string {
		_, timeline := call(actor, server.handleListProjectTimeline, http.MethodGet, "/", pid, ``, nil)
		events, _ := timeline["items"].([]any)
		types := make([]string, 0, len(events))
		for _, event := range events {
			types = append(types, event.(map[string]any)["type"].(string))
		}
		return types
	}

	// Estimate: four optional lines, totals per currency, null removes, unknown lines are refused.
	if code, res := call(manager, server.handleUpdateProjectEstimate, http.MethodPut, "/", pid, `{"lines":{"garage":{"amount":1,"currency":"PLN"}}}`, nil); code != http.StatusBadRequest || fieldErr(res, "lines.garage") == "" {
		t.Fatalf("unknown estimate line: %d %v", code, res)
	}
	if code, res := call(other, server.handleUpdateProjectEstimate, http.MethodPut, "/", pid, `{"lines":{"kitchen":{"amount":1,"currency":"PLN"}}}`, nil); code != http.StatusForbidden {
		t.Fatalf("colleague estimate: %d %v", code, res)
	}
	code, card := call(manager, server.handleUpdateProjectEstimate, http.MethodPut, "/", pid,
		`{"lines":{"kitchen":{"amount":50000,"currency":"PLN","note":"Oak"},"appliances":{"amount":10000,"currency":"PLN"},"countertop":{"amount":3000,"currency":"EUR"}}}`, nil)
	estimate, _ := card["estimate"].(map[string]any)
	totals, _ := estimate["totals"].([]any)
	if code != http.StatusOK || len(totals) != 2 || estimate["locked"] != false {
		t.Fatalf("estimate: %d %v", code, card)
	}
	code, card = call(manager, server.handleUpdateProjectEstimate, http.MethodPut, "/", pid, `{"lines":{"appliances":null}}`, nil)
	if lines, _ := card["estimate"].(map[string]any)["lines"].(map[string]any); code != http.StatusOK || lines["appliances"] != nil || lines["kitchen"] == nil {
		t.Fatalf("estimate remove: %d %v", code, card)
	}

	// Intro contract: automatic stage move, amount is free (the office default only prefills).
	introBody := `{"kind":"intro","number":"KW/2026/041-I","signedOn":"2026-10-02","totalAmount":500,"currency":"PLN"}`
	code, card = call(manager, server.handleAddProjectContract, http.MethodPost, "/", pid, introBody, idem())
	if code != http.StatusCreated || card["status"] != "intro_contract" || money(card, "intro", "remainingAmount") != 500 {
		t.Fatalf("intro contract: %d %v", code, card)
	}
	if _, present := card["introDefault"]; !present {
		t.Fatalf("introDefault must always be present: %v", card)
	}
	if code, res := call(manager, server.handleAddProjectContract, http.MethodPost, "/", pid, introBody, idem()); code != http.StatusConflict || res["code"] != "contract_exists" {
		t.Fatalf("second intro: %d %v", code, res)
	}
	// With no main contract yet, a payment without contractId goes to the intro contract; it never moves the status.
	code, card = call(manager, server.handleAddProjectPayment, http.MethodPost, "/", pid, `{"amount":500,"paidOn":"2026-10-02"}`, idem())
	if code != http.StatusCreated || card["status"] != "intro_contract" || money(card, "intro", "paidAmount") != 500 {
		t.Fatalf("intro payment: %d %v", code, card)
	}
	if code, res := call(manager, server.handleAddProjectPayment, http.MethodPost, "/", pid, `{"amount":1,"paidOn":"2026-10-03"}`, idem()); code != http.StatusBadRequest || fieldErr(res, "amount") == "" {
		t.Fatalf("over the intro balance: %d %v", code, res)
	}

	// Change status: by hand, both directions; contract stages and cancelled are not pickable.
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
	if code, res := call(manager, server.handleChangeProjectStatus, http.MethodPost, "/", pid, `{"status":"estimate"}`, nil); code != http.StatusOK || res["status"] != "estimate" {
		t.Fatalf("backwards: %d %v", code, res)
	}
	for _, status := range []string{"intro_contract", "main_contract", "cancelled", "express", "contract", "none"} {
		if code, res := call(manager, server.handleChangeProjectStatus, http.MethodPost, "/", pid, `{"status":"`+status+`"}`, nil); code != http.StatusBadRequest || fieldErr(res, "status") == "" {
			t.Fatalf("status %s by hand: %d %v", status, code, res)
		}
	}
	if code, res := call(manager, server.handleChangeProjectStatus, http.MethodPost, "/", pid, `{"status":"design"}`, nil); code != http.StatusOK || res["status"] != "design" {
		t.Fatalf("design again: %d %v", code, res)
	}

	// Main contract with a file: upload -> contract; status main_contract; estimate locks; the paid intro is credited.
	code, upload := call(manager, server.handleCreateProjectFileUpload, http.MethodPost, "/", pid, `{"kind":"contract","fileName":"contract-KW-2026-041.pdf","sizeBytes":1200}`, nil)
	if code != http.StatusOK || upload["fileId"] == nil {
		t.Fatalf("file upload: %d %v", code, upload)
	}
	fileID := upload["fileId"].(string)
	contractBody := `{"kind":"main","number":"KW/2026/041","signedOn":"2026-10-09","totalAmount":80000,"currency":"PLN","fileId":"` + fileID + `"}`
	code, card = call(manager, server.handleAddProjectContract, http.MethodPost, "/", pid, contractBody, idem())
	contract, _ := card["contract"].(map[string]any)
	if code != http.StatusCreated || card["status"] != "main_contract" || contract["number"] != "KW/2026/041" ||
		money(card, "main", "creditedAmount") != 500 || money(card, "main", "remainingAmount") != 79500 || money(card, "main", "target50Amount") != 40000 {
		t.Fatalf("main contract: %d %v", code, card)
	}
	if file, _ := contract["file"].(map[string]any); file["id"] != fileID {
		t.Fatalf("contract file: %v", contract)
	}
	if estimate, _ := card["estimate"].(map[string]any); estimate["locked"] != true {
		t.Fatalf("estimate must lock: %v", card["estimate"])
	}
	if code, res := call(manager, server.handleUpdateProjectEstimate, http.MethodPut, "/", pid, `{"lines":{"kitchen":null}}`, nil); code != http.StatusConflict || res["code"] != "estimate_locked" {
		t.Fatalf("estimate after main contract: %d %v", code, res)
	}
	if code, res := call(manager, server.handleAddProjectContract, http.MethodPost, "/", pid, contractBody, idem()); code != http.StatusConflict || res["code"] != "contract_exists" {
		t.Fatalf("second main contract: %d %v", code, res)
	}

	// Payments on the main contract: never above the remaining balance (the credit counts).
	if code, res := call(manager, server.handleAddProjectPayment, http.MethodPost, "/", pid, `{"amount":39500,"paidOn":"2026-10-09","note":"prepayment"}`, idem()); code != http.StatusCreated {
		t.Fatalf("payment 1: %d %v", code, res)
	}
	if code, res := call(manager, server.handleAddProjectPayment, http.MethodPost, "/", pid, `{"amount":45000,"paidOn":"2026-10-10"}`, idem()); code != http.StatusBadRequest || !strings.Contains(fieldErr(res, "amount"), "remaining 40 000 PLN") {
		t.Fatalf("over balance: %d %v", code, res)
	}
	code, card = call(manager, server.handleAddProjectPayment, http.MethodPost, "/", pid, `{"amount":40000,"paidOn":"2026-10-10"}`, idem())
	if code != http.StatusCreated || money(card, "main", "remainingAmount") != 0 || money(card, "main", "paidAmount") != 79500 {
		t.Fatalf("payment 2: %d %v", code, card)
	}
	payments, _ := card["payments"].([]any)
	if len(payments) != 3 || payments[2].(map[string]any)["contractKind"] != "main" {
		t.Fatalf("payments: %v", card["payments"])
	}
	lastPaymentID := payments[2].(map[string]any)["id"].(string)
	mainID := card["contract"].(map[string]any)["id"].(string)
	introID := card["contracts"].(map[string]any)["intro"].(map[string]any)["id"].(string)

	// Edit and delete: payments, then contracts; each writes a timeline event; colleagues are refused.
	payPath := map[string]string{"projectId": projectID, "paymentId": lastPaymentID}
	if code, res := call(other, server.handleUpdateProjectPayment, http.MethodPatch, "/", payPath, `{"note":"x"}`, nil); code != http.StatusForbidden {
		t.Fatalf("colleague payment edit: %d %v", code, res)
	}
	if code, res := call(manager, server.handleUpdateProjectPayment, http.MethodPatch, "/", payPath, `{"amount":41000}`, nil); code != http.StatusBadRequest || fieldErr(res, "amount") == "" {
		t.Fatalf("payment edit over balance: %d %v", code, res)
	}
	code, card = call(manager, server.handleUpdateProjectPayment, http.MethodPatch, "/", payPath, `{"amount":30000,"note":"corrected"}`, nil)
	if code != http.StatusOK || money(card, "main", "remainingAmount") != 10000 {
		t.Fatalf("payment edit: %d %v", code, card)
	}
	if code, res := call(manager, server.handleDeleteProjectContract, http.MethodDelete, "/", map[string]string{"projectId": projectID, "contractId": mainID}, ``, nil); code != http.StatusConflict || res["code"] != "contract_has_payments" {
		t.Fatalf("delete contract with payments: %d %v", code, res)
	}
	if code, res := call(manager, server.handleUpdateProjectContract, http.MethodPatch, "/", map[string]string{"projectId": projectID, "contractId": mainID}, `{"totalAmount":50000}`, nil); code != http.StatusBadRequest || fieldErr(res, "totalAmount") == "" {
		t.Fatalf("contract total below paid: %d %v", code, res)
	}
	code, card = call(manager, server.handleUpdateProjectContract, http.MethodPatch, "/", map[string]string{"projectId": projectID, "contractId": mainID}, `{"totalAmount":90000,"number":"KW/2026/041/A"}`, nil)
	if code != http.StatusOK || money(card, "main", "totalAmount") != 90000 || money(card, "main", "target50Amount") != 45000 {
		t.Fatalf("contract edit: %d %v", code, card)
	}
	code, card = call(manager, server.handleDeleteProjectPayment, http.MethodDelete, "/", payPath, ``, nil)
	if payments, _ := card["payments"].([]any); code != http.StatusOK || len(payments) != 2 {
		t.Fatalf("payment delete: %d %v", code, card)
	}

	// Comments: any user with office access; the card stays editable only for the manager.
	if code, res := call(other, server.handleAddProjectComment, http.MethodPost, "/", pid, `{"text":""}`, idem()); code != http.StatusBadRequest || fieldErr(res, "text") == "" {
		t.Fatalf("empty comment: %d %v", code, res)
	}
	commentKey := map[string]string{"Idempotency-Key": uuid.NewString()}
	if code, res := call(other, server.handleAddProjectComment, http.MethodPost, "/", pid, `{"text":"Client wants oak"}`, commentKey); code != http.StatusCreated {
		t.Fatalf("comment: %d %v", code, res)
	}
	if code, _ := call(other, server.handleAddProjectComment, http.MethodPost, "/", pid, `{"text":"Client wants oak"}`, commentKey); code != http.StatusCreated {
		t.Fatalf("comment replay: %d", code)
	}

	// Designer: optional, an active member of the project office; null clears it.
	if code, res := call(manager, server.handleUpdateProjectDetails, http.MethodPatch, "/", pid, `{"designerId":"`+uuid.NewString()+`"}`, nil); code != http.StatusBadRequest || fieldErr(res, "designerId") == "" {
		t.Fatalf("designer outside the office: %d %v", code, res)
	}
	code, card = call(manager, server.handleUpdateProjectDetails, http.MethodPatch, "/", pid, `{"designerId":"`+otherID.String()+`"}`, nil)
	if designer, _ := card["designer"].(map[string]any); code != http.StatusOK || designer["id"] != otherID.String() {
		t.Fatalf("designer: %d %v", code, card)
	}
	if code, res := call(manager, server.handleUpdateProjectDetails, http.MethodPatch, "/", pid, `{"managerId":null}`, nil); code != http.StatusBadRequest || fieldErr(res, "managerId") == "" {
		t.Fatalf("manager cannot be cleared: %d %v", code, res)
	}
	code, card = call(manager, server.handleUpdateProjectDetails, http.MethodPatch, "/", pid, `{"designerId":null}`, nil)
	if code != http.StatusOK || card["designer"] != nil {
		t.Fatalf("designer clear: %d %v", code, card)
	}

	// Lead info through the project: the manager edits the linked lead; a colleague cannot.
	if code, res := call(other, server.handleUpdateProjectLeadInfo, http.MethodPatch, "/", pid, `{"cityRegion":"Warszawa"}`, nil); code != http.StatusForbidden {
		t.Fatalf("colleague lead info: %d %v", code, res)
	}
	code, card = call(manager, server.handleUpdateProjectLeadInfo, http.MethodPatch, "/", pid, `{"cityRegion":"Legionowo"}`, nil)
	if client, _ := card["client"].(map[string]any); code != http.StatusOK || client["cityRegion"] != "Legionowo" {
		t.Fatalf("lead info: %d %v", code, card)
	}

	// Measurement appointment held -> `measurement_visit` on the project timeline.
	var appointmentID uuid.UUID
	if err := pool.QueryRow(ctx, `
		insert into public.lead_showroom_visits (lead_id, scheduled_at, ends_at, kind, responsible_manager_id, created_by)
		values ($1, now() + interval '1 day', now() + interval '1 day 1 hour', 'measurement', $2, $2) returning id
	`, leadID, managerID).Scan(&appointmentID); err != nil {
		t.Fatal(err)
	}
	if code, res := call(manager, server.handleUpdateAppointment, http.MethodPatch, "/", map[string]string{"appointmentId": appointmentID.String()},
		`{"status":"visited"}`, map[string]string{"If-Match": "1"}); code != http.StatusOK {
		t.Fatalf("mark held: %d %v", code, res)
	}

	// The data of the contract file downloads through the shared endpoint.
	if code, res := call(manager, server.handleFileDownloadURL, http.MethodGet, "/", map[string]string{"fileId": fileID}, ``, nil); code != http.StatusOK || res["url"] == nil {
		t.Fatalf("download url: %d %v", code, res)
	}

	// Deleting the contracts: the intro one has payments, so it is refused until they are deleted.
	if code, res := call(manager, server.handleDeleteProjectContract, http.MethodDelete, "/", map[string]string{"projectId": projectID, "contractId": introID}, ``, nil); code != http.StatusConflict || res["code"] != "contract_has_payments" {
		t.Fatalf("delete intro with payments: %d %v", code, res)
	}

	// Cancel needs reasons; restore returns the previous status.
	if code, res := call(manager, server.handleCancelProject, http.MethodPost, "/", pid, `{"reasons":["other"]}`, nil); code != http.StatusBadRequest || fieldErr(res, "comment") == "" {
		t.Fatalf("cancel without comment: %d %v", code, res)
	}
	code, card = call(manager, server.handleCancelProject, http.MethodPost, "/", pid, `{"reasons":["postponed_renovation"],"comment":"Moving to spring","cancelFutureEvents":true}`, nil)
	cancellation, _ := card["cancellation"].(map[string]any)
	if code != http.StatusOK || card["status"] != "cancelled" || cancellation["statusBeforeCancel"] != "main_contract" {
		t.Fatalf("cancel: %d %v", code, card)
	}
	if code, res := call(manager, server.handleChangeProjectStatus, http.MethodPost, "/", pid, `{"status":"design"}`, nil); code != http.StatusConflict || res["code"] != "project_cancelled" {
		t.Fatalf("status on cancelled: %d %v", code, res)
	}
	if code, res := call(other, server.handleAddProjectComment, http.MethodPost, "/", pid, `{"text":"After cancel"}`, idem()); code != http.StatusConflict || res["code"] != "project_cancelled" {
		t.Fatalf("comment on cancelled: %d %v", code, res)
	}
	code, card = call(manager, server.handleRestoreProject, http.MethodPost, "/", pid, ``, nil)
	if code != http.StatusOK || card["status"] != "main_contract" || card["cancellation"] != nil {
		t.Fatalf("restore: %d %v", code, card)
	}

	// List, facets, board and timeline.
	code, list := call(admin, server.handleListProjects, http.MethodGet, "/?"+url.Values{"status": {"main_contract"}, "q": {clientName}}.Encode(), nil, ``, nil)
	items, _ := list["items"].([]any)
	if code != http.StatusOK || len(items) != 1 {
		t.Fatalf("list: %d %v", code, list)
	}
	if last, _ := items[0].(map[string]any)["lastComment"].(map[string]any); last["text"] != "Client wants oak" && last["text"] != "Moving to spring" {
		t.Fatalf("last comment: %v", items[0])
	}
	code, facets := call(admin, server.handleProjectFacets, http.MethodGet, "/?"+url.Values{"q": {clientName}}.Encode(), nil, ``, nil)
	if statuses, _ := facets["status"].(map[string]any); code != http.StatusOK || statuses["main_contract"] != float64(1) || facets["total"] != float64(1) {
		t.Fatalf("facets: %d %v", code, facets)
	}
	code, board := call(admin, server.handleProjectBoard, http.MethodGet, "/?"+url.Values{"q": {clientName}, "limit": {"5"}}.Encode(), nil, ``, nil)
	columns, _ := board["columns"].([]any)
	if code != http.StatusOK || len(columns) != len(projectStatuses) || board["total"] != float64(1) {
		t.Fatalf("board: %d %v", code, board)
	}
	for _, column := range columns {
		column := column.(map[string]any)
		cards, _ := column["items"].([]any)
		want := 0
		if column["status"] == "main_contract" {
			want = 1
		}
		if len(cards) != want || column["count"] != float64(want) {
			t.Fatalf("board column %v: %v", column["status"], column)
		}
	}
	types := eventTypes(other)
	for _, want := range []string{
		"created", "estimate_updated", "contract_added", "payment_added", "status_changed",
		"payment_updated", "contract_updated", "payment_deleted", "comment", "details_updated",
		"lead_info_updated", "measurement_visit", "cancelled", "restored",
	} {
		found := false
		for _, got := range types {
			found = found || got == want
		}
		if !found {
			t.Fatalf("timeline lacks %q: %v", want, types)
		}
	}
	if types[0] != "restored" {
		t.Fatalf("timeline order: %v", types)
	}
}
