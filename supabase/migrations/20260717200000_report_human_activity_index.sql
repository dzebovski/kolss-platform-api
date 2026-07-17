-- Speed up date-range reports that select leads by human CRM activity.

create index if not exists lead_events_human_activity_created_idx
  on public.lead_events (created_at desc, lead_id)
  where actor_id is not null
    and event_category is distinct from 'system';
