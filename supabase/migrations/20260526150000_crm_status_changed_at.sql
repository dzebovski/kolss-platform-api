alter table public.leads
  add column if not exists crm_status_changed_at timestamptz;

update public.leads
set crm_status_changed_at = created_at
where crm_status_changed_at is null;

alter table public.leads
  alter column crm_status_changed_at set default now();
