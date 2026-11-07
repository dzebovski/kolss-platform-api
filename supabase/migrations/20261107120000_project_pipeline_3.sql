-- CRM v2 (P1c): project pipeline 3 — an update on top of pipeline 2.1 (owner decisions of 2026-11-07).
--
-- * Stages (in order): estimate, showroom_meeting, intro_contract, design (measurement and design
--   together), factory_review, presentation, main_contract, production, installation, completed,
--   plus cancelled. Dropped 2.1 stages are mapped: measure -> design, prepayment -> main_contract,
--   factory -> production, final_payment -> installation (also in status_before_cancel).
--   CRM v1 never reads projects.status (checked 2026-10-23), so only the v2 project API depends on it.
-- * Showroom appointments get an optional purpose: intro (getting to know) or presentation of the
--   project. Nullable, only for kind = showroom; CRM v1 ignores the column.
--
-- Additive for v1 (one nullable column; only the v2-only status column values change).

begin;

-- Status vocabulary -----------------------------------------------------------------------------
alter table public.projects
  drop constraint if exists projects_status_check,
  drop constraint if exists projects_status_before_cancel_check;

update public.projects
set status = case status
      when 'measure' then 'design'
      when 'prepayment' then 'main_contract'
      when 'factory' then 'production'
      when 'final_payment' then 'installation'
      else status
    end,
    status_before_cancel = case status_before_cancel
      when 'measure' then 'design'
      when 'prepayment' then 'main_contract'
      when 'factory' then 'production'
      when 'final_payment' then 'installation'
      else status_before_cancel
    end
where status in ('measure', 'prepayment', 'factory', 'final_payment')
   or status_before_cancel in ('measure', 'prepayment', 'factory', 'final_payment');

alter table public.projects
  add constraint projects_status_check check (
    status in (
      'estimate', 'showroom_meeting', 'intro_contract', 'design', 'factory_review', 'presentation',
      'main_contract', 'production', 'installation', 'completed', 'cancelled'
    )
  ),
  add constraint projects_status_before_cancel_check check (
    status_before_cancel is null or status_before_cancel in (
      'estimate', 'showroom_meeting', 'intro_contract', 'design', 'factory_review', 'presentation',
      'main_contract', 'production', 'installation', 'completed'
    )
  );

-- Showroom meeting purpose ----------------------------------------------------------------------
alter table public.lead_showroom_visits
  add column if not exists purpose text;

alter table public.lead_showroom_visits
  drop constraint if exists lead_showroom_visits_purpose_check,
  add constraint lead_showroom_visits_purpose_check check (
    purpose is null or (kind = 'showroom' and purpose in ('intro', 'presentation'))
  );

commit;
