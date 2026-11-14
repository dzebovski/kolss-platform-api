package crmapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestProjectMeetingsDB (P12): a project meeting and a project task on a disposable database
// (KOLSS_TEST_DATABASE_URL). The meeting keeps the lead's statuses, writes project events, can
// be marked held, shows in GET /activities, and is refused once the project is closed.
func TestProjectMeetingsDB(t *testing.T) {
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
	server := &Server{pool: pool, storage: fakeDocumentStorage{size: 1200}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	var officeID uuid.UUID
	if err := pool.QueryRow(ctx, `select id from public.offices order by code limit 1`).Scan(&officeID); err != nil {
		t.Fatal(err)
	}
	managerID := uuid.New()
	mustExec(t, pool, `insert into auth.users (id, email) values ($1, $2)`, managerID, managerID.String()+"@test.local")
	mustExec(t, pool, `insert into public.profiles (id, display_name) values ($1, 'Manager') on conflict (id) do nothing`, managerID)
	mustExec(t, pool, `insert into public.user_office_memberships (user_id, office_id) values ($1, $2)`, managerID, officeID)
	manager := Actor{ID: managerID, Role: "manager", IsActive: true, OfficeIDs: map[uuid.UUID]struct{}{officeID: {}}}

	leadID := uuid.New()
	mustExec(t, pool, `insert into public.leads (id, office_id, external_lead_id, name, phone, assigned_to) values ($1,$2,$3,'Meeting test','+48600000003',$4)`,
		leadID, officeID, leadID.String(), managerID)
	t.Cleanup(func() {
		mustExec(t, pool, `delete from public.tasks where link_project_id in (select id from public.projects where lead_id = $1)`, leadID)
		mustExec(t, pool, `delete from public.lead_showroom_visits where lead_id = $1`, leadID)
		mustExec(t, pool, `update public.leads set converted_project_id = null where id = $1`, leadID)
		mustExec(t, pool, `delete from public.projects where lead_id = $1`, leadID)
		mustExec(t, pool, `delete from public.lead_events where lead_id = $1`, leadID)
		mustExec(t, pool, `delete from public.leads where id = $1`, leadID)
	})

	call := func(handler http.HandlerFunc, method string, path map[string]string, body string, headers map[string]string) (int, map[string]any) {
		req := httptest.NewRequest(method, "/", strings.NewReader(body))
		for k, v := range path {
			req.SetPathValue(k, v)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		req = req.WithContext(context.WithValue(req.Context(), contextKey{}, manager))
		rec := httptest.NewRecorder()
		handler(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	idem := func() map[string]string { return map[string]string{"Idempotency-Key": uuid.NewString()} }

	code, project := call(server.handleCreateProject, http.MethodPost, map[string]string{"leadId": leadID.String()}, `{"projectType":"measure"}`, idem())
	if code != http.StatusCreated {
		t.Fatalf("create project: %d %v", code, project)
	}
	projectID := project["id"].(string)
	var clientStatusBefore string
	if err := pool.QueryRow(ctx, `select client_status from public.leads where id=$1`, leadID).Scan(&clientStatusBefore); err != nil {
		t.Fatal(err)
	}

	// Book a measurement on the project: the lead's status stays, the project gets the event.
	start := time.Now().AddDate(0, 0, 3).Format("2006-01-02") + "T10:00"
	body := `{"projectId":"` + projectID + `","kind":"measurement","startsAtLocal":"` + start + `","durationMinutes":60,"responsibleManagerId":"` + managerID.String() + `"}`
	code, created := call(server.handleCreateAppointment, http.MethodPost, nil, body, idem())
	if code != http.StatusCreated {
		t.Fatalf("create meeting: %d %v", code, created)
	}
	meeting := created["appointment"].(map[string]any)
	if meeting["projectId"] != projectID {
		t.Fatalf("meeting must carry the project: %v", meeting)
	}
	var clientStatusAfter string
	var leadEvents int
	if err := pool.QueryRow(ctx, `select client_status, (select count(*) from public.lead_events e where e.lead_id=l.id and e.event_type like 'appointment%') from public.leads l where id=$1`, leadID).Scan(&clientStatusAfter, &leadEvents); err != nil {
		t.Fatal(err)
	}
	if clientStatusAfter != clientStatusBefore || leadEvents != 0 {
		t.Fatalf("a project meeting must not touch the lead: %s → %s, %d lead events", clientStatusBefore, clientStatusAfter, leadEvents)
	}

	// Mark it held.
	version := int(meeting["version"].(float64))
	req := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"status":"visited"}`))
	req.SetPathValue("appointmentId", meeting["id"].(string))
	req.Header.Set("If-Match", `"`+strconv.Itoa(version)+`"`)
	req = req.WithContext(context.WithValue(req.Context(), contextKey{}, manager))
	rec := httptest.NewRecorder()
	server.handleUpdateAppointment(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mark held: %d %s", rec.Code, rec.Body.String())
	}

	// A task of the project.
	taskBody := `{"officeId":"` + officeID.String() + `","assigneeId":"` + managerID.String() + `","title":"Order the worktop","dueDate":null,"link":{"type":"project","id":"` + projectID + `"}}`
	if code, res := call(server.handleCreateTask, http.MethodPost, nil, taskBody, idem()); code != http.StatusCreated {
		t.Fatalf("create task: %d %v", code, res)
	}

	var events []string
	rows, err := pool.Query(ctx, `select event_type from public.project_events where project_id=$1 order by created_at`, projectID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var eventType string
		_ = rows.Scan(&eventType)
		events = append(events, eventType)
	}
	rows.Close()
	joined := strings.Join(events, ",")
	for _, want := range []string{"meeting_scheduled", "meeting_status_changed", "task_added"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("project events %s miss %s", joined, want)
		}
	}

	code, activities := call(server.handleListProjectActivities, http.MethodGet, map[string]string{"projectId": projectID}, ``, nil)
	if code != http.StatusOK {
		t.Fatalf("activities: %d %v", code, activities)
	}
	tasks, _ := activities["tasks"].([]any)
	meetings, _ := activities["meetings"].([]any)
	if len(tasks) != 1 || len(meetings) != 1 {
		t.Fatalf("activities: %d tasks, %d meetings", len(tasks), len(meetings))
	}
	link := tasks[0].(map[string]any)["link"].(map[string]any)
	if link["type"] != "project" || link["code"] == nil {
		t.Fatalf("task link: %v", link)
	}
	if meetings[0].(map[string]any)["status"] != "visited" {
		t.Fatalf("meeting status: %v", meetings[0])
	}

	// Closed project: no new meetings or tasks.
	if code, res := call(server.handleCancelProject, http.MethodPost, map[string]string{"projectId": projectID}, `{"reasons":["other"],"comment":"test"}`, nil); code != http.StatusOK {
		t.Fatalf("close: %d %v", code, res)
	}
	if code, res := call(server.handleCreateAppointment, http.MethodPost, nil, body, idem()); code != http.StatusConflict || res["code"] != "project_closed" {
		t.Fatalf("meeting on a closed project: %d %v", code, res)
	}
	if code, _ := call(server.handleCreateTask, http.MethodPost, nil, taskBody, idem()); code == http.StatusCreated {
		t.Fatal("task on a closed project must be refused")
	}
}
