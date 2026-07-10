-- Lead-centric workflow: workflow_status, child tables, task extensions

-- Workflow status lookup
create table public.lead_workflow_statuses (
  code text primary key,
  sort_order int not null,
  category text not null default 'active',
  is_terminal boolean not null default false
);

insert into public.lead_workflow_statuses (code, sort_order, category, is_terminal) values
  ('new', 0, 'intake', false),
  ('in_work', 10, 'intake', false),
  ('callback_required', 20, 'intake', false),
  ('contacted', 30, 'sales', false),
  ('showroom_scheduled', 40, 'sales', false),
  ('showroom_visited', 50, 'sales', false),
  ('showroom_no_show', 45, 'sales', false),
  ('contract_planned', 60, 'sales', false),
  ('contract_signed', 70, 'sales', false),
  ('prepayment_received', 80, 'production', false),
  ('in_production', 90, 'production', false),
  ('postpayment_received', 100, 'delivery', false),
  ('installed', 110, 'delivery', false),
  ('warranty', 120, 'delivery', true),
  ('bad_lead', 200, 'terminal', true);

-- Extend leads with workflow fields
alter table public.leads
  add column if not exists workflow_status text,
  add column if not exists workflow_status_changed_at timestamptz,
  add column if not exists source_channel text,
  add column if not exists source_note text,
  add column if not exists next_task_due_at timestamptz,
  add column if not exists next_task_title text,
  add column if not exists production_started_at timestamptz,
  add column if not exists postpayment_received_at timestamptz,
  add column if not exists installed_at timestamptz,
  add column if not exists warranty_started_at timestamptz;

-- Backfill workflow_status from lead_status
update public.leads
set
  workflow_status = case lead_status
    when 'new' then 'new'
    when 'in_progress' then 'in_work'
    when 'converted' then 'in_production'
    when 'failed' then 'bad_lead'
    else 'new'
  end,
  workflow_status_changed_at = coalesce(lead_status_changed_at, created_at)
where workflow_status is null;

alter table public.leads
  alter column workflow_status set default 'new',
  alter column workflow_status set not null;

alter table public.leads
  add constraint leads_workflow_status_fkey
  foreign key (workflow_status) references public.lead_workflow_statuses (code);

create index if not exists leads_workflow_status_idx on public.leads (workflow_status);
create index if not exists leads_office_workflow_created_idx
  on public.leads (office_id, workflow_status, created_at desc);
create index if not exists leads_next_task_due_idx
  on public.leads (next_task_due_at) where next_task_due_at is not null;

-- Contact attempts
create table public.lead_contact_attempts (
  id uuid primary key default gen_random_uuid(),
  lead_id uuid not null references public.leads (id) on delete cascade,
  manager_id uuid not null references public.profiles (id) on delete cascade,
  result text not null check (result in ('reached', 'no_answer', 'cannot_talk', 'bad_lead')),
  comment text not null check (char_length(trim(comment)) > 0),
  created_at timestamptz not null default now()
);

create index lead_contact_attempts_lead_id_idx
  on public.lead_contact_attempts (lead_id, created_at desc);

