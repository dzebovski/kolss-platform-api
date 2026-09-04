-- Office work is a calendar block linked to a lead. Unlike showroom meetings
-- and measurements, it does not move the lead through the sales workflow and
-- a lead may have several scheduled office-work blocks.

alter table public.lead_showroom_visits
  drop constraint if exists lead_showroom_visits_kind_check,
  add constraint lead_showroom_visits_kind_check
    check (kind in ('showroom', 'measurement', 'office_work'));

drop index if exists lead_showroom_visits_one_scheduled_kind_idx;

create unique index lead_showroom_visits_one_scheduled_kind_idx
  on public.lead_showroom_visits (lead_id, kind)
  where status = 'scheduled'
    and kind in ('showroom', 'measurement');
