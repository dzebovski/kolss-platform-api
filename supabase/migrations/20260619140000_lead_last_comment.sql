-- Denormalized last manager comment / task for leads list
alter table public.leads
  add column if not exists last_comment text,
  add column if not exists last_comment_at timestamptz;

create or replace function public.refresh_lead_last_comment(p_lead_id uuid)
returns void
language plpgsql
security definer
set search_path = public
as $$
declare
  v_text text;
  v_at timestamptz;
begin
  select text, at
  into v_text, v_at
  from (
    select c.body as text, c.created_at as at
    from public.lead_comments c
    where c.lead_id = p_lead_id
    union all
    select t.title, t.created_at
    from public.tasks t
    where t.entity_type = 'lead'
      and t.entity_id = p_lead_id
    union all
    select ca.comment, ca.created_at
    from public.lead_contact_attempts ca
    where ca.lead_id = p_lead_id
      and ca.comment is not null
      and btrim(ca.comment) <> ''
  ) candidates
  order by at desc nulls last
  limit 1;

  update public.leads
  set
    last_comment = v_text,
    last_comment_at = v_at
  where id = p_lead_id;
end;
$$;

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
    if coalesce(new.entity_type, old.entity_type) <> 'lead' then
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

drop trigger if exists lead_comments_refresh_last_comment on public.lead_comments;
create trigger lead_comments_refresh_last_comment
  after insert or update or delete on public.lead_comments
  for each row execute function public.trg_refresh_lead_last_comment();

drop trigger if exists tasks_refresh_last_comment on public.tasks;
create trigger tasks_refresh_last_comment
  after insert or update or delete on public.tasks
  for each row execute function public.trg_refresh_lead_last_comment();

drop trigger if exists lead_contact_attempts_refresh_last_comment on public.lead_contact_attempts;
create trigger lead_contact_attempts_refresh_last_comment
  after insert or update or delete on public.lead_contact_attempts
  for each row execute function public.trg_refresh_lead_last_comment();

do $$
declare
  lead_row record;
begin
  for lead_row in select id from public.leads loop
    perform public.refresh_lead_last_comment(lead_row.id);
  end loop;
end;
$$;
