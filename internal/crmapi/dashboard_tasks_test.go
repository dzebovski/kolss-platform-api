package crmapi

import "testing"

func TestDashboardTaskCursorRoundTripAndScope(t *testing.T) {
	raw := encodeDashboardTaskCursor(dashboardTaskCursor{
		OfficeID:  "office",
		ManagerID: "unassigned",
		Section:   "current",
		Bucket:    2,
		Date:      "9999-12-31",
		DueAt:     "9999-12-31T23:59:59Z",
		UpdatedAt: "2026-09-08T12:00:00Z",
		ID:        "lead:one",
	})
	cursor, fields := parseDashboardTaskCursor(raw, "office", "unassigned", "current")
	if len(fields) != 0 || cursor == nil || cursor.ID != "lead:one" {
		t.Fatalf("cursor=%#v fields=%#v", cursor, fields)
	}
	if _, fields := parseDashboardTaskCursor(raw, "other-office", "unassigned", "current"); fields["cursor"] == "" {
		t.Fatal("a cursor must not be reusable with another office filter")
	}
}

func TestDashboardTaskCursorRejectsMalformedOpenSortTuple(t *testing.T) {
	raw := encodeDashboardTaskCursor(dashboardTaskCursor{OfficeID: "", ManagerID: "unassigned", Section: "current", Bucket: 9, Date: "bad", DueAt: "also-bad", UpdatedAt: "2026-09-08T12:00:00Z", ID: "lead:one"})
	if _, fields := parseDashboardTaskCursor(raw, "", "unassigned", "current"); fields["cursor"] == "" {
		t.Fatal("open cursor with malformed seek tuple must be rejected before SQL")
	}
}

func TestDashboardTaskCursorHistoryDoesNotRequireOpenSortFields(t *testing.T) {
	raw := encodeDashboardTaskCursor(dashboardTaskCursor{OfficeID: "", ManagerID: "unassigned", Section: "done", UpdatedAt: "2026-09-08T12:00:00Z", ID: "manual:one"})
	if cursor, fields := parseDashboardTaskCursor(raw, "", "unassigned", "done"); len(fields) != 0 || cursor == nil {
		t.Fatalf("cursor=%#v fields=%#v", cursor, fields)
	}
}
