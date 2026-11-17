package crmapi

// HR portal — pure rules (HB3/HB4). Decisions D1–D20 of the HR plan (owner, 2026-11-16).
// Everything here is free of I/O so it can be unit-tested; handlers load rows and call these.

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// hrNow is the clock used for every "today / tomorrow / this month" rule. Tests replace it.
var hrNow = time.Now

const hrDateLayout = "2006-01-02"

// HR absence types (D4). "makeup_day" is accepted by the Add absence endpoint but stored as an
// hr_shifts row with kind = 'makeup' (dev brief §3.1); it never lands in hr_absences.
const (
	hrTypeVacation     = "vacation"
	hrTypeSick         = "sick"
	hrTypeDayOffUnpaid = "day_off_unpaid"
	hrTypeDayOffMakeup = "day_off_makeup"
	hrTypeRemote       = "remote"
	hrTypeOccasional   = "occasional"
	hrTypeOther        = "other"
	hrTypeMakeupDay    = "makeup_day"
	// hrTypeAbsent is what colleagues see instead of "sick" (D8).
	hrTypeAbsent = "absent"
)

var hrStoredAbsenceTypes = map[string]bool{
	hrTypeVacation: true, hrTypeSick: true, hrTypeDayOffUnpaid: true, hrTypeDayOffMakeup: true,
	hrTypeRemote: true, hrTypeOccasional: true, hrTypeOther: true,
}

const (
	hrPartFull   = "full_day"
	hrPartFirst  = "first_half"
	hrPartSecond = "second_half"
)

// Structured error codes the CRM maps to the texts on HR-add-absence-errors / HR-plan-hours.
const (
	hrErrPastDate      = "hr_past_date"
	hrErrClosedMonth   = "hr_closed_month"
	hrErrOutsideHours  = "hr_outside_office_hours"
	hrErrEndBefore     = "hr_end_before_start"
	hrErrCommentNeeded = "hr_comment_required"
	hrErrNotTracked    = "hr_not_tracked"
	hrErrForbidden     = "hr_forbidden"
	// hrErrOverlap: two absences on the same day. Not drawn on the boards; TODO(hr) Q-H28.
	hrErrOverlap = "hr_overlap"
)

// hrPlanStepMinutes: planned hours are picked in 30-minute steps. TODO(hr): Q-H27 proposed default.
const hrPlanStepMinutes = 30

// ---- dates ----------------------------------------------------------------------------------

