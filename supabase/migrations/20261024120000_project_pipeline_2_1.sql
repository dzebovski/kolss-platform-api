-- CRM v2 (P1b): project pipeline 2.1 (owner decisions of 2026-10-23, Q-P8…Q-P17).
--
-- * The status vocabulary becomes the 11-stage pipeline + cancelled:
--   estimate, intro_contract, measure, design, main_contract, prepayment, factory, production,
--   final_payment, installation, completed, cancelled (default `estimate`).
--   Existing rows: none / express -> estimate, contract -> main_contract, the rest keep their code
--   (also in status_before_cancel). CRM v1 never reads projects.status (checked 2026-10-23: no v1
--   screen, no API v1 handler, no RPC caller), so nothing outside the v2 project API depends on it.
-- * Estimate: up to 4 optional lines per project (kitchen, appliances, installation, countertop).
-- * Contracts get a `kind` (intro | main), at most one of each per project. Payments belong to a
--   contract. Existing contracts become `main`; existing payments attach to the project's contract.
-- * projects.designer_id, per-office intro contract default amount (config), and the new timeline
--   event types.
--
-- Additive for v1 (new tables / columns; only the v2-only status column values change).

begin;

-- Status vocabulary -----------------------------------------------------------------------------
alter table public.projects
  drop constraint if exists projects_status_check,
  drop constraint if exists projects_status_before_cancel_check;

alter table public.projects alter column status set default 'estimate';

update public.projects
set status = case status
      when 'none' then 'estimate'
      when 'express' then 'estimate'
      when 'contract' then 'main_contract'
      else status
    end,
    status_before_cancel = case status_before_cancel
      when 'none' then 'estimate'
      when 'express' then 'estimate'
      when 'contract' then 'main_contract'
      else status_before_cancel
    end
where status in ('none', 'express', 'contract')
   or status_before_cancel in ('none', 'express', 'contract');

alter table public.projects
  add constraint projects_status_check check (
    status in (
      'estimate', 'intro_contract', 'measure', 'design', 'main_contract', 'prepayment',
      'factory', 'production', 'final_payment', 'installation', 'completed', 'cancelled'
    )
  ),
  add constraint projects_status_before_cancel_check check (
    status_before_cancel is null or status_before_cancel in (
      'estimate', 'intro_contract', 'measure', 'design', 'main_contract', 'prepayment',
      'factory', 'production', 'final_payment', 'installation', 'completed'
    )
  );

-- Designer (optional, Q-P10) --------------------------------------------------------------------
alter table public.projects
  add column if not exists designer_id uuid references public.profiles (id) on delete set null;

-- Intro contract default per office (config, Q-P15) ---------------------------------------------
-- Legionowo (office code `warsaw`) 500 PLN. Kyiv is left empty on purpose: no prefill until the
-- owner gives the UAH amount (open question Q-P15b); then set both columns for office `kyiv`.
alter table public.offices
  add column if not exists intro_contract_default_amount numeric(14, 2),
  add column if not exists intro_contract_default_currency text;

alter table public.offices
  drop constraint if exists offices_intro_contract_default_check,
  add constraint offices_intro_contract_default_check check (
    (intro_contract_default_amount is null and intro_contract_default_currency is null)
    or (
      intro_contract_default_amount > 0
      and intro_contract_default_currency in ('PLN', 'UAH', 'EUR', 'USD')
    )
  );

update public.offices
set intro_contract_default_amount = 500, intro_contract_default_currency = 'PLN'
where code = 'warsaw' and intro_contract_default_amount is null;

-- Estimate lines --------------------------------------------------------------------------------
create table if not exists public.project_estimate_lines (
  project_id uuid not null references public.projects (id) on delete cascade,
  line text not null check (line in ('kitchen', 'appliances', 'installation', 'countertop')),
  amount numeric(14, 2) not null check (amount > 0),
  currency text not null check (currency in ('PLN', 'UAH', 'EUR', 'USD')),
  note text check (note is null or char_length(note) <= 300),
  updated_by uuid references public.profiles (id) on delete set null,
  updated_at timestamptz not null default now(),
  primary key (project_id, line)
);

alter table public.project_estimate_lines enable row level security;

drop policy if exists project_estimate_lines_select on public.project_estimate_lines;
create policy project_estimate_lines_select on public.project_estimate_lines
  for select to authenticated
  using (exists (
    select 1 from public.projects p
    where p.id = project_id and private.can_access_office(p.office_id)
  ));

drop policy if exists project_estimate_lines_api_runtime on public.project_estimate_lines;
create policy project_estimate_lines_api_runtime on public.project_estimate_lines
  for all to kolss_api using (true) with check (true);

-- Contracts: kind intro | main, one of each per project -----------------------------------------
alter table public.project_contracts
  add column if not exists kind text not null default 'main',
  add column if not exists updated_at timestamptz not null default now();

alter table public.project_contracts
  drop constraint if exists project_contracts_kind_check,
  add constraint project_contracts_kind_check check (kind in ('intro', 'main'));

alter table public.project_contracts drop constraint if exists project_contracts_project_id_key;
create unique index if not exists project_contracts_project_kind_key
  on public.project_contracts (project_id, kind);

-- Payments belong to a contract -----------------------------------------------------------------
alter table public.project_payments
  add column if not exists contract_id uuid references public.project_contracts (id) on delete restrict;

update public.project_payments pp
set contract_id = c.id
from public.project_contracts c
where c.project_id = pp.project_id and pp.contract_id is null;

alter table public.project_payments alter column contract_id set not null;
create index if not exists project_payments_contract_id_idx on public.project_payments (contract_id);

alter table public.project_payments
  add column if not exists updated_at timestamptz not null default now();

-- Timeline event types --------------------------------------------------------------------------
alter table public.project_events
  drop constraint if exists project_events_event_type_check,
  add constraint project_events_event_type_check check (
    event_type in (
      'created', 'status_changed', 'status_updated', 'cancelled', 'restored',
      'estimate_updated', 'contract_added', 'contract_updated', 'contract_deleted',
      'payment_added', 'payment_updated', 'payment_deleted',
      'details_updated', 'lead_info_updated', 'comment', 'measurement_visit'
    )
  );

-- API role: edit / delete of contracts and payments, estimate lines ------------------------------
grant update, delete on public.project_contracts, public.project_payments to kolss_api;
grant select, insert, update, delete on public.project_estimate_lines to kolss_api;

commit;
