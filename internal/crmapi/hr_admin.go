package crmapi

// HR portal — super admin endpoints (HB4, D16): team, tracking, schedules, vacation limits,
// office settings and holidays.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// hrOptional records whether a JSON field was sent (null clears it).
type hrOptional[T any] struct {
	Set   bool
	Value *T
}

func (o *hrOptional[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		o.Value = nil
		return nil
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

// ---- patterns -------------------------------------------------------------------------------

type hrPatternRequest struct {
	ValidFrom   string   `json:"validFrom"`
	Kind        string   `json:"kind"`
	Weekdays    []int    `json:"weekdays"`
	StartTime   *string  `json:"startTime"`
	EndTime     *string  `json:"endTime"`
	DaysPerWeek *int     `json:"daysPerWeek"`
	HoursPerDay *float64 `json:"hoursPerDay"`
	Note        *string  `json:"note"`
}

type hrPatternInput struct {
	ValidFrom   time.Time
	Kind        string
	Weekdays    []int
	Start       *int
	End         *int
	DaysPerWeek int
	HoursPerDay float64
	Note        *string
}

// hrValidatePattern: fixed = weekdays + start/end inside office hours (hours/day defaults to the
// span); flexible = days/week (1–6) × hours/day (D6).
func hrValidatePattern(req hrPatternRequest, office hrOffice, prefix string) (hrPatternInput, map[string]string) {
	f := map[string]string{}
	in := hrPatternInput{Kind: strings.TrimSpace(req.Kind)}
	from, ok := hrParseDate(req.ValidFrom)
	if !ok {
		f[prefix+"validFrom"] = "validation_error"
	}
	in.ValidFrom = from
	if req.HoursPerDay != nil {
		in.HoursPerDay = *req.HoursPerDay
	}
	switch in.Kind {
	case "fixed":
		seen := map[int]bool{}
		for _, wd := range req.Weekdays {
			if wd < 1 || wd > 7 || seen[wd] {
				f[prefix+"weekdays"] = "validation_error"
				continue
			}
			seen[wd] = true
			in.Weekdays = append(in.Weekdays, wd)
		}
		sort.Ints(in.Weekdays)
		if len(in.Weekdays) == 0 {
			f[prefix+"weekdays"] = "validation_error"
		}
		var start, end int
		var okS, okE bool
		if req.StartTime != nil {
			start, okS = hrParseClock(*req.StartTime)
		}
		if req.EndTime != nil {
			end, okE = hrParseClock(*req.EndTime)
		}
		if !okS {
			f[prefix+"startTime"] = "validation_error"
		}
		if !okE {
			f[prefix+"endTime"] = "validation_error"
		}
		if okS && okE {
			if end <= start {
				f[prefix+"endTime"] = hrErrEndBefore
			} else {
				for _, wd := range in.Weekdays {
					if code := hrShiftWithinOffice(office.Hours[wd], false, start, end); code != "" {
						f[prefix+"startTime"] = code
						break
					}
				}
			}
			in.Start, in.End = &start, &end
			if in.HoursPerDay == 0 {
				in.HoursPerDay = float64(end-start) / 60
			}
		}
		in.DaysPerWeek = len(in.Weekdays)
	case "flexible":
		if req.DaysPerWeek == nil || *req.DaysPerWeek < 1 || *req.DaysPerWeek > 6 {
			f[prefix+"daysPerWeek"] = "validation_error"
		} else {
			in.DaysPerWeek = *req.DaysPerWeek
		}
	default:
		f[prefix+"kind"] = "validation_error"
	}
	if in.HoursPerDay <= 0 || in.HoursPerDay > 12 || math.Mod(in.HoursPerDay*2, 1) != 0 {
		f[prefix+"hoursPerDay"] = "validation_error"
	}
	if req.Note != nil {
		n := strings.TrimSpace(*req.Note)
		if len([]rune(n)) > 500 {
			f[prefix+"note"] = "validation_error"
		}
		if n != "" {
			in.Note = &n
		}
	}
	return in, f
}

// hrInsertPattern closes the previous pattern (valid_to = valid_from − 1) and replaces patterns
// that start on or after the new one, so history never overlaps.
func hrInsertPattern(ctx context.Context, tx pgx.Tx, employeeID uuid.UUID, in hrPatternInput, actorID uuid.UUID) (uuid.UUID, error) {
	if _, err := tx.Exec(ctx, `delete from public.hr_work_patterns where employee_id = $1 and valid_from >= $2`, employeeID, in.ValidFrom); err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `
		update public.hr_work_patterns set valid_to = $2::date - 1
		where employee_id = $1 and (valid_to is null or valid_to >= $2)
	`, employeeID, in.ValidFrom); err != nil {
		return uuid.Nil, err
	}
	var start, end *string
	if in.Start != nil && in.End != nil {
		a, b := hrFormatClock(*in.Start), hrFormatClock(*in.End)
		start, end = &a, &b
	}
	var weekdays []int16
	for _, wd := range in.Weekdays {
		weekdays = append(weekdays, int16(wd))
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		insert into public.hr_work_patterns (employee_id, valid_from, kind, weekdays, start_time, end_time, days_per_week, hours_per_day, note, created_by)
		values ($1, $2, $3, $4, $5::time, $6::time, $7, $8, $9, $10)
		returning id
	`, employeeID, in.ValidFrom, in.Kind, weekdays, start, end, in.DaysPerWeek, in.HoursPerDay, in.Note, actorID).Scan(&id)
	return id, err
}

// hrPatternLabel is the schedule summary used in Slack ("Пн–Пт 10:00–18:00 · 8 год", "5 × 4 год").
func hrPatternLabel(in hrPatternInput) string {
	hours := strconv.FormatFloat(in.HoursPerDay, 'f', -1, 64)
	if in.Kind == "flexible" {
		return fmt.Sprintf("%d × %s год · гнучкий", in.DaysPerWeek, hours)
	}
	names := []string{"", "Пн", "Вт", "Ср", "Чт", "Пт", "Сб", "Нд"}
	days := []string{}
	for _, wd := range in.Weekdays {
		days = append(days, names[wd])
	}
	text := strings.Join(days, ", ")
	if in.Start != nil && in.End != nil {
		text += " " + hrFormatClock(*in.Start) + "–" + hrFormatClock(*in.End)
	}
	return text + " · " + hours + " год"
}

// ---- team -----------------------------------------------------------------------------------

type hrTeamRowJSON struct {
	Employee   hrEmployeeJSON    `json:"employee"`
	Today      *hrCellJSON       `json:"today"`
	Month      hrTotals          `json:"month"`
	Absences   []hrTypeDaysJSON  `json:"absences"`
	Vacation   hrVacationBalance `json:"vacation"`
	MakeupOwed float64           `json:"makeupOwed"`
	NextWeek   string            `json:"nextWeek"` // fixed | planned | missing | none
}

type hrTypeDaysJSON struct {
	Type string  `json:"type"`
	Days float64 `json:"days"`
}

func hrAbsenceBreakdown(t hrTotals) []hrTypeDaysJSON {
	out := []hrTypeDaysJSON{}
	add := func(kind string, days float64) {
		if days > 0 {
			out = append(out, hrTypeDaysJSON{Type: kind, Days: days})
		}
	}
	add(hrTypeVacation, t.Vacation)
	add(hrTypeSick, t.Sick)
	add(hrTypeDayOffUnpaid, t.DayOffUnpaid)
	add(hrTypeDayOffMakeup, t.DayOffMakeup)
	add(hrTypeMakeupDay, t.MakeupDone)
	add(hrTypeRemote, t.Remote)
	add(hrTypeOccasional, t.Occasional)
	add(hrTypeOther, t.Other)
	return out
}

func (s *Server) handleHRTeam(w http.ResponseWriter, r *http.Request) {
	v, ok := s.hrRequireAdmin(w, r)
	if !ok {
		return
	}
	officeID, okOffice := hrQueryUUID(r, "officeId")
	if !okOffice {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid office", map[string]string{"officeId": "validation_error"})
		return
	}
	ctx := r.Context()
	month := hrMonthStart(v.today)
	if raw := strings.TrimSpace(r.URL.Query().Get("month")); raw != "" {
		m, ok := hrParseDate(raw + "-01")
		if !ok {
			s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid month", map[string]string{"month": "validation_error"})
			return
		}
		month = m
	}
	employees, err := hrListEmployees(ctx, s.pool, officeID)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	includeUntracked := r.URL.Query().Get("includeUntracked") == "true"
	monthRows, err := hrLoadDays(ctx, s.pool, officeID, month, hrMonthEnd(month), nil)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	monthTotals := hrAggregate(monthRows)
	nextWeek := hrWeekStart(v.today).AddDate(0, 0, 7)
	soonRows, err := hrLoadDays(ctx, s.pool, officeID, v.today.AddDate(0, 0, -1), nextWeek.AddDate(0, 0, 7), nil)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	rows := []hrTeamRowJSON{}
	for _, e := range employees {
		today := hrToday(e.TimeZone)
		if !e.isTracked(today) && !includeUntracked {
			continue
		}
		pattern, err := hrPatternOn(ctx, s.pool, e.ID, today)
		if err != nil {
			s.hrFail(w, r, err)
			return
		}
		row := hrTeamRowJSON{Employee: hrEmployeeView(e, today, pattern, true), Month: monthTotals[e.ID]}
		row.Absences = hrAbsenceBreakdown(row.Month)
		var todayRow []hrDayRow
		nextPlanned := 0
		for _, d := range soonRows {
			if d.EmployeeID != e.ID {
				continue
			}
			if d.Day.Equal(today) {
				todayRow = append(todayRow, d)
			}
			if !d.Day.Before(nextWeek) && d.Day.Before(nextWeek.AddDate(0, 0, 7)) && d.ShiftID != nil && (d.ShiftKind == nil || *d.ShiftKind == "planned") {
				nextPlanned++
			}
		}
		if len(todayRow) == 1 {
			cells, err := s.hrCells(ctx, v, todayRow)
			if err != nil {
				s.hrFail(w, r, err)
				return
			}
			row.Today = &cells[0]
		}
		switch {
		case pattern == nil:
			row.NextWeek = "none"
		case pattern.Kind == "fixed":
			row.NextWeek = "fixed"
		case nextPlanned > 0:
			row.NextWeek = "planned"
		default:
			row.NextWeek = "missing"
		}
		balances, err := hrLoadBalances(ctx, s.pool, e, month.Year())
		if err != nil {
			s.hrFail(w, r, err)
			return
		}
		row.Vacation, row.MakeupOwed = balances.Vacation, balances.MakeupOwed
		rows = append(rows, row)
	}
	untracked, err := s.hrCountUntracked(ctx)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"month": month.Format("2006-01"), "today": hrFormatDate(v.today), "items": rows, "untrackedAccounts": untracked})
}

func (s *Server) hrCountUntracked(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		select count(*)::int from public.profiles p
		where p.is_active and not exists (select 1 from public.hr_employees e where e.profile_id = p.id and e.tracked_until is null)
	`).Scan(&n)
	return n, err
}

// GET /v1/hr/accounts — CRM accounts for "Add to HR tracking" (tracked ones are flagged).
func (s *Server) handleHRAccounts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.hrRequireAdmin(w, r); !ok {
		return
	}
	rows, err := s.pool.Query(r.Context(), `
		select p.id, p.display_name, nullif(u.email, ''), p.role::text, e.id, e.tracked_until,
		       coalesce((select array_agg(o.code order by o.code) from public.user_office_memberships m
		                 join public.offices o on o.id = m.office_id where m.user_id = p.id), '{}')
		from public.profiles p
		left join auth.users u on u.id = p.id
		left join public.hr_employees e on e.profile_id = p.id
		where p.is_active
		order by p.display_name nulls last, p.id
	`)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	defer rows.Close()
	type account struct {
		ProfileID   uuid.UUID  `json:"profileId"`
		DisplayName *string    `json:"displayName"`
		Email       *string    `json:"email"`
		Role        string     `json:"role"`
		OfficeCodes []string   `json:"officeCodes"`
		EmployeeID  *uuid.UUID `json:"employeeId"`
		IsTracked   bool       `json:"isTracked"`
	}
	items := []account{}
	for rows.Next() {
		var a account
		var until *time.Time
		if err := rows.Scan(&a.ProfileID, &a.DisplayName, &a.Email, &a.Role, &a.EmployeeID, &until, &a.OfficeCodes); err != nil {
			s.hrFail(w, r, err)
			return
		}
		a.IsTracked = a.EmployeeID != nil && until == nil
		items = append(items, a)
	}
	if err := rows.Err(); err != nil {
		s.hrFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// ---- employees ------------------------------------------------------------------------------

type hrCreateEmployeeRequest struct {
	ProfileID    string           `json:"profileId"`
	OfficeID     string           `json:"officeId"`
	Position     *string          `json:"position"`
	StartedOn    string           `json:"startedOn"`
	Pattern      hrPatternRequest `json:"pattern"`
	VacationDays *float64         `json:"vacationDays"`
}

func hrValidPosition(p *string) (*string, bool) {
	if p == nil {
		return nil, true
	}
	v := strings.TrimSpace(*p)
	if v == "" {
		return nil, true
	}
	return &v, len([]rune(v)) <= 120
}

func hrValidVacationDays(d *float64) bool {
	return d == nil || (*d >= 0 && *d <= 366 && math.Mod(*d*2, 1) == 0)
}

func (s *Server) handleHRCreateEmployee(w http.ResponseWriter, r *http.Request) {
	v, ok := s.hrRequireAdmin(w, r)
	if !ok {
		return
	}
	var req hrCreateEmployeeRequest
	if err := decodeJSON(w, r, 16*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid employee", nil)
		return
	}
	fields := map[string]string{}
	profileID, err := uuid.Parse(strings.TrimSpace(req.ProfileID))
	if err != nil {
		fields["profileId"] = "validation_error"
	}
	officeID, err := uuid.Parse(strings.TrimSpace(req.OfficeID))
	if err != nil {
		fields["officeId"] = "validation_error"
	}
	startedOn, okStart := hrParseDate(req.StartedOn)
	if !okStart {
		fields["startedOn"] = "validation_error"
	}
	position, okPos := hrValidPosition(req.Position)
	if !okPos {
		fields["position"] = "validation_error"
	}
	if !hrValidVacationDays(req.VacationDays) {
		fields["vacationDays"] = "validation_error"
	}
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid employee", fields)
		return
	}
	ctx := r.Context()
	office, err := hrLoadOffice(ctx, s.pool, officeID)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	if office == nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Unknown office", map[string]string{"officeId": "validation_error"})
		return
	}
	if req.Pattern.ValidFrom == "" {
		req.Pattern.ValidFrom = req.StartedOn
	}
	pattern, pf := hrValidatePattern(req.Pattern, *office, "pattern.")
	if len(pf) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid schedule", pf)
		return
	}
	vacation := office.DefaultVacationDays
	if req.VacationDays != nil {
		vacation = *req.VacationDays
	}
	const operation = "hr.employee.create"
	body, _ := json.Marshal(req)
	tx, key, ok := s.hrBeginTx(w, r, v.actor, operation, string(body))
	if !ok {
		return
	}
	defer tx.Rollback(ctx)
	var profileName *string
	var active bool
	if err := tx.QueryRow(ctx, `select display_name, is_active from public.profiles where id = $1`, profileID).Scan(&profileName, &active); err != nil || !active {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Unknown or inactive account", map[string]string{"profileId": "validation_error"})
		return
	}
	var employeeID uuid.UUID
	var trackedUntil *time.Time
	err = tx.QueryRow(ctx, `select id, tracked_until from public.hr_employees where profile_id = $1 for update`, profileID).Scan(&employeeID, &trackedUntil)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if err := tx.QueryRow(ctx, `
			insert into public.hr_employees (profile_id, office_id, position, started_on, created_by, updated_by)
			values ($1, $2, $3, $4, $5, $5) returning id
		`, profileID, officeID, position, startedOn, v.actor.ID).Scan(&employeeID); err != nil {
			s.hrFail(w, r, err)
			return
		}
	case err != nil:
		s.hrFail(w, r, err)
		return
	case trackedUntil == nil:
		s.writeError(w, r, http.StatusConflict, "hr_already_tracked", "This account is already tracked", map[string]string{"profileId": "hr_already_tracked"})
		return
	default:
		// Tracking again: keep the history row, move office / position, clear the stop date.
		if _, err := tx.Exec(ctx, `
			update public.hr_employees
			set office_id = $2, position = $3, tracked_until = null, ended_on = null,
			    started_on = least(started_on, $4), updated_by = $5, updated_at = now()
			where id = $1
		`, employeeID, officeID, position, startedOn, v.actor.ID); err != nil {
			s.hrFail(w, r, err)
			return
		}
	}
	if _, err := hrInsertPattern(ctx, tx, employeeID, pattern, v.actor.ID); err != nil {
		s.hrFail(w, r, err)
		return
	}
	if _, err := tx.Exec(ctx, `
		insert into public.hr_leave_limits (employee_id, year, days, updated_by) values ($1, $2, $3, $4)
		on conflict (employee_id, year) do update set days = excluded.days, updated_by = excluded.updated_by, updated_at = now()
	`, employeeID, startedOn.Year(), vacation, v.actor.ID); err != nil {
		s.hrFail(w, r, err)
		return
	}
	queued, err := s.hrNotify(ctx, tx, hrSlackMessage{Icon: "👤", Action: "Додано до HR-обліку", Person: profileName,
		Detail: fmt.Sprintf("з %s · %s · відпустка %s дн.", startedOn.Format("02.01.2006"), hrPatternLabel(pattern), strconv.FormatFloat(vacation, 'f', -1, 64)),
		Actor:  s.hrActorName(ctx, tx, v.actor.ID)})
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	detail, err := s.hrEmployeeDetail(ctx, tx, employeeID)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	out, err := s.hrFinishTx(r, tx, v.actor, operation, key, http.StatusCreated, detail)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	s.hrWake(queued)
	writeJSON(w, http.StatusCreated, out)
}

