package crmapi

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestTaskListValidation(t *testing.T) {
	desc, dates, color := "Fair", "12–15 Nov 2026", "#6d4ab8"
	if fields := validateCreateTaskList(createTaskListRequest{Name: "  Expo  ", Description: &desc, DatesText: &dates, Color: &color}); len(fields) != 0 {
		t.Fatalf("valid list rejected: %#v", fields)
	}
	long, badColor, nilOwner := strings.Repeat("x", 2001), "purple", uuid.Nil
	fields := validateCreateTaskList(createTaskListRequest{Name: " ", Description: &long, DatesText: &long, Color: &badColor, OwnerID: &nilOwner})
	for _, field := range []string{"name", "description", "datesText", "color", "ownerId"} {
		if fields[field] == "" {
			t.Fatalf("expected an error for %s, got %#v", field, fields)
		}
	}
	if fields := validateUpdateTaskList(updateTaskListRequest{}); fields["body"] == "" {
		t.Fatal("an empty list update must be rejected")
	}
	name := "Renamed"
	if fields := validateUpdateTaskList(updateTaskListRequest{Name: &name, DatesText: optional[string]{Set: true}}); len(fields) != 0 {
		t.Fatalf("renaming and clearing the dates is valid: %#v", fields)
	}
}

func TestTaskListEditPolicyAndColors(t *testing.T) {
	owner := uuid.New()
	if !canEditTaskList(Actor{ID: owner}, &owner) || !canEditTaskList(Actor{ID: uuid.New(), Role: "super_admin"}, &owner) {
		t.Fatal("the owner and a super admin edit a list")
	}
	if canEditTaskList(Actor{ID: uuid.New(), Role: "office_admin"}, &owner) || canEditTaskList(Actor{ID: owner}, nil) {
		t.Fatal("others (and an ownerless list) are not editable by non-super-admins")
	}
	if nextTaskListColor(0) == nextTaskListColor(1) || nextTaskListColor(0) != nextTaskListColor(len(taskListPalette)) {
		t.Fatal("colours must rotate through the palette")
	}
	for _, color := range taskListPalette {
		if !taskListColorPattern.MatchString(color) {
			t.Fatalf("palette colour %q does not satisfy the database check", color)
		}
	}
}
