-- Secure RPC functions (actor from auth.uid), office-scoped dashboard,
-- tasks_insert office scoping, is_active gating in RLS helpers

-- 1) RLS helpers: deactivated profiles get no access
create or replace function public.is_super_admin()
returns boolean
language sql
stable
security definer
set search_path = public
as $$
  select exists (
    select 1 from public.profiles
    where id = auth.uid()
      and role = 'super_admin'
      and is_active
  );
$$;

create or replace function public.can_access_office(target_office_id uuid)
returns boolean
language sql
stable
security definer
set search_path = public
as $$
  select exists (
    select 1 from public.profiles
    where id = auth.uid()
      and is_active
  )
  and (
    public.is_super_admin()
    or target_office_id in (select public.user_office_ids())
  );
$$;

-- 2) take_lead_in_progress: drop old signature (param list changes)
drop function if exists public.take_lead_in_progress(uuid, uuid);

create function public.take_lead_in_progress(p_lead_id uuid)
returns void
language plpgsql
security definer
set search_path = public
as $$
declare
  v_actor uuid := auth.uid();
  v_old_status text;
  v_assigned uuid;
  v_office_id uuid;
begin
  if v_actor is null then
    raise exception 'Не автентифіковано';
  end if;

  if not exists (
    select 1 from public.profiles where id = v_actor and is_active
  ) then
    raise exception 'Обліковий запис деактивовано';
  end if;

  select lead_status, assigned_to, office_id
  into v_old_status, v_assigned, v_office_id
  from public.leads
  where id = p_lead_id
  for update;

  if v_old_status is null then
    raise exception 'Lead not found';
  end if;

  if not public.can_access_office(v_office_id) then
    raise exception 'Немає доступу до цього офісу';
  end if;

  if v_old_status not in ('new', 'in_progress') then
    raise exception 'Лід уже закритий';
  end if;

  update public.leads
  set
    lead_status = 'in_progress',
    lead_status_changed_at = now(),
    assigned_to = coalesce(v_assigned, v_actor)
  where id = p_lead_id;

  insert into public.lead_events (lead_id, actor_id, event_type, old_value, new_value)
  values (
    p_lead_id,
    v_actor,
    'status_change',
    jsonb_build_object('lead_status', v_old_status),
    jsonb_build_object('lead_status', 'in_progress')
  );
end;
$$;

-- 3) mark_lead_failed: drop old signature (param list changes)
drop function if exists public.mark_lead_failed(uuid, uuid, text, numeric, numeric);

create function public.mark_lead_failed(
  p_lead_id uuid,
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
  v_actor uuid := auth.uid();
  v_old_status text;
  v_office_id uuid;
begin
  if v_actor is null then
    raise exception 'Не автентифіковано';
  end if;

  if not exists (
    select 1 from public.profiles where id = v_actor and is_active
  ) then
    raise exception 'Обліковий запис деактивовано';
  end if;

  select lead_status, office_id
  into v_old_status, v_office_id
  from public.leads
  where id = p_lead_id
  for update;

  if v_old_status is null then
    raise exception 'Lead not found';
  end if;

  if not public.can_access_office(v_office_id) then
    raise exception 'Немає доступу до цього офісу';
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
    v_actor,
    'status_change',
    jsonb_build_object('lead_status', v_old_status),
    jsonb_build_object('lead_status', 'failed', 'loss_reason', p_loss_reason)
  );
end;
$$;

-- 4) get_dashboard_stats: aggregates scoped to offices the caller can access
--    (super_admin sees all via can_access_office; deactivated users see nothing)
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
        where public.can_access_office(office_id)
        group by lead_status
      ) s
    ),
    'projects_by_status', (
      select coalesce(jsonb_object_agg(status, cnt), '{}'::jsonb)
      from (
        select status, count(*)::int as cnt
        from public.projects
        where public.can_access_office(office_id)
        group by status
      ) s
    ),
    'callback_overdue', (
      select count(*)::int
      from public.leads
      where callback_due_at is not null
        and callback_due_at < now()
        and lead_status = 'in_progress'
        and public.can_access_office(office_id)
    ),
    'total_leads', (
      select count(*)::int
      from public.leads
      where public.can_access_office(office_id)
    ),
    'total_projects', (
      select count(*)::int
      from public.projects
      where public.can_access_office(office_id)
    )
  );
