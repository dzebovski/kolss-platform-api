-- KOLSS CRM initial schema

create extension if not exists "pgcrypto";

-- Offices
create table public.offices (
  id uuid primary key default gen_random_uuid(),
  code text not null unique,
  name_uk text not null,
  name_pl text not null,
  is_active boolean not null default true,
  created_at timestamptz not null default now()
);

-- Pipeline stages
create table public.pipeline_stages (
  code text primary key,
  label_uk text not null,
  label_pl text not null,
  sort_order int not null,
  is_terminal boolean not null default false
);

-- User roles
create type public.user_role as enum (
  'super_admin',
  'office_admin',
  'office_member'
);

create table public.profiles (
  id uuid primary key references auth.users (id) on delete cascade,
  role public.user_role not null default 'office_member',
  display_name text,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create table public.user_office_memberships (
  user_id uuid not null references public.profiles (id) on delete cascade,
  office_id uuid not null references public.offices (id) on delete cascade,
  primary key (user_id, office_id)
);

-- Leads
create table public.leads (
  id uuid primary key default gen_random_uuid(),
  office_id uuid not null references public.offices (id),
  source_system text not null default 'meta_lead_ads',
  external_lead_id text not null,
  crm_status text not null default 'new' references public.pipeline_stages (code),
  crm_status_changed_at timestamptz not null default now(),
  assigned_to uuid references public.profiles (id) on delete set null,
  name text,
  phone text,
  email text,
  product_interest text,
  order_comment text,
  city_region text,
  project_stage_source text,
  stage_comment text,
  source_created_at timestamptz,
  ad_id text,
  ad_name text,
  campaign_id text,
  campaign_name text,
  form_id text,
  form_name text,
  platform text,
  is_organic text,
  raw_payload jsonb not null default '{}'::jsonb,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  unique (source_system, external_lead_id)
);

create index leads_office_id_idx on public.leads (office_id);
create index leads_crm_status_idx on public.leads (crm_status);
create index leads_created_at_idx on public.leads (created_at desc);

-- Import
create table public.lead_import_sources (
  id uuid primary key default gen_random_uuid(),
  office_id uuid not null references public.offices (id),
  name text not null,
  spreadsheet_id text not null,
  sheet_name text not null default 'Sheet1',
  header_row int not null default 1,
  is_enabled boolean not null default true,
  column_map jsonb not null default '{}'::jsonb,
  last_imported_at timestamptz,
  created_at timestamptz not null default now()
);

create table public.lead_import_runs (
  id uuid primary key default gen_random_uuid(),
  source_id uuid not null references public.lead_import_sources (id) on delete cascade,
  status text not null check (status in ('running', 'success', 'failed')),
  rows_processed int not null default 0,
  rows_created int not null default 0,
  rows_updated int not null default 0,
  rows_skipped int not null default 0,
  error_message text,
  started_at timestamptz not null default now(),
  finished_at timestamptz
);

-- Notifications outbox
create type public.notification_channel as enum ('telegram', 'slack');
create type public.notification_status as enum ('pending', 'sent', 'failed');

create table public.lead_notifications (
  id uuid primary key default gen_random_uuid(),
  lead_id uuid not null references public.leads (id) on delete cascade,
  channel public.notification_channel not null,
  status public.notification_status not null default 'pending',
  payload jsonb not null default '{}'::jsonb,
  attempts int not null default 0,
  last_error text,
  sent_at timestamptz,
  created_at timestamptz not null default now(),
  unique (lead_id, channel)
);

-- Audit events
create table public.lead_events (
  id uuid primary key default gen_random_uuid(),
  lead_id uuid not null references public.leads (id) on delete cascade,
  actor_id uuid references public.profiles (id) on delete set null,
  event_type text not null,
  old_value jsonb,
  new_value jsonb,
  created_at timestamptz not null default now()
);

create index lead_events_lead_id_idx on public.lead_events (lead_id, created_at desc);

-- Comments (free comments per pipeline stage)
create table public.lead_comments (
  id uuid primary key default gen_random_uuid(),
  lead_id uuid not null references public.leads (id) on delete cascade,
  author_id uuid not null references public.profiles (id) on delete cascade,
  pipeline_stage text not null references public.pipeline_stages (code),
  body text not null,
  created_at timestamptz not null default now()
);

create index lead_comments_lead_id_idx on public.lead_comments (lead_id, created_at desc);

-- Helpers for RLS
create or replace function public.is_super_admin()
returns boolean
language sql
stable
security definer
set search_path = public
as $$
  select exists (
    select 1 from public.profiles
    where id = auth.uid() and role = 'super_admin'
  );
$$;

create or replace function public.user_office_ids()
returns setof uuid
language sql
stable
security definer
set search_path = public
as $$
  select office_id from public.user_office_memberships where user_id = auth.uid();
$$;

create or replace function public.can_access_office(target_office_id uuid)
returns boolean
language sql
stable
security definer
set search_path = public
as $$
  select public.is_super_admin()
    or target_office_id in (select public.user_office_ids());
$$;

-- Auto-create profile on signup
create or replace function public.handle_new_user()
returns trigger
language plpgsql
security definer
set search_path = public
as $$
begin
  insert into public.profiles (id, display_name)
  values (new.id, coalesce(new.raw_user_meta_data->>'display_name', new.email));
  return new;
end;
$$;

create trigger on_auth_user_created
  after insert on auth.users
  for each row execute function public.handle_new_user();

-- Updated_at trigger
create or replace function public.set_updated_at()
returns trigger
language plpgsql
as $$
begin
  new.updated_at = now();
  return new;
end;
$$;

create trigger leads_updated_at before update on public.leads
  for each row execute function public.set_updated_at();

create trigger profiles_updated_at before update on public.profiles
  for each row execute function public.set_updated_at();

-- RLS
alter table public.offices enable row level security;
alter table public.pipeline_stages enable row level security;
alter table public.profiles enable row level security;
alter table public.user_office_memberships enable row level security;
alter table public.leads enable row level security;
alter table public.lead_import_sources enable row level security;
alter table public.lead_import_runs enable row level security;
alter table public.lead_notifications enable row level security;
alter table public.lead_events enable row level security;
alter table public.lead_comments enable row level security;

-- Offices: authenticated users can read
create policy offices_select on public.offices for select to authenticated using (true);

-- Pipeline stages: read all
create policy pipeline_stages_select on public.pipeline_stages for select to authenticated using (true);

-- Profiles
create policy profiles_select on public.profiles for select to authenticated
  using (id = auth.uid() or public.is_super_admin());
create policy profiles_update_own on public.profiles for update to authenticated
  using (id = auth.uid()) with check (id = auth.uid());

-- Memberships
create policy memberships_select on public.user_office_memberships for select to authenticated
  using (user_id = auth.uid() or public.is_super_admin());

-- Leads
create policy leads_select on public.leads for select to authenticated
  using (public.can_access_office(office_id));
create policy leads_insert on public.leads for insert to authenticated
  with check (public.can_access_office(office_id));
create policy leads_update on public.leads for update to authenticated
  using (public.can_access_office(office_id))
  with check (public.can_access_office(office_id));

-- Import sources (read for office users; super admin all)
create policy import_sources_select on public.lead_import_sources for select to authenticated
  using (public.can_access_office(office_id));

-- Import runs (read via source office)
create policy import_runs_select on public.lead_import_runs for select to authenticated
  using (
    exists (
      select 1 from public.lead_import_sources s
      where s.id = source_id and public.can_access_office(s.office_id)
    )
  );

-- Notifications
create policy lead_notifications_select on public.lead_notifications for select to authenticated
  using (
    exists (select 1 from public.leads l where l.id = lead_id and public.can_access_office(l.office_id))
  );

-- Events
create policy lead_events_select on public.lead_events for select to authenticated
  using (
    exists (select 1 from public.leads l where l.id = lead_id and public.can_access_office(l.office_id))
  );
create policy lead_events_insert on public.lead_events for insert to authenticated
  with check (
    exists (select 1 from public.leads l where l.id = lead_id and public.can_access_office(l.office_id))
  );

-- Comments
create policy lead_comments_select on public.lead_comments for select to authenticated
  using (
    exists (select 1 from public.leads l where l.id = lead_id and public.can_access_office(l.office_id))
  );
create policy lead_comments_insert on public.lead_comments for insert to authenticated
  with check (
    author_id = auth.uid()
    and exists (select 1 from public.leads l where l.id = lead_id and public.can_access_office(l.office_id))
  );

-- Seed offices
insert into public.offices (code, name_uk, name_pl, is_active) values
  ('kyiv', 'Київ', 'Kijów', true),
  ('warsaw', 'Варшава', 'Warszawa', true),
  ('london', 'Лондон', 'Londyn', false);

-- Seed pipeline stages
insert into public.pipeline_stages (code, label_uk, label_pl, sort_order, is_terminal) values
  ('new', 'Новий', 'Nowy', 0, false),
  ('contact', 'Контакт', 'Kontakt', 10, false),
  ('estimate', 'Прорахунок', 'Wycena', 20, false),
  ('measurement', 'Замір', 'Pomiar', 30, false),
  ('design', 'Дизайн', 'Projekt', 40, false),
  ('contract', 'Договір', 'Umowa', 50, false),
  ('production', 'Виробництво', 'Produkcja', 60, false),
  ('delivery', 'Доставка', 'Dostawa', 70, false),
  ('installation', 'Монтаж', 'Montaż', 80, false),
  ('activated_warranty', 'Гарантія 2 роки', 'Gwarancja 2 lata', 90, true),
  ('canceled_lost', 'Скасовано / Програно', 'Anulowano / Przegrane', 100, true);

-- Placeholder import sources (replace spreadsheet_id in Supabase dashboard)
insert into public.lead_import_sources (office_id, name, spreadsheet_id, sheet_name, is_enabled)
select o.id, 'Meta Lead Ads — ' || o.name_uk, 'REPLACE_WITH_SPREADSHEET_ID', 'Sheet1', false
from public.offices o
where o.code in ('kyiv', 'warsaw');