type hrLeaveYearJSON struct {
	Year    int               `json:"year"`
	Days    float64           `json:"days"`
	IsSet   bool              `json:"isSet"`
	Balance hrVacationBalance `json:"balance"`
}

type hrEmployeeDetailJSON struct {
	Employee   hrEmployeeJSON    `json:"employee"`
	Office     hrOfficeJSON      `json:"office"`
	Today      string            `json:"today"`
	Patterns   []hrPatternJSON   `json:"patterns"`
	Leave      []hrLeaveYearJSON `json:"leave"`
	MakeupOwed float64           `json:"makeupOwed"`
}

func (s *Server) hrEmployeeDetail(ctx context.Context, q hrQuerier, id uuid.UUID) (*hrEmployeeDetailJSON, error) {
	e, err := hrEmployeeByID(ctx, q, id, false)
	if err != nil || e == nil {
		return nil, err
	}
	today := hrToday(e.TimeZone)
	pattern, err := hrPatternOn(ctx, q, e.ID, today)
	if err != nil {
		return nil, err
	}
	office, err := hrLoadOffice(ctx, q, e.OfficeID)
	if err != nil || office == nil {
		return nil, fmt.Errorf("load office: %w", err)
	}
	patterns, err := hrPatternHistory(ctx, q, e.ID)
	if err != nil {
		return nil, err
	}
	out := &hrEmployeeDetailJSON{Employee: hrEmployeeView(*e, today, pattern, true), Office: office.json(), Today: hrFormatDate(today), Patterns: patterns}
	set := map[int]float64{}
	rows, err := q.Query(ctx, `select year, days::float8 from public.hr_leave_limits where employee_id = $1`, e.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var y int
		var d float64
		if err := rows.Scan(&y, &d); err != nil {
			rows.Close()
			return nil, err
		}
		set[y] = d
	}
	rows.Close()
	years := map[int]bool{today.Year(): true}
	for y := range set {
		years[y] = true
	}
	ordered := []int{}
	for y := range years {
		ordered = append(ordered, y)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ordered)))
	for _, y := range ordered {
		b, err := hrLoadBalances(ctx, q, *e, y)
		if err != nil {
			return nil, err
		}
		_, isSet := set[y]
		out.Leave = append(out.Leave, hrLeaveYearJSON{Year: y, Days: b.Vacation.Limit, IsSet: isSet, Balance: b.Vacation})
		out.MakeupOwed = b.MakeupOwed
	}
	return out, nil
}

