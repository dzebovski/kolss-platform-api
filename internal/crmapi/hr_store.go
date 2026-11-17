package crmapi

// HR portal — data access shared by the HR handlers (HB4).

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type hrQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type hrEmployee struct {
	ID           uuid.UUID
	ProfileID    uuid.UUID
	DisplayName  *string
	Email        *string
	OfficeID     uuid.UUID
	OfficeCode   string
	TimeZone     string
	Position     *string
	StartedOn    time.Time
	EndedOn      *time.Time
	TrackedUntil *time.Time
}

// lastDay is the last tracked day (nil = open-ended).
func (e hrEmployee) lastDay() *time.Time {
	var last *time.Time
	for _, d := range []*time.Time{e.EndedOn, e.TrackedUntil} {
		if d != nil && (last == nil || d.Before(*last)) {
			v := *d
			last = &v
		}
	}
	return last
}

// isTrackedOn: the person appears in calendars on that date.
func (e hrEmployee) isTrackedOn(day time.Time) bool {
	if day.Before(e.StartedOn) {
		return false
	}
	last := e.lastDay()
	return last == nil || !day.After(*last)
}

// isTracked: tracking was not stopped (tracked from today, or starts later).
func (e hrEmployee) isTracked(today time.Time) bool {
	last := e.lastDay()
	return last == nil || !last.Before(today)
}

const hrEmployeeSelect = `
	select e.id, e.profile_id, p.display_name, nullif(u.email, ''), e.office_id, o.code, o.timezone_name,
	       e.position, e.started_on, e.ended_on, e.tracked_until
	from public.hr_employees e
	join public.profiles p on p.id = e.profile_id
	join public.offices o on o.id = e.office_id
	left join auth.users u on u.id = e.profile_id
`

func scanHREmployee(row pgx.Row) (hrEmployee, error) {
	var e hrEmployee
	err := row.Scan(&e.ID, &e.ProfileID, &e.DisplayName, &e.Email, &e.OfficeID, &e.OfficeCode, &e.TimeZone,
		&e.Position, &e.StartedOn, &e.EndedOn, &e.TrackedUntil)
	return e, err
}

