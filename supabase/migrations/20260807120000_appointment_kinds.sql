-- KOLSS appointment kinds.
-- lead_showroom_visits now stores two kinds of scheduled visit: the showroom
-- meeting it was built for, and an on-site furniture measurement at the
-- customer's place. Both occupy the responsible manager's calendar time.

alter table public.lead_showroom_visits
  add column if not exists kind text not null default 'showroom';

alter table public.lead_showroom_visits
  drop constraint if exists lead_showroom_visits_kind_check,
  add constraint lead_showroom_visits_kind_check
    check (kind in ('showroom', 'measurement'));

-- A lead may hold one active appointment per kind: a showroom meeting and a
-- measurement can be booked at the same time. Replaces the per-lead index from
-- 20260723120000_appointments_calendar.sql.
drop index if exists lead_showroom_visits_one_scheduled_idx;

create unique index if not exists lead_showroom_visits_one_scheduled_kind_idx
  on public.lead_showroom_visits (lead_id, kind)
  where status = 'scheduled';

-- lead_showroom_visits_manager_range_idx already covers the cross-kind overlap
-- lookup that blocks double-booking a manager, so no extra index is needed.

alter table public.leads
  drop constraint if exists leads_client_status_check,
  add constraint leads_client_status_check check (
    client_status in (
      'new_lead',
      'showroom_invited',
      'measurement_scheduled',
      'calculation_in_progress',
      'thinking',
      'closed_lost',
      'contract_signed'
    )
  );
