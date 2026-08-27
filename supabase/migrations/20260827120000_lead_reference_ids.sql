-- Stable, human-readable lead references with an independent sequence per office.

create table private.lead_reference_counters (
  office_id uuid primary key references public.offices (id) on delete restrict,
  prefix text not null unique check (prefix ~ '^[a-z]$'),
  last_value bigint not null default 0 check (last_value >= 0)
);

insert into private.lead_reference_counters (office_id, prefix)
select
  id,
  case code
    when 'kyiv' then 'k'
    when 'warsaw' then 'w'
  end
from public.offices
where code in ('kyiv', 'warsaw');

do $$
begin
  if (select count(*) from private.lead_reference_counters) <> 2 then
    raise exception 'lead reference counters require both kyiv and warsaw offices';
  end if;
end
$$;

alter table public.leads
  add column reference_id text;

with ranked as (
  select
    l.id,
    c.prefix,
    row_number() over (
      partition by l.office_id
      order by l.created_at, l.id
    ) as reference_number
  from public.leads l
  join private.lead_reference_counters c on c.office_id = l.office_id
)
update public.leads l
set reference_id = ranked.prefix || lpad(ranked.reference_number::text, 4, '0')
from ranked
where ranked.id = l.id;

update private.lead_reference_counters c
set last_value = (
  select count(*)
  from public.leads l
  where l.office_id = c.office_id
);

alter table public.leads
  alter column reference_id set not null,
  add constraint leads_reference_id_format check (reference_id ~ '^[kw][0-9]{4,}$'),
  add constraint leads_reference_id_unique unique (reference_id);

create or replace function private.protect_and_assign_lead_reference_id()
returns trigger
language plpgsql
security definer
set search_path = ''
as $$
declare
  next_prefix text;
  next_value bigint;
begin
  if tg_op = 'UPDATE' then
    if new.office_id is distinct from old.office_id then
      raise exception 'lead office_id is immutable';
    end if;
    if new.reference_id is distinct from old.reference_id then
      raise exception 'lead reference_id is immutable';
    end if;
    return new;
  end if;

  if new.reference_id is not null then
    raise exception 'lead reference_id is database-generated';
  end if;

  update private.lead_reference_counters
  set last_value = last_value + 1
  where office_id = new.office_id
  returning prefix, last_value into next_prefix, next_value;

  if not found then
    raise exception 'lead reference prefix is not configured for office %', new.office_id;
  end if;

  new.reference_id := next_prefix || lpad(next_value::text, 4, '0');
  return new;
end
$$;

revoke all on function private.protect_and_assign_lead_reference_id() from public;

create trigger leads_reference_id_guard
before insert or update of office_id, reference_id on public.leads
for each row execute function private.protect_and_assign_lead_reference_id();

