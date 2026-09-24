-- CRM v2 (W4): one v2 lead status that includes the call result (decision D1a), the no-answer
-- attempt counter and the Successful call answers (budget as text, products). Additive only:
-- v1 never reads these columns; the API mirrors every v2 change into the v1 fields.
alter table public.leads
  add column if not exists v2_status text,
  add column if not exists v2_status_changed_at timestamptz,
  add column if not exists no_answer_attempts smallint not null default 0,
  add column if not exists estimated_budget_text text,
  add column if not exists products text[] not null default '{}';

alter table public.leads
  drop constraint if exists leads_v2_status_check,
  add constraint leads_v2_status_check check (
    v2_status is null or v2_status in
      ('new', 'later', 'noanswer', 'success', 'thinking', 'invited', 'lost', 'project')
  ),
  drop constraint if exists leads_no_answer_attempts_check,
  add constraint leads_no_answer_attempts_check check (no_answer_attempts >= 0),
  drop constraint if exists leads_estimated_budget_text_check,
  add constraint leads_estimated_budget_text_check check (
    estimated_budget_text is null or char_length(estimated_budget_text) <= 60
  ),
  drop constraint if exists leads_products_check,
  add constraint leads_products_check check (
    products <@ array['kitchen', 'wardrobe', 'furniture', 'bathroom', 'hallway', 'other']::text[]
  );

create index if not exists leads_v2_status_idx on public.leads (v2_status);

-- Backfill from the v1 fields with the rule the API uses for v1 activities (contract §3.6,
-- deriveV2LeadStatus): a client status beyond new_lead wins, then the call result, else new.
-- The legacy client statuses stay NULL (read-only in v2, decision D1c). v2_status_changed_at
-- takes the time of the field that decided. leads_updated_at is paused so v1 keeps its
-- updated_at; the marker reset trigger ignores these columns.
alter table public.leads disable trigger leads_updated_at;
update public.leads l
set v2_status = d.status,
    v2_status_changed_at = d.changed_at
from (
  select id,
    case
      when client_status = 'showroom_invited' then 'invited'
      when client_status = 'thinking' then 'thinking'
      when client_status = 'closed_lost' then 'lost'
      when client_status <> 'new_lead' then null
      when call_status = 'callback_requested' then 'later'
      when call_status = 'no_answer' then 'noanswer'
      when call_status = 'reached' then 'success'
      when call_status is null then 'new'
    end as status,
    case
      when client_status <> 'new_lead' then client_status_changed_at
      else call_status_changed_at
    end as changed_at
  from public.leads
) d
where d.id = l.id and l.v2_status is null;
alter table public.leads enable trigger leads_updated_at;
