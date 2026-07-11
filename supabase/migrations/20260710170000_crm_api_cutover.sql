-- CRM API cutover: optimistic concurrency, soft archive, idempotency and leased outbox.

alter table public.leads
  add column if not exists version bigint not null default 1,
  add column if not exists archived_at timestamptz,
  add column if not exists archived_by uuid references public.profiles (id) on delete set null;

alter table public.leads
  drop constraint if exists leads_version_positive;

alter table public.leads
  add constraint leads_version_positive check (version > 0);

create index if not exists leads_active_created_idx
  on public.leads (created_at desc, id desc)
  where archived_at is null;

create index if not exists leads_archived_created_idx
  on public.leads (archived_at desc, id desc)
  where archived_at is not null;

create index if not exists leads_office_assigned_created_idx
  on public.leads (office_id, assigned_to, created_at desc, id desc);

alter table public.lead_notifications
  add column if not exists next_attempt_at timestamptz not null default now(),
  add column if not exists claimed_at timestamptz,
  add column if not exists claim_token uuid;

create index if not exists lead_notifications_ready_idx
  on public.lead_notifications (next_attempt_at, created_at)
  where status in ('pending', 'failed');

create index if not exists lead_notifications_stale_claim_idx
  on public.lead_notifications (claimed_at)
  where claimed_at is not null and status in ('pending', 'failed');

create table if not exists public.api_idempotency_keys (
  id uuid primary key default gen_random_uuid(),
  actor_id uuid not null references public.profiles (id) on delete cascade,
  operation text not null,
  idempotency_key text not null,
  request_hash text not null,
  response_status int,
  response_body jsonb,
  created_at timestamptz not null default now(),
  expires_at timestamptz not null default (now() + interval '24 hours'),
  unique (actor_id, operation, idempotency_key)
);

create index if not exists api_idempotency_keys_expires_idx
  on public.api_idempotency_keys (expires_at);

alter table public.api_idempotency_keys enable row level security;

-- Runtime roles are NOLOGIN in migrations. Set LOGIN/password out-of-band and store
-- the credentials only in DigitalOcean encrypted environment variables.
do $$
begin
  if not exists (select 1 from pg_roles where rolname = 'kolss_api') then
    create role kolss_api nologin noinherit;
  end if;
  if not exists (select 1 from pg_roles where rolname = 'kolss_worker') then
    create role kolss_worker nologin noinherit;
  end if;
end
$$;

grant usage on schema public to kolss_api, kolss_worker;
grant usage, select on all sequences in schema public to kolss_api;

grant select, insert, update on public.leads to kolss_api;
grant select on public.offices to kolss_api;
grant select, insert, update on public.profiles to kolss_api;
grant select, insert, delete on public.user_office_memberships to kolss_api;
grant select on public.loss_reasons, public.lead_statuses, public.lead_workflow_statuses to kolss_api;
grant select, insert, update on public.lead_events to kolss_api;
grant select, insert on public.lead_comments, public.lead_contact_attempts to kolss_api;
grant select, insert, update on public.lead_showroom_visits, public.lead_contracts to kolss_api;
grant select on public.lead_attachments to kolss_api;
grant select, insert, update on public.lead_import_sources, public.lead_import_runs to kolss_api;
grant select, insert, update on public.lead_notifications to kolss_api, kolss_worker;
grant select, insert, update, delete on public.api_idempotency_keys to kolss_api;

grant select, update on public.lead_submissions, public.lead_submission_uploads to kolss_worker;
grant select, update on public.lead_attachments to kolss_worker;
