-- Closed leads must not carry reminders.
--
-- Until now a lead moved to a terminal client status (closed_lost /
-- contract_signed) kept leads.callback_due_at whenever call_status was
-- 'callback_requested', and contract_signed never cancelled its scheduled
-- visits. Both leaked into the reminder lists and the calendar.
--
-- The API now clears these on the close transition; this backfill cleans the
-- rows closed before that change. No lead_events are written — a backfill has
-- no actor.

update public.leads
set
  callback_due_at = null,
  updated_at = now()
where client_status in ('closed_lost', 'contract_signed')
  and callback_due_at is not null;

update public.lead_showroom_visits v
set
  status = 'canceled',
  updated_at = now(),
  version = v.version + 1
from public.leads l
where l.id = v.lead_id
  and v.status = 'scheduled'
  and l.client_status in ('closed_lost', 'contract_signed');
