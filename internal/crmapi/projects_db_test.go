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

// TestProjectsDB runs the project flow (pipeline 4) against a disposable database
// (KOLSS_TEST_DATABASE_URL): create from a lead, estimate total, change status, standalone
// payments, documents, permissions, finish / close / restore, list/facets/board/timeline and the
// project-file download.
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

	// Create: every project starts at `preliminary_estimate`; the lead becomes read-only (`project`).
	createKey := uuid.NewString()
	code, project := call(manager, server.handleCreateProject, http.MethodPost, "/", map[string]string{"leadId": leadID.String()},
		`{"projectType":"measure"}`, map[string]string{"Idempotency-Key": createKey})
	if code != http.StatusCreated || project["status"] != "preliminary_estimate" || project["code"] == "" {
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
	if code, res := call(other, server.handleChangeProjectStatus, http.MethodPost, "/", pid, `{"status":"measurement_design"}`, nil); code != http.StatusForbidden || res["code"] != "project_edit_forbidden" {
		t.Fatalf("colleague edit: %d %v", code, res)
	}

	totalOf := func(card map[string]any, key, currency string) float64 {
		items, _ := card[key].([]any)
		for _, item := range items {
			entry := item.(map[string]any)
			if entry["currency"] == currency {
				value, _ := entry["amount"].(float64)
				return value
			}
		}
		return 0
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

	// Estimate: one total; null clears it; only the manager and admins.
	if code, res := call(manager, server.handleUpdateProjectEstimate, http.MethodPut, "/", pid, `{"amount":0,"currency":"GBP"}`, nil); code != http.StatusBadRequest || fieldErr(res, "amount") == "" || fieldErr(res, "currency") == "" {
		t.Fatalf("bad estimate: %d %v", code, res)
	}
	if code, res := call(other, server.handleUpdateProjectEstimate, http.MethodPut, "/", pid, `{"amount":1,"currency":"PLN"}`, nil); code != http.StatusForbidden {
		t.Fatalf("colleague estimate: %d %v", code, res)
	}
	code, card := call(manager, server.handleUpdateProjectEstimate, http.MethodPut, "/", pid, `{"amount":63000,"currency":"PLN"}`, nil)
	if estimate, _ := card["estimate"].(map[string]any); code != http.StatusOK || estimate["amount"] != float64(63000) || estimate["currency"] != "PLN" {
		t.Fatalf("estimate: %d %v", code, card)
	}
	if code, card := call(manager, server.handleUpdateProjectEstimate, http.MethodPut, "/", pid, `{"amount":null}`, nil); code != http.StatusOK || card["estimate"] != nil {
		t.Fatalf("estimate clear: %d %v", code, card)
	}
	if code, _ := call(manager, server.handleUpdateProjectEstimate, http.MethodPut, "/", pid, `{"amount":80000,"currency":"PLN"}`, nil); code != http.StatusOK {
		t.Fatalf("estimate again: %d", code)
	}

	// Payments: standalone, any amount, never move the status.
	if code, res := call(manager, server.handleAddProjectPayment, http.MethodPost, "/", pid, `{"amount":500,"paidOn":"2026-10-02"}`, idem()); code != http.StatusBadRequest || fieldErr(res, "basis") == "" || fieldErr(res, "currency") == "" {
		t.Fatalf("payment without basis: %d %v", code, res)
	}
	if code, res := call(other, server.handleAddProjectPayment, http.MethodPost, "/", pid, `{"amount":500,"currency":"PLN","paidOn":"2026-10-02","basis":"receipt"}`, idem()); code != http.StatusForbidden {
		t.Fatalf("colleague payment: %d %v", code, res)
	}
	code, upload := call(manager, server.handleCreateProjectFileUpload, http.MethodPost, "/", pid, `{"kind":"receipt","fileName":"receipt-500.pdf","sizeBytes":1200}`, nil)
	if code != http.StatusOK || upload["fileId"] == nil {
		t.Fatalf("receipt upload: %d %v", code, upload)
	}
	receiptID := upload["fileId"].(string)
	code, card = call(manager, server.handleAddProjectPayment, http.MethodPost, "/", pid,
		`{"amount":500,"currency":"PLN","paidOn":"2026-10-02","basis":"receipt","note":"Paid measurement","fileId":"`+receiptID+`"}`, idem())
	if code != http.StatusCreated || card["status"] != "preliminary_estimate" || totalOf(card, "paidTotals", "PLN") != 500 {
		t.Fatalf("payment 1: %d %v", code, card)
	}
	code, card = call(manager, server.handleAddProjectPayment, http.MethodPost, "/", pid, `{"amount":40000,"currency":"PLN","paidOn":"2026-10-10","basis":"invoice"}`, idem())
	payments, _ := card["payments"].([]any)
	if code != http.StatusCreated || totalOf(card, "paidTotals", "PLN") != 40500 || len(payments) != 2 {
		t.Fatalf("payment 2: %d %v", code, card)
	}
	if file, _ := payments[0].(map[string]any)["file"].(map[string]any); file["id"] != receiptID {
		t.Fatalf("receipt on payment: %v", payments[0])
	}
	lastPaymentID := payments[1].(map[string]any)["id"].(string)
	payPath := map[string]string{"projectId": projectID, "paymentId": lastPaymentID}
	if code, res := call(other, server.handleUpdateProjectPayment, http.MethodPatch, "/", payPath, `{"note":"x"}`, nil); code != http.StatusForbidden {
		t.Fatalf("colleague payment edit: %d %v", code, res)
	}
	code, card = call(manager, server.handleUpdateProjectPayment, http.MethodPatch, "/", payPath, `{"amount":30000,"basis":"contract","note":"corrected"}`, nil)
	if code != http.StatusOK || totalOf(card, "paidTotals", "PLN") != 30500 {
		t.Fatalf("payment edit: %d %v", code, card)
	}
	code, card = call(manager, server.handleAddProjectPayment, http.MethodPost, "/", pid, `{"amount":100,"currency":"EUR","paidOn":"2026-10-11","basis":"other"}`, idem())
	if code != http.StatusCreated || totalOf(card, "paidTotals", "EUR") != 100 {
		t.Fatalf("payment EUR: %d %v", code, card)
	}
	eurID := card["payments"].([]any)[2].(map[string]any)["id"].(string)
	code, card = call(manager, server.handleDeleteProjectPayment, http.MethodDelete, "/", map[string]string{"projectId": projectID, "paymentId": eurID}, ``, nil)
	if payments, _ := card["payments"].([]any); code != http.StatusOK || len(payments) != 2 || totalOf(card, "paidTotals", "EUR") != 0 {
		t.Fatalf("payment delete: %d %v", code, card)
	}

	// Documents: any office member uploads and confirms; the uploader, manager and admins delete.
	code, upload = call(other, server.handleCreateProjectFileUpload, http.MethodPost, "/", pid, `{"kind":"document","fileName":"umowa-KW-2026-041.pdf","sizeBytes":1200}`, nil)
	if code != http.StatusOK || upload["fileId"] == nil {
		t.Fatalf("document upload: %d %v", code, upload)
	}
	fileID := upload["fileId"].(string)
	code, card = call(other, server.handleConfirmProjectDocument, http.MethodPost, "/", pid, `{"fileId":"`+fileID+`"}`, nil)
	documents, _ := card["documents"].([]any)
	if code != http.StatusCreated || len(documents) != 1 || documents[0].(map[string]any)["fileName"] != "umowa-KW-2026-041.pdf" {
		t.Fatalf("document confirm: %d %v", code, card)
	}
	if code, res := call(other, server.handleConfirmProjectDocument, http.MethodPost, "/", pid, `{"fileId":"`+receiptID+`"}`, nil); code != http.StatusBadRequest || fieldErr(res, "fileId") == "" {
		t.Fatalf("receipt as a document: %d %v", code, res)
	}
	code, upload = call(manager, server.handleCreateProjectFileUpload, http.MethodPost, "/", pid, `{"kind":"document","fileName":"plan.pdf","sizeBytes":1200}`, nil)
	planID := upload["fileId"].(string)
	if code, _ := call(manager, server.handleConfirmProjectDocument, http.MethodPost, "/", pid, `{"fileId":"`+planID+`"}`, nil); code != http.StatusCreated {
		t.Fatalf("plan confirm: %d", code)
	}
	planPath := map[string]string{"projectId": projectID, "fileId": planID}
	if code, res := call(other, server.handleDeleteProjectDocument, http.MethodDelete, "/", planPath, ``, nil); code != http.StatusForbidden {
		t.Fatalf("colleague deletes another's document: %d %v", code, res)
	}
	code, card = call(manager, server.handleDeleteProjectDocument, http.MethodDelete, "/", planPath, ``, nil)
	if documents, _ := card["documents"].([]any); code != http.StatusOK || len(documents) != 1 {
		t.Fatalf("document delete: %d %v", code, card)
	}

	// Change status: by hand, both directions, six stages; finals and old stages are refused.
	if code, res := call(manager, server.handleChangeProjectStatus, http.MethodPost, "/", pid, `{"status":"measurement_design","eventDate":"2026-10-05","eventTime":"10:00"}`, nil); code != http.StatusOK || res["eventTime"] != "10:00" {
		t.Fatalf("measurement: %d %v", code, res)
	}
	if code, res := call(manager, server.handleChangeProjectStatus, http.MethodPost, "/", pid, `{"status":"production_delivery"}`, nil); code != http.StatusOK || res["status"] != "production_delivery" {
		t.Fatalf("production without 50%%: %d %v", code, res)
	}
	if code, res := call(manager, server.handleChangeProjectStatus, http.MethodPost, "/", pid, `{"status":"factory_approval","comment":"Back to the factory"}`, nil); code != http.StatusOK || res["status"] != "factory_approval" {
		t.Fatalf("backwards: %d %v", code, res)
	}
	for _, status := range []string{"completed", "cancelled", "estimate", "main_contract", "none"} {
		if code, res := call(manager, server.handleChangeProjectStatus, http.MethodPost, "/", pid, `{"status":"`+status+`"}`, nil); code != http.StatusBadRequest || fieldErr(res, "status") == "" {
			t.Fatalf("status %s by hand: %d %v", status, code, res)
		}
	}

	// Project finished: only from Installation; a finished project is read-only, restore reopens it.
	if code, res := call(manager, server.handleFinishProject, http.MethodPost, "/", pid, `{}`, nil); code != http.StatusConflict || res["code"] != "project_not_installation" {
		t.Fatalf("finish before installation: %d %v", code, res)
	}
	if code, _ := call(manager, server.handleChangeProjectStatus, http.MethodPost, "/", pid, `{"status":"installation"}`, nil); code != http.StatusOK {
		t.Fatalf("installation: %d", code)
	}
	if code, res := call(other, server.handleFinishProject, http.MethodPost, "/", pid, `{}`, nil); code != http.StatusForbidden {
		t.Fatalf("colleague finish: %d %v", code, res)
	}
	if code, card := call(manager, server.handleFinishProject, http.MethodPost, "/", pid, `{"comment":"Handed over"}`, nil); code != http.StatusOK || card["status"] != "completed" {
		t.Fatalf("finish: %d %v", code, card)
	}
	if code, res := call(manager, server.handleChangeProjectStatus, http.MethodPost, "/", pid, `{"status":"installation"}`, nil); code != http.StatusConflict || res["code"] != "project_finished" {
		t.Fatalf("status on finished: %d %v", code, res)
	}
	if code, res := call(manager, server.handleCancelProject, http.MethodPost, "/", pid, `{"reasons":["other"],"comment":"x"}`, nil); code != http.StatusConflict || res["code"] != "project_finished" {
		t.Fatalf("close a finished project: %d %v", code, res)
	}
	if code, card := call(manager, server.handleRestoreProject, http.MethodPost, "/", pid, ``, nil); code != http.StatusOK || card["status"] != "installation" {
		t.Fatalf("reopen finished: %d %v", code, card)
	}
	if code, _ := call(manager, server.handleChangeProjectStatus, http.MethodPost, "/", pid, `{"status":"contract_signing"}`, nil); code != http.StatusOK {
		t.Fatalf("contract signing: %d", code)
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

	// A project document downloads through the shared endpoint.
	if code, res := call(manager, server.handleFileDownloadURL, http.MethodGet, "/", map[string]string{"fileId": fileID}, ``, nil); code != http.StatusOK || res["url"] == nil {
		t.Fatalf("download url: %d %v", code, res)
	}

	// Cancel needs reasons; restore returns the previous status.
	if code, res := call(manager, server.handleCancelProject, http.MethodPost, "/", pid, `{"reasons":["price_too_high"]}`, nil); code != http.StatusBadRequest || fieldErr(res, "comment") == "" {
		t.Fatalf("cancel without comment: %d %v", code, res)
	}
	code, card = call(manager, server.handleCancelProject, http.MethodPost, "/", pid, `{"reasons":["postponed_renovation"],"comment":"Moving to spring","cancelFutureEvents":true}`, nil)
	cancellation, _ := card["cancellation"].(map[string]any)
	if code != http.StatusOK || card["status"] != "cancelled" || cancellation["statusBeforeCancel"] != "contract_signing" {
		t.Fatalf("cancel: %d %v", code, card)
	}
	if code, res := call(manager, server.handleChangeProjectStatus, http.MethodPost, "/", pid, `{"status":"installation"}`, nil); code != http.StatusConflict || res["code"] != "project_cancelled" {
		t.Fatalf("status on cancelled: %d %v", code, res)
	}
	if code, res := call(other, server.handleAddProjectComment, http.MethodPost, "/", pid, `{"text":"After cancel"}`, idem()); code != http.StatusConflict || res["code"] != "project_cancelled" {
		t.Fatalf("comment on cancelled: %d %v", code, res)
	}
	code, card = call(manager, server.handleRestoreProject, http.MethodPost, "/", pid, ``, nil)
	if code != http.StatusOK || card["status"] != "contract_signing" || card["cancellation"] != nil {
		t.Fatalf("restore: %d %v", code, card)
	}

	// List, facets, board and timeline.
	code, list := call(admin, server.handleListProjects, http.MethodGet, "/?"+url.Values{"status": {"contract_signing"}, "q": {clientName}}.Encode(), nil, ``, nil)
	items, _ := list["items"].([]any)
	if code != http.StatusOK || len(items) != 1 {
		t.Fatalf("list: %d %v", code, list)
	}
	if last, _ := items[0].(map[string]any)["lastComment"].(map[string]any); last["text"] != "Client wants oak" && last["text"] != "Moving to spring" {
		t.Fatalf("last comment: %v", items[0])
	}
	if item := items[0].(map[string]any); totalOf(item, "paidTotals", "PLN") != 30500 || item["estimate"] == nil {
		t.Fatalf("list money: %v", item)
	}
	code, facets := call(admin, server.handleProjectFacets, http.MethodGet, "/?"+url.Values{"q": {clientName}}.Encode(), nil, ``, nil)
	if statuses, _ := facets["status"].(map[string]any); code != http.StatusOK || statuses["contract_signing"] != float64(1) || facets["total"] != float64(1) {
		t.Fatalf("facets: %d %v", code, facets)
	}
	code, board := call(admin, server.handleProjectBoard, http.MethodGet, "/?"+url.Values{"q": {clientName}, "limit": {"5"}}.Encode(), nil, ``, nil)
	columns, _ := board["columns"].([]any)
	if code != http.StatusOK || len(columns) != len(projectBoardStatuses) || board["total"] != float64(1) || board["cancelledCount"] != float64(0) || board["completedCount"] != float64(0) {
		t.Fatalf("board: %d %v", code, board)
	}
	for _, column := range columns {
		column := column.(map[string]any)
		cards, _ := column["items"].([]any)
		want := 0
		if column["status"] == "contract_signing" {
			want = 1
		}
		if len(cards) != want || column["count"] != float64(want) {
			t.Fatalf("board column %v: %v", column["status"], column)
		}
	}
	types := eventTypes(other)
	for _, want := range []string{
		"created", "estimate_updated", "payment_added", "payment_updated", "payment_deleted",
		"file_added", "file_deleted", "status_changed", "finished", "comment", "details_updated",
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
