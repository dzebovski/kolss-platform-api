-- Lock down SECURITY DEFINER functions and mutable search_path (Supabase Security Advisor).
--
-- Fixes the genuine security-advisor findings on the exposed `public` schema:
--   * 0028 anon_security_definer_function_executable
--   * 0029 authenticated_security_definer_function_executable (only where anon exposure is the real issue)
--   * 0011 function_search_path_mutable
--
-- What this migration does, and why:
--   1. Moves the three RLS helper functions (is_super_admin, user_office_ids,
--      can_access_office) out of the API-exposed `public` schema into a new,
--      non-exposed `private` schema. PostgREST does not expose `private`, so these
--      SECURITY DEFINER helpers can no longer be called over the Data API, which
--      clears 0028/0029 for them. RLS keeps working because policies now call the
--      `private.*` versions.
--   2. Every RLS policy and every internal function that referenced the `public`
--      helpers is repointed to `private.*` BEFORE the `public` helpers are dropped.
--      This is required because:
--        - RLS policies create a hard catalog dependency on the function, so the
--          DROP would fail unless all 43 policies are repointed first.
--        - Postgres does NOT track function-body -> function dependencies, so the
--          DROP would silently succeed while leaving caller function bodies broken
--          at runtime. 8 SECURITY DEFINER functions call these helpers in their
--          bodies (get_dashboard_overview, get_workflow_dashboard, get_dashboard_stats,
--          take_lead_in_progress, take_lead_in_work, mark_lead_failed,
--          convert_lead_to_project, profiles_guard_sensitive_fields), so their bodies
--          are recreated to call `private.*`.
--   DECISION on the caller dependency: we chose to UPDATE the caller function bodies
--   to call `private.*` (rather than keeping thin `public` wrappers). Wrappers would
--   have kept the helper names exposed via PostgREST; moving fully to `private` and
--   dropping the `public` helpers removes the exposed surface entirely, which is the
--   goal and the Supabase-recommended pattern.
--   3. Revokes EXECUTE from anon/authenticated/public on trigger/internal helper
--      functions that are never meant to be called over the API (clears 0028/0029 for
--      them; triggers still fire because trigger execution does not check EXECUTE).
--   4. Adds a fixed search_path to the three functions that were missing it (0011).
--   5. Removes anon (and PUBLIC) EXECUTE from the three real, signed-in-only RPCs
--      (get_dashboard_overview, get_workflow_dashboard, take_lead_in_work) and grants
--      EXECUTE only to authenticated. The residual 0029 (authenticated) on these and
--      on the other authenticated RPCs is intentional and accepted.
--   6. Future-proofing: new functions created in `public` by the postgres role no
--      longer auto-grant EXECUTE to anon/PUBLIC.
--
-- NOT touched (intentional):
--   * public.api_idempotency_keys (0008 RLS-enabled-no-policy INFO) — closed to clients on purpose.
--   * Accepted 0029 authenticated-executable RPC warnings.
--   * Application code (Angular/Go) — the helpers are only referenced in SQL.

-- =====================================================================================
-- 1a. Private schema for RLS helpers (not exposed to PostgREST).
-- =====================================================================================
create schema if not exists private;
grant usage on schema private to anon, authenticated;

-- =====================================================================================
-- 1b. Recreate the three RLS helpers in `private` (bodies preserved verbatim; the
--     only change is that can_access_office now calls the private helpers).
-- =====================================================================================
create or replace function private.is_super_admin()
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

create or replace function private.user_office_ids()
returns setof uuid
language sql
stable
security definer
set search_path = public
as $$
  select office_id from public.user_office_memberships where user_id = auth.uid();
$$;

create or replace function private.can_access_office(target_office_id uuid)
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
    private.is_super_admin()
    or target_office_id in (select private.user_office_ids())
  );
$$;

-- =====================================================================================
-- 1c. Repoint internal caller functions to `private.*` (bodies verbatim, only the
--     schema-qualified helper calls changed). Required before dropping public helpers.
-- =====================================================================================

create or replace function public.take_lead_in_progress(p_lead_id uuid)
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

  if not private.can_access_office(v_office_id) then
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

