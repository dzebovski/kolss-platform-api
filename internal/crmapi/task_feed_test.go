package crmapi

import (
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestParseTaskFeedKinds(t *testing.T) {
	kinds, ok := parseTaskFeedKinds("call, visit,call")
	if !ok || !reflect.DeepEqual(kinds, []string{"call", "visit"}) {
		t.Fatalf("got %v %v", kinds, ok)
	}
	if kinds, ok := parseTaskFeedKinds(""); !ok || kinds != nil {
		t.Fatal("no filter means every kind")
	}
	if _, ok := parseTaskFeedKinds("call,priority"); ok {
		t.Fatal("unknown kinds must be rejected")
	}
}

func TestTaskFeedQueryBindsFixedArguments(t *testing.T) {
	assignee := uuid.New()
	sql, args := buildTaskFeedQuery(taskFeedQuery{View: "upcoming", AssigneeID: &assignee, Kinds: []string{"call"}, Search: "  Kowalska "})
	if len(args) != 5 || args[4] != "Kowalska" || args[0] != nil {
		t.Fatalf("args: %#v", args)
	}
	for _, placeholder := range []string{"$1", "$2", "$3", "$4", "$5"} {
		if !strings.Contains(sql, placeholder) {
			t.Fatalf("query never uses %s, the driver would reject the argument list", placeholder)
		}
	}
	if !strings.Contains(sql, "limit 501") {
		t.Fatal("the feed is capped so the handler can report truncation")
	}
}

func TestOnlyAdminsViewAllTasks(t *testing.T) {
	if !canViewAllTasks(Actor{Role: "super_admin"}) || !canViewAllTasks(Actor{Role: "office_admin"}) {
		t.Fatal("admins see all tasks")
	}
	if canViewAllTasks(Actor{Role: "office_member"}) || canViewAllTasks(Actor{Role: "curator"}) {
		t.Fatal("members and curators do not")
	}
}
