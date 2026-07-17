-- Independent CRM call/client statuses. Legacy lead_status/workflow_status stay untouched.

begin;

alter table public.leads
  add column if not exists call_status text,
  add column if not exists call_status_changed_at timestamptz,
  add column if not exists client_status text,
  add column if not exists client_status_changed_at timestamptz;

alter table public.leads
  drop constraint if exists leads_call_status_check,
  add constraint leads_call_status_check check (
    call_status is null or call_status in ('reached', 'no_answer', 'callback_requested')
  ),
  drop constraint if exists leads_client_status_check,
  add constraint leads_client_status_check check (
    client_status in (
      'new_lead',
      'showroom_invited',
      'calculation_in_progress',
      'thinking',
      'closed_lost',
      'contract_signed'
    )
  );

alter table public.lead_events
  add column if not exists event_category text,
  add column if not exists status_code text;

alter table public.lead_events
  drop constraint if exists lead_events_event_category_check,
  add constraint lead_events_event_category_check check (
    event_category is null or event_category in ('call_status', 'client_status', 'comment', 'system')
  );

-- Backfill the current client snapshot without changing legacy columns or event rows.
update public.leads
set
  client_status = case workflow_status
    when 'new' then 'new_lead'
    when 'taken' then 'new_lead'
    when 'callback_required' then 'new_lead'
    when 'first_call_done' then 'new_lead'
    when 'in_work' then 'new_lead'
    when 'contacted' then 'new_lead'
    when 'visit_scheduled' then 'showroom_invited'
    when 'visit_rescheduled' then 'showroom_invited'
    when 'visit_completed' then 'showroom_invited'
    when 'showroom_scheduled' then 'showroom_invited'
    when 'showroom_no_show' then 'showroom_invited'
    when 'showroom_visited' then 'showroom_invited'
    when 'contract_planned' then 'showroom_invited'
    when 'thinking' then 'thinking'
    when 'closed' then 'closed_lost'
    when 'bad_lead' then 'closed_lost'
    when 'successful' then 'contract_signed'
    when 'contract_signed' then 'contract_signed'
    when 'prepayment_received' then 'contract_signed'
    when 'in_production' then 'contract_signed'
    when 'postpayment_received' then 'contract_signed'
    when 'installed' then 'contract_signed'
    when 'warranty' then 'contract_signed'
    else 'new_lead'
  end,
  client_status_changed_at = coalesce(workflow_status_changed_at, created_at)
where client_status is null;

-- The current call snapshot comes from the latest historical contact attempt.
with latest_attempt as (
  select distinct on (a.lead_id)
    a.lead_id,
    a.result,
    a.created_at
  from public.lead_contact_attempts a
  order by a.lead_id, a.created_at desc
), mapped as (
  select
    lead_id,
    case result
      when 'reached' then 'reached'
      when 'no_answer' then 'no_answer'
      when 'cannot_talk' then 'callback_requested'
      when 'bad_lead' then 'reached'
      else null
    end as call_status,
    created_at
  from latest_attempt
)
update public.leads l
set
  call_status = mapped.call_status,
  call_status_changed_at = mapped.created_at
from mapped
where mapped.lead_id = l.id
  and l.call_status is null
  and mapped.call_status is not null;

update public.leads
set
  call_status = 'no_answer',
  call_status_changed_at = coalesce(workflow_status_changed_at, updated_at, created_at)
where call_status is null and workflow_status = 'callback_required';

update public.leads
set
  call_status = 'reached',
  call_status_changed_at = coalesce(workflow_status_changed_at, updated_at, created_at)
where call_status is null and workflow_status = 'bad_lead';

alter table public.leads
  alter column client_status set default 'new_lead',
  alter column client_status set not null,
  alter column client_status_changed_at set default now(),
  alter column client_status_changed_at set not null;

create index if not exists leads_call_status_idx on public.leads (call_status);
create index if not exists leads_client_status_idx on public.leads (client_status);
create index if not exists leads_office_client_status_created_idx
  on public.leads (office_id, client_status, created_at desc);
create index if not exists lead_events_comment_timeline_idx
  on public.lead_events (lead_id, created_at desc)
  where comment is not null and btrim(comment) <> '';

insert into public.loss_reasons (code, label_uk, label_pl) values
  ('invalid', 'Невалідна заявка', 'Nieprawidłowe zgłoszenie'),
  ('other', 'Інше', 'Inne')
on conflict (code) do update
set label_uk = excluded.label_uk, label_pl = excluded.label_pl;

alter table public.lead_contracts
  add column if not exists contract_number text,
  add column if not exists amount numeric(12, 2),
  add column if not exists currency text;

alter table public.lead_contracts
  drop constraint if exists lead_contracts_amount_check,
  add constraint lead_contracts_amount_check check (amount is null or amount > 0),
  drop constraint if exists lead_contracts_currency_check,
  add constraint lead_contracts_currency_check check (
    currency is null or currency in ('UAH', 'USD', 'EUR', 'PLN')
  );

commit;