create or replace function public.mark_lead_failed(
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

  if not private.can_access_office(v_office_id) then
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
        where private.can_access_office(office_id)
        group by lead_status
      ) s
    ),
    'projects_by_status', (
      select coalesce(jsonb_object_agg(status, cnt), '{}'::jsonb)
      from (
        select status, count(*)::int as cnt
        from public.projects
        where private.can_access_office(office_id)
        group by status
      ) s
    ),
    'callback_overdue', (
      select count(*)::int
      from public.leads
      where callback_due_at is not null
        and callback_due_at < now()
        and lead_status = 'in_progress'
        and private.can_access_office(office_id)
    ),
    'total_leads', (
      select count(*)::int
      from public.leads
      where private.can_access_office(office_id)
    ),
    'total_projects', (
      select count(*)::int
      from public.projects
      where private.can_access_office(office_id)
    )
  );
$$;

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

  if not private.can_access_office(v_lead.office_id) then
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
  if not private.can_access_office(v_lead.office_id) then
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

create or replace function public.get_dashboard_overview(
  p_office_id uuid default null,
  p_period_days integer default 30
)
returns jsonb
language sql
stable
security definer
set search_path = public
as $$
  with params as (
    select
      greatest(1, least(coalesce(p_period_days, 30), 365)) as period_days,
      now() - make_interval(
        days => greatest(1, least(coalesce(p_period_days, 30), 365))
      ) as period_start
  ),
  scoped_offices as (
    select
      o.id,
      o.code,
      o.name_uk,
      case o.code
        when 'warsaw' then 'Europe/Warsaw'
        when 'london' then 'Europe/London'
        else 'Europe/Kyiv'
      end as timezone_name
    from public.offices o
    where o.is_active
      and private.can_access_office(o.id)
      and (p_office_id is null or o.id = p_office_id)
  ),
  scoped_leads as (
    select
      l.*,
      o.code as office_code,
      o.name_uk as office_name,
      o.timezone_name
    from public.leads l
    join scoped_offices o on o.id = l.office_id
  ),
  scoped_projects as (
    select
      p.*,
      o.code as office_code,
      o.name_uk as office_name,
      l.name as lead_name,
      l.phone as lead_phone
    from public.projects p
    join scoped_offices o on o.id = p.office_id
    left join public.leads l on l.id = p.lead_id
  ),
  work_items as (
    select *
    from (
      select
        'callback_overdue'::text as kind,
        'lead'::text as entity_type,
        l.id,
        coalesce(nullif(l.name, ''), 'Лід без імені') as title,
        coalesce(nullif(l.phone, ''), nullif(l.product_interest, ''), 'Передзвонити клієнту') as detail,
        l.callback_due_at as due_at,
        l.created_at,
        l.office_id,
        l.office_code,
        l.office_name,
        1 as priority_rank
      from scoped_leads l
      where l.lead_status = 'in_progress'
        and l.callback_due_at is not null
        and l.callback_due_at < now()

      union all

      select
        'stale_approval'::text,
        'project'::text,
        p.id,
        coalesce(nullif(p.lead_name, ''), 'Проєкт без імені'),
        coalesce(nullif(p.product_type, ''), nullif(p.lead_phone, ''), 'Потрібен контакт із клієнтом'),
        p.last_activity_at + interval '3 days',
        p.created_at,
        p.office_id,
        p.office_code,
        p.office_name,
        2
      from scoped_projects p
      where p.status = 'approval'
        and p.last_activity_at < now() - interval '3 days'

      union all

      select
        'callback_today'::text,
        'lead'::text,
        l.id,
        coalesce(nullif(l.name, ''), 'Лід без імені'),
        coalesce(nullif(l.phone, ''), nullif(l.product_interest, ''), 'Передзвонити клієнту'),
        l.callback_due_at,
        l.created_at,
        l.office_id,
        l.office_code,
        l.office_name,
        3
      from scoped_leads l
      where l.lead_status = 'in_progress'
        and l.callback_due_at is not null
        and l.callback_due_at >= now()
        and (l.callback_due_at at time zone l.timezone_name)::date =
          (now() at time zone l.timezone_name)::date

      union all

      select
        'new_unassigned'::text,
        'lead'::text,
        l.id,
        coalesce(nullif(l.name, ''), 'Лід без імені'),
        coalesce(nullif(l.product_interest, ''), nullif(l.phone, ''), 'Нова заявка'),
        null::timestamptz,
        l.created_at,
        l.office_id,
        l.office_code,
        l.office_name,
        4
      from scoped_leads l
      where l.lead_status = 'new'
        and l.assigned_to is null
    ) items
    order by priority_rank, due_at asc nulls last, created_at asc
    limit 16
  ),
  recent_leads as (
    select
      l.id,
      coalesce(nullif(l.name, ''), 'Лід без імені') as name,
      l.phone,
      l.product_interest,
      l.lead_status,
      l.created_at,
      l.office_id,
      l.office_code,
      l.office_name
    from scoped_leads l
    order by l.created_at desc
    limit 6
  ),
  team_members as (
    select
      p.id,
      coalesce(nullif(p.display_name, ''), 'Без імені') as display_name,
      jsonb_agg(distinct so.name_uk) as offices
    from public.profiles p
    join public.user_office_memberships m on m.user_id = p.id
    join scoped_offices so on so.id = m.office_id
    where p.is_active
      and p.role in ('curator', 'office_admin', 'office_member')
    group by p.id, p.display_name
  ),
  team_rollup as (
    select
      tm.id,
      tm.display_name,
      tm.offices,
      (
        select count(*)::int
        from scoped_leads l
        where l.assigned_to = tm.id
          and l.lead_status = 'in_progress'
      ) as active_leads,
      (
        select count(*)::int
        from scoped_projects p
        where p.assigned_to = tm.id
          and p.status not in ('completed', 'archived')
      ) as active_projects,
      (
        select count(*)::int
        from scoped_leads l
        where l.assigned_to = tm.id
          and l.lead_status = 'in_progress'
          and l.callback_due_at is not null
          and l.callback_due_at < now()
      ) as overdue_callbacks
    from team_members tm
  )
  select jsonb_build_object(
    'period_days', (select period_days from params),
    'totals', jsonb_build_object(
      'leads_created', (
        select count(*)::int
        from scoped_leads l, params p
        where l.created_at >= p.period_start
      ),
      'leads_new', (
        select count(*)::int from scoped_leads where lead_status = 'new'
      ),
      'leads_in_progress', (
        select count(*)::int from scoped_leads where lead_status = 'in_progress'
      ),
      'leads_converted', (
        select count(*)::int
        from scoped_leads l, params p
        where l.created_at >= p.period_start and l.lead_status = 'converted'
      ),
      'leads_failed', (
        select count(*)::int
        from scoped_leads l, params p
        where l.created_at >= p.period_start and l.lead_status = 'failed'
      ),
      'active_projects', (
        select count(*)::int
        from scoped_projects
        where status not in ('completed', 'archived')
      ),
      'completed_projects', (
        select count(*)::int
        from scoped_projects p, params q
        where p.status = 'completed' and p.status_changed_at >= q.period_start
      ),
      'callback_overdue', (
        select count(*)::int
        from scoped_leads
        where lead_status = 'in_progress'
          and callback_due_at is not null
          and callback_due_at < now()
      ),
      'callback_today', (
        select count(*)::int
        from scoped_leads
        where lead_status = 'in_progress'
          and callback_due_at is not null
          and callback_due_at >= now()
          and (callback_due_at at time zone timezone_name)::date =
            (now() at time zone timezone_name)::date
      ),
      'new_unassigned', (
        select count(*)::int
        from scoped_leads
        where lead_status = 'new' and assigned_to is null
      ),
      'stale_approvals', (
        select count(*)::int
        from scoped_projects
        where status = 'approval'
          and last_activity_at < now() - interval '3 days'
      )
    ),
    'leads_by_status', (
      select coalesce(jsonb_object_agg(lead_status, count), '{}'::jsonb)
      from (
        select lead_status, count(*)::int as count
        from scoped_leads
        group by lead_status
      ) counts
    ),
    'projects_by_status', (
      select coalesce(jsonb_object_agg(status, count), '{}'::jsonb)
      from (
        select status, count(*)::int as count
        from scoped_projects
        group by status
      ) counts
    ),
    'work_items', (
      select coalesce(
        jsonb_agg(
          to_jsonb(w) - 'priority_rank'
          order by w.priority_rank, w.due_at asc nulls last, w.created_at asc
        ),
        '[]'::jsonb
      )
      from work_items w
    ),
    'recent_leads', (
      select coalesce(jsonb_agg(to_jsonb(r)), '[]'::jsonb)
      from recent_leads r
    ),
    'team', (
      select coalesce(
        jsonb_agg(
          to_jsonb(t)
          order by t.overdue_callbacks desc, t.active_leads desc, t.display_name
        ),
        '[]'::jsonb
      )
      from team_rollup t
    )
  );
