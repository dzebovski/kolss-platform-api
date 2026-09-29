-- CRM v2 (P1): projects, contract, payments, files and timeline.
--
-- Reuses the orphaned public.projects table (20260610120000): no application code reads or writes
-- it, it is empty in practice, and its id / lead_id / office_id / assigned_to columns, the
-- leads.converted_project_id link and the tasks RLS policies that already point at it stay valid.
-- Only the stage vocabulary is replaced: the 10 project_stages codes give way to the 9 statuses of
-- the v2 boards (owner decision Q-P1, 2026-09-29). Additive for v1: no v1 screen reads projects.

begin;

-- The legacy conversion RPC inserts the retired 'needs_discovery' stage. Nothing calls it (v1 and
-- the API convert leads through their own flows), so it is removed instead of left to fail.
drop function if exists public.convert_lead_to_project(uuid);

-- Status vocabulary -----------------------------------------------------------------------------
alter table public.projects drop constraint if exists projects_status_fkey;
alter table public.projects alter column status set default 'none';

update public.projects
set status = case status
  when 'needs_discovery' then 'none'
  when 'design_quote' then 'design'
  when 'approval' then 'design'
  when 'measurement' then 'measure'
  when 'contract' then 'contract'
  when 'production' then 'production'
  when 'installation' then 'installation'
  when 'final_payment' then 'installation'
  when 'completed' then 'completed'
  when 'archived' then 'cancelled'
  else 'none'
end;

alter table public.projects
  add column if not exists project_type text,
  add column if not exists event_date date,
  add column if not exists event_time time,
  add column if not exists status_details jsonb not null default '{}'::jsonb,
  add column if not exists cancelled_at timestamptz,
  add column if not exists cancelled_by uuid references public.profiles (id) on delete set null,
  add column if not exists cancel_reasons text[] not null default '{}',
  add column if not exists cancel_comment text,
  add column if not exists status_before_cancel text,
  add column if not exists created_by uuid references public.profiles (id) on delete set null;

-- Cancelled rows created by the legacy 'archived' mapping have no cancellation details.
update public.projects
set cancelled_at = coalesce(cancelled_at, status_changed_at),
    status_before_cancel = coalesce(status_before_cancel, 'none')
where status = 'cancelled';

alter table public.projects
  drop constraint if exists projects_status_check,
  add constraint projects_status_check check (
    status in ('none', 'express', 'measure', 'design', 'contract', 'production', 'installation', 'completed', 'cancelled')
  ),
  drop constraint if exists projects_project_type_check,
  add constraint projects_project_type_check check (
    project_type is null or project_type in ('express', 'measure', 'contract')
  ),
  drop constraint if exists projects_event_time_needs_date_check,
  add constraint projects_event_time_needs_date_check check (event_time is null or event_date is not null),
  drop constraint if exists projects_cancelled_check,
  add constraint projects_cancelled_check check ((status = 'cancelled') = (cancelled_at is not null)),
  drop constraint if exists projects_cancel_reasons_check,
  add constraint projects_cancel_reasons_check check (
    cancel_reasons <@ array[
      'chose_another_supplier', 'price_too_high', 'postponed_renovation',
      'disagreed_design', 'not_relevant', 'other'
    ]::text[]
  ),
  drop constraint if exists projects_status_before_cancel_check,
  add constraint projects_status_before_cancel_check check (
    status_before_cancel is null or status_before_cancel in
      ('none', 'express', 'measure', 'design', 'contract', 'production', 'installation', 'completed')
  ),
  drop constraint if exists projects_cancel_comment_check,
  add constraint projects_cancel_comment_check check (
    cancel_comment is null or char_length(cancel_comment) <= 1000
  );

create index if not exists projects_assigned_to_idx on public.projects (assigned_to);
create index if not exists projects_office_created_idx on public.projects (office_id, created_at desc, id desc);

-- Files (contract PDF, payment receipts) --------------------------------------------------------
-- Objects live in the existing private 'lead-attachments' bucket (25 MiB, pdf/jpg/png/heic) under
-- <office_id>/<project_id>/<file_id>/<name>; uploads use the same presigned two-step flow as
-- lead documents. A file row is attached to a contract or payment when that record is created.
create table public.project_files (
  id uuid primary key default gen_random_uuid(),
  project_id uuid not null references public.projects (id) on delete cascade,
  kind text not null check (kind in ('contract', 'receipt')),
  file_name text not null,
  storage_bucket text not null default 'lead-attachments',
  storage_path text not null,
  mime_type text not null,
  size_bytes bigint not null check (size_bytes > 0 and size_bytes <= 26214400),
  status text not null default 'awaiting_upload' check (status in ('awaiting_upload', 'ready', 'blocked')),
  uploaded_by uuid references public.profiles (id) on delete set null,
  created_at timestamptz not null default now()
);

