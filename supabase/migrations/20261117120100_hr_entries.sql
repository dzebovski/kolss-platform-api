-- HR portal (HB2): absences and planned shifts. Decisions D3, D4, D11, D15, D17, D20.
--
-- * Absences are information entries saved at once (no approvals, D3). Soft delete keeps the
--   row for the Slack log. Types (D4): vacation, sick, day_off_unpaid, day_off_makeup, remote,
--   occasional, other. A "make-up day" is NOT an absence: it is an hr_shifts row with
--   kind = 'makeup' (it has hours; D15).
-- * Half days (D20) only when date_from = date_to. Comment required for "other".
-- * One shift per employee per day. For fixed schedules the shift is derived from the pattern; a
--   row here overrides it for that date. Flexible schedules plan every day here.
-- * Edit windows (D12, D17) and "no overlapping absences" are checked by the API.
--
-- v1 impact: none (new tables only).

begin;

create table if not exists public.hr_absences (
  id uuid primary key default gen_random_uuid(),
  employee_id uuid not null references public.hr_employees (id) on delete cascade,
  type text not null check (type in ('vacation', 'sick', 'day_off_unpaid', 'day_off_makeup', 'remote', 'occasional', 'other')),
  date_from date not null,
  date_to date not null,
  part text not null default 'full_day' check (part in ('full_day', 'first_half', 'second_half')),
  comment text check (comment is null or char_length(comment) <= 1000),
  created_at timestamptz not null default now(),
  created_by uuid references public.profiles (id),
  updated_at timestamptz not null default now(),
  updated_by uuid references public.profiles (id),
  deleted_at timestamptz,
  deleted_by uuid references public.profiles (id),
  constraint hr_absences_range check (date_to >= date_from),
  constraint hr_absences_half_single_day check (part = 'full_day' or date_from = date_to),
  constraint hr_absences_other_comment check (type <> 'other' or char_length(btrim(coalesce(comment, ''))) > 0)
);
create index if not exists hr_absences_employee_range_idx
  on public.hr_absences (employee_id, date_from, date_to)
  where deleted_at is null;

create table if not exists public.hr_shifts (
  id uuid primary key default gen_random_uuid(),
  employee_id uuid not null references public.hr_employees (id) on delete cascade,
  work_date date not null,
  start_time time not null,
  end_time time not null,
  kind text not null default 'planned' check (kind in ('planned', 'makeup')),
  created_at timestamptz not null default now(),
  created_by uuid references public.profiles (id),
  updated_at timestamptz not null default now(),
  updated_by uuid references public.profiles (id),
  constraint hr_shifts_order check (end_time > start_time),
  unique (employee_id, work_date)
);

alter table public.hr_absences enable row level security;
alter table public.hr_shifts enable row level security;

drop policy if exists hr_absences_api_runtime on public.hr_absences;
create policy hr_absences_api_runtime on public.hr_absences for all to kolss_api using (true) with check (true);
drop policy if exists hr_shifts_api_runtime on public.hr_shifts;
create policy hr_shifts_api_runtime on public.hr_shifts for all to kolss_api using (true) with check (true);

grant select, insert, update on public.hr_absences to kolss_api;
grant select, insert, update, delete on public.hr_shifts to kolss_api;

commit;