func (s *Server) hrPathEmployee(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("employeeId"))
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "not_found", "Employee not found", nil)
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) handleHRGetEmployee(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.hrRequireAdmin(w, r); !ok {
		return
	}
	id, ok := s.hrPathEmployee(w, r)
	if !ok {
		return
	}
	detail, err := s.hrEmployeeDetail(r.Context(), s.pool, id)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	if detail == nil {
		s.writeError(w, r, http.StatusNotFound, "not_found", "Employee not found", nil)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

type hrUpdateEmployeeRequest struct {
	OfficeID  hrOptional[string] `json:"officeId"`
	Position  hrOptional[string] `json:"position"`
	StartedOn hrOptional[string] `json:"startedOn"`
	EndedOn   hrOptional[string] `json:"endedOn"`
	Tracked   hrOptional[bool]   `json:"tracked"`
}

func (s *Server) handleHRUpdateEmployee(w http.ResponseWriter, r *http.Request) {
	v, ok := s.hrRequireAdmin(w, r)
	if !ok {
		return
	}
	id, ok := s.hrPathEmployee(w, r)
	if !ok {
		return
	}
	var req hrUpdateEmployeeRequest
	if err := decodeJSON(w, r, 8*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid employee", nil)
		return
	}
	ctx := r.Context()
	const operation = "hr.employee.update"
	body, _ := json.Marshal(map[string]any{"o": req.OfficeID.Value, "p": req.Position.Value, "s": req.StartedOn.Value, "e": req.EndedOn.Value, "t": req.Tracked.Value,
		"set": []bool{req.OfficeID.Set, req.Position.Set, req.StartedOn.Set, req.EndedOn.Set, req.Tracked.Set}})
	tx, key, ok := s.hrBeginTx(w, r, v.actor, operation, id.String(), string(body))
	if !ok {
		return
	}
	defer tx.Rollback(ctx)
	e, err := hrEmployeeByID(ctx, tx, id, true)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	if e == nil {
		s.writeError(w, r, http.StatusNotFound, "not_found", "Employee not found", nil)
		return
	}
	fields := map[string]string{}
	officeID, position, startedOn, endedOn, trackedUntil := e.OfficeID, e.Position, e.StartedOn, e.EndedOn, e.TrackedUntil
	changes := []string{}
	if req.OfficeID.Set {
		if req.OfficeID.Value == nil {
			fields["officeId"] = "validation_error"
		} else if oid, err := uuid.Parse(*req.OfficeID.Value); err != nil {
			fields["officeId"] = "validation_error"
		} else if oid != officeID {
			office, err := hrLoadOffice(ctx, tx, oid)
			if err != nil || office == nil {
				fields["officeId"] = "validation_error"
			} else {
				officeID = oid
				changes = append(changes, "офіс: "+office.NameUK)
			}
		}
	}
	if req.Position.Set {
		p, okPos := hrValidPosition(req.Position.Value)
		if !okPos {
			fields["position"] = "validation_error"
		}
		if hrStr(p) != hrStr(position) {
			changes = append(changes, "посада: "+hrStr(p))
		}
		position = p
	}
	if req.StartedOn.Set {
		d, okD := time.Time{}, false
		if req.StartedOn.Value != nil {
			d, okD = hrParseDate(*req.StartedOn.Value)
		}
		if !okD {
			fields["startedOn"] = "validation_error"
		} else if !d.Equal(startedOn) {
			startedOn = d
			changes = append(changes, "початок: "+d.Format("02.01.2006"))
		}
	}
	if req.EndedOn.Set {
		if req.EndedOn.Value == nil {
			if endedOn != nil {
				changes = append(changes, "дату завершення знято")
			}
			endedOn = nil
		} else if d, okD := hrParseDate(*req.EndedOn.Value); !okD {
			fields["endedOn"] = "validation_error"
		} else {
			endedOn = &d
			changes = append(changes, "завершення: "+d.Format("02.01.2006"))
		}
	}
	if endedOn != nil && endedOn.Before(startedOn) {
		fields["endedOn"] = "validation_error"
	}
	today := hrToday(e.TimeZone)
	action := "Змінено налаштування HR"
	if req.Tracked.Set && req.Tracked.Value != nil {
		wasTracked := e.isTracked(today)
		if !*req.Tracked.Value && wasTracked {
			// Stop tracking: the person disappears from calendars and reports from tomorrow.
			t := today
			trackedUntil = &t
			action = "Зупинено HR-облік (з завтра)"
		} else if *req.Tracked.Value && !wasTracked {
			trackedUntil = nil
			action = "Відновлено HR-облік"
		}
	}
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid employee", fields)
		return
	}
	if _, err := tx.Exec(ctx, `
		update public.hr_employees
		set office_id = $2, position = $3, started_on = $4, ended_on = $5, tracked_until = $6, updated_by = $7, updated_at = now()
		where id = $1
	`, id, officeID, position, startedOn, endedOn, trackedUntil, v.actor.ID); err != nil {
		s.hrFail(w, r, err)
		return
	}
	queued := false
	if len(changes) > 0 || action != "Змінено налаштування HR" {
		queued, err = s.hrNotify(ctx, tx, hrSlackMessage{Icon: "👤", Action: action, Person: e.DisplayName,
			Detail: strings.Join(changes, "; "), Actor: s.hrActorName(ctx, tx, v.actor.ID)})
		if err != nil {
			s.hrFail(w, r, err)
			return
		}
	}
	detail, err := s.hrEmployeeDetail(ctx, tx, id)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	out, err := s.hrFinishTx(r, tx, v.actor, operation, key, http.StatusOK, detail)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	s.hrWake(queued)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleHRListPatterns(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.hrRequireAdmin(w, r); !ok {
		return
	}
	id, ok := s.hrPathEmployee(w, r)
	if !ok {
		return
	}
	items, err := hrPatternHistory(r.Context(), s.pool, id)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleHRCreatePattern(w http.ResponseWriter, r *http.Request) {
	v, ok := s.hrRequireAdmin(w, r)
	if !ok {
		return
	}
	id, ok := s.hrPathEmployee(w, r)
	if !ok {
		return
	}
	var req hrPatternRequest
	if err := decodeJSON(w, r, 8*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid schedule", nil)
		return
	}
	ctx := r.Context()
	e, err := hrEmployeeByID(ctx, s.pool, id, false)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	if e == nil {
		s.writeError(w, r, http.StatusNotFound, "not_found", "Employee not found", nil)
		return
	}
	office, err := hrLoadOffice(ctx, s.pool, e.OfficeID)
	if err != nil || office == nil {
		s.hrFail(w, r, err)
		return
	}
	in, fields := hrValidatePattern(req, *office, "")
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid schedule", fields)
		return
	}
	const operation = "hr.pattern.create"
	body, _ := json.Marshal(req)
	tx, key, ok := s.hrBeginTx(w, r, v.actor, operation, id.String(), string(body))
	if !ok {
		return
	}
	defer tx.Rollback(ctx)
	if _, err := hrEmployeeByID(ctx, tx, id, true); err != nil {
		s.hrFail(w, r, err)
		return
	}
	if _, err := hrInsertPattern(ctx, tx, id, in, v.actor.ID); err != nil {
		s.hrFail(w, r, err)
		return
	}
	queued, err := s.hrNotify(ctx, tx, hrSlackMessage{Icon: "🗓️", Action: "Новий графік", Person: e.DisplayName,
		Detail: "з " + in.ValidFrom.Format("02.01.2006") + ": " + hrPatternLabel(in), Actor: s.hrActorName(ctx, tx, v.actor.ID)})
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	items, err := hrPatternHistory(ctx, tx, id)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	out, err := s.hrFinishTx(r, tx, v.actor, operation, key, http.StatusCreated, map[string]any{"items": items})
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	s.hrWake(queued)
	writeJSON(w, http.StatusCreated, out)
}

type hrLeaveLimitRequest struct {
	Days float64 `json:"days"`
}

func (s *Server) handleHRPutLeaveLimit(w http.ResponseWriter, r *http.Request) {
	v, ok := s.hrRequireAdmin(w, r)
	if !ok {
		return
	}
	id, ok := s.hrPathEmployee(w, r)
	if !ok {
		return
	}
	year, err := strconv.Atoi(r.PathValue("year"))
	var req hrLeaveLimitRequest
	if decodeErr := decodeJSON(w, r, 1024, &req); decodeErr != nil || err != nil || year < 2000 || year > 2100 || !hrValidVacationDays(&req.Days) {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid vacation limit", map[string]string{"days": "validation_error"})
		return
	}
	ctx := r.Context()
	const operation = "hr.leave_limit.put"
	tx, key, ok := s.hrBeginTx(w, r, v.actor, operation, id.String(), strconv.Itoa(year), strconv.FormatFloat(req.Days, 'f', -1, 64))
	if !ok {
		return
	}
	defer tx.Rollback(ctx)
	e, err := hrEmployeeByID(ctx, tx, id, true)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	if e == nil {
		s.writeError(w, r, http.StatusNotFound, "not_found", "Employee not found", nil)
		return
	}
	before, err := hrLeaveLimit(ctx, tx, *e, year)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	if _, err := tx.Exec(ctx, `
		insert into public.hr_leave_limits (employee_id, year, days, updated_by) values ($1, $2, $3, $4)
		on conflict (employee_id, year) do update set days = excluded.days, updated_by = excluded.updated_by, updated_at = now()
	`, id, year, req.Days, v.actor.ID); err != nil {
		s.hrFail(w, r, err)
		return
	}
	queued, err := s.hrNotify(ctx, tx, hrSlackMessage{Icon: "✏️", Action: "Ліміт відпустки", Person: e.DisplayName,
		Detail: fmt.Sprintf("%d: %s дн.", year, strconv.FormatFloat(req.Days, 'f', -1, 64)),
		Before: strconv.FormatFloat(before, 'f', -1, 64) + " дн.", Actor: s.hrActorName(ctx, tx, v.actor.ID)})
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	detail, err := s.hrEmployeeDetail(ctx, tx, id)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	out, err := s.hrFinishTx(r, tx, v.actor, operation, key, http.StatusOK, detail)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	s.hrWake(queued)
	writeJSON(w, http.StatusOK, out)
}

// ---- settings and holidays ------------------------------------------------------------------

func (s *Server) hrPathOffice(w http.ResponseWriter, r *http.Request) (*hrOffice, bool) {
	id, err := uuid.Parse(r.PathValue("officeId"))
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "not_found", "Office not found", nil)
		return nil, false
	}
	office, err := hrLoadOffice(r.Context(), s.pool, id)
	if err != nil {
		s.hrFail(w, r, err)
		return nil, false
	}
	if office == nil {
		s.writeError(w, r, http.StatusNotFound, "not_found", "Office not found", nil)
		return nil, false
	}
	return office, true
}

