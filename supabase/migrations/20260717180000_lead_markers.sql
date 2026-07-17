-- Shared lead review markers used by the CRM dashboard and lead detail drawer.

create table if not exists public.lead_markers (
  lead_id uuid not null references public.leads (id) on delete cascade,
  kind text not null,
  actor_id uuid not null references public.profiles (id) on delete cascade,
  marked_at timestamptz not null default now(),
  primary key (lead_id, kind),
  constraint lead_markers_kind_check check (kind in ('reviewed', 'manager_aware'))
);

create index if not exists lead_markers_actor_idx
  on public.lead_markers (actor_id, marked_at desc);

grant select, insert, update, delete on public.lead_markers to kolss_api;

-- Reset only for meaningful CRM changes. Import bookkeeping such as raw_payload,
-- updated_at and version-only updates deliberately does not invalidate a review.
create or replace function public.reset_lead_markers_after_lead_change()
returns trigger
language plpgsql
as $$
begin
  if row(
    old.office_id,
    old.lead_status,
    old.workflow_status,
    old.call_status,
    old.client_status,
    old.assigned_to,
    old.loss_reason,
    old.converted_project_id,
    old.estimated_budget,
    old.our_quote,
    old.callback_due_at,
    old.source_channel,
    old.source_note,
    old.next_task_due_at,
    old.next_task_title,
    old.last_comment,
    old.name,
    old.phone,
    old.email,
    old.product_interest,
    old.order_comment,
    old.city_region,
    old.project_stage_source,
    old.source_created_at,
    old.archived_at
  ) is distinct from row(
    new.office_id,
    new.lead_status,
    new.workflow_status,
    new.call_status,
    new.client_status,
    new.assigned_to,
    new.loss_reason,
    new.converted_project_id,
    new.estimated_budget,
    new.our_quote,
    new.callback_due_at,
    new.source_channel,
    new.source_note,
    new.next_task_due_at,
    new.next_task_title,
    new.last_comment,
    new.name,
    new.phone,
    new.email,
    new.product_interest,
    new.order_comment,
    new.city_region,
    new.project_stage_source,
    new.source_created_at,
    new.archived_at
  ) then
    delete from public.lead_markers
    where lead_id = new.id and kind = 'reviewed';
  end if;

  if old.assigned_to is distinct from new.assigned_to then
    delete from public.lead_markers
    where lead_id = new.id and kind = 'manager_aware';
  end if;

  return new;
end
$$;

drop trigger if exists reset_lead_markers_after_lead_change on public.leads;
create trigger reset_lead_markers_after_lead_change
after update on public.leads
for each row execute function public.reset_lead_markers_after_lead_change();

create or replace function public.reset_reviewed_marker_after_event_change()
returns trigger
language plpgsql
as $$
declare
  target_lead_id uuid;
begin
  target_lead_id := coalesce(new.lead_id, old.lead_id);
  delete from public.lead_markers
  where lead_id = target_lead_id and kind = 'reviewed';
  if tg_op = 'DELETE' then
    return old;
  end if;
  return new;
end
$$;

drop trigger if exists reset_reviewed_marker_after_event_change on public.lead_events;
create trigger reset_reviewed_marker_after_event_change
after insert or update or delete on public.lead_events
for each row execute function public.reset_reviewed_marker_after_event_change();
