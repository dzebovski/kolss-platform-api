-- "Duplicate of…" (v1 lead card): a repeat request is closed with loss reason `duplicate` and
-- points at the original lead. Reports skip leads with duplicate_of_lead_id; reopening the
-- duplicate clears it. Deleting the original keeps the duplicate as a closed lead.
alter table public.leads
  add column if not exists duplicate_of_lead_id uuid references public.leads (id) on delete set null;

create index if not exists leads_duplicate_of_lead_id_idx
  on public.leads (duplicate_of_lead_id)
  where duplicate_of_lead_id is not null;

-- Not a v2 loss reason: v2 never offers it, the action sets it.
insert into public.loss_reasons (code, label_uk, label_pl, label_en, is_v2) values
  ('duplicate', 'Дубль заявки', 'Duplikat zgłoszenia', 'Duplicate request', false)
on conflict (code) do nothing;
