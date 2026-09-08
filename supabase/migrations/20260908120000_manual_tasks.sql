-- Standalone manual tasks belong to an office rather than a lead or project.
-- Keep entity-linked historical tasks intact and keep profile references nullable
-- so the existing profile deletion behaviour remains valid.

alter table public.tasks
  alter column entity_type drop not null,
  alter column entity_id drop not null,
  alter column due_at drop not null,
  add column if not exists office_id uuid references public.offices (id),
  add column if not exists version bigint not null default 1,
  add column if not exists updated_at timestamptz not null default now(),
  add column if not exists updated_by uuid references public.profiles (id) on delete set null,
  add column if not exists completed_by uuid references public.profiles (id) on delete set null,
  add column if not exists canceled_at timestamptz,
  add column if not exists canceled_by uuid references public.profiles (id) on delete set null;

alter table public.tasks drop constraint if exists tasks_entity_pair_check;
alter table public.tasks add constraint tasks_entity_pair_check
  check ((entity_type is null) = (entity_id is null));

alter table public.tasks drop constraint if exists tasks_standalone_office_check;
alter table public.tasks add constraint tasks_standalone_office_check
  check (entity_type is not null or office_id is not null);

create index if not exists tasks_manual_office_status_due_idx
  on public.tasks (office_id, status, due_at, created_at)
  where entity_type is null and entity_id is null;

-- Direct authenticated access keeps legacy task permissions, while standalone
-- rows are always scoped through their stored office (never assignee-only).
drop policy if exists tasks_select on public.tasks;
create policy tasks_select on public.tasks
  for select to authenticated
  using (
    (
      entity_type is not null and (
        assignee_id = auth.uid()
        or private.is_super_admin()
        or (entity_type = 'lead' and exists (
          select 1 from public.leads l
          where l.id = entity_id and private.can_access_office(l.office_id)
        ))
        or (entity_type = 'project' and exists (
          select 1 from public.projects p
          where p.id = entity_id and private.can_access_office(p.office_id)
        ))
      )
    )
    or (
      entity_type is null and entity_id is null
      and private.can_access_office(office_id)
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
    or (
      entity_type is null and entity_id is null
      and private.can_access_office(office_id)
      and exists (
        select 1 from public.profiles p
        join public.user_office_memberships m on m.user_id = p.id
        where p.id = assignee_id and p.is_active = true and m.office_id = tasks.office_id
      )
    )
  );

drop policy if exists tasks_update on public.tasks;
create policy tasks_update on public.tasks
  for update to authenticated
  using (
    (
      entity_type is not null and (
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
      )
    )
    or (
      entity_type is null and entity_id is null
      and private.can_access_office(office_id)
    )
  )
  with check (
    (
      entity_type is not null and (
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
      )
    )
    or (
      entity_type is null and entity_id is null
      and private.can_access_office(office_id)
    )
  );

grant select, insert, update on public.tasks to kolss_api;

-- The API authenticates users and checks office access before every query.
-- Its database role is separate from authenticated, so a grant alone does not
-- permit it through RLS. Keep its access limited to the new standalone tasks.
create policy tasks_api_runtime on public.tasks
  for all to kolss_api
  using (
    entity_type is null and entity_id is null
    and source = 'manual' and task_type = 'manual'
  )
  with check (
    entity_type is null and entity_id is null
    and source = 'manual' and task_type = 'manual'
  );

-- The existing task trigger predates nullable entity links. Avoid calling the
-- lead refresh function for standalone rows.
create or replace function public.trg_refresh_lead_last_comment()
returns trigger
language plpgsql
security definer
set search_path = public
as $$
declare
  v_lead_id uuid;
begin
  if tg_table_name = 'lead_comments' then
    v_lead_id := coalesce(new.lead_id, old.lead_id);
  elsif tg_table_name = 'tasks' then
    if coalesce(new.entity_type, old.entity_type) is distinct from 'lead'
       or coalesce(new.entity_id, old.entity_id) is null then
      return coalesce(new, old);
    end if;
    v_lead_id := coalesce(new.entity_id, old.entity_id);
  elsif tg_table_name = 'lead_contact_attempts' then
    v_lead_id := coalesce(new.lead_id, old.lead_id);
  end if;

  if v_lead_id is not null then
    perform public.refresh_lead_last_comment(v_lead_id);
  end if;

  return coalesce(new, old);
end;
$$;