$$;

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
    where private.can_access_office(l.office_id)
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

create or replace function public.profiles_guard_sensitive_fields()
returns trigger
language plpgsql
security definer
set search_path = public
as $$
begin
  if private.is_super_admin() then
    return new;
  end if;

  if new.role is distinct from old.role then
    raise exception 'Cannot change role';
  end if;

  if new.is_active is distinct from old.is_active then
    raise exception 'Cannot change active status';
  end if;

  if new.deactivated_at is distinct from old.deactivated_at then
    raise exception 'Cannot change deactivation status';
  end if;

  return new;
end;
$$;

-- =====================================================================================
-- 1d. Repoint every RLS policy that referenced the public helpers to `private.*`.
--     43 policies total: 39 on public tables + 4 on storage.objects. Bodies are
--     preserved exactly (to/using/with check/command); only the schema-qualified
--     helper calls change.
-- =====================================================================================

-- profiles
drop policy if exists profiles_select on public.profiles;
create policy profiles_select on public.profiles for select to authenticated
  using (id = auth.uid() or private.is_super_admin());

drop policy if exists profiles_update_admin on public.profiles;
create policy profiles_update_admin on public.profiles
  for update to authenticated
  using (private.is_super_admin())
  with check (private.is_super_admin());

-- user_office_memberships
drop policy if exists memberships_select on public.user_office_memberships;
create policy memberships_select on public.user_office_memberships for select to authenticated
  using (user_id = auth.uid() or private.is_super_admin());

