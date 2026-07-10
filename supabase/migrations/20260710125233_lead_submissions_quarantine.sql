-- Expand: public lead submissions, quarantine uploads, attachment metadata for site forms.

create type public.lead_submission_status as enum (
  'awaiting_upload',
  'accepted',
  'expired',
  'failed'
);

create type public.lead_upload_status as enum (
  'awaiting_upload',
  'uploaded',
  'pending_scan',
  'ready',
  'blocked',
  'deleted'
);

create type public.lead_attachment_source as enum ('crm', 'site_form');

create type public.lead_attachment_status as enum (
  'pending_scan',
  'ready',
  'blocked',
  'deleted'
);

create table if not exists public.lead_submissions (
  id uuid primary key default gen_random_uuid(),
  lead_id uuid unique references public.leads (id) on delete set null,
  site_code text not null references public.sites (code),
  idempotency_key uuid not null,
  name text not null,
  phone text not null,
  email text,
  city text,
  project_description text,
  privacy_policy_version text not null,
  consented_at timestamptz not null,
  page_url text,
  completion_token_hash text not null,
  status public.lead_submission_status not null default 'awaiting_upload',
  expires_at timestamptz not null,
  completed_at timestamptz,
  created_at timestamptz not null default now(),
  unique (site_code, idempotency_key)
);

create index if not exists lead_submissions_status_expires_idx
  on public.lead_submissions (status, expires_at);

create index if not exists lead_submissions_lead_id_idx
  on public.lead_submissions (lead_id)
  where lead_id is not null;

alter table public.lead_submissions enable row level security;

create policy lead_submissions_select_authenticated on public.lead_submissions
  for select to authenticated
  using (
    lead_id is not null
    and exists (
      select 1 from public.leads l
      where l.id = lead_id and public.can_access_office(l.office_id)
    )
  );

create table if not exists public.lead_submission_uploads (
  id uuid primary key default gen_random_uuid(),
  submission_id uuid not null references public.lead_submissions (id) on delete cascade,
  client_file_id uuid not null,
  storage_bucket text not null,
  storage_path text not null,
  original_filename text not null,
  declared_content_type text not null,
  declared_size_bytes int not null
    check (declared_size_bytes > 0 and declared_size_bytes <= 5242880),
  actual_content_type text,
  actual_size_bytes int,
  etag text,
  sha256 text,
  status public.lead_upload_status not null default 'awaiting_upload',
  created_at timestamptz not null default now(),
  uploaded_at timestamptz,
  scanned_at timestamptz,
  unique (submission_id, client_file_id),
  unique (storage_bucket, storage_path)
);

create index if not exists lead_submission_uploads_submission_id_idx
  on public.lead_submission_uploads (submission_id);

create index if not exists lead_submission_uploads_status_idx
  on public.lead_submission_uploads (status);

alter table public.lead_submission_uploads enable row level security;

create policy lead_submission_uploads_select_authenticated on public.lead_submission_uploads
  for select to authenticated
  using (
    exists (
      select 1
      from public.lead_submissions s
      join public.leads l on l.id = s.lead_id
      where s.id = submission_id and public.can_access_office(l.office_id)
    )
  );

-- Expand lead_attachments for site_form quarantine files (CRM rows stay source=crm, status=ready).
alter table public.lead_attachments
  alter column uploaded_by drop not null;

alter table public.lead_attachments
  add column if not exists source public.lead_attachment_source not null default 'crm',
  add column if not exists status public.lead_attachment_status not null default 'ready',
  add column if not exists storage_bucket text not null default 'lead-attachments',
  add column if not exists sha256 text;

create index if not exists lead_attachments_status_idx
  on public.lead_attachments (status)
  where status <> 'ready';

-- Private quarantine bucket: uploads only via object-specific presigned URLs (no anon/auth policies).
insert into storage.buckets (id, name, public, file_size_limit, allowed_mime_types)
values (
  'lead-uploads-quarantine',
  'lead-uploads-quarantine',
  false,
  5242880,
  array[
    'application/pdf',
    'text/plain',
    'text/csv',
    'image/jpeg',
    'image/png',
    'image/webp'
  ]
)
on conflict (id) do update set
  public = excluded.public,
  file_size_limit = excluded.file_size_limit,
  allowed_mime_types = excluded.allowed_mime_types;

-- Production + local + controlled Vercel preview origins for API CORS seed.
update public.sites
set allowed_origins = array[
  'http://localhost:4200',
  'http://127.0.0.1:4200',
  'https://kolss-web-pl.vercel.app',
  'https://kolss.pl',
  'https://www.kolss.pl'
]
where code = 'kolss-pl';

update public.sites
set allowed_origins = array[
  'http://localhost:4201',
  'http://127.0.0.1:4201',
  'https://kolss-web-ua.vercel.app',
  'https://kolss.com.ua',
  'https://www.kolss.com.ua'
]
where code = 'kolss-ua';
