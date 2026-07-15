-- Add "thinking" workflow status and allow kolss_api to permanently delete archived leads.

begin;

insert into public.lead_workflow_statuses (code, sort_order, category, is_terminal)
values ('thinking', 50, 'sales', false)
on conflict (code) do update
set
  sort_order = excluded.sort_order,
  category = excluded.category,
  is_terminal = excluded.is_terminal;

grant delete on public.leads to kolss_api;
grant delete on public.projects to kolss_api;
grant delete on public.project_comments to kolss_api;
grant delete on public.project_attachments to kolss_api;
grant delete on public.lead_events to kolss_api;
grant delete on public.lead_comments to kolss_api;
grant delete on public.lead_contact_attempts to kolss_api;
grant delete on public.lead_showroom_visits to kolss_api;
grant delete on public.lead_contracts to kolss_api;
grant delete on public.lead_payments to kolss_api;
grant delete on public.lead_attachments to kolss_api;
grant delete on public.lead_notifications to kolss_api;

commit;
