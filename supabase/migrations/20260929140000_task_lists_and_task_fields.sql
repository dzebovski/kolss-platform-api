-- CRM v2 (T1): task lists and richer standalone manual tasks.
--
-- Additive only (Rule zero): one new table and new nullable / defaulted columns on public.tasks.
-- No existing column, enum value, constraint or policy is changed, so CRM v1 and
-- GET /v1/dashboard/manager-tasks read the same rows exactly as before. "In progress" is a boolean
-- next to the unchanged task_status enum (open / done / canceled): a new enum value would hide such
-- tasks from every v1 query that filters status = 'open'.
--
-- Links: a task may point at a task list or at one lead / project / client (never two, the boards
-- offer one link kind). Only the lead link has a foreign key. Projects and clients are not
-- guaranteed to exist when this migration runs (P1 / CL1), so their columns are plain nullable
-- uuids that the API does not accept until those features land.

begin;

create table public.task_lists (
  id uuid primary key default gen_random_uuid(),
  name text not null check (char_length(btrim(name)) between 1 and 200),
  description text check (description is null or char_length(description) <= 2000),
  -- Free text such as "12–15 Nov 2026" or "by 31 Oct 2026" (drawn as text, not a date range).
  dates_text text check (dates_text is null or char_length(dates_text) <= 100),
  -- Nullable so that deleting a profile keeps working, like every other profile reference.
  owner_id uuid references public.profiles (id) on delete set null,
  color text not null check (color ~ '^#[0-9a-f]{6}$'),
  created_by uuid references public.profiles (id) on delete set null,
  version bigint not null default 1,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create index task_lists_created_idx on public.task_lists (created_at, id);
create index task_lists_owner_idx on public.task_lists (owner_id);

create trigger task_lists_updated_at before update on public.task_lists
  for each row execute function public.set_updated_at();

alter table public.tasks
  add column if not exists due_time time,
  add column if not exists note text,
  add column if not exists in_progress boolean not null default false,
  add column if not exists list_id uuid references public.task_lists (id) on delete restrict,
  add column if not exists link_lead_id uuid references public.leads (id) on delete set null,
  add column if not exists link_project_id uuid,
  add column if not exists link_client_id uuid;

alter table public.tasks drop constraint if exists tasks_note_length_check;
alter table public.tasks add constraint tasks_note_length_check
  check (note is null or char_length(note) <= 5000);

-- A time of day only makes sense on a dated task.
alter table public.tasks drop constraint if exists tasks_due_time_needs_due_at_check;
alter table public.tasks add constraint tasks_due_time_needs_due_at_check
  check (due_time is null or due_at is not null);

-- "In progress" is a state of an open task only.
alter table public.tasks drop constraint if exists tasks_in_progress_open_check;
alter table public.tasks add constraint tasks_in_progress_open_check
  check (not in_progress or status = 'open');

-- One link at most: a list, or a lead, or a project, or a client.
alter table public.tasks drop constraint if exists tasks_single_link_check;
alter table public.tasks add constraint tasks_single_link_check
  check (num_nonnulls(list_id, link_lead_id, link_project_id, link_client_id) <= 1);

create index if not exists tasks_list_idx on public.tasks (list_id) where list_id is not null;
create index if not exists tasks_link_lead_idx on public.tasks (link_lead_id) where link_lead_id is not null;

-- RLS: task lists are visible to every signed-in CRM user (owner decision on the Tasks boards:
-- "Task lists are visible to everyone"). Writes go through the API only.
alter table public.task_lists enable row level security;

create policy task_lists_select on public.task_lists
  for select to authenticated
  using (true);

-- The API authenticates users and checks permissions before every query. Its database role is
-- not `authenticated`, so (as for manual tasks in 20260908120000) it gets an explicit policy.
create policy task_lists_api_runtime on public.task_lists
  for all to kolss_api using (true) with check (true);

grant select, insert, update on public.task_lists to kolss_api;
-- public.tasks already grants select, insert, update to kolss_api (20260908120000); the new
-- columns are covered by that table-level grant and tasks_api_runtime keeps limiting the API to
-- standalone manual rows.

commit;
