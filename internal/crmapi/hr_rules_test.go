package crmapi

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func hrTestDate(t *testing.T, value string) time.Time {
	t.Helper()
	d, ok := hrParseDate(value)
	if !ok {
		t.Fatalf("bad date %q", value)
	}
	return d
}

func TestHREditWindow(t *testing.T) {
	today := hrTestDate(t, "2026-11-17")
	cases := []struct {
		name  string
		admin bool
		scope hrEditScope
		date  string
		want  string
	}{
		{"hours today locked", false, hrScopeHours, "2026-11-17", hrErrPastDate},
		{"hours tomorrow open", false, hrScopeHours, "2026-11-18", ""},
		{"absence today open", false, hrScopeAbsences, "2026-11-17", ""},
		{"absence yesterday locked", false, hrScopeAbsences, "2026-11-16", hrErrPastDate},
		{"closed month wins", false, hrScopeAbsences, "2026-10-30", hrErrClosedMonth},
		{"admin edits closed month", true, hrScopeHours, "2026-10-01", ""},
	}
	for _, c := range cases {
		if got := hrEditWindowError(c.admin, c.scope, today, hrTestDate(t, c.date)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestHRValidateAbsence(t *testing.T) {
	comment := "  "
	cases := []struct {
		name  string
		req   hrAbsenceRequest
		code  string
		field string
	}{
		{"ok", hrAbsenceRequest{Type: "vacation", DateFrom: "2026-11-20", DateTo: "2026-11-24"}, "", ""},
		{"end before start", hrAbsenceRequest{Type: "vacation", DateFrom: "2026-11-20", DateTo: "2026-11-19"}, hrErrEndBefore, "dateTo"},
		{"other needs comment", hrAbsenceRequest{Type: "other", DateFrom: "2026-11-20", DateTo: "2026-11-20", Comment: &comment}, hrErrCommentNeeded, "comment"},
		{"half day only single day", hrAbsenceRequest{Type: "remote", DateFrom: "2026-11-20", DateTo: "2026-11-21", Part: "first_half"}, "validation_error", "part"},
		{"unknown type", hrAbsenceRequest{Type: "holiday", DateFrom: "2026-11-20", DateTo: "2026-11-20"}, "validation_error", "type"},
		{"make-up day allowed", hrAbsenceRequest{Type: "makeup_day", DateFrom: "2026-11-21", DateTo: "2026-11-21"}, "", ""},
	}
	for _, c := range cases {
		in, code, fields := hrValidateAbsence(c.req)
		if code != c.code {
			t.Errorf("%s: code %q, want %q (%v)", c.name, code, c.code, fields)
		}
		if c.field != "" && fields[c.field] == "" {
			t.Errorf("%s: missing field error %q (%v)", c.name, c.field, fields)
		}
		if c.code == "" && in.Part != hrPartFull && c.req.Part == "" {
			t.Errorf("%s: default part %q", c.name, in.Part)
		}
	}
}

func TestHRShiftWithinOffice(t *testing.T) {
	mon := hrOfficeDay{Open: true, Opens: 10 * 60, Closes: 19 * 60}
	cases := []struct {
		day        hrOfficeDay
		holiday    bool
		start, end int
		want       string
	}{
		{mon, false, 10 * 60, 18 * 60, ""},
		{mon, false, 9 * 60, 15 * 60, hrErrOutsideHours},
		{mon, false, 16 * 60, 12 * 60, hrErrEndBefore},
		{mon, true, 10 * 60, 14 * 60, hrErrOutsideHours},
		{hrOfficeDay{}, false, 10 * 60, 14 * 60, hrErrOutsideHours},
		{mon, false, 10*60 + 15, 14 * 60, "validation_error"},
	}
	for i, c := range cases {
		if got := hrShiftWithinOffice(c.day, c.holiday, c.start, c.end); got != c.want {
			t.Errorf("case %d: got %q, want %q", i, got, c.want)
		}
	}
}

func TestHRMakeupShiftClampsToClosing(t *testing.T) {
	sat := hrOfficeDay{Open: true, Opens: 10 * 60, Closes: 15 * 60}
	start, end, ok := hrMakeupShift(sat, 8, false)
	if !ok || start != 600 || end != 900 {
		t.Fatalf("got %d–%d ok=%v, want 10:00–15:00", start, end, ok)
	}
	if _, _, ok := hrMakeupShift(hrOfficeDay{}, 8, false); ok {
		t.Fatal("closed day must not get a make-up shift")
	}
}

func TestHRVisibleAbsencePrivacy(t *testing.T) {
	comment := "flu"
	typ, c := hrVisibleAbsence(hrTypeSick, &comment, false)
	if typ != hrTypeAbsent || c != nil {
		t.Fatalf("colleague sees %q / %v, want absent without comment", typ, c)
	}
	typ, c = hrVisibleAbsence(hrTypeSick, &comment, true)
	if typ != hrTypeSick || c == nil {
		t.Fatalf("person / admin sees %q / %v", typ, c)
	}
	if _, c = hrVisibleAbsence(hrTypeVacation, &comment, false); c != nil {
		t.Fatal("comments are private for every type")
	}
}

func TestHRVacationBalanceOverLimit(t *testing.T) {
	b := hrBalance(2026, 20, 22)
	if b.Left != 0 || b.Over != 2 {
		t.Fatalf("got %+v, want 2 over", b)
	}
	b = hrBalance(2026, 20, 5.5)
	if b.Left != 14.5 || b.Over != 0 {
		t.Fatalf("got %+v", b)
	}
}

func TestHRAggregateAndMakeupOwed(t *testing.T) {
	id := uuid.New()
	vac, makeup := hrTypeVacation, hrTypeDayOffMakeup
	rows := []hrDayRow{
		{EmployeeID: id, NormDays: 1, OfficeDays: 1, OfficeMinutes: 480},
		{EmployeeID: id, NormDays: 1, OfficeDays: 0.5, OfficeMinutes: 240, AbsenceType: &vac, AbsenceDays: 0.5},
		{EmployeeID: id, NormDays: 1, AbsenceType: &makeup, AbsenceDays: 1},
		{EmployeeID: id, OfficeDays: 1, OfficeMinutes: 300, MakeupDays: 0.5},
	}
	got := hrAggregate(rows)[id]
	if got.NormDays != 3 || got.PlannedDays != 2.5 || got.PlannedHours != 17 || got.Vacation != 0.5 || got.DayOffMakeup != 1 || got.MakeupDone != 0.5 {
		t.Fatalf("totals %+v", got)
	}
	if owed := hrMakeupOwed(rows); owed != 0.5 {
		t.Fatalf("owed %v, want 0.5", owed)
	}
}

func TestHRSummariseWeekWarnsOnly(t *testing.T) {
	planned, makeup := "planned", "makeup"
	rows := []hrDayRow{
		{ShiftKind: &planned, OfficeDays: 1, OfficeMinutes: 240},
		{ShiftKind: &planned, OfficeDays: 1, OfficeMinutes: 240},
		{ShiftKind: &makeup, OfficeDays: 1, OfficeMinutes: 480},
	}
	s := hrSummariseWeek(rows, 3, 4)
	if s.PlannedMinutes != 480 || s.Warning == nil || *s.Warning != "under" {
		t.Fatalf("summary %+v", s)
	}
}

func TestHRValidatePattern(t *testing.T) {
	office := hrOffice{Hours: map[int]hrOfficeDay{
		1: {Open: true, Opens: 600, Closes: 1140}, 2: {Open: true, Opens: 600, Closes: 1140}, 6: {Open: true, Opens: 600, Closes: 900},
	}}
	start, end := "10:00", "18:00"
	in, f := hrValidatePattern(hrPatternRequest{ValidFrom: "2026-11-23", Kind: "fixed", Weekdays: []int{2, 1}, StartTime: &start, EndTime: &end}, office, "")
	if len(f) > 0 || in.HoursPerDay != 8 || in.DaysPerWeek != 2 || in.Weekdays[0] != 1 {
		t.Fatalf("fixed: %+v %v", in, f)
	}
	_, f = hrValidatePattern(hrPatternRequest{ValidFrom: "2026-11-23", Kind: "fixed", Weekdays: []int{6}, StartTime: &start, EndTime: &end}, office, "")
	if f["startTime"] != hrErrOutsideHours {
		t.Fatalf("Saturday 10–18 must be outside office hours: %v", f)
	}
	dpw, hpd := 5, 4.0
	in, f = hrValidatePattern(hrPatternRequest{ValidFrom: "2026-11-23", Kind: "flexible", DaysPerWeek: &dpw, HoursPerDay: &hpd}, office, "")
	if len(f) > 0 || in.DaysPerWeek != 5 {
		t.Fatalf("flexible: %+v %v", in, f)
	}
	bad := 4.25
	if _, f = hrValidatePattern(hrPatternRequest{ValidFrom: "2026-11-23", Kind: "flexible", DaysPerWeek: &dpw, HoursPerDay: &bad}, office, ""); f["hoursPerDay"] == "" {
		t.Fatal("hours per day must be in half-hour steps")
	}
}

func TestHRSlackHidesSickAndEscapes(t *testing.T) {
	name, actor := "Ann <b>", "Admin"
	text := hrSlackMessage{Icon: "🗓️", Action: "Нова відсутність", Person: &name,
		Detail: hrSlackAbsence(hrTypeSick, hrTestDate(t, "2026-11-20"), hrTestDate(t, "2026-11-20"), hrPartFull), Actor: &actor}.text()
	if strings.Contains(strings.ToLower(text), "лікар") || !strings.Contains(text, "Відсутність") {
		t.Fatalf("sick leave must be posted as an absence: %s", text)
	}
	if !strings.Contains(text, "&lt;b&gt;") {
		t.Fatalf("names must be escaped: %s", text)
	}
}

func TestHRReportCSV(t *testing.T) {
	name := "Ola"
	report := &hrReportJSON{Period: "month", Year: 2026, Rows: []hrMonthReportRowJSON{{Person: hrReportPersonJSON{DisplayName: &name},
		Totals: hrTotals{NormDays: 20, PlannedDays: 18.5, Vacation: 1.5}, VacationLeft: 18.5}}}
	data, err := hrReportCSV(report, "en")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(strings.TrimPrefix(string(data), "\xef\xbb\xbf")), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "Employee,Position,Norm days") || !strings.HasPrefix(lines[1], "Ola,,20,18.5,0,1.5") {
		t.Fatalf("csv:\n%s", data)
	}
}
