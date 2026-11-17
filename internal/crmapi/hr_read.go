package crmapi

// HR portal — read endpoints for every tracked person (HB4): GET /v1/hr/me, /v1/hr/calendar,
// /v1/hr/entries. Privacy (D8) is applied here, not in the UI.

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// hrViewer is the actor seen through HR: the super admin and/or a tracked employee.
type hrViewer struct {
	actor Actor
	self  *hrEmployee
	today time.Time // in the viewer's office time zone (Europe/Warsaw when not tracked)
}

func (v hrViewer) isAdmin() bool { return v.actor.IsSuperAdmin() }

func (v hrViewer) tracked() bool { return v.self != nil && v.self.isTracked(v.today) }

// canSeePrivate: the person themself or the super admin (D8).
func (v hrViewer) canSeePrivate(employeeID uuid.UUID) bool {
	return v.isAdmin() || (v.self != nil && v.self.ID == employeeID)
}

func (s *Server) hrViewer(ctx context.Context, actor Actor) (hrViewer, error) {
	v := hrViewer{actor: actor}
	self, err := hrEmployeeByProfile(ctx, s.pool, actor.ID)
	if err != nil {
		return v, err
	}
	v.self = self
	tz := "Europe/Warsaw"
	if self != nil && self.TimeZone != "" {
		tz = self.TimeZone
	}
	v.today = hrToday(tz)
	return v, nil
}

// hrRequireViewer: HR pages are for tracked people and the super admin (D5, D16).
func (s *Server) hrRequireViewer(w http.ResponseWriter, r *http.Request) (hrViewer, bool) {
	actor, _ := actorFromContext(r.Context())
	v, err := s.hrViewer(r.Context(), actor)
	if err != nil {
		s.hrFail(w, r, err)
		return v, false
	}
	if !v.isAdmin() && !v.tracked() {
		s.writeError(w, r, http.StatusForbidden, hrErrNotTracked, "You are not tracked in HR", nil)
		return v, false
	}
	return v, true
}

func (s *Server) hrRequireAdmin(w http.ResponseWriter, r *http.Request) (hrViewer, bool) {
	v, ok := s.hrRequireViewer(w, r)
	if !ok {
		return v, false
	}
	if !v.isAdmin() {
		s.writeError(w, r, http.StatusForbidden, hrErrForbidden, "Super admin only", nil)
		return v, false
	}
	return v, true
}

// ---- JSON shapes ----------------------------------------------------------------------------

type hrOfficeJSON struct {
	ID                  uuid.UUID           `json:"id"`
	Code                string              `json:"code"`
	NameUK              string              `json:"nameUk"`
	NamePL              string              `json:"namePl"`
	TimeZone            string              `json:"timeZone"`
	DefaultVacationDays float64             `json:"defaultVacationDays"`
	Hours               []hrOfficeHoursJSON `json:"hours"`
}

func (o hrOffice) json() hrOfficeJSON {
	return hrOfficeJSON{ID: o.ID, Code: o.Code, NameUK: o.NameUK, NamePL: o.NamePL, TimeZone: o.TimeZone,
		DefaultVacationDays: o.DefaultVacationDays, Hours: o.hoursJSON()}
}

type hrEmployeeJSON struct {
	ID           uuid.UUID      `json:"id"`
	ProfileID    uuid.UUID      `json:"profileId"`
	DisplayName  *string        `json:"displayName"`
	Email        *string        `json:"email,omitempty"`
	OfficeID     uuid.UUID      `json:"officeId"`
	OfficeCode   string         `json:"officeCode"`
	Position     *string        `json:"position"`
	StartedOn    string         `json:"startedOn"`
	EndedOn      *string        `json:"endedOn"`
	TrackedUntil *string        `json:"trackedUntil"`
	IsTracked    bool           `json:"isTracked"`
	Pattern      *hrPatternJSON `json:"pattern"`
}

