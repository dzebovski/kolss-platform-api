package crmapi

import (
	"context"
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