func (s *Server) handleHRGetSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.hrRequireAdmin(w, r); !ok {
		return
	}
	office, ok := s.hrPathOffice(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, office.json())
}

type hrSettingsRequest struct {
	DefaultVacationDays float64             `json:"defaultVacationDays"`
	Hours               []hrOfficeHoursJSON `json:"hours"`
}

func (s *Server) handleHRPutSettings(w http.ResponseWriter, r *http.Request) {
	v, ok := s.hrRequireAdmin(w, r)
	if !ok {
		return
	}
	office, ok := s.hrPathOffice(w, r)
	if !ok {
		return
	}
	var req hrSettingsRequest
	if err := decodeJSON(w, r, 8*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid settings", nil)
		return
	}
	fields := map[string]string{}
	if !hrValidVacationDays(&req.DefaultVacationDays) {
		fields["defaultVacationDays"] = "validation_error"
	}
	hours := map[int]hrOfficeDay{}
	for _, h := range req.Hours {
		key := fmt.Sprintf("hours.%d", h.Weekday)
		if h.Weekday < 1 || h.Weekday > 7 {
			fields[key] = "validation_error"
			continue
		}
		if h.Opens == nil && h.Closes == nil {
			hours[h.Weekday] = hrOfficeDay{}
			continue
		}
		if h.Opens == nil || h.Closes == nil {
			fields[key] = "validation_error"
			continue
		}
		a, okA := hrParseClock(*h.Opens)
		b, okB := hrParseClock(*h.Closes)
		if !okA || !okB || a%hrPlanStepMinutes != 0 || b%hrPlanStepMinutes != 0 {
			fields[key] = "validation_error"
			continue
		}
		if b <= a {
			fields[key] = hrErrEndBefore
			continue
		}
		hours[h.Weekday] = hrOfficeDay{Open: true, Opens: a, Closes: b}
	}
	if len(hours) != 7 {
		fields["hours"] = "validation_error"
	}
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid settings", fields)
		return
	}
	ctx := r.Context()
	const operation = "hr.settings.put"
	body, _ := json.Marshal(req)
	tx, key, ok := s.hrBeginTx(w, r, v.actor, operation, office.ID.String(), string(body))
	if !ok {
		return
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		insert into public.hr_office_settings (office_id, default_vacation_days, updated_by) values ($1, $2, $3)
		on conflict (office_id) do update set default_vacation_days = excluded.default_vacation_days, updated_by = excluded.updated_by, updated_at = now()
	`, office.ID, req.DefaultVacationDays, v.actor.ID); err != nil {
		s.hrFail(w, r, err)
		return
	}
	for wd := 1; wd <= 7; wd++ {
		d := hours[wd]
		var opens, closes *string
		if d.Open {
			a, b := hrFormatClock(d.Opens), hrFormatClock(d.Closes)
			opens, closes = &a, &b
		}
		if _, err := tx.Exec(ctx, `
			insert into public.hr_office_hours (office_id, weekday, opens, closes) values ($1, $2, $3::time, $4::time)
			on conflict (office_id, weekday) do update set opens = excluded.opens, closes = excluded.closes
		`, office.ID, wd, opens, closes); err != nil {
			s.hrFail(w, r, err)
			return
		}
	}
	queued, err := s.hrNotify(ctx, tx, hrSlackMessage{Icon: "⚙️", Action: "Змінено налаштування офісу " + office.NameUK,
		Detail: "години роботи та відпустка за замовчуванням " + strconv.FormatFloat(req.DefaultVacationDays, 'f', -1, 64) + " дн.",
		Actor:  s.hrActorName(ctx, tx, v.actor.ID)})
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	updated, err := hrLoadOffice(ctx, tx, office.ID)
	if err != nil || updated == nil {
		s.hrFail(w, r, err)
		return
	}
	out, err := s.hrFinishTx(r, tx, v.actor, operation, key, http.StatusOK, updated.json())
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	s.hrWake(queued)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleHRListHolidays(w http.ResponseWriter, r *http.Request) {
	v, ok := s.hrRequireViewer(w, r)
	if !ok {
		return
	}
	officeID, okOffice := hrQueryUUID(r, "officeId")
	year, err := strconv.Atoi(r.URL.Query().Get("year"))
	if !okOffice || officeID == nil || err != nil || year < 2000 || year > 2100 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid request", map[string]string{"officeId": "validation_error", "year": "validation_error"})
		return
	}
	if !v.isAdmin() && v.self.OfficeID != *officeID && !v.actor.CanAccessOffice(*officeID) {
		s.writeError(w, r, http.StatusForbidden, hrErrForbidden, "No access to this office", nil)
		return
	}
	items, err := hrLoadHolidays(r.Context(), s.pool, *officeID, time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(year, 12, 31, 0, 0, 0, 0, time.UTC))
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type hrHolidayRequest struct {
	OfficeID string `json:"officeId"`
	Day      string `json:"day"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
}

