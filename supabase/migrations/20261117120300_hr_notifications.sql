-- HR portal (HB5): transactional outbox for HR Slack messages (D3: every create / change /
-- delete posts to the HR channel, C0C03GC1Q8K, configured by SLACK_CHANNEL_ID_HR).
--
-- Same delivery columns as lead_notifications so the existing dispatcher can claim, retry and
-- mark rows; the message text is built by the API when the change is saved (payload.text).
--
-- v1 impact: none.

begin;

create table if not exists public.hr_notifications (
  id uuid primary key default gen_random_uuid(),
  channel public.notification_channel not null default 'slack',
  destination text not null,
  status public.notification_status not null default 'pending',
  payload jsonb not null default '{}'::jsonb,
  attempts int not null default 0,
  last_error text,
  next_attempt_at timestamptz not null default now(),
  claimed_at timestamptz,
  claim_token uuid,
  sent_at timestamptz,
  created_at timestamptz not null default now()
);

create index if not exists hr_notifications_ready_idx
  on public.hr_notifications (next_attempt_at, created_at)
  where status in ('pending', 'failed');

alter table public.hr_notifications enable row level security;
drop policy if exists hr_notifications_api_runtime on public.hr_notifications;
create policy hr_notifications_api_runtime on public.hr_notifications for all to kolss_api using (true) with check (true);

grant select, insert, update on public.hr_notifications to kolss_api;

commit;
