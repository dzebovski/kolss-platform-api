-- CRM v2 (W2): lead rating Cold / Medium / Hot, a new field separate from lead_quality
-- (decision D3). Additive: nullable, v1 never reads it. Changes are written by the `rating`
-- activity as `rating_changed` events in the existing `system` category.
alter table public.leads
  add column if not exists rating text;

alter table public.leads
  drop constraint if exists leads_rating_check,
  add constraint leads_rating_check check (rating is null or rating in ('cold', 'medium', 'hot'));

create index if not exists leads_rating_idx on public.leads (rating) where rating is not null;
