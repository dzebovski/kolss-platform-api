-- Re-add "callback_required" workflow status for first-call no_answer.

begin;

insert into public.lead_workflow_statuses (code, sort_order, category, is_terminal)
values ('callback_required', 15, 'intake', false)
on conflict (code) do update
set
  sort_order = excluded.sort_order,
  category = excluded.category,
  is_terminal = excluded.is_terminal;

commit;
