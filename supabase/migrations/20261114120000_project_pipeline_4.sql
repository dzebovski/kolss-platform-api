-- CRM v2 (P8): project pipeline 4 + project card v4 (owner decisions of 2026-11-14).
--
-- * Stages (in order): preliminary_estimate, measurement_design, factory_approval,
--   contract_signing, production_delivery, installation, plus the finals completed and cancelled.
--   Pipeline 3 stages are mapped (also in status_before_cancel):
--     estimate, showroom_meeting                         -> preliminary_estimate
--     intro_contract, design, presentation               -> measurement_design
--     factory_review                                     -> factory_approval
--     main_contract                                      -> contract_signing
--     production                                         -> production_delivery
--   CRM v1 never reads projects.status (checked 2026-10-23); only the v2 project API depends on it.
-- * Contracts are no longer an entity: signed contracts are uploaded as project documents.
--   Payments become standalone records (amount, currency, date, basis, note, optional file).
--   Existing payments keep their contract's currency with basis = contract. Contract rows are
--   dropped (prod had none on 2026-11-14); their files stay as documents.
-- * The estimate is one total (amount + currency) on the project; the four lines are summed into
--   it when they share one currency (prod had no lines on 2026-11-14).
-- * Office intro-contract defaults are dropped (no intro contract any more).
-- * New timeline events: finished, file_added, file_deleted. Old types stay valid for history.
--
-- v1 impact: none (v1 does not read projects, project_* tables or the dropped office columns).

begin;

-- Status vocabulary -----------------------------------------------------------------------------
alter table public.projects
  drop constraint if exists projects_status_check,
  drop constraint if exists projects_status_before_cancel_check;

update public.projects
set status = case status
      when 'estimate' then 'preliminary_estimate'
      when 'showroom_meeting' then 'preliminary_estimate'
      when 'intro_contract' then 'measurement_design'
      when 'design' then 'measurement_design'
      when 'presentation' then 'measurement_design'
      when 'factory_review' then 'factory_approval'
      when 'main_contract' then 'contract_signing'
      when 'production' then 'production_delivery'
      else status
    end,
    status_before_cancel = case status_before_cancel
      when 'estimate' then 'preliminary_estimate'
      when 'showroom_meeting' then 'preliminary_estimate'
      when 'intro_contract' then 'measurement_design'
      when 'design' then 'measurement_design'
      when 'presentation' then 'measurement_design'
      when 'factory_review' then 'factory_approval'
      when 'main_contract' then 'contract_signing'
      when 'production' then 'production_delivery'
      else status_before_cancel
    end;

alter table public.projects
  add constraint projects_status_check check (
    status in (
      'preliminary_estimate', 'measurement_design', 'factory_approval', 'contract_signing',
      'production_delivery', 'installation', 'completed', 'cancelled'
    )
  ),
  add constraint projects_status_before_cancel_check check (
    status_before_cancel is null or status_before_cancel in (
      'preliminary_estimate', 'measurement_design', 'factory_approval', 'contract_signing',
      'production_delivery', 'installation', 'completed'
    )
  );

-- Estimate: one total ---------------------------------------------------------------------------
alter table public.projects
  add column if not exists estimate_amount numeric(14, 2),
  add column if not exists estimate_currency text;

update public.projects p
set estimate_amount = t.total, estimate_currency = t.currency
from (
  select project_id, min(currency) as currency, sum(amount) as total
  from public.project_estimate_lines
  group by project_id
  having count(distinct currency) = 1
) t
where t.project_id = p.id;

alter table public.projects
  drop constraint if exists projects_estimate_check,
  add constraint projects_estimate_check check (
    (estimate_amount is null and estimate_currency is null)
    or (estimate_amount > 0 and estimate_currency in ('PLN', 'UAH', 'EUR', 'USD'))
  );

drop table if exists public.project_estimate_lines;

-- Payments: standalone --------------------------------------------------------------------------
alter table public.project_payments
  add column if not exists currency text,
  add column if not exists basis text;

update public.project_payments pp
set currency = c.currency, basis = 'contract'
from public.project_contracts c
where c.id = pp.contract_id and pp.currency is null;

update public.project_payments set basis = 'other' where basis is null;
update public.project_payments set currency = 'PLN' where currency is null;

alter table public.project_payments
  alter column currency set not null,
  alter column basis set not null,
  drop constraint if exists project_payments_currency_check,
  add constraint project_payments_currency_check check (currency in ('PLN', 'UAH', 'EUR', 'USD')),
  drop constraint if exists project_payments_basis_check,
  add constraint project_payments_basis_check check (basis in ('receipt', 'invoice', 'contract', 'other'));

drop index if exists public.project_payments_contract_id_idx;
alter table public.project_payments drop column if exists contract_id;

drop table if exists public.project_contracts;

create index if not exists project_payments_project_id_idx on public.project_payments (project_id, paid_on);

-- Files: documents and payment receipts ---------------------------------------------------------
alter table public.project_files drop constraint if exists project_files_kind_check;
update public.project_files set kind = 'document' where kind = 'contract';
alter table public.project_files
  add constraint project_files_kind_check check (kind in ('document', 'receipt'));

grant delete on public.project_files to kolss_api;

-- Offices: no intro contract default ------------------------------------------------------------
alter table public.offices
  drop constraint if exists offices_intro_contract_default_check,
  drop column if exists intro_contract_default_amount,
  drop column if exists intro_contract_default_currency;

-- Timeline event types --------------------------------------------------------------------------
alter table public.project_events
  drop constraint if exists project_events_event_type_check,
  add constraint project_events_event_type_check check (
    event_type in (
      'created', 'status_changed', 'status_updated', 'cancelled', 'restored', 'finished',
      'estimate_updated', 'contract_added', 'contract_updated', 'contract_deleted',
      'payment_added', 'payment_updated', 'payment_deleted', 'file_added', 'file_deleted',
      'details_updated', 'lead_info_updated', 'comment', 'measurement_visit'
    )
  );

commit;