func hrValidateHoliday(req hrHolidayRequest) (uuid.UUID, time.Time, string, string, map[string]string) {
	f := map[string]string{}
	officeID, err := uuid.Parse(strings.TrimSpace(req.OfficeID))
	if err != nil {
		f["officeId"] = "validation_error"
	}
	day, ok := hrParseDate(req.Day)
	if !ok {
		f["day"] = "validation_error"
	}
	kind := strings.TrimSpace(req.Kind)
	if kind != "public_holiday" && kind != "office_closed" {
		f["kind"] = "validation_error"
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len([]rune(name)) > 120 {
		f["name"] = "validation_error"
	}
	return officeID, day, kind, name, f
}

var hrHolidayKindLabels = map[string]string{"public_holiday": "державне свято", "office_closed": "офіс зачинено"}

func (s *Server) handleHRCreateHoliday(w http.ResponseWriter, r *http.Request) {
	s.hrSaveHoliday(w, r, nil)
}

func (s *Server) handleHRUpdateHoliday(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("holidayId"))
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "not_found", "Holiday not found", nil)
		return
	}
	s.hrSaveHoliday(w, r, &id)
}

func (s *Server) hrSaveHoliday(w http.ResponseWriter, r *http.Request, id *uuid.UUID) {
	v, ok := s.hrRequireAdmin(w, r)
	if !ok {
		return
	}
	var req hrHolidayRequest
	if err := decodeJSON(w, r, 4*1024, &req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid holiday", nil)
		return
	}
	officeID, day, kind, name, fields := hrValidateHoliday(req)
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid holiday", fields)
		return
	}
	ctx := r.Context()
	operation := "hr.holiday.create"
	idPart := ""
	if id != nil {
		operation, idPart = "hr.holiday.update", id.String()
	}
	tx, key, ok := s.hrBeginTx(w, r, v.actor, operation, idPart, officeID.String(), hrFormatDate(day), kind, name)
	if !ok {
		return
	}
	defer tx.Rollback(ctx)
	office, err := hrLoadOffice(ctx, tx, officeID)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	if office == nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Unknown office", map[string]string{"officeId": "validation_error"})
		return
	}
	var h hrHoliday
	var saved time.Time
	if id == nil {
		err = tx.QueryRow(ctx, `
			insert into public.hr_calendar_days (office_id, day, kind, name, created_by) values ($1, $2, $3, $4, $5)
			on conflict (office_id, day) do nothing
			returning id, office_id, day, kind, name
		`, officeID, day, kind, name, v.actor.ID).Scan(&h.ID, &h.OfficeID, &saved, &h.Kind, &h.Name)
	} else {
		err = tx.QueryRow(ctx, `
			update public.hr_calendar_days set office_id = $2, day = $3, kind = $4, name = $5 where id = $1
			returning id, office_id, day, kind, name
		`, *id, officeID, day, kind, name).Scan(&h.ID, &h.OfficeID, &saved, &h.Kind, &h.Name)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if id == nil {
			s.writeError(w, r, http.StatusConflict, "hr_holiday_exists", "This day is already in the list", map[string]string{"day": "hr_holiday_exists"})
		} else {
			s.writeError(w, r, http.StatusNotFound, "not_found", "Holiday not found", nil)
		}
		return
	}
	if err != nil {
		if isUniqueViolation(err) {
			s.writeError(w, r, http.StatusConflict, "hr_holiday_exists", "This day is already in the list", map[string]string{"day": "hr_holiday_exists"})
			return
		}
		s.hrFail(w, r, err)
		return
	}
	h.Day = hrFormatDate(saved)
	action := "Нове свято / вихідний офісу"
	if id != nil {
		action = "Змінено свято / вихідний офісу"
	}
	queued, err := s.hrNotify(ctx, tx, hrSlackMessage{Icon: "📅", Action: action + " " + office.NameUK,
		Detail: day.Format("02.01.2006") + " · " + name + " (" + hrHolidayKindLabels[kind] + ")", Actor: s.hrActorName(ctx, tx, v.actor.ID)})
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	status := http.StatusCreated
	if id != nil {
		status = http.StatusOK
	}
	out, err := s.hrFinishTx(r, tx, v.actor, operation, key, status, h)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	s.hrWake(queued)
	writeJSON(w, status, out)
}