func hrParseDate(value string) (time.Time, bool) {
	t, err := time.Parse(hrDateLayout, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func hrFormatDate(t time.Time) string { return t.Format(hrDateLayout) }

// hrToday is the civil date in the office time zone (UTC midnight value). Rules never use the
// browser clock (dev brief §1).
func hrToday(timeZone string) time.Time {
	loc, err := time.LoadLocation(timeZone)
	if err != nil || timeZone == "" {
		loc = time.UTC
	}
	now := hrNow().In(loc)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

func hrMonthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func hrMonthEnd(t time.Time) time.Time {
	return hrMonthStart(t).AddDate(0, 1, -1)
}

// hrWeekStart returns the ISO Monday of the week containing t.
func hrWeekStart(t time.Time) time.Time {
	offset := (int(t.Weekday()) + 6) % 7
	return t.AddDate(0, 0, -offset)
}

func hrISOWeekday(t time.Time) int {
	wd := int(t.Weekday())
	if wd == 0 {
		return 7
	}
	return wd
}

// hrIsClosedMonth: a month before the current one (office time zone) is closed (D12).
func hrIsClosedMonth(date, today time.Time) bool {
	return hrMonthStart(date).Before(hrMonthStart(today))
}

// ---- clock times ----------------------------------------------------------------------------

// hrParseClock parses "HH:MM" (also "HH:MM:SS") into minutes after midnight.
func hrParseClock(value string) (int, bool) {
	value = strings.TrimSpace(value)
	if len(value) == 8 && strings.HasSuffix(value, ":00") {
		value = value[:5]
	}
	t, err := time.Parse("15:04", value)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

func hrFormatClock(minutes int) string {
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}

// ---- edit windows (D12, D17) -----------------------------------------------------------------

type hrEditScope int

const (
	// hrScopeHours: an employee changes own planned hours only from tomorrow on.
	hrScopeHours hrEditScope = iota
	// hrScopeAbsences: an employee creates / edits / deletes own absences from today on.
	hrScopeAbsences
)

// hrEditWindowError returns "" when the actor may change every date, otherwise the error code.
// The super admin edits anything, any date (the boards show a closed-month warning instead).
func hrEditWindowError(isSuperAdmin bool, scope hrEditScope, today time.Time, dates ...time.Time) string {
	if isSuperAdmin {
		return ""
	}
	for _, d := range dates {
		if hrIsClosedMonth(d, today) {
			return hrErrClosedMonth
		}
		switch scope {
		case hrScopeHours:
			if !d.After(today) {
				return hrErrPastDate
			}
		case hrScopeAbsences:
			if d.Before(today) {
				return hrErrPastDate
			}
		}
	}
	return ""
}

// hrEditableFrom is the first date the actor may change for a scope (shown to the UI).
func hrEditableFrom(isSuperAdmin bool, scope hrEditScope, today time.Time) *string {
	if isSuperAdmin {
		return nil
	}
	if scope == hrScopeHours {
		v := hrFormatDate(today.AddDate(0, 0, 1))
		return &v
	}
	v := hrFormatDate(today)
	return &v
}

// ---- absence validation ---------------------------------------------------------------------

type hrAbsenceInput struct {
	Type     string
	DateFrom time.Time
	DateTo   time.Time
	Part     string
	Comment  *string
}

type hrAbsenceRequest struct {
	EmployeeID *string `json:"employeeId"`
	Type       string  `json:"type"`
	DateFrom   string  `json:"dateFrom"`
	DateTo     string  `json:"dateTo"`
	Part       string  `json:"part"`
	Comment    *string `json:"comment"`
}

// hrValidateAbsence checks shape rules (D4, D20). It returns the first rule code (or
// validation_error) and per-field codes.
func hrValidateAbsence(req hrAbsenceRequest) (hrAbsenceInput, string, map[string]string) {
	fields := map[string]string{}
	in := hrAbsenceInput{Type: strings.TrimSpace(req.Type), Part: strings.TrimSpace(req.Part)}
	if in.Part == "" {
		in.Part = hrPartFull
	}
	if !hrStoredAbsenceTypes[in.Type] && in.Type != hrTypeMakeupDay {
		fields["type"] = "validation_error"
	}
	from, okFrom := hrParseDate(req.DateFrom)
	to, okTo := hrParseDate(req.DateTo)
	if !okFrom {
		fields["dateFrom"] = "validation_error"
	}
	if !okTo {
		fields["dateTo"] = "validation_error"
	}
	in.DateFrom, in.DateTo = from, to
	code := ""
	if okFrom && okTo {
		if to.Before(from) {
			fields["dateTo"] = hrErrEndBefore
			code = hrErrEndBefore
		} else if to.Sub(from) > 366*24*time.Hour {
			fields["dateTo"] = "validation_error"
		}
	}
	switch in.Part {
	case hrPartFull:
	case hrPartFirst, hrPartSecond:
		if okFrom && okTo && !from.Equal(to) {
			fields["part"] = "validation_error"
		}
	default:
		fields["part"] = "validation_error"
	}
	if req.Comment != nil {
		c := strings.TrimSpace(*req.Comment)
		if len([]rune(c)) > 1000 {
			fields["comment"] = "validation_error"
		}
		if c != "" {
			in.Comment = &c
		}
	}
	if in.Type == hrTypeOther && in.Comment == nil {
		fields["comment"] = hrErrCommentNeeded
		if code == "" {
			code = hrErrCommentNeeded
		}
	}
	if len(fields) > 0 && code == "" {
		code = "validation_error"
	}
	return in, code, fields
}

// hrDatesBetween lists every date from..to inclusive.
func hrDatesBetween(from, to time.Time) []time.Time {
	out := []time.Time{}
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		out = append(out, d)
	}
	return out
}

// ---- office hours / shifts -------------------------------------------------------------------

// hrOfficeDay is one weekday of the office hours (minutes after midnight; closed = !Open).
type hrOfficeDay struct {
	Open   bool
	Opens  int
	Closes int
}

// hrShiftWithinOffice: planned hours must lie inside office hours (D6) and the office must be
// open that day (not a holiday / office-closed day).
func hrShiftWithinOffice(day hrOfficeDay, isHoliday bool, start, end int) string {
	if end <= start {
		return hrErrEndBefore
	}
	if !day.Open || isHoliday || start < day.Opens || end > day.Closes {
		return hrErrOutsideHours
	}
	if start%hrPlanStepMinutes != 0 || end%hrPlanStepMinutes != 0 {
		return "validation_error"
	}
	return ""
}

// hrMakeupShift is the make-up shift created from "Make-up day" in Add absence: the person's
// daily norm (half for a half day), starting at office opening, clamped to closing time.
// TODO(hr): clamping a long norm on a short day (e.g. Sat 10–15) is a proposed default.
func hrMakeupShift(day hrOfficeDay, hoursPerDay float64, half bool) (int, int, bool) {
	if !day.Open {
		return 0, 0, false
	}
	minutes := int(math.Round(hoursPerDay * 60))
	if half {
		minutes /= 2
	}
	minutes = (minutes / hrPlanStepMinutes) * hrPlanStepMinutes
	if minutes <= 0 {
		minutes = hrPlanStepMinutes
	}
	end := day.Opens + minutes
	if end > day.Closes {
		end = day.Closes
	}
	return day.Opens, end, true
}

// ---- day rows and aggregation (HB3) ----------------------------------------------------------

// hrDayRow mirrors one row of public.hr_days.
type hrDayRow struct {
	EmployeeID     uuid.UUID
	Day            time.Time
	OfficeID       uuid.UUID
	IsOfficeOpen   bool
	Opens          *string
	Closes         *string
	HolidayKind    *string
	HolidayName    *string
	PatternID      *uuid.UUID
	PatternKind    *string
	HoursPerDay    *float64
	DaysPerWeek    *int
	IsPatternDay   bool
	ShiftID        *uuid.UUID
	ShiftKind      *string
	PlannedStart   *string
	PlannedEnd     *string
	PlannedMinutes int
	IsWorkday      bool
	AbsenceID      *uuid.UUID
	AbsenceType    *string
	AbsencePart    *string
	AbsenceDays    float64
	IsMakeup       bool
	MakeupDays     float64
	OfficeDays     float64
	OfficeMinutes  int
	NormDays       float64
}

func (r hrDayRow) openDay() bool { return r.IsOfficeOpen && r.HolidayKind == nil }

// absenceVisible: the calendar shows an absence on a day the person would work — fixed: a
// planned day; flexible: any open non-holiday day (they plan hours themselves).
func (r hrDayRow) absenceVisible() bool {
	if r.AbsenceID == nil || !r.openDay() {
		return false
	}
	if r.PatternKind != nil && *r.PatternKind == "flexible" {
		return true
	}
	return r.PlannedStart != nil
}

// hrTotals is one report row (HR-reports-month), all in days except PlannedHours.
type hrTotals struct {
	NormDays     float64 `json:"normDays"`
	PlannedDays  float64 `json:"plannedDays"`
	PlannedHours float64 `json:"plannedHours"`
	Vacation     float64 `json:"vacation"`
	Sick         float64 `json:"sick"`
	DayOffUnpaid float64 `json:"dayOffUnpaid"`
	DayOffMakeup float64 `json:"dayOffMakeup"`
	MakeupDone   float64 `json:"makeupDone"`
	Remote       float64 `json:"remote"`
	Occasional   float64 `json:"occasional"`
	Other        float64 `json:"other"`
}

func (t *hrTotals) add(r hrDayRow) {
	t.NormDays += r.NormDays
	t.PlannedDays += r.OfficeDays
	t.PlannedHours += float64(r.OfficeMinutes) / 60
	t.MakeupDone += r.MakeupDays
	if r.AbsenceType == nil || r.AbsenceDays == 0 {
		return
	}
	switch *r.AbsenceType {
	case hrTypeVacation:
		t.Vacation += r.AbsenceDays
	case hrTypeSick:
		t.Sick += r.AbsenceDays
	case hrTypeDayOffUnpaid:
		t.DayOffUnpaid += r.AbsenceDays
	case hrTypeDayOffMakeup:
		t.DayOffMakeup += r.AbsenceDays
	case hrTypeRemote:
		t.Remote += r.AbsenceDays
	case hrTypeOccasional:
		t.Occasional += r.AbsenceDays
	case hrTypeOther:
		t.Other += r.AbsenceDays
	}
}

// round: norm to 0.5 (flexible norms are prorated), hours to 0.1, the rest are already halves.
func (t hrTotals) round() hrTotals {
	t.NormDays = hrRoundHalf(t.NormDays)
	t.PlannedDays = hrRoundHalf(t.PlannedDays)
	t.PlannedHours = math.Round(t.PlannedHours*10) / 10
	return t
}

func hrRoundHalf(v float64) float64 { return math.Round(v*2) / 2 }

// hrAggregate groups day rows per employee.
func hrAggregate(rows []hrDayRow) map[uuid.UUID]hrTotals {
	acc := map[uuid.UUID]*hrTotals{}
	for _, r := range rows {
		t := acc[r.EmployeeID]
		if t == nil {
			t = &hrTotals{}
			acc[r.EmployeeID] = t
		}
		t.add(r)
	}
	out := make(map[uuid.UUID]hrTotals, len(acc))
	for id, t := range acc {
		out[id] = t.round()
	}
	return out
}

// hrMakeupOwed = Σ "day off to be made up" − Σ make-up days, never below 0 (D11).
func hrMakeupOwed(rows []hrDayRow) float64 {
	owed := 0.0
	for _, r := range rows {
		if r.AbsenceType != nil && *r.AbsenceType == hrTypeDayOffMakeup {
			owed += r.AbsenceDays
		}
		owed -= r.MakeupDays
	}
	if owed < 0 {
		return 0
	}
	return hrRoundHalf(owed)
}

// hrVacationUsed = Σ vacation days on working days in the rows (callers pass one year).
func hrVacationUsed(rows []hrDayRow) float64 {
	used := 0.0
	for _, r := range rows {
		if r.AbsenceType != nil && *r.AbsenceType == hrTypeVacation {
			used += r.AbsenceDays
		}
	}
	return hrRoundHalf(used)
}

type hrVacationBalance struct {
	Year  int     `json:"year"`
	Limit float64 `json:"limit"`
	Used  float64 `json:"used"`
	Left  float64 `json:"left"`
	Over  float64 `json:"over"`
}

// hrBalance: over the limit is allowed and only shown (D10).
func hrBalance(year int, limit, used float64) hrVacationBalance {
	b := hrVacationBalance{Year: year, Limit: limit, Used: used}
	if used > limit {
		b.Over = used - limit
	} else {
		b.Left = limit - used
	}
	return b
}

// ---- privacy (D8) ---------------------------------------------------------------------------

// hrVisibleAbsence applies sick-leave privacy: anyone except the person and the super admin sees
// "absent", and comments of every type are only for the person and the super admin.
func hrVisibleAbsence(absenceType string, comment *string, canSeePrivate bool) (string, *string) {
	if canSeePrivate {
		return absenceType, comment
	}
	if absenceType == hrTypeSick {
		return hrTypeAbsent, nil
	}
	return absenceType, nil
}

// ---- staffing (D7) --------------------------------------------------------------------------

type hrDayTotal struct {
	Date        string  `json:"date"`
	IsOpen      bool    `json:"isOpen"`
	HolidayKind *string `json:"holidayKind"`
	HolidayName *string `json:"holidayName"`
	InOffice    int     `json:"inOffice"`
	NobodyIn    bool    `json:"nobodyIn"`
}

// hrDayTotals counts people in the office per day. "Nobody in" = an open, non-holiday day with
// tracked people in the office calendar and none of them planned (computed, not stored).
func hrDayTotals(from, to time.Time, office map[int]hrOfficeDay, holidays map[string]hrHoliday, rows []hrDayRow, peopleByDay map[string]int) []hrDayTotal {
	in := map[string]int{}
	for _, r := range rows {
		if r.OfficeDays > 0 {
			in[hrFormatDate(r.Day)]++
		}
	}
	out := []hrDayTotal{}
	for _, d := range hrDatesBetween(from, to) {
		key := hrFormatDate(d)
		t := hrDayTotal{Date: key, IsOpen: office[hrISOWeekday(d)].Open, InOffice: in[key]}
		if h, ok := holidays[key]; ok {
			kind, name := h.Kind, h.Name
			t.HolidayKind, t.HolidayName = &kind, &name
		}
		t.NobodyIn = t.IsOpen && t.HolidayKind == nil && peopleByDay[key] > 0 && t.InOffice == 0
		out = append(out, t)
	}
	return out
}

type hrHoliday struct {
	ID       uuid.UUID `json:"id"`
	OfficeID uuid.UUID `json:"officeId"`
	Day      string    `json:"day"`
	Kind     string    `json:"kind"`
	Name     string    `json:"name"`
}

// ---- week planner summary (Plan hours counter) ------------------------------------------------

type hrWeekSummary struct {
	PlannedMinutes int     `json:"plannedMinutes"`
	PlannedDays    float64 `json:"plannedDays"`
	NormMinutes    int     `json:"normMinutes"`
	NormDays       int     `json:"normDays"`
	// Warning: "under" / "over" the weekly norm — never an error (D6, plan §6).
	Warning *string `json:"warning"`
}

func hrSummariseWeek(rows []hrDayRow, daysPerWeek int, hoursPerDay float64) hrWeekSummary {
	s := hrWeekSummary{NormDays: daysPerWeek, NormMinutes: int(math.Round(float64(daysPerWeek) * hoursPerDay * 60))}
	for _, r := range rows {
		if r.ShiftKind != nil && *r.ShiftKind == "makeup" {
			continue // make-up days are on top of the weekly norm
		}
		s.PlannedMinutes += r.OfficeMinutes
		s.PlannedDays += r.OfficeDays
	}
	if daysPerWeek > 0 {
		var w string
		switch {
		case s.PlannedMinutes < s.NormMinutes:
			w = "under"
		case s.PlannedMinutes > s.NormMinutes:
			w = "over"
		}
		if w != "" {
			s.Warning = &w
		}
	}
	return s
}

// hrSortedIDs returns map keys in a stable order (tests, CSV).
func hrSortedIDs(m map[uuid.UUID]hrTotals) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}
