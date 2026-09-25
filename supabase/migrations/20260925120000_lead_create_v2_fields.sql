-- CRM v2 (W8): Create lead popup fields not covered by earlier W tasks (contract, task W8).
-- Additive only: new nullable columns v1 never reads. channel, products, estimated_budget_text
-- and estimated_budget_currency already exist (W3, W4/W7); only referred_by and about_client
-- are new. Named for reuse by PATCH /v1/leads/{leadId}/info in W9.
alter table public.leads
  add column if not exists referred_by text,
  add column if not exists about_client text;

alter table public.leads
  drop constraint if exists leads_referred_by_check,
  add constraint leads_referred_by_check check (
    referred_by is null or char_length(referred_by) <= 200
  ),
  drop constraint if exists leads_about_client_check,
  add constraint leads_about_client_check check (
    about_client is null or char_length(about_client) <= 2000
  );
