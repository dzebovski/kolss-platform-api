package crmapi

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestHRDaysDB checks the hr_days rules (HB3) against a disposable database with all migrations
// applied. Runs only when KOLSS_TEST_DATABASE_URL is set (never production): it writes fixtures.
// Dates are in March 2031 (2031-03-03 is a Monday) to stay clear of seed data.
func TestHRDaysDB(t *testing.T) {
	dsn := os.Getenv("KOLSS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("KOLSS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var officeID uuid.UUID
	if err := pool.QueryRow(ctx, `select id from public.offices where code = 'warsaw'`).Scan(&officeID); err != nil {
		t.Fatal("office fixture:", err)
	}
	// Known office hours: Mon–Fri 10–19, Sat 10–15, Sunday closed.
	for wd := 1; wd <= 7; wd++ {
		opens, closes := "10:00", "19:00"
		switch wd {
		case 6:
			closes = "15:00"
		case 7:
			mustExec(t, pool, `insert into public.hr_office_hours (office_id, weekday, opens, closes) values ($1, 7, null, null)
				on conflict (office_id, weekday) do update set opens = null, closes = null`, officeID)
			continue
		}
		mustExec(t, pool, `insert into public.hr_office_hours (office_id, weekday, opens, closes) values ($1, $2, $3::time, $4::time)
			on conflict (office_id, weekday) do update set opens = excluded.opens, closes = excluded.closes`, officeID, wd, opens, closes)
	}
	mustExec(t, pool, `insert into public.hr_calendar_days (office_id, day, kind, name) values ($1, '2031-03-05', 'public_holiday', 'Test holiday')
		on conflict (office_id, day) do nothing`, officeID)

	newEmployee := func(name string) uuid.UUID {
		profileID, employeeID := uuid.New(), uuid.New()
		mustExec(t, pool, `insert into auth.users (id, email) values ($1, $2)`, profileID, profileID.String()+"@test.local")
		mustExec(t, pool, `insert into public.profiles (id, display_name) values ($1, $2) on conflict (id) do update set display_name = excluded.display_name`, profileID, name)
		mustExec(t, pool, `insert into public.hr_employees (id, profile_id, office_id, started_on) values ($1, $2, $3, '2031-01-01')`, employeeID, profileID, officeID)
		return employeeID
	}
	fixed := func(id uuid.UUID, from string) {
		mustExec(t, pool, `insert into public.hr_work_patterns (employee_id, valid_from, kind, weekdays, start_time, end_time, days_per_week, hours_per_day)
			values ($1, $2, 'fixed', '{1,2,3,4,5}', '10:00', '18:00', 5, 8)`, id, from)
	}
	flexible := func(id uuid.UUID, from string, dpw int, hpd float64) {
		mustExec(t, pool, `insert into public.hr_work_patterns (employee_id, valid_from, kind, days_per_week, hours_per_day)
			values ($1, $2, 'flexible', $3, $4)`, id, from, dpw, hpd)
	}
	absence := func(id uuid.UUID, kind, from, to, part string) {
		mustExec(t, pool, `insert into public.hr_absences (employee_id, type, date_from, date_to, part, comment) values ($1, $2, $3, $4, $5, 'test')`,
			id, kind, from, to, part)
	}
	shift := func(id uuid.UUID, day, start, end, kind string) {
		mustExec(t, pool, `insert into public.hr_shifts (employee_id, work_date, start_time, end_time, kind) values ($1, $2, $3::time, $4::time, $5)`,
			id, day, start, end, kind)
	}
	totals := func(id uuid.UUID, from, to string) (hrTotals, []hrDayRow) {
		t.Helper()
		rows, err := hrLoadDays(ctx, pool, &officeID, hrTestDate(t, from), hrTestDate(t, to), &id)
		if err != nil {
			t.Fatal(err)
		}
		return hrAggregate(rows)[id], rows
	}
	const weekFrom, weekTo = "2031-03-03", "2031-03-09"
	if hrTestDate(t, weekFrom).Weekday() != time.Monday {
		t.Fatal("fixture week must start on Monday")
	}

	t.Run("fixed 5x8 with a mid-week holiday", func(t *testing.T) {
		id := newEmployee("Fixed")
		fixed(id, "2031-01-01")
		got, _ := totals(id, weekFrom, weekTo)
		if got.NormDays != 4 || got.PlannedDays != 4 || got.PlannedHours != 32 {
			t.Fatalf("got %+v, want norm 4 / planned 4 days 32 h", got)
		}
	})

	t.Run("flexible 5x4 without plan", func(t *testing.T) {
		id := newEmployee("Flexible")
		flexible(id, "2031-01-01", 5, 4)
		got, _ := totals(id, "2031-03-10", "2031-03-16")
		if got.NormDays != 5 || got.PlannedDays != 0 {
			t.Fatalf("got %+v, want norm 5, nothing planned", got)
		}
	})

	t.Run("half day and multi-day absence over a weekend", func(t *testing.T) {
		id := newEmployee("Absent")
		fixed(id, "2031-01-01")
		absence(id, "vacation", "2031-03-06", "2031-03-06", "first_half") // Thursday
		absence(id, "vacation", "2031-03-07", "2031-03-11", "full_day")   // Fri → Tue
		got, _ := totals(id, weekFrom, "2031-03-16")
		// Week 1: Mon, Tue planned; Wed holiday; Thu half; Fri vacation. Week 2: Mon, Tue vacation, Wed–Fri planned.
		if got.Vacation != 3.5 || got.PlannedDays != 5.5 || got.PlannedHours != 44 {
			t.Fatalf("got %+v, want vacation 3.5, planned 5.5 days 44 h", got)
		}
	})

	t.Run("flexible absence is capped by the weekly norm", func(t *testing.T) {
		id := newEmployee("Flexible absent")
		flexible(id, "2031-01-01", 3, 6)
		absence(id, "vacation", "2031-03-17", "2031-03-22", "full_day") // whole week Mon–Sat
		got, _ := totals(id, "2031-03-17", "2031-03-23")
		if got.Vacation != 3 {
			t.Fatalf("got %+v, want 3 vacation days (3 days a week)", got)
		}
	})

	t.Run("make-up cycle", func(t *testing.T) {
		id := newEmployee("Make-up")
		fixed(id, "2031-01-01")
		absence(id, "day_off_makeup", "2031-03-03", "2031-03-03", "full_day")
		shift(id, "2031-03-08", "10:00", "15:00", "makeup") // Saturday, 5 of 8 h → 0.5
		_, rows := totals(id, weekFrom, weekTo)
		if owed := hrMakeupOwed(rows); owed != 0.5 {
			t.Fatalf("owed %v after one short make-up day, want 0.5", owed)
		}
		shift(id, "2031-03-15", "10:00", "15:00", "makeup")
		_, rows = totals(id, weekFrom, "2031-03-16")
		if owed := hrMakeupOwed(rows); owed != 0 {
			t.Fatalf("owed %v, want 0", owed)
		}
	})

	t.Run("pattern change in the middle of the month", func(t *testing.T) {
		id := newEmployee("Changed")
		fixed(id, "2031-01-01")
		mustExec(t, pool, `update public.hr_work_patterns set valid_to = '2031-03-15' where employee_id = $1`, id)
		flexible(id, "2031-03-16", 3, 6)
		got, _ := totals(id, "2031-03-01", "2031-03-31")
		// Fixed 1–15 March: 10 weekdays − 1 holiday = 9. Flexible 16 (Sun)–31: 13 open days × 3/6 = 6.5.
		if got.NormDays != 15.5 {
			t.Fatalf("norm %v, want 15.5", got.NormDays)
		}
	})

	t.Run("sick leave is absent for colleagues", func(t *testing.T) {
		id := newEmployee("Sick")
		fixed(id, "2031-01-01")
		absence(id, "sick", "2031-03-04", "2031-03-04", "full_day")
		got, rows := totals(id, weekFrom, weekTo)
		if got.Sick != 1 {
			t.Fatalf("sick %v", got.Sick)
		}
		for _, r := range rows {
			if r.AbsenceType != nil {
				if typ, _ := hrVisibleAbsence(*r.AbsenceType, nil, false); typ != hrTypeAbsent {
					t.Fatalf("colleague sees %q", typ)
				}
			}
		}
	})

	t.Run("vacation over the limit is allowed and shown", func(t *testing.T) {
		id := newEmployee("Over")
		fixed(id, "2031-01-01")
		mustExec(t, pool, `insert into public.hr_leave_limits (employee_id, year, days) values ($1, 2031, 2)`, id)
		absence(id, "vacation", "2031-03-10", "2031-03-12", "full_day")
		var e hrEmployee
		e.ID, e.OfficeID = id, officeID
		b, err := hrLoadBalances(ctx, pool, e, 2031)
		if err != nil {
			t.Fatal(err)
		}
		if b.Vacation.Used != 3 || b.Vacation.Over != 1 {
			t.Fatalf("balance %+v, want 3 used, 1 over", b.Vacation)
		}
	})
}
