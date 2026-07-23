-- KOLSS appointments calendar.
-- lead_showroom_visits remains the canonical appointment table; external
-- calendar providers can link to its stable id/version in a later migration.

alter table public.offices
  add column if not exists timezone_name text;

update public.offices
set timezone_name = case code
  when 'warsaw' then 'Europe/Warsaw'
  when 'kyiv' then 'Europe/Kyiv'
  else 'UTC'
end
where timezone_name is null or btrim(timezone_name) = '';

alter table public.offices
  alter column timezone_name set not null;

alter table public.offices
  drop constraint if exists offices_timezone_name_not_blank,
  add constraint offices_timezone_name_not_blank
    check (btrim(timezone_name) <> '');

alter table public.lead_showroom_visits
  add column if not exists ends_at timestamptz,
  add column if not exists responsible_manager_id uuid,
  add column if not exists updated_by uuid,
  add column if not exists version bigint not null default 1;

alter table public.lead_showroom_visits
  drop constraint if exists lead_showroom_visits_responsible_manager_id_fkey,
  add constraint lead_showroom_visits_responsible_manager_id_fkey
    foreign key (responsible_manager_id)
    references public.profiles (id)
    on delete set null,
  drop constraint if exists lead_showroom_visits_updated_by_fkey,
  add constraint lead_showroom_visits_updated_by_fkey
    foreign key (updated_by)
    references public.profiles (id)
    on delete set null;

update public.lead_showroom_visits v
set
  ends_at = coalesce(v.ends_at, v.scheduled_at + interval '60 minutes'),
  responsible_manager_id = coalesce(
    v.responsible_manager_id,
    l.assigned_to,
    v.created_by
  ),
  updated_by = coalesce(v.updated_by, v.created_by)
from public.leads l
where l.id = v.lead_id
  and (
    v.ends_at is null
    or v.responsible_manager_id is null
    or v.updated_by is null
  );

alter table public.lead_showroom_visits
  alter column ends_at set not null,
  drop constraint if exists lead_showroom_visits_valid_range,
  add constraint lead_showroom_visits_valid_range
    check (ends_at > scheduled_at),
  drop constraint if exists lead_showroom_visits_version_positive,
  add constraint lead_showroom_visits_version_positive
    check (version > 0);

alter table public.lead_showroom_visits
  drop constraint if exists lead_showroom_visits_created_by_fkey,
  alter column created_by drop not null,
  add constraint lead_showroom_visits_created_by_fkey
    foreign key (created_by)
    references public.profiles (id)
    on delete set null;

-- Historical data could contain more than one row still marked scheduled.
-- Keep the newest as the active appointment and retain older rows as history.
with ranked as (
  select
    id,
    row_number() over (
      partition by lead_id
      order by scheduled_at desc, created_at desc, id desc
    ) as position
  from public.lead_showroom_visits
  where status = 'scheduled'
)
update public.lead_showroom_visits v
set
  status = 'rescheduled',
  updated_at = now(),
  version = v.version + 1
from ranked r
where r.id = v.id
  and r.position > 1;

-- Backfill active showroom invitations that predate visit rows. Only ISO
-- timestamps written by the legacy workflow are considered valid.
with latest_invitation as (
  select distinct on (e.lead_id)
    e.lead_id,
    (e.new_value->>'callback_due_at')::timestamptz as scheduled_at,
    e.actor_id
  from public.lead_events e
  join public.leads l on l.id = e.lead_id
  where l.archived_at is null
    and l.client_status = 'showroom_invited'
    and e.event_category = 'client_status'
    and e.status_code = 'showroom_invited'
    and jsonb_typeof(e.new_value->'callback_due_at') = 'string'
    and (e.new_value->>'callback_due_at')
      ~ '^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2}(\.\d+)?)?(Z|[+-]\d{2}:\d{2})$'
  order by e.lead_id, e.created_at desc
)
insert into public.lead_showroom_visits (
  lead_id,
  scheduled_at,
  ends_at,
  status,
  responsible_manager_id,
  created_by,
  updated_by
)
select
  l.id,
  i.scheduled_at,
  i.scheduled_at + interval '60 minutes',
  'scheduled',
  coalesce(l.assigned_to, i.actor_id),
  i.actor_id,
  i.actor_id
from latest_invitation i
join public.leads l on l.id = i.lead_id
where not exists (
  select 1
  from public.lead_showroom_visits v
  where v.lead_id = l.id
    and v.status = 'scheduled'
);

create unique index if not exists lead_showroom_visits_one_scheduled_idx
  on public.lead_showroom_visits (lead_id)
  where status = 'scheduled';

create index if not exists lead_showroom_visits_range_status_idx
  on public.lead_showroom_visits (scheduled_at, ends_at, status);

create index if not exists lead_showroom_visits_manager_range_idx
  on public.lead_showroom_visits (
    responsible_manager_id,
    scheduled_at,
    ends_at
  )
  where status = 'scheduled';

grant select, update on public.offices to kolss_api;
grant select, insert, update on public.lead_showroom_visits to kolss_api;