func hrEmployeeByProfile(ctx context.Context, q hrQuerier, profileID uuid.UUID) (*hrEmployee, error) {
	e, err := scanHREmployee(q.QueryRow(ctx, hrEmployeeSelect+` where e.profile_id = $1`, profileID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func hrEmployeeByID(ctx context.Context, q hrQuerier, id uuid.UUID, lock bool) (*hrEmployee, error) {
	if lock {
		if _, err := q.Exec(ctx, `select 1 from public.hr_employees where id = $1 for update`, id); err != nil {
			return nil, err
		}
	}
	e, err := scanHREmployee(q.QueryRow(ctx, hrEmployeeSelect+` where e.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func hrListEmployees(ctx context.Context, q hrQuerier, officeID *uuid.UUID) ([]hrEmployee, error) {
	rows, err := q.Query(ctx, hrEmployeeSelect+` where ($1::uuid is null or e.office_id = $1) order by p.display_name nulls last, e.id`, officeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []hrEmployee{}
	for rows.Next() {
		e, err := scanHREmployee(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- office ---------------------------------------------------------------------------------

type hrOffice struct {
	ID                  uuid.UUID
	Code                string
	NameUK              string
	NamePL              string
	TimeZone            string
	DefaultVacationDays float64
	Hours               map[int]hrOfficeDay
}

func hrLoadOffice(ctx context.Context, q hrQuerier, officeID uuid.UUID) (*hrOffice, error) {
	o := hrOffice{ID: officeID, Hours: map[int]hrOfficeDay{}, DefaultVacationDays: 20}
	err := q.QueryRow(ctx, `
		select o.code, o.name_uk, o.name_pl, o.timezone_name, coalesce(s.default_vacation_days, 20)::float8
		from public.offices o left join public.hr_office_settings s on s.office_id = o.id
		where o.id = $1
	`, officeID).Scan(&o.Code, &o.NameUK, &o.NamePL, &o.TimeZone, &o.DefaultVacationDays)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `
		select weekday, to_char(opens, 'HH24:MI'), to_char(closes, 'HH24:MI')
		from public.hr_office_hours where office_id = $1
	`, officeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var wd int
		var opens, closes *string
		if err := rows.Scan(&wd, &opens, &closes); err != nil {
			return nil, err
		}
		day := hrOfficeDay{}
		if opens != nil && closes != nil {
			a, okA := hrParseClock(*opens)
			b, okB := hrParseClock(*closes)
			day = hrOfficeDay{Open: okA && okB, Opens: a, Closes: b}
		}
		o.Hours[wd] = day
	}
	return &o, rows.Err()
}

type hrOfficeHoursJSON struct {
	Weekday int     `json:"weekday"`
	Opens   *string `json:"opens"`
	Closes  *string `json:"closes"`
}

func (o hrOffice) hoursJSON() []hrOfficeHoursJSON {
	out := make([]hrOfficeHoursJSON, 0, 7)
	for wd := 1; wd <= 7; wd++ {
		d := o.Hours[wd]
		item := hrOfficeHoursJSON{Weekday: wd}
		if d.Open {
			a, b := hrFormatClock(d.Opens), hrFormatClock(d.Closes)
			item.Opens, item.Closes = &a, &b
		}
		out = append(out, item)
	}
	return out
}

func hrLoadHolidays(ctx context.Context, q hrQuerier, officeID uuid.UUID, from, to time.Time) ([]hrHoliday, error) {
	rows, err := q.Query(ctx, `
		select id, office_id, day, kind, name from public.hr_calendar_days
		where office_id = $1 and day between $2 and $3 order by day
	`, officeID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []hrHoliday{}
	for rows.Next() {
		var h hrHoliday
		var day time.Time
		if err := rows.Scan(&h.ID, &h.OfficeID, &day, &h.Kind, &h.Name); err != nil {
			return nil, err
		}
		h.Day = hrFormatDate(day)
		out = append(out, h)
	}
	return out, rows.Err()
}

// ---- day rows -------------------------------------------------------------------------------

func hrLoadDays(ctx context.Context, q hrQuerier, officeID *uuid.UUID, from, to time.Time, employeeID *uuid.UUID) ([]hrDayRow, error) {
	rows, err := q.Query(ctx, `
		select employee_id, day, office_id, is_office_open, to_char(opens, 'HH24:MI'), to_char(closes, 'HH24:MI'),
		       holiday_kind, holiday_name, pattern_id, pattern_kind, hours_per_day::float8, days_per_week::int,
		       is_pattern_day, shift_id, shift_kind, to_char(planned_start, 'HH24:MI'), to_char(planned_end, 'HH24:MI'),
		       planned_minutes, is_workday, absence_id, absence_type, absence_part, absence_days::float8,
		       is_makeup, makeup_days::float8, office_days::float8, office_minutes, norm_days::float8
		from public.hr_days($1, $2, $3, $4)
	`, officeID, from, to, employeeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []hrDayRow{}
	for rows.Next() {
		var r hrDayRow
		if err := rows.Scan(&r.EmployeeID, &r.Day, &r.OfficeID, &r.IsOfficeOpen, &r.Opens, &r.Closes,
			&r.HolidayKind, &r.HolidayName, &r.PatternID, &r.PatternKind, &r.HoursPerDay, &r.DaysPerWeek,
			&r.IsPatternDay, &r.ShiftID, &r.ShiftKind, &r.PlannedStart, &r.PlannedEnd,
			&r.PlannedMinutes, &r.IsWorkday, &r.AbsenceID, &r.AbsenceType, &r.AbsencePart, &r.AbsenceDays,
			&r.IsMakeup, &r.MakeupDays, &r.OfficeDays, &r.OfficeMinutes, &r.NormDays); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- patterns -------------------------------------------------------------------------------

type hrPatternJSON struct {
	ID            uuid.UUID  `json:"id"`
	ValidFrom     string     `json:"validFrom"`
	ValidTo       *string    `json:"validTo"`
	Kind          string     `json:"kind"`
	Weekdays      []int      `json:"weekdays"`
	StartTime     *string    `json:"startTime"`
	EndTime       *string    `json:"endTime"`
	DaysPerWeek   int        `json:"daysPerWeek"`
	HoursPerDay   float64    `json:"hoursPerDay"`
	Note          *string    `json:"note"`
	CreatedAt     time.Time  `json:"createdAt"`
	CreatedBy     *uuid.UUID `json:"createdBy"`
	CreatedByName *string    `json:"createdByName"`
}

const hrPatternSelect = `
	select p.id, p.valid_from, p.valid_to, p.kind, coalesce(p.weekdays, '{}'::smallint[])::int[],
	       to_char(p.start_time, 'HH24:MI'), to_char(p.end_time, 'HH24:MI'), p.days_per_week::int,
	       p.hours_per_day::float8, p.note, p.created_at, p.created_by, cp.display_name
	from public.hr_work_patterns p
	left join public.profiles cp on cp.id = p.created_by
`

func scanHRPattern(row pgx.Row) (hrPatternJSON, error) {
	var p hrPatternJSON
	var from time.Time
	var to *time.Time
	err := row.Scan(&p.ID, &from, &to, &p.Kind, &p.Weekdays, &p.StartTime, &p.EndTime, &p.DaysPerWeek,
		&p.HoursPerDay, &p.Note, &p.CreatedAt, &p.CreatedBy, &p.CreatedByName)
	if err != nil {
		return p, err
	}
	p.ValidFrom = hrFormatDate(from)
	if to != nil {
		v := hrFormatDate(*to)
		p.ValidTo = &v
	}
	if p.Weekdays == nil {
		p.Weekdays = []int{}
	}
	return p, nil
}

func hrPatternOn(ctx context.Context, q hrQuerier, employeeID uuid.UUID, day time.Time) (*hrPatternJSON, error) {
	p, err := scanHRPattern(q.QueryRow(ctx, hrPatternSelect+`
		where p.employee_id = $1 and p.valid_from <= $2 and (p.valid_to is null or $2 <= p.valid_to)
		order by p.valid_from desc limit 1`, employeeID, day))
	if errors.Is(err, pgx.ErrNoRows) {
		// Not started yet: show the first future pattern.
		p, err = scanHRPattern(q.QueryRow(ctx, hrPatternSelect+`
			where p.employee_id = $1 and p.valid_from > $2 order by p.valid_from limit 1`, employeeID, day))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func hrPatternHistory(ctx context.Context, q hrQuerier, employeeID uuid.UUID) ([]hrPatternJSON, error) {
	rows, err := q.Query(ctx, hrPatternSelect+` where p.employee_id = $1 order by p.valid_from desc`, employeeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []hrPatternJSON{}
	for rows.Next() {
		p, err := scanHRPattern(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- balances -------------------------------------------------------------------------------

func hrLeaveLimit(ctx context.Context, q hrQuerier, e hrEmployee, year int) (float64, error) {
	var days float64
	err := q.QueryRow(ctx, `
		select coalesce(
		  (select l.days::float8 from public.hr_leave_limits l where l.employee_id = $1 and l.year = $2),
		  (select s.default_vacation_days::float8 from public.hr_office_settings s where s.office_id = $3),
		  20)
	`, e.ID, year, e.OfficeID).Scan(&days)
	return days, err
}

type hrBalancesJSON struct {
	Vacation   hrVacationBalance `json:"vacation"`
	MakeupOwed float64           `json:"makeupOwed"`
}

func hrLoadBalances(ctx context.Context, q hrQuerier, e hrEmployee, year int) (hrBalancesJSON, error) {
	var out hrBalancesJSON
	from := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(year, 12, 31, 0, 0, 0, 0, time.UTC)
	rows, err := hrLoadDays(ctx, q, nil, from, to, &e.ID)
	if err != nil {
		return out, err
	}
	limit, err := hrLeaveLimit(ctx, q, e, year)
	if err != nil {
		return out, err
	}
	out.Vacation = hrBalance(year, limit, hrVacationUsed(rows))
	owed, err := hrLoadMakeupOwed(ctx, q, e)
	if err != nil {
		return out, err
	}
	out.MakeupOwed = owed
	return out, nil
}

// hrLoadMakeupOwed: all-time balance over the span of the person's make-up entries.
func hrLoadMakeupOwed(ctx context.Context, q hrQuerier, e hrEmployee) (float64, error) {
	var from, to *time.Time
	if err := q.QueryRow(ctx, `
		select min(d), max(d) from (
		  select date_from as d from public.hr_absences where employee_id = $1 and type = 'day_off_makeup' and deleted_at is null
		  union all
		  select date_to from public.hr_absences where employee_id = $1 and type = 'day_off_makeup' and deleted_at is null
		  union all
		  select work_date from public.hr_shifts where employee_id = $1 and kind = 'makeup'
		) s
	`, e.ID).Scan(&from, &to); err != nil {
		return 0, err
	}
	if from == nil || to == nil {
		return 0, nil
	}
	rows, err := hrLoadDays(ctx, q, nil, *from, *to, &e.ID)
	if err != nil {
		return 0, err
	}
	return hrMakeupOwed(rows), nil
}

// ---- helpers --------------------------------------------------------------------------------

func (s *Server) hrFail(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Error("hr request failed", "error", err, "request_id", requestID(r.Context()), "path", r.URL.Path)
	s.writeError(w, r, http.StatusInternalServerError, "hr_failed", "Could not complete the HR request", nil)
}

func hrDateString(t *time.Time) *string {
	if t == nil {
		return nil
	}
	v := hrFormatDate(*t)
	return &v
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