func hrEmployeeView(e hrEmployee, today time.Time, pattern *hrPatternJSON, withEmail bool) hrEmployeeJSON {
	out := hrEmployeeJSON{
		ID: e.ID, ProfileID: e.ProfileID, DisplayName: e.DisplayName, OfficeID: e.OfficeID, OfficeCode: e.OfficeCode,
		Position: e.Position, StartedOn: hrFormatDate(e.StartedOn), EndedOn: hrDateString(e.EndedOn),
		TrackedUntil: hrDateString(e.TrackedUntil), IsTracked: e.isTracked(today), Pattern: pattern,
	}
	if withEmail {
		out.Email = e.Email
	}
	return out
}

type hrAbsenceCellJSON struct {
	ID            *uuid.UUID `json:"id,omitempty"`
	Type          string     `json:"type"`
	Part          string     `json:"part"`
	DateFrom      string     `json:"dateFrom"`
	DateTo        string     `json:"dateTo"`
	Comment       *string    `json:"comment,omitempty"`
	CreatedAt     *time.Time `json:"createdAt,omitempty"`
	CreatedByName *string    `json:"createdByName,omitempty"`
}

type hrCellJSON struct {
	EmployeeID     uuid.UUID          `json:"employeeId"`
	Date           string             `json:"date"`
	IsWorkday      bool               `json:"isWorkday"`
	IsPatternDay   bool               `json:"isPatternDay"`
	PlannedStart   *string            `json:"plannedStart"`
	PlannedEnd     *string            `json:"plannedEnd"`
	PlannedMinutes int                `json:"plannedMinutes"`
	ShiftKind      *string            `json:"shiftKind"`
	IsMakeup       bool               `json:"isMakeup"`
	OfficeDays     float64            `json:"officeDays"`
	Absence        *hrAbsenceCellJSON `json:"absence"`
}

type hrAbsenceDetail struct {
	ID            uuid.UUID
	Type          string
	DateFrom      time.Time
	DateTo        time.Time
	Part          string
	Comment       *string
	CreatedAt     time.Time
	CreatedBy     *uuid.UUID
	CreatedByName *string
	UpdatedAt     time.Time
}

