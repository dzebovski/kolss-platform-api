-- Keep API-only operational tables out of the PostgREST browser roles while
-- preserving the least privileges already granted to the Go API runtime.

begin;

alter table public.lead_markers enable row level security;
alter table public.daily_report_runs enable row level security;

revoke all privileges on table public.lead_markers, public.daily_report_runs
  from public, anon, authenticated;

drop policy if exists lead_markers_api_runtime on public.lead_markers;
create policy lead_markers_api_runtime on public.lead_markers
  for all to kolss_api
  using (true)
  with check (true);

drop policy if exists daily_report_runs_api_select on public.daily_report_runs;
create policy daily_report_runs_api_select on public.daily_report_runs
  for select to kolss_api
  using (true);

drop policy if exists daily_report_runs_api_insert on public.daily_report_runs;
create policy daily_report_runs_api_insert on public.daily_report_runs
  for insert to kolss_api
  with check (true);

commit;
