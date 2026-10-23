-- CRM v2 (CL1): clients. A client is the contact that links leads and projects. Its code is the
-- code of its first lead (leads.reference_id); contact fields live on the client.
--
-- Additive for v1: new tables, new nullable columns leads.client_id / projects.client_id, no
-- existing column or value changes. The backfill leaves leads.updated_at, version and markers
-- untouched (same technique as 20260924130000_lead_channel.sql). Idempotent: only leads without
-- a client are processed.
--
-- Owner decisions (2026-09-29, Q-CL5…Q-CL7): a lead attaches to the existing client of the same
-- office by phone, then by email, silently; otherwise a new client is created. Done by a trigger
-- so every insert path (CRM Create lead, website forms, Meta ingestion, sheet import) behaves
-- the same without code changes.

begin;

-- Matching keys -------------------------------------------------------------------------------
-- Phone: the last 9 digits of the number (a Polish or Ukrainian national number), so
-- "+48 601 334 812", "601334812" and "0048601334812" match. Fewer than 9 digits never match.
create or replace function public.client_phone_key(p_phone text)
returns text
language sql
immutable
parallel safe
set search_path = ''
as $$
  select case
    when length(d.digits) >= 9 then right(d.digits, 9)
    else null
  end
  from (select regexp_replace(coalesce(p_phone, ''), '\D', '', 'g') as digits) d
$$;

create or replace function public.client_email_key(p_email text)
returns text
language sql
immutable
parallel safe
set search_path = ''
as $$
  select case
    when position('@' in btrim(coalesce(p_email, ''))) > 1 then lower(btrim(p_email))
    else null
  end
$$;

-- Clients -------------------------------------------------------------------------------------
create table if not exists public.clients (
  id uuid primary key default gen_random_uuid(),
  office_id uuid not null references public.offices (id) on delete restrict,
  code text not null,
  name text not null default '',
  phone text,
  second_phone text,
  email text,
  address text,
  language text,
  about text,
  phone_key text generated always as (public.client_phone_key(phone)) stored,
  second_phone_key text generated always as (public.client_phone_key(second_phone)) stored,
  email_key text generated always as (public.client_email_key(email)) stored,
  version bigint not null default 1,
  merged_into_id uuid references public.clients (id) on delete restrict,
  merged_at timestamptz,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  constraint clients_code_format check (code ~ '^[kw][0-9]{4,}$'),
  constraint clients_code_unique unique (code),
  constraint clients_language_check check (language is null or language in ('pl', 'uk', 'en', 'ru')),
  constraint clients_merged_check check ((merged_into_id is null) = (merged_at is null)),
  constraint clients_merged_not_self check (merged_into_id is null or merged_into_id <> id)
);

create index if not exists clients_office_created_idx
  on public.clients (office_id, created_at desc, id desc) where merged_into_id is null;
create index if not exists clients_phone_key_idx
  on public.clients (office_id, phone_key) where merged_into_id is null and phone_key is not null;
create index if not exists clients_second_phone_key_idx
  on public.clients (office_id, second_phone_key) where merged_into_id is null and second_phone_key is not null;
create index if not exists clients_email_key_idx
  on public.clients (office_id, email_key) where merged_into_id is null and email_key is not null;

drop trigger if exists clients_updated_at on public.clients;
create trigger clients_updated_at before update on public.clients
  for each row execute function public.set_updated_at();

-- Client timeline: contact edits and merges.
create table if not exists public.client_events (
  id uuid primary key default gen_random_uuid(),
  client_id uuid not null references public.clients (id) on delete cascade,
  actor_id uuid references public.profiles (id) on delete set null,
  event_type text not null check (event_type in ('updated', 'merged')),
  old_value jsonb,
  new_value jsonb,
  created_at timestamptz not null default now()
);

create index if not exists client_events_client_idx
  on public.client_events (client_id, created_at desc, id desc);

-- Links ---------------------------------------------------------------------------------------
alter table public.leads
  add column if not exists client_id uuid references public.clients (id) on delete restrict;
alter table public.projects
  add column if not exists client_id uuid references public.clients (id) on delete restrict;

create index if not exists leads_client_id_idx on public.leads (client_id) where client_id is not null;
create index if not exists projects_client_id_idx on public.projects (client_id) where client_id is not null;

-- Attach rule ---------------------------------------------------------------------------------
-- One rule for the backfill and for new leads. Advisory locks serialise two simultaneous leads
-- with the same phone / email so they cannot both create a client (phone lock first, then email,
-- always in that order).
create or replace function private.resolve_client_for_lead(
  p_office_id uuid,
  p_code text,
  p_name text,
  p_phone text,
  p_email text,
  p_address text,
  p_created_at timestamptz
)
returns uuid
language plpgsql
security definer
set search_path = ''
as $$
declare
  v_phone_key text := public.client_phone_key(p_phone);
  v_email_key text := public.client_email_key(p_email);
  v_id uuid;