func (s *Server) hrAbsenceDetails(ctx context.Context, q hrQuerier, ids []uuid.UUID) (map[uuid.UUID]hrAbsenceDetail, error) {
	out := map[uuid.UUID]hrAbsenceDetail{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		select a.id, a.type, a.date_from, a.date_to, a.part, a.comment, a.created_at, a.created_by, p.display_name, a.updated_at
		from public.hr_absences a left join public.profiles p on p.id = a.created_by
		where a.id = any($1::uuid[])
	`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d hrAbsenceDetail
		if err := rows.Scan(&d.ID, &d.Type, &d.DateFrom, &d.DateTo, &d.Part, &d.Comment, &d.CreatedAt, &d.CreatedBy, &d.CreatedByName, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out[d.ID] = d
	}
	return out, rows.Err()
}

// hrCells turns day rows into calendar cells with privacy applied for the viewer.
func (s *Server) hrCells(ctx context.Context, v hrViewer, rows []hrDayRow) ([]hrCellJSON, error) {
	ids := []uuid.UUID{}
	seen := map[uuid.UUID]bool{}
	for _, r := range rows {
		if r.absenceVisible() && !seen[*r.AbsenceID] {
			seen[*r.AbsenceID] = true
			ids = append(ids, *r.AbsenceID)
		}
	}
	details, err := s.hrAbsenceDetails(ctx, s.pool, ids)
	if err != nil {
		return nil, err
	}
	cells := make([]hrCellJSON, 0, len(rows))
	for _, r := range rows {
		c := hrCellJSON{
			EmployeeID: r.EmployeeID, Date: hrFormatDate(r.Day), IsWorkday: r.IsWorkday, IsPatternDay: r.IsPatternDay,
			PlannedStart: r.PlannedStart, PlannedEnd: r.PlannedEnd, PlannedMinutes: r.PlannedMinutes,
			ShiftKind: r.ShiftKind, IsMakeup: r.IsMakeup, OfficeDays: r.OfficeDays,
		}
		if r.absenceVisible() {
			d := details[*r.AbsenceID]
			private := v.canSeePrivate(r.EmployeeID)
			t, comment := hrVisibleAbsence(d.Type, d.Comment, private)
			a := &hrAbsenceCellJSON{Type: t, Part: d.Part, DateFrom: hrFormatDate(d.DateFrom), DateTo: hrFormatDate(d.DateTo), Comment: comment}
			if private {
				id, created := d.ID, d.CreatedAt
				a.ID, a.CreatedAt, a.CreatedByName = &id, &created, d.CreatedByName
			}
			c.Absence = a
		}
		cells = append(cells, c)
	}
	return cells, nil
}

// ---- GET /v1/hr/me --------------------------------------------------------------------------

type hrNextDayJSON struct {
	Date    string `json:"date"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Minutes int    `json:"minutes"`
}

type hrMeResponse struct {
	Tracked              bool               `json:"tracked"`
	CanManageHR          bool               `json:"canManageHr"`
	Today                string             `json:"today"`
	Month                string             `json:"month"`
	Employee             *hrEmployeeJSON    `json:"employee"`
	Office               *hrOfficeJSON      `json:"office"`
	EditableHoursFrom    *string            `json:"editableHoursFrom"`
	EditableAbsencesFrom *string            `json:"editableAbsencesFrom"`
	MonthTotals          *hrTotals          `json:"monthTotals"`
	Vacation             *hrVacationBalance `json:"vacation"`
	MakeupOwed           float64            `json:"makeupOwed"`
	NextWorkingDay       *hrNextDayJSON     `json:"nextWorkingDay"`
}

func (s *Server) handleHRMe(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFromContext(r.Context())
	v, err := s.hrViewer(r.Context(), actor)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	month := hrMonthStart(v.today)
	if raw := strings.TrimSpace(r.URL.Query().Get("month")); raw != "" {
		m, ok := hrParseDate(raw + "-01")
		if !ok {
			s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid month", map[string]string{"month": "validation_error"})
			return
		}
		month = m
	}
	resp := hrMeResponse{Tracked: v.tracked(), CanManageHR: v.isAdmin(), Today: hrFormatDate(v.today), Month: month.Format("2006-01")}
	if !resp.Tracked {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	e := *v.self
	ctx := r.Context()
	pattern, err := hrPatternOn(ctx, s.pool, e.ID, v.today)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	office, err := hrLoadOffice(ctx, s.pool, e.OfficeID)
	if err != nil || office == nil {
		s.hrFail(w, r, err)
		return
	}
	view := hrEmployeeView(e, v.today, pattern, true)
	officeJSON := office.json()
	resp.Employee, resp.Office = &view, &officeJSON
	resp.EditableHoursFrom = hrEditableFrom(v.isAdmin(), hrScopeHours, v.today)
	resp.EditableAbsencesFrom = hrEditableFrom(v.isAdmin(), hrScopeAbsences, v.today)

	monthRows, err := hrLoadDays(ctx, s.pool, nil, month, hrMonthEnd(month), &e.ID)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	totals := hrAggregate(monthRows)[e.ID]
	resp.MonthTotals = &totals
	balances, err := hrLoadBalances(ctx, s.pool, e, v.today.Year())
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	resp.Vacation, resp.MakeupOwed = &balances.Vacation, balances.MakeupOwed

	ahead, err := hrLoadDays(ctx, s.pool, nil, v.today.AddDate(0, 0, 1), v.today.AddDate(0, 0, 62), &e.ID)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	for _, d := range ahead {
		if d.OfficeDays > 0 && d.PlannedStart != nil && d.PlannedEnd != nil {
			resp.NextWorkingDay = &hrNextDayJSON{Date: hrFormatDate(d.Day), Start: *d.PlannedStart, End: *d.PlannedEnd, Minutes: d.OfficeMinutes}
			break
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- GET /v1/hr/calendar --------------------------------------------------------------------

type hrCalendarResponse struct {
	Office   hrOfficeJSON     `json:"office"`
	From     string           `json:"from"`
	To       string           `json:"to"`
	Today    string           `json:"today"`
	Holidays []hrHoliday      `json:"holidays"`
	People   []hrEmployeeJSON `json:"people"`
	Cells    []hrCellJSON     `json:"cells"`
	Days     []hrDayTotal     `json:"days"`
}

// hrParseRange reads from/to (inclusive, at most maxDays long).
func hrParseRange(r *http.Request, maxDays int) (time.Time, time.Time, map[string]string) {
	q := r.URL.Query()
	from, okFrom := hrParseDate(q.Get("from"))
	to, okTo := hrParseDate(q.Get("to"))
	fields := map[string]string{}
	if !okFrom {
		fields["from"] = "validation_error"
	}
	if !okTo {
		fields["to"] = "validation_error"
	}
	if okFrom && okTo && (to.Before(from) || to.Sub(from) > time.Duration(maxDays)*24*time.Hour) {
		fields["to"] = "validation_error"
	}
	return from, to, fields
}

func hrQueryUUID(r *http.Request, name string) (*uuid.UUID, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return nil, true
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, false
	}
	return &id, true
}

func (s *Server) handleHRCalendar(w http.ResponseWriter, r *http.Request) {
	v, ok := s.hrRequireViewer(w, r)
	if !ok {
		return
	}
	officeID, okOffice := hrQueryUUID(r, "officeId")
	employeeID, okEmployee := hrQueryUUID(r, "employeeId")
	from, to, fields := hrParseRange(r, 62)
	if !okOffice || officeID == nil {
		fields["officeId"] = "validation_error"
	}
	if !okEmployee {
		fields["employeeId"] = "validation_error"
	}
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid calendar request", fields)
		return
	}
	// Employees see their own HR office and the offices they belong to; the super admin all.
	if !v.isAdmin() && v.self.OfficeID != *officeID && !v.actor.CanAccessOffice(*officeID) {
		s.writeError(w, r, http.StatusForbidden, hrErrForbidden, "No access to this office", nil)
		return
	}
	ctx := r.Context()
	office, err := hrLoadOffice(ctx, s.pool, *officeID)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	if office == nil {
		s.writeError(w, r, http.StatusNotFound, "not_found", "Office not found", nil)
		return
	}
	officeToday := hrToday(office.TimeZone)
	rows, err := hrLoadDays(ctx, s.pool, officeID, from, to, employeeID)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	holidays, err := hrLoadHolidays(ctx, s.pool, *officeID, from, to)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	employees, err := hrListEmployees(ctx, s.pool, officeID)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	present := map[uuid.UUID]bool{}
	peopleByDay := map[string]int{}
	for _, row := range rows {
		present[row.EmployeeID] = true
		peopleByDay[hrFormatDate(row.Day)]++
	}
	patternDay := officeToday
	if patternDay.Before(from) || patternDay.After(to) {
		patternDay = from
	}
	people := []hrEmployeeJSON{}
	for _, e := range employees {
		if !present[e.ID] {
			continue
		}
		pattern, err := hrPatternOn(ctx, s.pool, e.ID, patternDay)
		if err != nil {
			s.hrFail(w, r, err)
			return
		}
		people = append(people, hrEmployeeView(e, officeToday, pattern, false))
	}
	cells, err := s.hrCells(ctx, v, rows)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	holidayMap := map[string]hrHoliday{}
	for _, h := range holidays {
		holidayMap[h.Day] = h
	}
	writeJSON(w, http.StatusOK, hrCalendarResponse{
		Office: office.json(), From: hrFormatDate(from), To: hrFormatDate(to), Today: hrFormatDate(officeToday),
		Holidays: holidays, People: people, Cells: cells,
		Days: hrDayTotals(from, to, office.Hours, holidayMap, rows, peopleByDay),
	})
}

// ---- GET /v1/hr/entries ---------------------------------------------------------------------

type hrEntryJSON struct {
	ID            uuid.UUID  `json:"id"`
	Kind          string     `json:"kind"` // absence | makeup
	EmployeeID    uuid.UUID  `json:"employeeId"`
	Type          string     `json:"type"`
	DateFrom      string     `json:"dateFrom"`
	DateTo        string     `json:"dateTo"`
	Part          string     `json:"part"`
	StartTime     *string    `json:"startTime"`
	EndTime       *string    `json:"endTime"`
	Days          float64    `json:"days"`
	Comment       *string    `json:"comment"`
	CreatedAt     time.Time  `json:"createdAt"`
	CreatedBy     *uuid.UUID `json:"createdBy"`
	CreatedByName *string    `json:"createdByName"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	ClosedMonth   bool       `json:"closedMonth"`
	Editable      bool       `json:"editable"`
	LockReason    *string    `json:"lockReason"`
}

func hrLock(isAdmin bool, scope hrEditScope, today time.Time, dates ...time.Time) (bool, *string) {
	code := hrEditWindowError(isAdmin, scope, today, dates...)
	if code == "" {
		return true, nil
	}
	return false, &code
}

func hrListEntriesWith(ctx context.Context, q hrQuerier, v hrViewer, e hrEmployee, from, to time.Time, typeFilter string) ([]hrEntryJSON, error) {
	today := hrToday(e.TimeZone)
	out := []hrEntryJSON{}
	rows, err := q.Query(ctx, `
		select a.id, a.type, a.date_from, a.date_to, a.part, a.comment, a.created_at, a.created_by, p.display_name, a.updated_at
		from public.hr_absences a left join public.profiles p on p.id = a.created_by
		where a.employee_id = $1 and a.deleted_at is null and a.date_to >= $2 and a.date_from <= $3
		  and ($4 = '' or a.type = $4)
		order by a.date_from desc, a.created_at desc
	`, e.ID, from, to, typeFilter)
	if err != nil {
		return nil, err
	}
	minDay, maxDay := to, from
	for rows.Next() {
		var d hrAbsenceDetail
		if err := rows.Scan(&d.ID, &d.Type, &d.DateFrom, &d.DateTo, &d.Part, &d.Comment, &d.CreatedAt, &d.CreatedBy, &d.CreatedByName, &d.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		editable, reason := hrLock(v.isAdmin(), hrScopeAbsences, today, d.DateFrom)
		out = append(out, hrEntryJSON{
			ID: d.ID, Kind: "absence", EmployeeID: e.ID, Type: d.Type, DateFrom: hrFormatDate(d.DateFrom), DateTo: hrFormatDate(d.DateTo),
			Part: d.Part, Comment: d.Comment, CreatedAt: d.CreatedAt, CreatedBy: d.CreatedBy, CreatedByName: d.CreatedByName,
			UpdatedAt: d.UpdatedAt, ClosedMonth: hrIsClosedMonth(d.DateFrom, today), Editable: editable, LockReason: reason,
		})
		if d.DateFrom.Before(minDay) {
			minDay = d.DateFrom
		}
		if d.DateTo.After(maxDay) {
			maxDay = d.DateTo
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if typeFilter == "" || typeFilter == hrTypeMakeupDay {
		shiftRows, err := q.Query(ctx, `
			select s.id, s.work_date, to_char(s.start_time, 'HH24:MI'), to_char(s.end_time, 'HH24:MI'), s.created_at, s.created_by, p.display_name, s.updated_at
			from public.hr_shifts s left join public.profiles p on p.id = s.created_by
			where s.employee_id = $1 and s.kind = 'makeup' and s.work_date between $2 and $3
			order by s.work_date desc
		`, e.ID, from, to)
		if err != nil {
			return nil, err
		}
		for shiftRows.Next() {
			var item hrEntryJSON
			var day time.Time
			var start, end string
			if err := shiftRows.Scan(&item.ID, &day, &start, &end, &item.CreatedAt, &item.CreatedBy, &item.CreatedByName, &item.UpdatedAt); err != nil {
				shiftRows.Close()
				return nil, err
			}
			item.Kind, item.EmployeeID, item.Type, item.Part = "makeup", e.ID, hrTypeMakeupDay, hrPartFull
			item.DateFrom, item.DateTo = hrFormatDate(day), hrFormatDate(day)
			item.StartTime, item.EndTime = &start, &end
			item.ClosedMonth = hrIsClosedMonth(day, today)
			item.Editable, item.LockReason = hrLock(v.isAdmin(), hrScopeHours, today, day)
			out = append(out, item)
			if day.Before(minDay) {
				minDay = day
			}
			if day.After(maxDay) {
				maxDay = day
			}
		}
		shiftRows.Close()
		if err := shiftRows.Err(); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return out, nil
	}
	dayRows, err := hrLoadDays(ctx, q, nil, minDay, maxDay, &e.ID)
	if err != nil {
		return nil, err
	}
	byAbsence := map[uuid.UUID]float64{}
	byShift := map[uuid.UUID]float64{}
	for _, d := range dayRows {
		if d.AbsenceID != nil {
			byAbsence[*d.AbsenceID] += d.AbsenceDays
		}
		if d.ShiftID != nil && d.IsMakeup {
			byShift[*d.ShiftID] += d.MakeupDays
		}
	}
	for i := range out {
		if out[i].Kind == "absence" {
			out[i].Days = byAbsence[out[i].ID]
		} else {
			out[i].Days = byShift[out[i].ID]
		}
	}
	// Newest first across both kinds.
	sortEntries(out)
	return out, nil
}

func sortEntries(items []hrEntryJSON) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].DateFrom > items[j-1].DateFrom; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// hrTargetEmployee resolves ?employeeId= (or a body field) to an employee the viewer may act on:
// themself, or anyone for the super admin.
func (s *Server) hrTargetEmployee(w http.ResponseWriter, r *http.Request, v hrViewer, raw *string) (*hrEmployee, bool) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		if v.self == nil || !v.tracked() {
			s.writeError(w, r, http.StatusForbidden, hrErrNotTracked, "You are not tracked in HR", nil)
			return nil, false
		}
		return v.self, true
	}
	id, err := uuid.Parse(strings.TrimSpace(*raw))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid employee", map[string]string{"employeeId": "validation_error"})
		return nil, false
	}
	if !v.isAdmin() && (v.self == nil || v.self.ID != id) {
		s.writeError(w, r, http.StatusForbidden, hrErrForbidden, "Only the super admin can act for another person", nil)
		return nil, false
	}
	e, err := hrEmployeeByID(r.Context(), s.pool, id, false)
	if err != nil {
		s.hrFail(w, r, err)
		return nil, false
	}
	if e == nil {
		s.writeError(w, r, http.StatusNotFound, "not_found", "Employee not found", nil)
		return nil, false
	}
	return e, true
}

func (s *Server) handleHREntries(w http.ResponseWriter, r *http.Request) {
	v, ok := s.hrRequireViewer(w, r)
	if !ok {
		return
	}
	raw := r.URL.Query().Get("employeeId")
	e, ok := s.hrTargetEmployee(w, r, v, &raw)
	if !ok {
		return
	}
	q := r.URL.Query()
	from, to := e.StartedOn, hrToday(e.TimeZone).AddDate(1, 0, 0)
	if q.Get("from") != "" || q.Get("to") != "" {
		var fields map[string]string
		from, to, fields = hrParseRange(r, 800)
		if len(fields) > 0 {
			s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid range", fields)
			return
		}
	}
	typeFilter := strings.TrimSpace(q.Get("type"))
	if typeFilter != "" && !hrStoredAbsenceTypes[typeFilter] && typeFilter != hrTypeMakeupDay {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid type", map[string]string{"type": "validation_error"})
		return
	}
	items, err := hrListEntriesWith(r.Context(), s.pool, v, *e, from, to, typeFilter)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
