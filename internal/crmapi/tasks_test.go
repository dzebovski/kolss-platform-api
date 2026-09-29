package crmapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCreateTaskValidationTrimsTitleAndValidatesDueDate(t *testing.T) {
	officeID := uuid.New()
	assigneeID := uuid.New()
	validDate := "2026-09-08"
	if fields := validateCreateTask(createTaskRequest{OfficeID: officeID, AssigneeID: assigneeID, Title: "  Call customer  ", DueDate: &validDate}); len(fields) != 0 {
		t.Fatalf("valid task rejected: %#v", fields)
	}

	tooLong := strings.Repeat("a", 1001)
	badDate := "08-09-2026"
	fields := validateCreateTask(createTaskRequest{Title: tooLong, DueDate: &badDate})
	for _, field := range []string{"officeId", "assigneeId", "title", "dueDate"} {
		if fields[field] == "" {
			t.Fatalf("expected validation error for %s, got %#v", field, fields)
		}
	}
}

func TestTaskDueDateUsesOfficeMidnight(t *testing.T) {
	location, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		t.Fatal(err)
	}
	date := "2026-07-23"
	got, err := parseTaskDueDate(&date, location)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, time.July, 22, 22, 0, 0, 0, time.UTC)
	if got == nil || !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, err := parseTaskDueDate(nil, location); err != nil || got != nil {
		t.Fatalf("null due date = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestTaskStatusValidation(t *testing.T) {
	for _, status := range []string{"open", "done", "canceled"} {
		if fields := validateUpdateTask(updateTaskRequest{Status: status}); len(fields) != 0 {
			t.Fatalf("%q rejected: %#v", status, fields)
		}
	}
	if fields := validateUpdateTask(updateTaskRequest{Status: "deleted"}); fields["status"] == "" {
		t.Fatal("unknown status must be rejected")
	}
}

func TestTaskMutationRequestGuards(t *testing.T) {
	s := New(Options{})
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader(`{"officeId":"`+uuid.NewString()+`","assigneeId":"`+uuid.NewString()+`","title":"x","dueDate":null}`))
	request = request.WithContext(context.WithValue(request.Context(), contextKey{}, Actor{}))
	response := httptest.NewRecorder()
	s.handleCreateTask(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "idempotency_key_required") {
		t.Fatalf("create without key = %d %s", response.Code, response.Body.String())
	}

	patch := httptest.NewRequest(http.MethodPatch, "/v1/tasks/"+uuid.NewString(), strings.NewReader(`{"status":"done"}`))
	patch.SetPathValue("taskId", uuid.NewString())
	patch = patch.WithContext(context.WithValue(patch.Context(), contextKey{}, Actor{}))
	response = httptest.NewRecorder()
	s.handleUpdateTask(response, patch)
	if response.Code != http.StatusPreconditionRequired || !strings.Contains(response.Body.String(), "version_required") {
		t.Fatalf("patch without If-Match = %d %s", response.Code, response.Body.String())
	}

	patch = httptest.NewRequest(http.MethodPatch, "/v1/tasks/"+uuid.NewString(), strings.NewReader(`{"status":"deleted"}`))
	patch.SetPathValue("taskId", uuid.NewString())
	patch.Header.Set("If-Match", "1")
	patch = patch.WithContext(context.WithValue(patch.Context(), contextKey{}, Actor{}))
	response = httptest.NewRecorder()
	s.handleUpdateTask(response, patch)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"status"`) {
		t.Fatalf("patch with invalid status = %d %s", response.Code, response.Body.String())
	}
}

func TestTaskPermissionOfficeScope(t *testing.T) {
	officeID := uuid.New()
	otherOfficeID := uuid.New()
	member := Actor{OfficeIDs: map[uuid.UUID]struct{}{officeID: {}}}
	if !member.CanManageTasks(officeID) || member.CanManageTasks(otherOfficeID) {
		t.Fatal("task permission must be limited to the actor's office memberships")
	}
	if !(Actor{Role: "super_admin"}).CanManageTasks(otherOfficeID) {
		t.Fatal("super admin must manage tasks in every office")
	}

}

func TestUpdateTaskRequestDistinguishesAbsentFromNull(t *testing.T) {
	var req updateTaskRequest
	if err := json.Unmarshal([]byte(`{"note":null,"dueTime":"10:30"}`), &req); err != nil {
		t.Fatal(err)
	}
	if !req.Note.Set || req.Note.Value != nil {
		t.Fatalf("note null must be set and empty: %#v", req.Note)
	}
	if req.DueDate.Set || !req.DueTime.Set || req.DueTime.Value == nil || *req.DueTime.Value != "10:30" {
		t.Fatalf("absent dueDate / present dueTime: %#v %#v", req.DueDate, req.DueTime)
	}
}

func TestValidateUpdateTaskAcceptsPartialUpdates(t *testing.T) {
	if fields := validateUpdateTask(updateTaskRequest{}); fields["status"] == "" {
		t.Fatal("an empty update must be rejected (v1 required status)")
	}
	title := "  "
	badDate, badTime := "2026-13-40", "25:00"
	fields := validateUpdateTask(updateTaskRequest{Title: &title, DueDate: optional[string]{Set: true, Value: &badDate}, DueTime: optional[string]{Set: true, Value: &badTime}})
	for _, field := range []string{"title", "dueDate", "dueTime"} {
		if fields[field] == "" {
			t.Fatalf("expected an error for %s, got %#v", field, fields)
		}
	}
	inProgress := true
	if fields := validateUpdateTask(updateTaskRequest{InProgress: &inProgress}); len(fields) != 0 {
		t.Fatalf("in progress alone is a valid update: %#v", fields)
	}
}

func TestApplyTaskPatchStateRules(t *testing.T) {
	date, timeOfDay := "2026-09-30", "10:00"
	open := taskState{Status: "open", Title: "Send estimate", DueDate: &date, DueTime: &timeOfDay}
	yes, no := true, false

	started, _, fields := applyTaskPatch(open, updateTaskRequest{InProgress: &yes})
	if len(fields) != 0 || !started.InProgress {
		t.Fatalf("start: %#v %#v", started, fields)
	}
	done, _, _ := applyTaskPatch(started, updateTaskRequest{Status: "done"})
	if done.Status != "done" || done.InProgress {
		t.Fatalf("done clears in progress: %#v", done)
	}
	if _, _, fields := applyTaskPatch(done, updateTaskRequest{InProgress: &yes}); fields["inProgress"] == "" {
		t.Fatal("a done task cannot be in progress")
	}
	reopened, _, _ := applyTaskPatch(done, updateTaskRequest{Status: "open"})
	if reopened.Status != "open" || reopened.InProgress {
		t.Fatalf("reopen starts at To do: %#v", reopened)
	}
	stopped, _, _ := applyTaskPatch(started, updateTaskRequest{InProgress: &no})
	if stopped.InProgress {
		t.Fatal("in progress can be switched off")
	}

	cleared, _, _ := applyTaskPatch(open, updateTaskRequest{DueDate: optional[string]{Set: true}})
	if cleared.DueDate != nil || cleared.DueTime != nil {
		t.Fatalf("clearing the date drops the time: %#v", cleared)
	}
	if _, _, fields := applyTaskPatch(cleared, updateTaskRequest{DueTime: optional[string]{Set: true, Value: &timeOfDay}}); fields["dueTime"] == "" {
		t.Fatal("a time needs a date")
	}
	if next, _, fields := applyTaskPatch(cleared, updateTaskRequest{DueDate: optional[string]{Set: true, Value: &date}, DueTime: optional[string]{Set: true, Value: &timeOfDay}}); len(fields) != 0 || next.DueTime == nil {
		t.Fatalf("date and time together: %#v %#v", next, fields)
	}
}

func TestApplyTaskPatchReportsContentChanges(t *testing.T) {
	note := "old"
	cur := taskState{Status: "open", Title: "Same", Note: &note}
	same, blank := "  Same ", "   "
	if _, effect, _ := applyTaskPatch(cur, updateTaskRequest{Title: &same}); effect.ContentChanged {
		t.Fatal("re-sending the same title is not a content change")
	}
	next, effect, _ := applyTaskPatch(cur, updateTaskRequest{Note: optional[string]{Set: true, Value: &blank}})
	if !effect.ContentChanged || next.Note != nil {
		t.Fatalf("a blank note clears it: %#v %#v", next, effect)
	}
	if _, effect, _ := applyTaskPatch(cur, updateTaskRequest{Status: "done"}); effect.ContentChanged {
		t.Fatal("a status change is not a content change")
	}
}

func TestCreateTaskValidationCoversV2Fields(t *testing.T) {
	base := createTaskRequest{OfficeID: uuid.New(), AssigneeID: uuid.New(), Title: "Task"}
	date, timeOfDay, badTime := "2026-09-30", "14:00", "9:5"
	note := strings.Repeat("n", taskNoteMax+1)
	listID := uuid.New()

	ok := base
	ok.DueDate, ok.DueTime, ok.Link = &date, &timeOfDay, &taskLinkRequest{Type: "lead", ID: uuid.New()}
	if fields := validateCreateTask(ok); len(fields) != 0 {
		t.Fatalf("valid v2 task rejected: %#v", fields)
	}
	noDate := base
	noDate.DueTime = &timeOfDay
	if fields := validateCreateTask(noDate); fields["dueTime"] == "" {
		t.Fatal("a time without a date must be rejected")
	}
	badFormat := base
	badFormat.DueDate, badFormat.DueTime, badFormat.Note = &date, &badTime, &note
	if fields := validateCreateTask(badFormat); fields["dueTime"] == "" || fields["note"] == "" {
		t.Fatalf("bad time / long note: %#v", validateCreateTask(badFormat))
	}
	both := base
	both.ListID, both.Link = &listID, &taskLinkRequest{Type: "lead", ID: uuid.New()}
	if fields := validateCreateTask(both); fields["link"] == "" {
		t.Fatal("a list and a link together must be rejected")
	}
	for _, linkType := range []string{"project", "client", "deal"} {
		req := base
		req.Link = &taskLinkRequest{Type: linkType, ID: uuid.New()}
		if fields := validateCreateTask(req); fields["link"] == "" {
			t.Fatalf("%s link must be rejected until it is available", linkType)
		}
	}
}

func TestTaskDueCombinesDateAndTimeInOfficeZone(t *testing.T) {
	location, err := time.LoadLocation("Europe/Kyiv")
	if err != nil {
		t.Fatal(err)
	}
	date, timeOfDay := "2026-07-23", "09:30"
	got, err := parseTaskDue(&date, &timeOfDay, location)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, time.July, 23, 6, 30, 0, 0, time.UTC); got == nil || !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, err := parseTaskDue(nil, &timeOfDay, location); err != nil || got != nil {
		t.Fatalf("a time without a date is ignored: (%v, %v)", got, err)
	}
}

func TestTaskContentEditPolicy(t *testing.T) {
	officeID, otherOffice := uuid.New(), uuid.New()
	creator := Actor{ID: uuid.New(), Role: "office_member", OfficeIDs: map[uuid.UUID]struct{}{officeID: {}}}
	assignee := Actor{ID: uuid.New(), Role: "office_member", OfficeIDs: map[uuid.UUID]struct{}{officeID: {}}}
	stranger := Actor{ID: uuid.New(), Role: "office_member", OfficeIDs: map[uuid.UUID]struct{}{officeID: {}}}
	officeAdmin := Actor{ID: uuid.New(), Role: "office_admin", OfficeIDs: map[uuid.UUID]struct{}{officeID: {}}}
	otherAdmin := Actor{ID: uuid.New(), Role: "office_admin", OfficeIDs: map[uuid.UUID]struct{}{otherOffice: {}}}
	listOwner := Actor{ID: uuid.New(), Role: "office_member", OfficeIDs: map[uuid.UUID]struct{}{otherOffice: {}}}
	task := taskAccess{OfficeID: officeID, CreatedBy: &creator.ID, AssigneeID: &assignee.ID, ListOwnerID: &listOwner.ID}

	for name, actor := range map[string]Actor{"creator": creator, "assignee": assignee, "office admin": officeAdmin, "list owner": listOwner, "super admin": {ID: uuid.New(), Role: "super_admin"}} {
		if !canEditTaskContent(actor, task) {
			t.Errorf("%s must edit the title and note", name)
		}
	}
	for name, actor := range map[string]Actor{"stranger": stranger, "admin of another office": otherAdmin} {
		if canEditTaskContent(actor, task) {
			t.Errorf("%s must not edit the title and note", name)
		}
	}
	if !canManageTask(listOwner, task) || canManageTask(otherAdmin, task) {
		t.Fatal("a list owner manages the list's tasks even outside the office; an admin of another office does not")
	}
}