$$;

-- 5) convert_lead_to_project: transactional replacement for the 3-step
--    insert project → update lead → insert event chain in the app code
create or replace function public.convert_lead_to_project(p_lead_id uuid)
returns uuid
language plpgsql
security definer
set search_path = public
as $$
declare
  v_actor uuid := auth.uid();
  v_lead public.leads%rowtype;
  v_project_id uuid;
begin
  if v_actor is null then
    raise exception 'Не автентифіковано';
  end if;

  if not exists (
    select 1 from public.profiles where id = v_actor and is_active
  ) then
    raise exception 'Обліковий запис деактивовано';
  end if;

  select * into v_lead
  from public.leads
  where id = p_lead_id
  for update;

  if v_lead.id is null then
    raise exception 'Lead not found';
  end if;

  if not public.can_access_office(v_lead.office_id) then
    raise exception 'Немає доступу до цього офісу';
  end if;

  if v_lead.lead_status = 'converted' then
    raise exception 'Проєкт уже створено';
  end if;

  if v_lead.lead_status = 'failed' then
    raise exception 'Невдалий лід не можна конвертувати';
  end if;

  insert into public.projects (
    lead_id,
    office_id,
    status,
    status_changed_at,
    last_activity_at,
    product_type,
    product_details,
    assigned_to
  )
  values (
    p_lead_id,
    v_lead.office_id,
    'needs_discovery',
    now(),
    now(),
    nullif(v_lead.product_interest, ''),
    case
      when v_lead.product_interest in ('home_furniture', 'other')
        then v_lead.order_comment
      else null
    end,
    coalesce(v_lead.assigned_to, v_actor)
  )
  returning id into v_project_id;

  update public.leads
  set
    lead_status = 'converted',
    lead_status_changed_at = now(),
    converted_project_id = v_project_id,
    assigned_to = coalesce(v_lead.assigned_to, v_actor)
  where id = p_lead_id;

  insert into public.lead_events (lead_id, actor_id, event_type, old_value, new_value)
  values (
    p_lead_id,
    v_actor,
    'converted_to_project',
    jsonb_build_object('lead_status', v_lead.lead_status),
    jsonb_build_object('lead_status', 'converted', 'project_id', v_project_id)
  );

  return v_project_id;
end;
$$;

-- 6) Lock down execution: nothing for PUBLIC/anon, execute for authenticated
revoke all on function public.take_lead_in_progress(uuid) from public, anon;
revoke all on function public.mark_lead_failed(uuid, text, numeric, numeric) from public, anon;
revoke all on function public.get_dashboard_stats() from public, anon;
revoke all on function public.convert_lead_to_project(uuid) from public, anon;

grant execute on function public.take_lead_in_progress(uuid) to authenticated;
grant execute on function public.mark_lead_failed(uuid, text, numeric, numeric) to authenticated;
grant execute on function public.get_dashboard_stats() to authenticated;
grant execute on function public.convert_lead_to_project(uuid) to authenticated;

-- 7) tasks_insert: office-scoped check instead of with check (true)
drop policy if exists tasks_insert on public.tasks;

create policy tasks_insert on public.tasks
  for insert to authenticated
  with check (
    (
      entity_type = 'lead' and exists (
        select 1 from public.leads l
        where l.id = entity_id and public.can_access_office(l.office_id)
      )
    )
    or (
      entity_type = 'project' and exists (
        select 1 from public.projects p
        where p.id = entity_id and public.can_access_office(p.office_id)
      )
    )
  );
