-- HR portal (HB3): hr_days — one row per tracked employee per date. Store rules and entries,
-- compute days (HR plan §5). The API builds the office calendar, My time tiles and the month /
-- year reports from these rows; nothing here is stored.
--
-- Rules (D6, D7, D10, D11, D15, D18, D20):
-- * A person is in the result on days between hr_employees.started_on and the earlier of
--   ended_on / tracked_until.
-- * planned hours: an hr_shifts row wins; otherwise a fixed pattern gives start/end on its
--   weekdays. Flexible patterns have planned hours only where a shift exists.
-- * is_workday: planned hours exist, the office is open that weekday and the day is not a
--   holiday / office-closed day.
-- * absence_days (1, or 0.5 for a half day) count only on days the person works:
--     - fixed (or no pattern): on is_workday days;
--     - flexible: on open, non-holiday days, at most days_per_week per ISO week minus the days
--       that week with a planned shift and no absence (a 3 × 6 person taking Mon–Fri off uses
--       3 days, not 5).
-- * norm_days (expected days, before absences):
--     - fixed: 1 on pattern weekdays when the office is open and it is not a holiday;
--     - flexible: days_per_week / (open weekdays of the office) on every open non-holiday day,
--       so a full week sums to days_per_week, and a holiday or a month boundary prorates it.
-- * office_days / office_minutes: what is left of the planned day after an absence
--   (full-day absence → 0, half → 0.5).
-- * make-up (D15): a shift with kind 'makeup'; makeup_days = its length / the daily norm,
--   rounded to 0.5, between 0.5 and 1.
--
-- v1 impact: none.

begin;

