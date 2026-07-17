-- Idempotency claim for the daily Telegram lead report so a redeploy near 09:00
-- cannot send the same office report twice on the same local date.

begin;

create table if not exists public.daily_report_runs (
  office_code text not null,
  report_date date not null,
  sent_at timestamptz not null default now(),
  primary key (office_code, report_date)
);

grant select, insert on public.daily_report_runs to kolss_api;

commit;
