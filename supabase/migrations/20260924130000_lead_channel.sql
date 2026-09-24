-- CRM v2 (W3): lead channel, a new field; v1 source_system / source_channel stay untouched.
-- Additive: nullable, v1 never reads it. Referral, Phone and Google Ads are set by hand through
-- PATCH /v1/leads/{leadId}; `other` marks the old Google Sheet import (display only).
alter table public.leads
  add column if not exists channel text;

alter table public.leads
  drop constraint if exists leads_channel_check,
  add constraint leads_channel_check check (
    channel is null or channel in
      ('referral', 'phone', 'office', 'website', 'meta_ads', 'google_ads', 'other')
  );

-- One rule for the backfill and for new leads (contract §6.11): the channel follows the source.
create or replace function private.lead_channel_from_source(p_source_system text)
returns text
language sql
immutable
set search_path = ''
as $$
  select case p_source_system
    when 'meta_lead_ads' then 'meta_ads'
    when 'site_form' then 'website'
    when 'google_ads' then 'google_ads'
    when 'manual' then 'office'
    when 'google_sheet_backfill' then 'other'
    else null
  end
$$;

-- Every insert path (Meta processor, site forms, manual create, Google Sheet import) gets a
-- channel without code changes; an explicit channel on insert wins. Security definer, like
-- private.protect_and_assign_lead_reference_id: the API roles have no rights on `private`.
create or replace function private.set_lead_default_channel()
returns trigger
language plpgsql
security definer
set search_path = ''
as $$
begin
  if new.channel is null then
    new.channel := private.lead_channel_from_source(new.source_system);
  end if;
  return new;
end
$$;

revoke all on function private.lead_channel_from_source(text) from public;
revoke all on function private.set_lead_default_channel() from public;

drop trigger if exists leads_default_channel on public.leads;
create trigger leads_default_channel
before insert on public.leads
for each row execute function private.set_lead_default_channel();

-- Backfill without touching what v1 reads: leads_updated_at would stamp every lead as just
-- updated. The marker reset trigger ignores `channel`, and the reference id guard only fires
-- for office_id / reference_id.
alter table public.leads disable trigger leads_updated_at;
update public.leads
set channel = private.lead_channel_from_source(source_system)
where channel is null;
alter table public.leads enable trigger leads_updated_at;
