package crmapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestTasksV2DB runs the T1 flow (task lists, extended tasks, view queries, counts) against a
// disposable database with all migrations applied. It only runs when KOLSS_TEST_DATABASE_URL is
// set (never production): it writes fixtures.
func TestTasksV2DB(t *testing.T) {
	dsn := os.Getenv("KOLSS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("KOLSS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	server := &Server{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	var officeID uuid.UUID
	var timezone string
	if err := pool.QueryRow(ctx, `select id, timezone_name from public.offices order by code limit 1`).Scan(&officeID, &timezone); err != nil {
		t.Fatal("office fixture:", err)
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		t.Fatal(err)
	}
	newUser := func(name string) Actor {
		id := uuid.New()
		mustExec(t, pool, `insert into auth.users (id, email) values ($1, $2)`, id, id.String()+"@test.local")
		mustExec(t, pool, `insert into public.profiles (id, display_name) values ($1, $2) on conflict (id) do update set display_name=excluded.display_name`, id, name)
		mustExec(t, pool, `insert into public.user_office_memberships (user_id, office_id) values ($1,$2)`, id, officeID)
		return Actor{ID: id, Role: "office_member", IsActive: true, DisplayName: ptr(name), OfficeIDs: map[uuid.UUID]struct{}{officeID: {}}}
	}
	anna, boris, carol := newUser("Anna"), newUser("Boris"), newUser("Carol")
	admin := Actor{ID: uuid.New(), Role: "super_admin", IsActive: true}
	mustExec(t, pool, `insert into auth.users (id, email) values ($1, $2)`, admin.ID, admin.ID.String()+"@test.local")
	mustExec(t, pool, `insert into public.profiles (id, display_name) values ($1, 'Admin') on conflict (id) do nothing`, admin.ID)

	call := func(actor Actor, handler http.HandlerFunc, method, target, body string, pathValues map[string]string, headers map[string]string) (int, map[string]any) {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		for k, v := range pathValues {
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
	key := func() map[string]string { return map[string]string{"Idempotency-Key": uuid.NewString()} }

	// Task list: created by the admin (owner defaults to the actor), colour assigned from the palette.
	// Profile roles cannot be changed in the database, so the admin is a super admin only in Go; a
	// membership satisfies the owner check (a real super admin passes it by role).
	mustExec(t, pool, `insert into public.user_office_memberships (user_id, office_id) values ($1,$2)`, admin.ID, officeID)
	code, list := call(admin, server.handleCreateTaskList, http.MethodPost, "/v1/task-lists", `{"name":"  Expo 2026  ","description":"Fair","datesText":"12–15 Nov"}`, nil, key())
	if code != http.StatusCreated || list["name"] != "Expo 2026" || list["ownerId"] != admin.ID.String() || !strings.HasPrefix(list["color"].(string), "#") {
		t.Fatalf("create list: %d %v", code, list)
	}
	listID := list["id"].(string)
	if code, out := call(anna, server.handleUpdateTaskList, http.MethodPatch, "/v1/task-lists/x", `{"name":"Hijack"}`, map[string]string{"listId": listID}, map[string]string{"If-Match": "1"}); code != http.StatusForbidden {
		t.Fatalf("non-owner list edit must be 403: %d %v", code, out)
	}
	if code, out := call(admin, server.handleUpdateTaskList, http.MethodPatch, "/v1/task-lists/x", `{"datesText":null,"ownerId":"`+anna.ID.String()+`"}`, map[string]string{"listId": listID}, map[string]string{"If-Match": "1"}); code != http.StatusOK || out["datesText"] != nil || out["ownerId"] != anna.ID.String() || out["version"] != float64(2) {
		t.Fatalf("owner list edit: %d %v", code, out)
	}
	if code, _ := call(admin, server.handleUpdateTaskList, http.MethodPatch, "/v1/task-lists/x", `{"name":"Stale"}`, map[string]string{"listId": listID}, map[string]string{"If-Match": "1"}); code != http.StatusConflict {
		t.Fatalf("stale list version must be 409: %d", code)
	}

	// Tasks. Dates are office-local, relative to today.
	day := func(offset int) string { return time.Now().In(location).AddDate(0, 0, offset).Format(taskDateLayout) }
	leadID := uuid.New()
	mustExec(t, pool, `insert into public.leads (id, office_id, external_lead_id, name, phone, archived_at) values ($1,$2,$3,'Linked lead','+48600000002', null)`, leadID, officeID, leadID.String())
	create := func(actor Actor, assignee Actor, extra string) (int, map[string]any) {
		body := `{"officeId":"` + officeID.String() + `","assigneeId":"` + assignee.ID.String() + `"` + extra + `}`
		return call(actor, server.handleCreateTask, http.MethodPost, "/v1/tasks", body, nil, key())
	}
	mk := func(actor, assignee Actor, extra string) string {
		code, out := create(actor, assignee, extra)
		if code != http.StatusCreated {
			t.Fatalf("create task %s: %d %v", extra, code, out)
		}
		return out["id"].(string)
	}
	overdue := mk(anna, anna, `,"title":"Overdue","dueDate":"`+day(-2)+`"`)
	today := mk(anna, anna, `,"title":"Today with time","dueDate":"`+day(0)+`","dueTime":"09:30","note":"Bring plans"`)
	linked := mk(anna, anna, `,"title":"Linked","dueDate":"`+day(0)+`","link":{"type":"lead","id":"`+leadID.String()+`"}`)
	inList := mk(boris, anna, `,"title":"Book hotel","dueDate":"`+day(3)+`","listId":"`+listID+`"`)
	undated := mk(anna, anna, `,"title":"Someday","dueDate":null`)
	mk(boris, boris, `,"title":"Boris only","dueDate":"`+day(0)+`"`)

	if code, out := create(anna, anna, `,"title":"x","dueTime":"10:00"`); code != http.StatusBadRequest {
		t.Fatalf("time without date must be 400: %d %v", code, out)
	}
	if code, out := create(anna, anna, `,"title":"x","link":{"type":"lead","id":"`+uuid.NewString()+`"}`); code != http.StatusBadRequest {
		t.Fatalf("unknown lead must be 400: %d %v", code, out)
	}
	if code, out := create(anna, anna, `,"title":"x","listId":"`+uuid.NewString()+`"`); code != http.StatusBadRequest {
		t.Fatalf("unknown list must be 400: %d %v", code, out)
	}
	if code, out := create(anna, anna, `,"title":"x","link":{"type":"project","id":"`+uuid.NewString()+`"}`); code != http.StatusBadRequest {
		t.Fatalf("project links are not available yet: %d %v", code, out)
	}

	feed := func(actor Actor, query string) []string {
		code, out := call(actor, server.handleListTasks, http.MethodGet, "/v1/tasks?"+query, "", nil, nil)
		if code != http.StatusOK {
			t.Fatalf("feed %s: %d %v", query, code, out)
		}
		titles := []string{}
		for _, raw := range out["items"].([]any) {
			item := raw.(map[string]any)
			if title, ok := item["title"].(string); ok && item["source"] == "manual" {
				titles = append(titles, title)
			}
		}
		return titles
	}
	equal := func(got []string, want ...string) {
		t.Helper()
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	equal(feed(anna, "view=my_day"), "Overdue", "Today with time", "Linked")
	equal(feed(anna, "view=upcoming"), "Book hotel", "Someday")
	equal(feed(anna, "view=upcoming&kinds=list"), "Book hotel")
	equal(feed(anna, "view=my_day&q=linked"), "Linked")
	equal(feed(carol, "view=my_day"))
	equal(feed(carol, "view=list&listId="+listID), "Book hotel") // lists are visible to everyone
	if code, _ := call(anna, server.handleListTasks, http.MethodGet, "/v1/tasks?view=all", "", nil, nil); code != http.StatusForbidden {
		t.Fatalf("view=all is for admins: %d", code)
	}
	if got := feed(admin, "view=all&assigneeId="+boris.ID.String()); strings.Join(got, "|") != "Boris only" {
		t.Fatalf("admin all/assignee: %v", got)
	}
	code, counts := call(anna, server.handleTaskCounts, http.MethodGet, "/v1/tasks/counts", "", nil, nil)
	if code != http.StatusOK || counts["myDay"] != float64(3) || counts["upcoming"] != float64(2) || counts["overdue"] != nil {
		t.Fatalf("counts: %d %v", code, counts)
	}

	// Detail keeps time, note, link and list.
	code, detail := call(anna, server.handleGetTask, http.MethodGet, "/v1/tasks/x", "", map[string]string{"taskId": today}, nil)
	if code != http.StatusOK || detail["dueTime"] != "09:30" || detail["note"] != "Bring plans" || detail["kind"] != "personal" || detail["canEdit"] != true {
		t.Fatalf("detail: %d %v", code, detail)
	}
	code, detail = call(anna, server.handleGetTask, http.MethodGet, "/v1/tasks/x", "", map[string]string{"taskId": linked}, nil)
	if link, _ := detail["link"].(map[string]any); code != http.StatusOK || detail["kind"] != "comment" || link["name"] != "Linked lead" {
		t.Fatalf("linked detail: %d %v", code, detail)
	}

	patch := func(actor Actor, id string, version int, body string) (int, map[string]any) {
		return call(actor, server.handleUpdateTask, http.MethodPatch, "/v1/tasks/x", body, map[string]string{"taskId": id}, map[string]string{"If-Match": string(rune('0' + version))})
	}
	// In progress, then done clears it, reopening starts at To do.
	if code, out := patch(anna, today, 1, `{"inProgress":true}`); code != http.StatusOK || out["inProgress"] != true || out["version"] != float64(2) {
		t.Fatalf("in progress: %d %v", code, out)
	}
	if code, out := patch(anna, today, 2, `{"status":"done"}`); code != http.StatusOK || out["inProgress"] != false || out["status"] != "done" {
		t.Fatalf("done: %d %v", code, out)
	}
	if code, out := patch(anna, today, 3, `{"inProgress":true}`); code != http.StatusBadRequest {
		t.Fatalf("done task cannot be in progress: %d %v", code, out)
	}
	equal(feed(anna, "view=done"), "Today with time")
	equal(feed(anna, "view=my_day"), "Overdue", "Today with time", "Linked") // done today stays in My day
	if code, out := patch(anna, today, 3, `{"status":"open"}`); code != http.StatusOK || out["inProgress"] != false {
		t.Fatalf("reopen: %d %v", code, out)
	}
	// Reschedule: clearing the date drops the time; a time needs a date.
	if code, out := patch(anna, today, 4, `{"dueDate":null}`); code != http.StatusOK {
		t.Fatalf("clear date: %d %v", code, out)
	}
	if code, out := patch(anna, today, 5, `{"dueTime":"11:00"}`); code != http.StatusBadRequest {
		t.Fatalf("time without date: %d %v", code, out)
	}
	if code, out := patch(anna, today, 5, `{"dueDate":"`+day(1)+`","dueTime":"11:00"}`); code != http.StatusOK {
		t.Fatalf("reschedule: %d %v", code, out)
	}
	code, detail = call(anna, server.handleGetTask, http.MethodGet, "/v1/tasks/x", "", map[string]string{"taskId": today}, nil)
	if detail["dueDate"] != day(1) || detail["dueTime"] != "11:00" {
		t.Fatalf("rescheduled detail: %v", detail)
	}
	// Reassign to a member; a stranger is refused; title edits follow the content policy.
	if code, out := patch(anna, overdue, 1, `{"assigneeId":"`+boris.ID.String()+`"}`); code != http.StatusOK {
		t.Fatalf("reassign: %d %v", code, out)
	}
	if code, out := patch(anna, overdue, 2, `{"assigneeId":"`+uuid.NewString()+`"}`); code != http.StatusBadRequest {
		t.Fatalf("reassign to a stranger: %d %v", code, out)
	}
	if code, out := patch(carol, undated, 1, `{"status":"done"}`); code != http.StatusOK {
		t.Fatalf("any office member changes status (v1 rule): %d %v", code, out)
	}
	if code, out := patch(carol, inList, 1, `{"title":"Renamed by a stranger"}`); code != http.StatusForbidden {
		t.Fatalf("title edit by an unrelated member: %d %v", code, out)
	}
	if code, out := patch(anna, inList, 1, `{"title":"Book two hotels","note":"  "}`); code != http.StatusOK {
		t.Fatalf("title edit by the list owner: %d %v", code, out)
	}
	if code, out := patch(anna, inList, 1, `{"status":"done"}`); code != http.StatusConflict {
		t.Fatalf("stale version: %d %v", code, out)
	}

	// The list reports progress and members.
	code, out := call(anna, server.handleGetTaskList, http.MethodGet, "/v1/task-lists/x", "", map[string]string{"listId": listID}, nil)
	members, _ := out["members"].([]any)
	if code != http.StatusOK || out["taskCount"] != float64(1) || out["doneCount"] != float64(0) || len(members) != 1 {
		t.Fatalf("list detail: %d %v", code, out)
	}

	// Automatic items join the feed read-only: a lead with nothing planned is "no next step" due
	// today, a scheduled showroom visit is a "visit" at its local time.
	autoLead := uuid.New()
	mustExec(t, pool, `insert into public.leads (id, office_id, external_lead_id, name, phone, assigned_to) values ($1,$2,$3,'Auto lead','+48600000003',$4)`, autoLead, officeID, autoLead.String(), anna.ID)
	visitAt, err := time.ParseInLocation(taskDateLayout+" 15:04", day(1)+" 12:00", location)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `insert into public.lead_showroom_visits (lead_id, scheduled_at, ends_at, kind, responsible_manager_id, created_by) values ($1,$2,$3,'showroom',$4,$4)`, autoLead, visitAt, visitAt.Add(time.Hour), anna.ID)
	code, out = call(anna, server.handleListTasks, http.MethodGet, "/v1/tasks?view=upcoming&kinds=visit", "", nil, nil)
	autoItems, _ := out["items"].([]any)
	if code != http.StatusOK || len(autoItems) != 1 {
		t.Fatalf("visit feed: %d %v", code, out)
	}
	visit := autoItems[0].(map[string]any)
	if visit["source"] != "appointment" || visit["subKind"] != "showroom" || visit["canManage"] != false || visit["dueTime"] == nil || visit["title"] != nil {
		t.Fatalf("visit item: %v", visit)
	}
	code, out = call(anna, server.handleListTasks, http.MethodGet, "/v1/tasks?view=my_day&kinds=nonext", "", nil, nil)
	autoItems, _ = out["items"].([]any)
	if code != http.StatusOK || len(autoItems) != 0 {
		t.Fatalf("a lead with a scheduled visit has a next step: %d %v", code, out)
	}
	mustExec(t, pool, `update public.lead_showroom_visits set status='canceled' where lead_id=$1`, autoLead)
	code, out = call(anna, server.handleListTasks, http.MethodGet, "/v1/tasks?view=my_day&kinds=nonext", "", nil, nil)
	autoItems, _ = out["items"].([]any)
	if code != http.StatusOK || len(autoItems) != 1 || autoItems[0].(map[string]any)["subKind"] != "no_next_step" {
		t.Fatalf("no-next-step feed: %d %v", code, out)
	}

	// v1 keeps working: a status-only PATCH and the dashboard section query still run.
	if code, out := patch(anna, linked, 1, `{"status":"canceled"}`); code != http.StatusOK || out["status"] != "canceled" {
		t.Fatalf("v1 status patch: %d %v", code, out)
	}
	dashboardSQL, dashboardArgs := buildDashboardManagerTasksQuery(nil, anna.ID.String(), "important", nil)
	rows, err := pool.Query(ctx, dashboardSQL, dashboardArgs...)
	if err != nil {
		t.Fatal("dashboard query:", err)
	}
	rows.Close()
}
