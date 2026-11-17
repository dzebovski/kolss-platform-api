-- HR portal (HB1): tracked employees, office hours and settings, work patterns, holidays,
-- vacation limits. Decisions D1–D20 in the HR plan (owner, 2026-11-16).
--
-- * Only CRM accounts the super admin adds are tracked (D5). Stopping tracking keeps the row
--   and sets tracked_until (last tracked day), so history and reports stay.
-- * Office hours per ISO weekday (1 = Mon … 7 = Sun); null opens/closes = closed (D6, D18).
--   Seeded Mon–Fri 10–19, Sat 10–15, Sun closed for every existing office.
-- * Work patterns are effective-dated (D6): fixed = weekdays + start/end + hours/day;
--   flexible = days/week × hours/day (the person plans exact hours in hr_shifts).
--   No overlaps per employee: the API closes the previous pattern in the same transaction.
-- * Holidays / office-closed days are entered per office by the super admin (D18).
-- * Vacation limit per employee per year in days (D10, D19); a missing year falls back to the
--   office default (hr_office_settings.default_vacation_days, 20).
-- * The office time zone is offices.timezone_name (already exists).
--
-- Authorization lives in the API (Go). RLS is enabled as legacy defence; the API role gets an
-- explicit runtime policy like projects/clients.
--
-- v1 impact: none (new tables only).

begin;

create table if not exists public.hr_office_settings (
  office_id uuid primary key references public.offices (id) on delete cascade,
  default_vacation_days numeric(5,1) not null default 20 check (default_vacation_days >= 0 and default_vacation_days <= 366),
  updated_at timestamptz not null default now(),
  updated_by uuid references public.profiles (id)
);

create table if not exists public.hr_office_hours (
  office_id uuid not null references public.offices (id) on delete cascade,
  weekday smallint not null check (weekday between 1 and 7),
  opens time,
  closes time,
  primary key (office_id, weekday),
  constraint hr_office_hours_open_pair check ((opens is null) = (closes is null)),
  constraint hr_office_hours_order check (opens is null or closes > opens)
);

create table if not exists public.hr_employees (
  id uuid primary key default gen_random_uuid(),
  profile_id uuid not null unique references public.profiles (id) on delete restrict,
  office_id uuid not null references public.offices (id),
  position text check (position is null or char_length(position) <= 120),
  started_on date not null,
  -- Last day of employment (Employee settings "End date"); null = not set.
  ended_on date,
  -- Last tracked day; null = tracked. "Stop tracking" sets it to today (office time zone), so the
  -- person disappears from calendars and reports from tomorrow.
  tracked_until date,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  created_by uuid references public.profiles (id),
  updated_by uuid references public.profiles (id),
  constraint hr_employees_end_after_start check (ended_on is null or ended_on >= started_on)
);
create index if not exists hr_employees_office_idx on public.hr_employees (office_id);

create table if not exists public.hr_work_patterns (
  id uuid primary key default gen_random_uuid(),
  employee_id uuid not null references public.hr_employees (id) on delete cascade,
  valid_from date not null,
  valid_to date,
  kind text not null check (kind in ('fixed', 'flexible')),
  weekdays smallint[],
  start_time time,
  end_time time,
  days_per_week smallint not null check (days_per_week between 1 and 7),
  hours_per_day numeric(4,2) not null check (hours_per_day > 0 and hours_per_day <= 24),
  note text check (note is null or char_length(note) <= 500),
  created_by uuid references public.profiles (id),
  created_at timestamptz not null default now(),
  constraint hr_work_patterns_range check (valid_to is null or valid_to >= valid_from),
  constraint hr_work_patterns_fixed_shape check (
    kind <> 'fixed' or (
      weekdays is not null and cardinality(weekdays) between 1 and 7
      and weekdays <@ array[1,2,3,4,5,6,7]::smallint[]
      and start_time is not null and end_time is not null and end_time > start_time
    )
  )
);
create index if not exists hr_work_patterns_employee_idx on public.hr_work_patterns (employee_id, valid_from);

create table if not exists public.hr_calendar_days (
  id uuid primary key default gen_random_uuid(),
  office_id uuid not null references public.offices (id) on delete cascade,
  day date not null,
  kind text not null check (kind in ('public_holiday', 'office_closed')),
  name text not null check (char_length(btrim(name)) between 1 and 120),
  created_at timestamptz not null default now(),
  created_by uuid references public.profiles (id),
  unique (office_id, day)
);

create table if not exists public.hr_leave_limits (
  employee_id uuid not null references public.hr_employees (id) on delete cascade,
  year int not null check (year between 2000 and 2100),
  days numeric(5,1) not null check (days >= 0 and days <= 366),
  updated_at timestamptz not null default now(),
  updated_by uuid references public.profiles (id),
  primary key (employee_id, year)
);

-- Seed: every existing office gets settings and Mon–Fri 10–19, Sat 10–15, Sun closed (D18).
insert into public.hr_office_settings (office_id)
select id from public.offices
on conflict (office_id) do nothing;

insert into public.hr_office_hours (office_id, weekday, opens, closes)
select o.id, w.weekday, w.opens, w.closes
from public.offices o
cross join (values
  (1::smallint, time '10:00', time '19:00'),
  (2::smallint, time '10:00', time '19:00'),
  (3::smallint, time '10:00', time '19:00'),
  (4::smallint, time '10:00', time '19:00'),
  (5::smallint, time '10:00', time '19:00'),
  (6::smallint, time '10:00', time '15:00'),
  (7::smallint, null::time, null::time)
) as w (weekday, opens, closes)
on conflict (office_id, weekday) do nothing;

-- RLS and grants -------------------------------------------------------------------------------
alter table public.hr_office_settings enable row level security;
alter table public.hr_office_hours enable row level security;
alter table public.hr_employees enable row level security;
alter table public.hr_work_patterns enable row level security;
alter table public.hr_calendar_days enable row level security;
alter table public.hr_leave_limits enable row level security;

drop policy if exists hr_office_settings_api_runtime on public.hr_office_settings;
create policy hr_office_settings_api_runtime on public.hr_office_settings for all to kolss_api using (true) with check (true);
drop policy if exists hr_office_hours_api_runtime on public.hr_office_hours;
create policy hr_office_hours_api_runtime on public.hr_office_hours for all to kolss_api using (true) with check (true);
drop policy if exists hr_employees_api_runtime on public.hr_employees;
create policy hr_employees_api_runtime on public.hr_employees for all to kolss_api using (true) with check (true);
drop policy if exists hr_work_patterns_api_runtime on public.hr_work_patterns;
create policy hr_work_patterns_api_runtime on public.hr_work_patterns for all to kolss_api using (true) with check (true);
drop policy if exists hr_calendar_days_api_runtime on public.hr_calendar_days;
create policy hr_calendar_days_api_runtime on public.hr_calendar_days for all to kolss_api using (true) with check (true);
drop policy if exists hr_leave_limits_api_runtime on public.hr_leave_limits;
create policy hr_leave_limits_api_runtime on public.hr_leave_limits for all to kolss_api using (true) with check (true);

grant select, insert, update on public.hr_office_settings to kolss_api;
grant select, insert, update on public.hr_office_hours to kolss_api;
grant select, insert, update on public.hr_employees to kolss_api;
grant select, insert, update, delete on public.hr_work_patterns to kolss_api;
grant select, insert, update, delete on public.hr_calendar_days to kolss_api;
grant select, insert, update, delete on public.hr_leave_limits to kolss_api;

commit;