begin
  if v_phone_key is not null then
    perform pg_advisory_xact_lock(hashtextextended('client-phone:' || p_office_id::text || ':' || v_phone_key, 0));
    select c.id into v_id
    from public.clients c
    where c.office_id = p_office_id
      and c.merged_into_id is null
      and (c.phone_key = v_phone_key or c.second_phone_key = v_phone_key)
    order by c.created_at, c.id
    limit 1;
  end if;

  if v_email_key is not null then
    perform pg_advisory_xact_lock(hashtextextended('client-email:' || p_office_id::text || ':' || v_email_key, 0));
    if v_id is null then
      select c.id into v_id
      from public.clients c
      where c.office_id = p_office_id
        and c.merged_into_id is null
        and c.email_key = v_email_key
      order by c.created_at, c.id
      limit 1;
    end if;
  end if;

  if v_id is null then
    insert into public.clients (office_id, code, name, phone, email, address, created_at)
    values (
      p_office_id,
      p_code,
      coalesce(btrim(p_name), ''),
      nullif(btrim(coalesce(p_phone, '')), ''),
      nullif(btrim(coalesce(p_email, '')), ''),
      nullif(btrim(coalesce(p_address, '')), ''),
      coalesce(p_created_at, now())
    )
    returning id into v_id;
  end if;

  return v_id;
end
$$;

create or replace function private.attach_lead_to_client()
returns trigger
language plpgsql
security definer
set search_path = ''
as $$
begin
  if new.client_id is not null then
    return new;
  end if;
  -- `insert … on conflict do nothing` (manual create replays, site forms, Meta upserts) still
  -- fires this trigger before the conflict is found: do not create a client for a skipped row.
  if exists (
    select 1 from public.leads l
    where l.source_system = new.source_system and l.external_lead_id = new.external_lead_id
  ) then
    return new;
  end if;
  new.client_id := private.resolve_client_for_lead(
    new.office_id, new.reference_id, new.name, new.phone, new.email, new.city_region, new.created_at
  );
  return new;
end
$$;

create or replace function private.attach_project_to_client()
returns trigger
language plpgsql
security definer
set search_path = ''
as $$
begin
  if new.client_id is null then
    select l.client_id into new.client_id from public.leads l where l.id = new.lead_id;
  end if;
  return new;
end
$$;

revoke all on function private.resolve_client_for_lead(uuid, text, text, text, text, text, timestamptz) from public;
revoke all on function private.attach_lead_to_client() from public;
revoke all on function private.attach_project_to_client() from public;

-- Backfill ------------------------------------------------------------------------------------
-- Oldest lead first, so the client code is the code of the client's first lead and the result is
-- the same as if every lead had arrived through the trigger.
alter table public.leads disable trigger leads_updated_at;

do $$
declare
  r record;
begin
  for r in
    select l.id, l.office_id, l.reference_id, l.name, l.phone, l.email, l.city_region, l.created_at
    from public.leads l
    where l.client_id is null
    order by l.created_at, l.id
  loop
    update public.leads
    set client_id = private.resolve_client_for_lead(
      r.office_id, r.reference_id, r.name, r.phone, r.email, r.city_region, r.created_at
    )
    where id = r.id;
  end loop;
end
$$;

alter table public.leads enable trigger leads_updated_at;

update public.projects p
set client_id = l.client_id
from public.leads l
where l.id = p.lead_id and p.client_id is null and l.client_id is not null;

-- Triggers last, so the backfill above does not run them twice. `leads_v2_client_attach` sorts
-- after leads_default_channel and leads_reference_id_guard (before-insert triggers fire by name),
-- so reference_id is already assigned when it runs.
drop trigger if exists leads_v2_client_attach on public.leads;
create trigger leads_v2_client_attach
before insert on public.leads
for each row execute function private.attach_lead_to_client();

drop trigger if exists projects_v2_client_attach on public.projects;
create trigger projects_v2_client_attach
before insert on public.projects
for each row execute function private.attach_project_to_client();

-- RLS and grants ------------------------------------------------------------------------------
alter table public.clients enable row level security;
alter table public.client_events enable row level security;

drop policy if exists clients_select on public.clients;
create policy clients_select on public.clients
  for select to authenticated
  using (private.can_access_office(office_id));

drop policy if exists client_events_select on public.client_events;
create policy client_events_select on public.client_events
  for select to authenticated
  using (exists (
    select 1 from public.clients c
    where c.id = client_id and private.can_access_office(c.office_id)
  ));

-- The API authenticates users and checks office access before every query. Its database role is
-- not `authenticated`, so it gets explicit policies (same as projects, 20260929120000).
drop policy if exists clients_api_runtime on public.clients;
create policy clients_api_runtime on public.clients
  for all to kolss_api using (true) with check (true);
drop policy if exists client_events_api_runtime on public.client_events;
create policy client_events_api_runtime on public.client_events
  for all to kolss_api using (true) with check (true);

grant select, insert, update on public.clients to kolss_api;
grant select, insert on public.client_events to kolss_api;
-- 20260723140000 revokes execute on new public functions from public; the generated key columns
-- and the match queries call these as the API role.
grant execute on function public.client_phone_key(text) to kolss_api, authenticated;
grant execute on function public.client_email_key(text) to kolss_api, authenticated;

commit;
