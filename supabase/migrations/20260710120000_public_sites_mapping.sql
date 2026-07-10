-- Expand-compatible public sites mapping for local contact-form MVP.
-- Does not alter legacy CRM source migrations.

create table if not exists public.sites (
  code text primary key,
  office_id uuid not null references public.offices (id),
  locale text not null,
  privacy_policy_version text not null,
  allowed_origins text[] not null default '{}'::text[],
  is_active boolean not null default true,
  created_at timestamptz not null default now()
);

create index if not exists sites_office_id_idx on public.sites (office_id);

alter table public.sites enable row level security;

create policy sites_select_authenticated on public.sites
  for select to authenticated
  using (true);

-- Seed PL → Warsaw, UA → Kyiv with privacy versions used by local Angular forms.
insert into public.sites (code, office_id, locale, privacy_policy_version, allowed_origins, is_active)
select
  'kolss-pl',
  o.id,
  'pl-PL',
  'pl-v1',
  array['http://localhost:4200'],
  true
from public.offices o
where o.code = 'warsaw'
on conflict (code) do update set
  office_id = excluded.office_id,
  locale = excluded.locale,
  privacy_policy_version = excluded.privacy_policy_version,
  allowed_origins = excluded.allowed_origins,
  is_active = excluded.is_active;

insert into public.sites (code, office_id, locale, privacy_policy_version, allowed_origins, is_active)
select
  'kolss-ua',
  o.id,
  'uk-UA',
  'ua-v1',
  array['http://localhost:4201'],
  true
from public.offices o
where o.code = 'kyiv'
on conflict (code) do update set
  office_id = excluded.office_id,
  locale = excluded.locale,
  privacy_policy_version = excluded.privacy_policy_version,
  allowed_origins = excluded.allowed_origins,
  is_active = excluded.is_active;