func (s *Server) handleHRDeleteHoliday(w http.ResponseWriter, r *http.Request) {
	v, ok := s.hrRequireAdmin(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("holidayId"))
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "not_found", "Holiday not found", nil)
		return
	}
	ctx := r.Context()
	const operation = "hr.holiday.delete"
	tx, key, ok := s.hrBeginTx(w, r, v.actor, operation, id.String())
	if !ok {
		return
	}
	defer tx.Rollback(ctx)
	var day time.Time
	var name, officeName string
	err = tx.QueryRow(ctx, `
		delete from public.hr_calendar_days d using public.offices o
		where d.id = $1 and o.id = d.office_id
		returning d.day, d.name, o.name_uk
	`, id).Scan(&day, &name, &officeName)
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, http.StatusNotFound, "not_found", "Holiday not found", nil)
		return
	}
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	queued, err := s.hrNotify(ctx, tx, hrSlackMessage{Icon: "🗑️", Action: "Видалено свято / вихідний офісу " + officeName,
		Detail: day.Format("02.01.2006") + " · " + name, Actor: s.hrActorName(ctx, tx, v.actor.ID)})
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	out, err := s.hrFinishTx(r, tx, v.actor, operation, key, http.StatusOK, map[string]any{"id": id})
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	s.hrWake(queued)
	writeJSON(w, http.StatusOK, out)
}
