-- Simplified CRM workflow statuses for Angular CRM (Phase 3).
-- Canonical migration: apply via `npx supabase db push` from kolss-crm repo.

begin;

insert into public.lead_workflow_statuses (code, sort_order, category, is_terminal)
values
  ('taken', 10, 'intake', false),
  ('first_call_done', 20, 'sales', false),
  ('visit_scheduled', 30, 'sales', false),
  ('visit_rescheduled', 35, 'sales', false),
  ('visit_completed', 40, 'sales', false),
  ('closed', 190, 'terminal', true),
  ('successful', 180, 'terminal', true)
on conflict (code) do update
set
  sort_order = excluded.sort_order,
  category = excluded.category,
  is_terminal = excluded.is_terminal;

update public.leads
set workflow_status = case workflow_status
  when 'in_work' then 'taken'
  when 'callback_required' then 'taken'
  when 'contacted' then 'first_call_done'
  when 'showroom_scheduled' then 'visit_scheduled'
  when 'showroom_no_show' then 'visit_rescheduled'
  when 'showroom_visited' then 'visit_completed'
  when 'contract_planned' then 'visit_completed'
  when 'contract_signed' then 'successful'
  when 'prepayment_received' then 'successful'
  when 'in_production' then 'successful'
  when 'postpayment_received' then 'successful'
  when 'installed' then 'successful'
  when 'warranty' then 'successful'
  when 'bad_lead' then 'closed'
  else workflow_status
end
where workflow_status in (
  'in_work',
  'callback_required',
  'contacted',
  'showroom_scheduled',
  'showroom_visited',
  'showroom_no_show',
  'contract_planned',
  'contract_signed',
  'prepayment_received',
  'in_production',
  'postpayment_received',
  'installed',
  'warranty',
  'bad_lead'
);

delete from public.lead_workflow_statuses
where code in (
  'in_work',
  'callback_required',
  'contacted',
  'showroom_scheduled',
  'showroom_visited',
  'showroom_no_show',
  'contract_planned',
  'contract_signed',
  'prepayment_received',
  'in_production',
  'postpayment_received',
  'installed',
  'warranty',
  'bad_lead'
);

commit;
