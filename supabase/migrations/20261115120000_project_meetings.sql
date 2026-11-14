-- CRM v2 (P12): meetings and tasks of a project (owner decision of 2026-11-14: full P12).
--
-- * A meeting (row of lead_showroom_visits) may belong to a project. It still points at the
--   project's lead (calendar, manager overlap, Tasks page keep working), but a project meeting
--   never changes the lead's statuses and writes project timeline events instead of lead events.
-- * Manual tasks already have tasks.link_project_id (2026-09-29); the API now accepts project
--   links. No schema change for tasks.
-- * New project timeline events: meeting_scheduled, meeting_rescheduled, meeting_updated,
--   meeting_status_changed, task_added.
--
-- v1 impact: none (v1 ignores the new nullable column; project meetings show in the v1 calendar
-- like any other visit of the lead).

begin;

alter table public.lead_showroom_visits
  add column if not exists project_id uuid references public.projects(id) on delete set null;

create index if not exists lead_showroom_visits_project_id_idx
  on public.lead_showroom_visits (project_id, scheduled_at desc)
  where project_id is not null;

alter table public.project_events
  drop constraint if exists project_events_event_type_check,
  add constraint project_events_event_type_check check (
    event_type in (
      'created', 'status_changed', 'status_updated', 'cancelled', 'restored', 'finished',
      'estimate_updated', 'contract_added', 'contract_updated', 'contract_deleted',
      'payment_added', 'payment_updated', 'payment_deleted', 'file_added', 'file_deleted',
      'details_updated', 'lead_info_updated', 'comment', 'measurement_visit',
      'meeting_scheduled', 'meeting_rescheduled', 'meeting_updated', 'meeting_status_changed',
      'task_added'
    )
  );

commit;