drop policy if exists memberships_insert_admin on public.user_office_memberships;
create policy memberships_insert_admin on public.user_office_memberships
  for insert to authenticated
  with check (private.is_super_admin());

drop policy if exists memberships_delete_admin on public.user_office_memberships;
create policy memberships_delete_admin on public.user_office_memberships
  for delete to authenticated
  using (private.is_super_admin());

-- leads
drop policy if exists leads_select on public.leads;
create policy leads_select on public.leads for select to authenticated
  using (private.can_access_office(office_id));

drop policy if exists leads_insert on public.leads;
create policy leads_insert on public.leads for insert to authenticated
  with check (private.can_access_office(office_id));

drop policy if exists leads_update on public.leads;
create policy leads_update on public.leads for update to authenticated
  using (private.can_access_office(office_id))
  with check (private.can_access_office(office_id));

-- lead_import_sources
drop policy if exists import_sources_select on public.lead_import_sources;
create policy import_sources_select on public.lead_import_sources for select to authenticated
  using (private.can_access_office(office_id));

-- lead_import_runs
drop policy if exists import_runs_select on public.lead_import_runs;
create policy import_runs_select on public.lead_import_runs for select to authenticated
  using (
    exists (
      select 1 from public.lead_import_sources s
      where s.id = source_id and private.can_access_office(s.office_id)
    )
  );

-- lead_notifications
drop policy if exists lead_notifications_select on public.lead_notifications;
create policy lead_notifications_select on public.lead_notifications for select to authenticated
  using (
    exists (select 1 from public.leads l where l.id = lead_id and private.can_access_office(l.office_id))
  );

-- lead_events
drop policy if exists lead_events_select on public.lead_events;
create policy lead_events_select on public.lead_events for select to authenticated
  using (
    exists (select 1 from public.leads l where l.id = lead_id and private.can_access_office(l.office_id))
  );

drop policy if exists lead_events_insert on public.lead_events;
create policy lead_events_insert on public.lead_events for insert to authenticated
  with check (
    exists (select 1 from public.leads l where l.id = lead_id and private.can_access_office(l.office_id))
  );

