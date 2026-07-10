-- Role-aware dashboard data in one office-scoped request.
-- Financial totals are intentionally excluded because offices use different currencies.

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
      and public.can_access_office(o.id)
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

grant execute on function public.get_dashboard_overview(uuid, integer) to authenticated;
