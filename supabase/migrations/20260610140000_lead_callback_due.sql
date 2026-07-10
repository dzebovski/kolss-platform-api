-- Callback reminder on lead (no separate tasks/cron)

alter table public.leads
  add column if not exists callback_due_at timestamptz;

create index if not exists leads_callback_due_at_idx
  on public.leads (callback_due_at)
  where callback_due_at is not null;
