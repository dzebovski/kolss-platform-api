-- Track each Telegram delivery destination independently so one failed chat retry
-- cannot resend a notification to a chat that already received it.
alter table public.lead_notifications
  add column if not exists destination text not null default '';

alter table public.lead_notifications
  drop constraint if exists lead_notifications_lead_id_channel_key;

create unique index if not exists lead_notifications_lead_channel_destination_key
  on public.lead_notifications (lead_id, channel, destination);
