-- Performance indexes and RPC mutations for fewer round-trips

-- Composite indexes for list/filter queries
create index if not exists leads_office_status_created_idx
  on public.leads (office_id, lead_status, created_at desc);

create index if not exists leads_callback_due_idx
  on public.leads (callback_due_at)
  where callback_due_at is not null;

create index if not exists projects_office_status_updated_idx
  on public.projects (office_id, status, updated_at desc);

-- Take lead in progress: status update + event in one transaction
create or replace function public.take_lead_in_progress(
  p_lead_id uuid,
  p_actor_id uuid
)
returns void
language plpgsql
security definer
set search_path = public
as $$
declare
  v_old_status text;
  v_assigned uuid;
begin
  select lead_status, assigned_to
  into v_old_status, v_assigned
  from public.leads
  where id = p_lead_id
  for update;

  if v_old_status is null then
    raise exception 'Lead not found';
  end if;

  if v_old_status not in ('new', 'in_progress') then
    raise exception 'Лід уже закритий';
  end if;

  update public.leads
  set
    lead_status = 'in_progress',
    lead_status_changed_at = now(),
    assigned_to = coalesce(v_assigned, p_actor_id)
  where id = p_lead_id;

  insert into public.lead_events (lead_id, actor_id, event_type, old_value, new_value)
  values (
    p_lead_id,
    p_actor_id,
    'status_change',
    jsonb_build_object('lead_status', v_old_status),
    jsonb_build_object('lead_status', 'in_progress')
  );
end;
$$;

-- Mark lead failed: status update + event
create or replace function public.mark_lead_failed(
  p_lead_id uuid,
  p_actor_id uuid,
  p_loss_reason text,
  p_estimated_budget numeric default null,
  p_our_quote numeric default null
)
returns void
language plpgsql
security definer
set search_path = public
as $$
declare
  v_old_status text;
begin
  select lead_status into v_old_status
  from public.leads
  where id = p_lead_id
  for update;

  if v_old_status is null then
    raise exception 'Lead not found';
  end if;

  if v_old_status in ('converted', 'failed') then
    raise exception 'Лід уже закритий';
  end if;

  update public.leads
  set
    lead_status = 'failed',
    lead_status_changed_at = now(),
    loss_reason = p_loss_reason,
    estimated_budget = p_estimated_budget,
    our_quote = p_our_quote
  where id = p_lead_id;

  insert into public.lead_events (lead_id, actor_id, event_type, old_value, new_value)
  values (
    p_lead_id,
    p_actor_id,
    'status_change',
    jsonb_build_object('lead_status', v_old_status),
    jsonb_build_object('lead_status', 'failed', 'loss_reason', p_loss_reason)
  );
end;
$$;

-- Dashboard aggregates
create or replace function public.get_dashboard_stats()
returns jsonb
language sql
stable
security definer
set search_path = public
as $$
  select jsonb_build_object(
    'leads_by_status', (
      select coalesce(jsonb_object_agg(lead_status, cnt), '{}'::jsonb)
      from (
        select lead_status, count(*)::int as cnt
        from public.leads
        group by lead_status
      ) s
    ),
    'projects_by_status', (
      select coalesce(jsonb_object_agg(status, cnt), '{}'::jsonb)
      from (
        select status, count(*)::int as cnt
        from public.projects
        group by status
      ) s
    ),
    'callback_overdue', (
      select count(*)::int
      from public.leads
      where callback_due_at is not null
        and callback_due_at < now()
        and lead_status = 'in_progress'
    ),
    'total_leads', (select count(*)::int from public.leads),
    'total_projects', (select count(*)::int from public.projects)
  );
$$;

grant execute on function public.take_lead_in_progress(uuid, uuid) to authenticated;
grant execute on function public.mark_lead_failed(uuid, uuid, text, numeric, numeric) to authenticated;
grant execute on function public.get_dashboard_stats() to authenticated;
