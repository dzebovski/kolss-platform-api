-- CRM v2 (W7): the "Lead info" answers of lead card v1.3 (contract §2 "C2 Lead info").
-- Additive only: new nullable columns v1 never reads. Budget text and products already exist (W4).
-- The marker reset trigger compares an explicit column list without these columns, so a lead info
-- edit keeps the v1 markers (only its timeline event clears `reviewed`, like any event).
alter table public.leads
  add column if not exists material_fronts text,
  add column if not exists material_worktop text,
  add column if not exists material_appliances text,
  add column if not exists expected_lead_time text,
  add column if not exists preferred_measurement_at timestamptz;

alter table public.leads
  drop constraint if exists leads_material_fronts_check,
  add constraint leads_material_fronts_check check (
    material_fronts is null or char_length(material_fronts) <= 200
  ),
  drop constraint if exists leads_material_worktop_check,
  add constraint leads_material_worktop_check check (
    material_worktop is null or char_length(material_worktop) <= 200
  ),
  drop constraint if exists leads_material_appliances_check,
  add constraint leads_material_appliances_check check (
    material_appliances is null or char_length(material_appliances) <= 200
  ),
  drop constraint if exists leads_expected_lead_time_check,
  add constraint leads_expected_lead_time_check check (
    expected_lead_time is null or char_length(expected_lead_time) <= 60
  );