-- lead_comments
drop policy if exists lead_comments_select on public.lead_comments;
create policy lead_comments_select on public.lead_comments for select to authenticated
  using (
    exists (select 1 from public.leads l where l.id = lead_id and private.can_access_office(l.office_id))
  );

drop policy if exists lead_comments_insert on public.lead_comments;
create policy lead_comments_insert on public.lead_comments for insert to authenticated
  with check (
    author_id = auth.uid()
    and exists (select 1 from public.leads l where l.id = lead_id and private.can_access_office(l.office_id))
  );

-- projects
drop policy if exists projects_select on public.projects;
create policy projects_select on public.projects
  for select to authenticated using (private.can_access_office(office_id));

drop policy if exists projects_insert on public.projects;
create policy projects_insert on public.projects
  for insert to authenticated
  with check (private.can_access_office(office_id));

drop policy if exists projects_update on public.projects;
create policy projects_update on public.projects
  for update to authenticated
  using (private.can_access_office(office_id))
  with check (private.can_access_office(office_id));

-- project_comments
drop policy if exists project_comments_select on public.project_comments;
create policy project_comments_select on public.project_comments
  for select to authenticated
  using (
    exists (
      select 1 from public.projects p
      where p.id = project_id and private.can_access_office(p.office_id)
    )
  );

drop policy if exists project_comments_insert on public.project_comments;
create policy project_comments_insert on public.project_comments
  for insert to authenticated
  with check (
    author_id = auth.uid()
    and exists (
      select 1 from public.projects p
      where p.id = project_id and private.can_access_office(p.office_id)
    )
  );

-- project_attachments
drop policy if exists project_attachments_select on public.project_attachments;
create policy project_attachments_select on public.project_attachments
  for select to authenticated
  using (
    exists (
      select 1 from public.projects p
      where p.id = project_id and private.can_access_office(p.office_id)
    )
  );

drop policy if exists project_attachments_insert on public.project_attachments;
create policy project_attachments_insert on public.project_attachments
  for insert to authenticated
  with check (
    uploaded_by = auth.uid()
    and exists (
      select 1 from public.projects p
      where p.id = project_id and private.can_access_office(p.office_id)
    )
  );

-- tasks
drop policy if exists tasks_select on public.tasks;
create policy tasks_select on public.tasks
  for select to authenticated
  using (
    assignee_id = auth.uid()
    or private.is_super_admin()
    or (
      entity_type = 'lead' and exists (
        select 1 from public.leads l
        where l.id = entity_id and private.can_access_office(l.office_id)
      )
    )
    or (
      entity_type = 'project' and exists (
        select 1 from public.projects p
        where p.id = entity_id and private.can_access_office(p.office_id)
      )
    )
  );

drop policy if exists tasks_insert on public.tasks;
create policy tasks_insert on public.tasks
  for insert to authenticated
  with check (
    (
      entity_type = 'lead' and exists (
        select 1 from public.leads l
        where l.id = entity_id and private.can_access_office(l.office_id)
      )
    )
    or (
      entity_type = 'project' and exists (
        select 1 from public.projects p
        where p.id = entity_id and private.can_access_office(p.office_id)
      )
    )
  );

drop policy if exists tasks_update on public.tasks;
create policy tasks_update on public.tasks
  for update to authenticated
  using (
    assignee_id = auth.uid()
    or private.is_super_admin()
    or exists (
      select 1 from public.leads l
      where entity_type = 'lead' and l.id = entity_id
        and private.can_access_office(l.office_id)
    )
    or exists (
      select 1 from public.projects p
      where entity_type = 'project' and p.id = entity_id
        and private.can_access_office(p.office_id)
    )
  );

-- lead_contact_attempts
drop policy if exists lead_contact_attempts_select on public.lead_contact_attempts;
create policy lead_contact_attempts_select on public.lead_contact_attempts
  for select to authenticated
  using (
    exists (
      select 1 from public.leads l
      where l.id = lead_contact_attempts.lead_id
        and private.can_access_office(l.office_id)
    )
  );

drop policy if exists lead_contact_attempts_insert on public.lead_contact_attempts;
create policy lead_contact_attempts_insert on public.lead_contact_attempts
  for insert to authenticated
  with check (
    manager_id = auth.uid()
    and exists (
      select 1 from public.leads l
      where l.id = lead_contact_attempts.lead_id
        and private.can_access_office(l.office_id)
    )
  );