create unique index project_files_storage_path_key on public.project_files (storage_bucket, storage_path);
create index project_files_project_id_idx on public.project_files (project_id, created_at desc);

-- Contract (one per project) --------------------------------------------------------------------
create table public.project_contracts (
  id uuid primary key default gen_random_uuid(),
  project_id uuid not null unique references public.projects (id) on delete cascade,
  number text not null check (char_length(btrim(number)) between 1 and 60),
  signed_on date not null,
  total_amount numeric(14, 2) not null check (total_amount > 0),
  currency text not null check (currency in ('PLN', 'UAH', 'EUR', 'USD')),
  file_id uuid references public.project_files (id) on delete set null,
  created_by uuid references public.profiles (id) on delete set null,
  created_at timestamptz not null default now()
);

-- Payments --------------------------------------------------------------------------------------
-- The currency is the contract's. The "not more than the remaining balance" rule is enforced by
-- the API while it holds the project row lock.
create table public.project_payments (
  id uuid primary key default gen_random_uuid(),
  project_id uuid not null references public.projects (id) on delete cascade,
  amount numeric(14, 2) not null check (amount > 0),
  paid_on date not null,
  note text check (note is null or char_length(note) <= 300),
  file_id uuid references public.project_files (id) on delete set null,
  created_by uuid references public.profiles (id) on delete set null,
  created_at timestamptz not null default now()
);

create index project_payments_project_id_idx on public.project_payments (project_id, paid_on, created_at);

-- Timeline --------------------------------------------------------------------------------------
create table public.project_events (
  id uuid primary key default gen_random_uuid(),
  project_id uuid not null references public.projects (id) on delete cascade,
  actor_id uuid references public.profiles (id) on delete set null,
  event_type text not null check (
    event_type in ('created', 'status_changed', 'status_updated', 'contract_added', 'payment_added', 'cancelled', 'restored')
  ),
  old_value jsonb,
  new_value jsonb,
  comment text check (comment is null or char_length(comment) <= 1000),
  created_at timestamptz not null default now()
);

create index project_events_project_id_idx on public.project_events (project_id, created_at desc, id desc);

-- RLS: same office scope as projects (the API role bypasses it like it does for leads) ----------
alter table public.project_files enable row level security;
alter table public.project_contracts enable row level security;
alter table public.project_payments enable row level security;
alter table public.project_events enable row level security;

create policy project_files_select on public.project_files
  for select to authenticated
  using (exists (
    select 1 from public.projects p
    where p.id = project_id and private.can_access_office(p.office_id)
  ));

create policy project_contracts_select on public.project_contracts
  for select to authenticated
  using (exists (
    select 1 from public.projects p
    where p.id = project_id and private.can_access_office(p.office_id)
  ));

create policy project_payments_select on public.project_payments
  for select to authenticated
  using (exists (
    select 1 from public.projects p
    where p.id = project_id and private.can_access_office(p.office_id)
  ));

create policy project_events_select on public.project_events
  for select to authenticated
  using (exists (
    select 1 from public.projects p
    where p.id = project_id and private.can_access_office(p.office_id)
  ));

-- The API authenticates users and checks office access before every query. Its database role is
-- not `authenticated`, so (as for manual tasks in 20260908120000) it gets explicit policies
-- instead of relying on how the role is configured out-of-band.
create policy projects_api_runtime on public.projects
  for all to kolss_api using (true) with check (true);
create policy project_files_api_runtime on public.project_files
  for all to kolss_api using (true) with check (true);
create policy project_contracts_api_runtime on public.project_contracts
  for all to kolss_api using (true) with check (true);
create policy project_payments_api_runtime on public.project_payments
  for all to kolss_api using (true) with check (true);
create policy project_events_api_runtime on public.project_events
  for all to kolss_api using (true) with check (true);

-- API role. projects already has delete (20260715120000); the child tables are removed by cascade.
grant select, insert, update on public.projects to kolss_api;
grant select, insert, update on public.project_files to kolss_api;
grant select, insert on public.project_contracts, public.project_payments, public.project_events to kolss_api;

commit;
