-- Optional lead quality marker for manager prioritization (does not close the lead).
alter table public.leads
  add column if not exists lead_quality text
    check (lead_quality is null or lead_quality in ('good', 'bad'));

comment on column public.leads.lead_quality is
  'Manager quality marker: good or bad. Independent of workflow_status.';