create or replace function public.hr_days(p_office uuid, p_from date, p_to date, p_employee uuid default null)
returns table (
  employee_id uuid,
  day date,
  office_id uuid,
  is_office_open boolean,
  opens time,
  closes time,
  holiday_kind text,
  holiday_name text,
  pattern_id uuid,
  pattern_kind text,
  hours_per_day numeric,
  days_per_week smallint,
  is_pattern_day boolean,
  shift_id uuid,
  shift_kind text,
  planned_start time,
  planned_end time,
  planned_minutes int,
  is_workday boolean,
  absence_id uuid,
  absence_type text,
  absence_part text,
  absence_days numeric,
  is_makeup boolean,
  makeup_days numeric,
  office_days numeric,
  office_minutes int,
  norm_days numeric
)
language sql
stable
set search_path = public, pg_temp
as $$
with emp as (
  select e.id, e.office_id, e.started_on,
         least(coalesce(e.ended_on, 'infinity'::date), coalesce(e.tracked_until, 'infinity'::date)) as last_day
  from public.hr_employees e
  where (p_office is null or e.office_id = p_office)
    and (p_employee is null or e.id = p_employee)
),
d as (
  select gs::date as day
  from generate_series(p_from::timestamp, p_to::timestamp, interval '1 day') gs
),
base as (
  select emp.id as employee_id, emp.office_id, d.day, extract(isodow from d.day)::smallint as wd
  from emp
  join d on d.day >= emp.started_on and d.day <= emp.last_day
),
week_open as (
  select oh.office_id, count(*)::numeric as open_days
  from public.hr_office_hours oh
  where oh.opens is not null
  group by oh.office_id
),
x as (
  select
    b.employee_id, b.day, b.office_id, b.wd,
    (oh.opens is not null) as is_office_open, oh.opens, oh.closes,
    cd.kind as holiday_kind, cd.name as holiday_name,
    p.id as pattern_id, p.kind as pattern_kind, p.weekdays, p.start_time as p_start, p.end_time as p_end,
    p.hours_per_day, p.days_per_week,
    s.id as shift_id, s.kind as shift_kind, s.start_time as s_start, s.end_time as s_end,
    a.id as absence_id, a.type as absence_type, a.part as absence_part,
    coalesce(wo.open_days, 0) as week_open_days
  from base b
  left join public.hr_office_hours oh on oh.office_id = b.office_id and oh.weekday = b.wd
  left join public.hr_calendar_days cd on cd.office_id = b.office_id and cd.day = b.day
  left join week_open wo on wo.office_id = b.office_id
  left join lateral (
    select p.* from public.hr_work_patterns p
    where p.employee_id = b.employee_id and p.valid_from <= b.day and (p.valid_to is null or b.day <= p.valid_to)
    order by p.valid_from desc
    limit 1
  ) p on true
  left join public.hr_shifts s on s.employee_id = b.employee_id and s.work_date = b.day
  left join lateral (
    select a.* from public.hr_absences a
    where a.employee_id = b.employee_id and a.deleted_at is null and b.day between a.date_from and a.date_to
    order by a.created_at
    limit 1
  ) a on true
),
y as (
  select x.*,
    (x.pattern_kind = 'fixed' and x.wd = any(x.weekdays)) as pattern_day,
    coalesce(x.s_start, case when x.pattern_kind = 'fixed' and x.wd = any(x.weekdays) then x.p_start end) as p_start2,
    coalesce(x.s_end, case when x.pattern_kind = 'fixed' and x.wd = any(x.weekdays) then x.p_end end) as p_end2,
    (x.is_office_open and x.holiday_kind is null) as open_day,
    case when x.absence_part in ('first_half', 'second_half') then 0.5 else 1 end::numeric as absence_fraction
  from x
),
z as (
  select y.*,
    (y.p_start2 is not null and y.open_day) as workday,
    -- Candidate days on which an absence uses the person's working time.
    (y.absence_id is not null and y.open_day and (
      (y.pattern_kind = 'flexible')
      or (coalesce(y.pattern_kind, 'fixed') = 'fixed' and y.p_start2 is not null)
    )) as absence_candidate
  from y
),
w as (
  select z.*,
    case when z.absence_candidate then
      row_number() over (
        partition by z.employee_id, date_trunc('week', z.day), z.absence_candidate
        order by z.day
      )
    end as absence_rank,
    count(*) filter (where z.shift_id is not null and z.absence_id is null and z.open_day)
      over (partition by z.employee_id, date_trunc('week', z.day)) as week_shift_days
  from z
)
select
  w.employee_id,
  w.day,
  w.office_id,
  w.is_office_open,
  w.opens,
  w.closes,
  w.holiday_kind,
  w.holiday_name,
  w.pattern_id,
  w.pattern_kind,
  w.hours_per_day,
  w.days_per_week,
  coalesce(w.pattern_day, false) as is_pattern_day,
  w.shift_id,
  w.shift_kind,
  w.p_start2 as planned_start,
  w.p_end2 as planned_end,
  case when w.p_start2 is not null then (extract(epoch from (w.p_end2 - w.p_start2)) / 60)::int else 0 end as planned_minutes,
  coalesce(w.workday, false) as is_workday,
  w.absence_id,
  w.absence_type,
  w.absence_part,
  case
    when not w.absence_candidate then 0
    when w.pattern_kind = 'flexible' then
      case when w.absence_rank <= greatest(0, w.days_per_week - w.week_shift_days) then w.absence_fraction else 0 end
    else w.absence_fraction
  end::numeric as absence_days,
  coalesce(w.shift_kind = 'makeup', false) as is_makeup,
  case when w.shift_kind = 'makeup' then
    least(1, greatest(0.5, round(
      (extract(epoch from (w.s_end - w.s_start)) / 3600) / coalesce(nullif(w.hours_per_day, 0), extract(epoch from (w.s_end - w.s_start)) / 3600) * 2
    ) / 2))
  else 0 end::numeric as makeup_days,
  case
    when not w.workday then 0
    when w.absence_id is not null then 1 - w.absence_fraction
    else 1
  end::numeric as office_days,
  case
    when not w.workday then 0
    else round((extract(epoch from (w.p_end2 - w.p_start2)) / 60) *
      case when w.absence_id is not null then 1 - w.absence_fraction else 1 end)::int
  end as office_minutes,
  case
    when not w.open_day or w.pattern_kind is null then 0
    when w.pattern_kind = 'fixed' then case when w.pattern_day then 1 else 0 end
    when w.week_open_days > 0 then round(w.days_per_week::numeric / w.week_open_days, 4)
    else 0
  end::numeric as norm_days
from w
order by w.employee_id, w.day
$$;

revoke all on function public.hr_days(uuid, date, date, uuid) from public;
grant execute on function public.hr_days(uuid, date, date, uuid) to kolss_api;

commit;