-- lead_showroom_visits
drop policy if exists lead_showroom_visits_select on public.lead_showroom_visits;
create policy lead_showroom_visits_select on public.lead_showroom_visits
  for select to authenticated
  using (
    exists (
      select 1 from public.leads l
      where l.id = lead_showroom_visits.lead_id
        and private.can_access_office(l.office_id)
    )
  );

drop policy if exists lead_showroom_visits_insert on public.lead_showroom_visits;
create policy lead_showroom_visits_insert on public.lead_showroom_visits
  for insert to authenticated
  with check (
    created_by = auth.uid()
    and exists (
      select 1 from public.leads l
      where l.id = lead_showroom_visits.lead_id
        and private.can_access_office(l.office_id)
    )
  );

drop policy if exists lead_showroom_visits_update on public.lead_showroom_visits;
create policy lead_showroom_visits_update on public.lead_showroom_visits
  for update to authenticated
  using (
    exists (
      select 1 from public.leads l
      where l.id = lead_showroom_visits.lead_id
        and private.can_access_office(l.office_id)
    )
  );

-- lead_contracts
drop policy if exists lead_contracts_select on public.lead_contracts;
create policy lead_contracts_select on public.lead_contracts
  for select to authenticated
  using (
    exists (
      select 1 from public.leads l
      where l.id = lead_contracts.lead_id
        and private.can_access_office(l.office_id)
    )
  );

drop policy if exists lead_contracts_insert on public.lead_contracts;
create policy lead_contracts_insert on public.lead_contracts
  for insert to authenticated
  with check (
    created_by = auth.uid()
    and exists (
      select 1 from public.leads l
      where l.id = lead_contracts.lead_id
        and private.can_access_office(l.office_id)
    )
  );

drop policy if exists lead_contracts_update on public.lead_contracts;
create policy lead_contracts_update on public.lead_contracts
  for update to authenticated
  using (
    exists (
      select 1 from public.leads l
      where l.id = lead_contracts.lead_id
        and private.can_access_office(l.office_id)
    )
  );

-- lead_payments
drop policy if exists lead_payments_select on public.lead_payments;
create policy lead_payments_select on public.lead_payments
  for select to authenticated
  using (
    exists (
      select 1 from public.leads l
      where l.id = lead_payments.lead_id
        and private.can_access_office(l.office_id)
    )
  );

drop policy if exists lead_payments_insert on public.lead_payments;
create policy lead_payments_insert on public.lead_payments
  for insert to authenticated
  with check (
    created_by = auth.uid()
    and exists (
      select 1 from public.leads l
      where l.id = lead_payments.lead_id
        and private.can_access_office(l.office_id)
    )
  );

-- lead_attachments
drop policy if exists lead_attachments_select on public.lead_attachments;
create policy lead_attachments_select on public.lead_attachments
  for select to authenticated
  using (
    exists (
      select 1 from public.leads l
      where l.id = lead_id and private.can_access_office(l.office_id)
    )
  );

drop policy if exists lead_attachments_insert on public.lead_attachments;
create policy lead_attachments_insert on public.lead_attachments
  for insert to authenticated
  with check (
    uploaded_by = auth.uid()
    and exists (
      select 1 from public.leads l
      where l.id = lead_id and private.can_access_office(l.office_id)
    )
  );

-- lead_submissions
drop policy if exists lead_submissions_select_authenticated on public.lead_submissions;
create policy lead_submissions_select_authenticated on public.lead_submissions
  for select to authenticated
  using (
    lead_id is not null
    and exists (
      select 1 from public.leads l
      where l.id = lead_id and private.can_access_office(l.office_id)
    )
  );

-- lead_submission_uploads
drop policy if exists lead_submission_uploads_select_authenticated on public.lead_submission_uploads;
create policy lead_submission_uploads_select_authenticated on public.lead_submission_uploads
  for select to authenticated
  using (
    exists (
      select 1
      from public.lead_submissions s
      join public.leads l on l.id = s.lead_id
      where s.id = submission_id and private.can_access_office(l.office_id)
    )
  );