-- Showroom visits
create table public.lead_showroom_visits (
  id uuid primary key default gen_random_uuid(),
  lead_id uuid not null references public.leads (id) on delete cascade,
  scheduled_at timestamptz not null,
  status text not null default 'scheduled'
    check (status in ('scheduled', 'visited', 'no_show', 'canceled', 'rescheduled')),
  comment text,
  materials text,
  quoted_price_amount numeric(12, 2),
  quoted_price_currency text check (
    quoted_price_currency is null
    or quoted_price_currency in ('PLN', 'EUR', 'USD', 'GBP', 'UAH')
  ),
  created_by uuid not null references public.profiles (id) on delete cascade,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create index lead_showroom_visits_lead_id_idx
  on public.lead_showroom_visits (lead_id, scheduled_at desc);

create trigger lead_showroom_visits_updated_at before update on public.lead_showroom_visits
  for each row execute function public.set_updated_at();

-- Contracts
create table public.lead_contracts (
  id uuid primary key default gen_random_uuid(),
  lead_id uuid not null references public.leads (id) on delete cascade,
  planned_at timestamptz,
  signed_at timestamptz,
  status text not null default 'planned'
    check (status in ('planned', 'signed', 'canceled')),
  comment text,
  created_by uuid not null references public.profiles (id) on delete cascade,
  created_at timestamptz not null default now()
);

create index lead_contracts_lead_id_idx
  on public.lead_contracts (lead_id, created_at desc);

create unique index lead_contracts_one_active_planned_idx
  on public.lead_contracts (lead_id)
  where status = 'planned';

-- Payments
create type public.lead_payment_type as enum ('prepayment', 'postpayment');
create type public.lead_payment_currency as enum ('PLN', 'EUR', 'USD', 'GBP', 'UAH');

create table public.lead_payments (
  id uuid primary key default gen_random_uuid(),
  lead_id uuid not null references public.leads (id) on delete cascade,
  payment_type public.lead_payment_type not null,
  amount numeric(12, 2) not null check (amount > 0),
  currency public.lead_payment_currency not null,
  paid_at timestamptz not null,
  comment text,
  created_by uuid not null references public.profiles (id) on delete cascade,
  created_at timestamptz not null default now()
);

create index lead_payments_lead_id_idx
  on public.lead_payments (lead_id, paid_at desc);

-- Extend lead_events with comment
alter table public.lead_events
  add column if not exists comment text;

-- Extend tasks
create type public.task_type as enum (
  'callback',
  'showroom_no_show_followup',
  'showroom_visit',
  'contract_followup',
  'prepayment_followup',
  'manual'
);

alter table public.tasks
  add column if not exists task_type public.task_type,
  add column if not exists created_by uuid references public.profiles (id) on delete set null;

alter type public.task_source add value if not exists 'auto_callback';
alter type public.task_source add value if not exists 'auto_showroom_no_show';
alter type public.task_source add value if not exists 'auto_showroom_visit';
alter type public.task_source add value if not exists 'auto_contract';
alter type public.task_source add value if not exists 'auto_prepayment';

-- RLS
alter table public.lead_workflow_statuses enable row level security;
alter table public.lead_contact_attempts enable row level security;
alter table public.lead_showroom_visits enable row level security;
alter table public.lead_contracts enable row level security;
alter table public.lead_payments enable row level security;

create policy lead_workflow_statuses_select on public.lead_workflow_statuses
  for select to authenticated using (true);

create policy lead_contact_attempts_select on public.lead_contact_attempts
  for select to authenticated
  using (
    exists (
      select 1 from public.leads l
      where l.id = lead_contact_attempts.lead_id
        and public.can_access_office(l.office_id)
    )
  );

create policy lead_contact_attempts_insert on public.lead_contact_attempts
  for insert to authenticated
  with check (
    manager_id = auth.uid()
    and exists (
      select 1 from public.leads l
      where l.id = lead_contact_attempts.lead_id
        and public.can_access_office(l.office_id)
    )
  );

create policy lead_showroom_visits_select on public.lead_showroom_visits
  for select to authenticated
  using (
    exists (
      select 1 from public.leads l
      where l.id = lead_showroom_visits.lead_id
        and public.can_access_office(l.office_id)
    )
  );

create policy lead_showroom_visits_insert on public.lead_showroom_visits
  for insert to authenticated
  with check (
    created_by = auth.uid()
    and exists (
      select 1 from public.leads l
      where l.id = lead_showroom_visits.lead_id
        and public.can_access_office(l.office_id)
    )
  );

create policy lead_showroom_visits_update on public.lead_showroom_visits
  for update to authenticated
  using (
    exists (
      select 1 from public.leads l
      where l.id = lead_showroom_visits.lead_id
        and public.can_access_office(l.office_id)
    )
  );

create policy lead_contracts_select on public.lead_contracts
  for select to authenticated
  using (
    exists (
      select 1 from public.leads l
      where l.id = lead_contracts.lead_id
        and public.can_access_office(l.office_id)
    )
  );

create policy lead_contracts_insert on public.lead_contracts
  for insert to authenticated
  with check (
    created_by = auth.uid()
    and exists (
      select 1 from public.leads l
      where l.id = lead_contracts.lead_id
        and public.can_access_office(l.office_id)
    )
  );

create policy lead_contracts_update on public.lead_contracts
  for update to authenticated
  using (
    exists (
      select 1 from public.leads l
      where l.id = lead_contracts.lead_id
        and public.can_access_office(l.office_id)
    )
  );

create policy lead_payments_select on public.lead_payments
  for select to authenticated
  using (
    exists (
      select 1 from public.leads l
      where l.id = lead_payments.lead_id
        and public.can_access_office(l.office_id)
    )
  );

create policy lead_payments_insert on public.lead_payments
  for insert to authenticated
  with check (
    created_by = auth.uid()
    and exists (
      select 1 from public.leads l
      where l.id = lead_payments.lead_id
        and public.can_access_office(l.office_id)
    )
  );

-- RPC: take lead in work (workflow-aware)
create or replace function public.take_lead_in_work(p_lead_id uuid)
returns void
language plpgsql
security definer
set search_path = public
as $$
declare
  v_actor uuid := auth.uid();
  v_lead public.leads%rowtype;
begin
  if v_actor is null then
    raise exception 'Unauthorized';
  end if;

  select * into v_lead from public.leads where id = p_lead_id for update;
  if not found then raise exception 'Lead not found'; end if;
  if not public.can_access_office(v_lead.office_id) then
    raise exception 'Forbidden';
  end if;

  if v_lead.workflow_status not in ('new', 'in_work', 'callback_required') then
    raise exception 'Invalid workflow status for take in work';
  end if;

  update public.leads
  set
    workflow_status = 'in_work',
    workflow_status_changed_at = now(),
    lead_status = 'in_progress',
    lead_status_changed_at = now(),
    assigned_to = coalesce(assigned_to, v_actor)
  where id = p_lead_id;

  insert into public.lead_events (lead_id, actor_id, event_type, old_value, new_value)
  values (
    p_lead_id,
    v_actor,
    'lead_assigned',
    jsonb_build_object('workflow_status', v_lead.workflow_status, 'assigned_to', v_lead.assigned_to),
    jsonb_build_object('workflow_status', 'in_work', 'assigned_to', coalesce(v_lead.assigned_to, v_actor))
  );
end;
$$;

grant execute on function public.take_lead_in_work(uuid) to authenticated;

-- Workflow dashboard overview RPC
create or replace function public.get_workflow_dashboard(
  p_office_id uuid default null,
  p_manager_id uuid default null,
  p_period_days int default 30
)
returns jsonb
language plpgsql
security definer
set search_path = public
as $$
declare
  v_since timestamptz := now() - make_interval(days => greatest(p_period_days, 1));
  v_today_start timestamptz := date_trunc('day', now());
  v_yesterday_start timestamptz := date_trunc('day', now() - interval '1 day');
  v_result jsonb;
begin
  if auth.uid() is null then raise exception 'Unauthorized'; end if;

  with accessible_leads as (
    select l.*
    from public.leads l
    where public.can_access_office(l.office_id)
      and (p_office_id is null or l.office_id = p_office_id)
      and (p_manager_id is null or l.assigned_to = p_manager_id)
  ),
  prepayments as (
    select lp.currency::text as currency, sum(lp.amount) as total
    from public.lead_payments lp
    join accessible_leads al on al.id = lp.lead_id
    where lp.payment_type = 'prepayment' and lp.paid_at >= v_since
    group by lp.currency
  )
  select jsonb_build_object(
    'period_days', p_period_days,
    'totals', (
      select jsonb_build_object(
        'leads_created', (select count(*) from accessible_leads where created_at >= v_since),
        'not_taken', (select count(*) from accessible_leads where workflow_status = 'new'),
        'showroom_scheduled', (select count(*) from public.lead_showroom_visits sv join accessible_leads al on al.id = sv.lead_id where sv.status = 'scheduled' and sv.scheduled_at >= v_since),
        'showroom_completed', (select count(*) from public.lead_showroom_visits sv join accessible_leads al on al.id = sv.lead_id where sv.status = 'visited' and sv.updated_at >= v_since),
        'contracts_planned', (select count(*) from public.lead_contracts c join accessible_leads al on al.id = c.lead_id where c.status = 'planned' and c.planned_at >= v_since),
        'contracts_signed', (select count(*) from public.lead_contracts c join accessible_leads al on al.id = c.lead_id where c.status = 'signed' and c.signed_at >= v_since),
        'overdue_tasks', (select count(*) from public.tasks t join accessible_leads al on al.id = t.entity_id where t.entity_type = 'lead' and t.status = 'open' and t.due_at < now()),
        'no_contact_attempt', (select count(*) from accessible_leads al where al.workflow_status in ('new', 'in_work') and not exists (select 1 from public.lead_contact_attempts ca where ca.lead_id = al.id)),
        'no_show', (select count(*) from public.lead_showroom_visits sv join accessible_leads al on al.id = sv.lead_id where sv.status = 'no_show' and sv.updated_at >= v_since),
        'reached', (select count(*) filter (where ca.result = 'reached') from public.lead_contact_attempts ca join accessible_leads al on al.id = ca.lead_id where ca.created_at >= v_since),
        'not_reached', (select count(*) filter (where ca.result in ('no_answer', 'cannot_talk')) from public.lead_contact_attempts ca join accessible_leads al on al.id = ca.lead_id where ca.created_at >= v_since),
        'reached_today', (select count(*) filter (where ca.result = 'reached' and ca.created_at >= v_today_start) from public.lead_contact_attempts ca join accessible_leads al on al.id = ca.lead_id),
        'not_reached_today', (select count(*) filter (where ca.result in ('no_answer', 'cannot_talk') and ca.created_at >= v_today_start) from public.lead_contact_attempts ca join accessible_leads al on al.id = ca.lead_id),
        'reached_yesterday', (select count(*) filter (where ca.result = 'reached' and ca.created_at >= v_yesterday_start and ca.created_at < v_today_start) from public.lead_contact_attempts ca join accessible_leads al on al.id = ca.lead_id),
        'not_reached_yesterday', (select count(*) filter (where ca.result in ('no_answer', 'cannot_talk') and ca.created_at >= v_yesterday_start and ca.created_at < v_today_start) from public.lead_contact_attempts ca join accessible_leads al on al.id = ca.lead_id)
      )
    ),
    'prepayments_by_currency', coalesce((select jsonb_object_agg(currency, total) from prepayments), '{}'::jsonb),
    'funnel', jsonb_build_object(
      'new', (select count(*) from accessible_leads where workflow_status = 'new'),
      'in_work', (select count(*) from accessible_leads where workflow_status = 'in_work'),
      'contacted', (select count(*) from accessible_leads where workflow_status = 'contacted'),
      'showroom_scheduled', (select count(*) from accessible_leads where workflow_status = 'showroom_scheduled'),
      'showroom_visited', (select count(*) from accessible_leads where workflow_status = 'showroom_visited'),
      'contract_signed', (select count(*) from accessible_leads where workflow_status = 'contract_signed'),
      'prepayment_received', (select count(*) from accessible_leads where workflow_status in ('prepayment_received', 'in_production')),
      'in_production', (select count(*) from accessible_leads where workflow_status = 'in_production')
    ),
    'queues', jsonb_build_object(
      'new_leads', coalesce((
        select jsonb_agg(jsonb_build_object('id', al.id, 'name', al.name, 'phone', al.phone, 'workflow_status', al.workflow_status, 'source_channel', al.source_channel, 'next_task_due_at', al.next_task_due_at, 'created_at', al.created_at) order by al.created_at desc)
        from (select * from accessible_leads where workflow_status = 'new' order by created_at desc limit 20) al
      ), '[]'::jsonb),
      'callbacks', coalesce((
        select jsonb_agg(jsonb_build_object('id', al.id, 'name', al.name, 'phone', al.phone, 'workflow_status', al.workflow_status, 'next_task_due_at', al.next_task_due_at, 'next_task_title', al.next_task_title, 'is_overdue', al.next_task_due_at < now()) order by al.next_task_due_at nulls last)
        from (select * from accessible_leads where workflow_status in ('callback_required', 'in_work') and next_task_due_at is not null order by next_task_due_at limit 20) al
      ), '[]'::jsonb),
      'no_show', coalesce((
        select jsonb_agg(jsonb_build_object('id', al.id, 'name', al.name, 'phone', al.phone, 'workflow_status', al.workflow_status) order by al.workflow_status_changed_at desc)
        from (select * from accessible_leads where workflow_status = 'showroom_no_show' order by workflow_status_changed_at desc limit 20) al
      ), '[]'::jsonb),
      'scheduled_showroom', coalesce((
        select jsonb_agg(jsonb_build_object('id', al.id, 'name', al.name, 'phone', al.phone, 'workflow_status', al.workflow_status, 'next_task_due_at', al.next_task_due_at) order by al.next_task_due_at)
        from (select * from accessible_leads where workflow_status = 'showroom_scheduled' order by next_task_due_at nulls last limit 20) al
      ), '[]'::jsonb),
      'contract_prepay', coalesce((
        select jsonb_agg(jsonb_build_object('id', al.id, 'name', al.name, 'phone', al.phone, 'workflow_status', al.workflow_status) order by al.workflow_status_changed_at desc)
        from (select * from accessible_leads where workflow_status in ('showroom_visited', 'contract_planned', 'contract_signed') order by workflow_status_changed_at desc limit 20) al
      ), '[]'::jsonb)
    )
  ) into v_result;

  return v_result;
end;
$$;

grant execute on function public.get_workflow_dashboard(uuid, uuid, int) to authenticated;
