-- Questions are timeline-only events. They must remain separate from comment
-- events so they cannot become lead reminders, dashboard tasks, or report text.
begin;

alter table public.lead_events
  drop constraint if exists lead_events_event_category_check,
  add constraint lead_events_event_category_check check (
    event_category is null or event_category in ('call_status', 'client_status', 'comment', 'question', 'system')
  );

create index if not exists lead_events_question_timeline_idx
  on public.lead_events (lead_id, created_at desc)
  where event_category = 'question';

commit;