-- storage.objects (lead-attachments bucket)
drop policy if exists lead_attachments_storage_insert on storage.objects;
create policy lead_attachments_storage_insert on storage.objects
  for insert to authenticated
  with check (
    bucket_id = 'lead-attachments'
    and private.can_access_office(split_part(name, '/', 1)::uuid)
    and exists (
      select 1
      from public.leads l
      where l.id = split_part(name, '/', 2)::uuid
        and l.office_id = split_part(name, '/', 1)::uuid
    )
  );

drop policy if exists lead_attachments_storage_select on storage.objects;
create policy lead_attachments_storage_select on storage.objects
  for select to authenticated
  using (
    bucket_id = 'lead-attachments'
    and exists (
      select 1
      from public.lead_attachments la
      join public.leads l on l.id = la.lead_id
      where la.storage_path = storage.objects.name
        and private.can_access_office(l.office_id)
    )
  );

-- storage.objects (project-attachments bucket)
drop policy if exists project_attachments_storage_select on storage.objects;
create policy project_attachments_storage_select on storage.objects
  for select to authenticated
  using (
    bucket_id = 'project-attachments'
    and (storage.foldername(name))[1]::uuid in (select private.user_office_ids())
  );

drop policy if exists project_attachments_storage_insert on storage.objects;
create policy project_attachments_storage_insert on storage.objects
  for insert to authenticated
  with check (
    bucket_id = 'project-attachments'
    and (storage.foldername(name))[1]::uuid in (select private.user_office_ids())
  );

-- =====================================================================================
-- 1e. Drop the now-unreferenced public helpers. Safe: all 43 policies and all 8
--     caller function bodies above now point at `private.*`.
-- =====================================================================================
drop function if exists public.can_access_office(uuid);
drop function if exists public.is_super_admin();
drop function if exists public.user_office_ids();

-- =====================================================================================
-- 2. Add a fixed search_path to the functions that were missing it (lint 0011).
--    ALTER FUNCTION only changes the config; the bodies are left untouched.
-- =====================================================================================
alter function public.set_updated_at() set search_path = public;
alter function public.reset_lead_markers_after_lead_change() set search_path = public;
alter function public.reset_reviewed_marker_after_event_change() set search_path = public;

-- =====================================================================================
-- 3. Lock down trigger/internal functions: no role should be able to call them over
--    the API. Triggers still fire (trigger execution does not check EXECUTE), and
--    SECURITY DEFINER functions invoked internally run as their owner. Clears 0028/0029.
-- =====================================================================================
revoke all on function public.handle_new_user() from anon, authenticated, public;
revoke all on function public.profiles_guard_sensitive_fields() from anon, authenticated, public;
revoke all on function public.trg_refresh_lead_last_comment() from anon, authenticated, public;
revoke all on function public.refresh_lead_last_comment(uuid) from anon, authenticated, public;
revoke all on function public.set_updated_at() from anon, authenticated, public;
revoke all on function public.reset_lead_markers_after_lead_change() from anon, authenticated, public;
revoke all on function public.reset_reviewed_marker_after_event_change() from anon, authenticated, public;

-- =====================================================================================
-- 4. Real RPCs meant only for signed-in users: remove anon (and PUBLIC) EXECUTE,
--    keep EXECUTE for authenticated. Clears 0028 (anon). The remaining 0029
--    (authenticated) on these is intentional and accepted.
-- =====================================================================================
revoke execute on function public.get_dashboard_overview(uuid, integer) from anon, public;
grant execute on function public.get_dashboard_overview(uuid, integer) to authenticated;

revoke execute on function public.get_workflow_dashboard(uuid, uuid, integer, timestamptz, timestamptz) from anon, public;
grant execute on function public.get_workflow_dashboard(uuid, uuid, integer, timestamptz, timestamptz) to authenticated;

revoke execute on function public.take_lead_in_work(uuid) from anon, public;
grant execute on function public.take_lead_in_work(uuid) to authenticated;

-- =====================================================================================
-- 5. Future-proofing: functions later created in `public` by the postgres role do not
--    auto-grant EXECUTE to anon/PUBLIC anymore. (Affects future creations only.)
-- =====================================================================================
alter default privileges for role postgres in schema public revoke execute on functions from anon, public;
