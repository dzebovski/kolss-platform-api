-- Lead form fields + attachments

alter table public.leads
  add column if not exists order_comment text,
  add column if not exists stage_comment text;

create table if not exists public.lead_attachments (
  id uuid primary key default gen_random_uuid(),
  lead_id uuid not null references public.leads (id) on delete cascade,
  uploaded_by uuid not null references public.profiles (id) on delete cascade,
  file_name text not null,
  storage_path text not null,
  mime_type text not null,
  size_bytes int not null check (size_bytes > 0 and size_bytes <= 5242880),
  created_at timestamptz not null default now()
);

create index if not exists lead_attachments_lead_id_idx
  on public.lead_attachments (lead_id, created_at desc);

alter table public.lead_attachments enable row level security;

create policy lead_attachments_select on public.lead_attachments
  for select to authenticated
  using (
    exists (
      select 1 from public.leads l
      where l.id = lead_id and public.can_access_office(l.office_id)
    )
  );

create policy lead_attachments_insert on public.lead_attachments
  for insert to authenticated
  with check (
    uploaded_by = auth.uid()
    and exists (
      select 1 from public.leads l
      where l.id = lead_id and public.can_access_office(l.office_id)
    )
  );

-- Storage bucket (private, 5MB, allowed types)
insert into storage.buckets (id, name, public, file_size_limit, allowed_mime_types)
values (
  'lead-attachments',
  'lead-attachments',
  false,
  5242880,
  array[
    'application/pdf',
    'image/jpeg',
    'image/png',
    'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
    'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet'
  ]
)
on conflict (id) do update set
  file_size_limit = excluded.file_size_limit,
  allowed_mime_types = excluded.allowed_mime_types;
