drop function if exists public.get_workflow_dashboard(uuid, uuid, int);

create or replace function public.get_workflow_dashboard(
  p_office_id uuid default null,
  p_manager_id uuid default null,
  p_period_days int default null,
  p_from timestamptz default null,
  p_to timestamptz default null
)
returns jsonb
language plpgsql
security definer
set search_path = public
as $$
declare
  v_to timestamptz := coalesce(p_to, now());
  v_from timestamptz := coalesce(
    p_from,
    v_to - make_interval(days => greatest(coalesce(p_period_days, 30), 1))
  );
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
    where lp.payment_type = 'prepayment'
      and lp.paid_at >= v_from
      and lp.paid_at <= v_to
    group by lp.currency
  )
  select jsonb_build_object(
    'period_from', v_from,
    'period_to', v_to,
    'totals', (
      select jsonb_build_object(
        'leads_created', (select count(*) from accessible_leads where created_at >= v_from and created_at <= v_to),
        'not_taken', (select count(*) from accessible_leads where workflow_status = 'new'),
        'showroom_scheduled', (select count(*) from public.lead_showroom_visits sv join accessible_leads al on al.id = sv.lead_id where sv.status = 'scheduled' and sv.scheduled_at >= v_from and sv.scheduled_at <= v_to),
        'showroom_completed', (select count(*) from public.lead_showroom_visits sv join accessible_leads al on al.id = sv.lead_id where sv.status = 'visited' and sv.updated_at >= v_from and sv.updated_at <= v_to),
        'contracts_planned', (select count(*) from public.lead_contracts c join accessible_leads al on al.id = c.lead_id where c.status = 'planned' and c.planned_at >= v_from and c.planned_at <= v_to),
        'contracts_signed', (select count(*) from public.lead_contracts c join accessible_leads al on al.id = c.lead_id where c.status = 'signed' and c.signed_at >= v_from and c.signed_at <= v_to),
        'overdue_tasks', (select count(*) from public.tasks t join accessible_leads al on al.id = t.entity_id where t.entity_type = 'lead' and t.status = 'open' and t.due_at < now()),
        'no_contact_attempt', (select count(*) from accessible_leads al where al.workflow_status in ('new', 'in_work') and not exists (select 1 from public.lead_contact_attempts ca where ca.lead_id = al.id)),
        'no_show', (select count(*) from public.lead_showroom_visits sv join accessible_leads al on al.id = sv.lead_id where sv.status = 'no_show' and sv.updated_at >= v_from and sv.updated_at <= v_to),
        'reached', (select count(*) filter (where ca.result = 'reached') from public.lead_contact_attempts ca join accessible_leads al on al.id = ca.lead_id where ca.created_at >= v_from and ca.created_at <= v_to),
        'not_reached', (select count(*) filter (where ca.result in ('no_answer', 'cannot_talk')) from public.lead_contact_attempts ca join accessible_leads al on al.id = ca.lead_id where ca.created_at >= v_from and ca.created_at <= v_to),
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
        select jsonb_agg(
          jsonb_build_object(
            'id', al.id,
            'event_id', t.id,
            'name', al.name,
            'phone', al.phone,
            'workflow_status', al.workflow_status,
            'next_task_due_at', t.due_at,
            'next_task_title', t.title,
            'task_type', t.task_type,
            'office_code', o.code,
            'is_overdue', t.due_at < now()
          )
          order by t.due_at, t.created_at
        )
        from public.tasks t
        join accessible_leads al on al.id = t.entity_id
        join public.offices o on o.id = al.office_id
        where t.entity_type = 'lead'
          and t.status = 'open'
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

grant execute on function public.get_workflow_dashboard(uuid, uuid, int, timestamptz, timestamptz) to authenticated;
