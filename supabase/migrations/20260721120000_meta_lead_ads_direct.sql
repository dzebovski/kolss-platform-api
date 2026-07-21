-- Direct Meta Lead Ads ingestion: page configuration, durable webhook inbox,
-- automatic form discovery, reconciliation audit, and Google Sheets shutdown.

create table public.meta_page_connections (
  id uuid primary key default gen_random_uuid(),
  office_id uuid not null unique references public.offices (id),
  page_id text not null unique,
  page_name text,
  ingest_after timestamptz not null,
  token_status text not null default 'unknown'
    check (token_status in ('unknown', 'valid', 'invalid')),
  token_checked_at timestamptz,
  health_status text not null default 'unknown'
    check (health_status in ('unknown', 'healthy', 'unhealthy')),
  consecutive_failures int not null default 0 check (consecutive_failures >= 0),
  last_success_at timestamptz,
  last_reconciled_at timestamptz,
  last_error text,
  last_error_at timestamptz,
  last_alert_key text,
  last_alerted_at timestamptz,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create table public.meta_forms (
  form_id text primary key,
  connection_id uuid not null references public.meta_page_connections (id) on delete cascade,
  name text,
  status text not null default 'UNKNOWN',
  locale text,
  questions jsonb not null default '[]'::jsonb,
  first_seen_at timestamptz not null default now(),
  last_seen_at timestamptz not null default now(),
  last_reconciled_at timestamptz
);

create index meta_forms_connection_status_idx
  on public.meta_forms (connection_id, status, last_seen_at desc);

create table public.meta_lead_events (
  id uuid primary key default gen_random_uuid(),
  leadgen_id text not null unique,
  page_id text not null,
  form_id text,
  ad_id text,
  event_created_at timestamptz,
  status text not null default 'pending'
    check (status in ('pending', 'processing', 'retry', 'processed', 'ignored', 'dead_letter')),
  webhook_payload jsonb not null default '{}'::jsonb,
  attempts int not null default 0 check (attempts >= 0),
  next_attempt_at timestamptz not null default now(),
  claimed_at timestamptz,
  claim_token uuid,
  last_error text,
  alerted_at timestamptz,
  lead_id uuid references public.leads (id) on delete set null,
  received_at timestamptz not null default now(),
  processed_at timestamptz
);

create index meta_lead_events_ready_idx
  on public.meta_lead_events (next_attempt_at, received_at)
  where status in ('pending', 'retry', 'processing');

create index meta_lead_events_page_status_idx
  on public.meta_lead_events (page_id, status, received_at desc);

create table public.meta_sync_runs (
  id uuid primary key default gen_random_uuid(),
  connection_id uuid not null references public.meta_page_connections (id) on delete cascade,
  sync_type text not null check (sync_type in ('forms', 'active_reconcile', 'full_reconcile')),
  status text not null check (status in ('running', 'success', 'failed')),
  range_from timestamptz,
  range_to timestamptz,
  forms_processed int not null default 0,
  leads_seen int not null default 0,
  events_created int not null default 0,
  error_message text,
  started_at timestamptz not null default now(),
  finished_at timestamptz
);

create index meta_sync_runs_connection_started_idx
  on public.meta_sync_runs (connection_id, started_at desc);

alter table public.meta_page_connections enable row level security;
alter table public.meta_forms enable row level security;
alter table public.meta_lead_events enable row level security;
alter table public.meta_sync_runs enable row level security;

grant select, insert, update on public.meta_page_connections to kolss_api;
grant select, insert, update on public.meta_forms to kolss_api;
grant select, insert, update on public.meta_lead_events to kolss_api;
grant select, insert, update on public.meta_sync_runs to kolss_api;

create policy meta_page_connections_api_runtime on public.meta_page_connections
  for all to kolss_api using (true) with check (true);
create policy meta_forms_api_runtime on public.meta_forms
  for all to kolss_api using (true) with check (true);
create policy meta_lead_events_api_runtime on public.meta_lead_events
  for all to kolss_api using (true) with check (true);
create policy meta_sync_runs_api_runtime on public.meta_sync_runs
  for all to kolss_api using (true) with check (true);

update public.lead_import_sources set is_enabled = false where is_enabled = true;
revoke insert, update on public.lead_import_sources, public.lead_import_runs from kolss_api;
