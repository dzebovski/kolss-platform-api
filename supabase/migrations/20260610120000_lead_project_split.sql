-- Lead / Project split: simplified lead funnel + project pipeline, tasks, attachments

-- Lead statuses (qualification funnel)
create table public.lead_statuses (
  code text primary key,
  label_uk text not null,
  label_pl text not null,
  sort_order int not null,
  is_terminal boolean not null default false
);

insert into public.lead_statuses (code, label_uk, label_pl, sort_order, is_terminal) values
  ('new', 'Нова заявка', 'Nowe zgłoszenie', 0, false),
  ('in_progress', 'В роботі', 'W trakcie', 10, false),
  ('converted', 'Конвертований', 'Skonwertowany', 90, true),
  ('failed', 'Невдалий лід', 'Nieudany lead', 100, true);

-- Project stages (delivery funnel)
create table public.project_stages (
  code text primary key,
  label_uk text not null,
  label_pl text not null,
  sort_order int not null,
  is_terminal boolean not null default false
);

insert into public.project_stages (code, label_uk, label_pl, sort_order, is_terminal) values
  ('needs_discovery', 'Виявлення потреб', 'Identyfikacja potrzeb', 10, false),
  ('design_quote', 'Створення проєкту та оцінка', 'Projekt i wycena', 20, false),
  ('approval', 'Погодження', 'Akceptacja', 30, false),
  ('measurement', 'Заміри', 'Pomiar', 40, false),
  ('contract', 'Договір та передплата', 'Umowa i zaliczka', 50, false),
  ('production', 'Виготовлення', 'Produkcja', 60, false),
  ('installation', 'Встановлення', 'Montaż', 70, false),
  ('final_payment', 'Постоплата та акт', 'Płatność końcowa i akt', 80, false),
  ('completed', 'Успішно реалізовано', 'Zrealizowano', 90, true),
  ('archived', 'Архів', 'Archiwum', 100, true);

-- Loss reasons (shared by leads and projects)
create table public.loss_reasons (
  code text primary key,
  label_uk text not null,
  label_pl text not null
);

insert into public.loss_reasons (code, label_uk, label_pl) values
  ('spam', 'Сміття / Спам', 'Spam'),
  ('not_target', 'Нецільовий', 'Niecelowy'),
  ('price', 'Не підійшла ціна', 'Cena nie pasuje');

-- Drop FKs that reference pipeline_stages before migration
alter table public.lead_comments drop constraint if exists lead_comments_pipeline_stage_fkey;
alter table public.leads drop constraint if exists leads_crm_status_fkey;

-- New lead columns
alter table public.leads
  add column if not exists lead_status text,
  add column if not exists lead_status_changed_at timestamptz,
  add column if not exists loss_reason text references public.loss_reasons (code),
  add column if not exists converted_project_id uuid,
  add column if not exists estimated_budget numeric(12, 2),
  add column if not exists our_quote numeric(12, 2);

-- Reset active leads per migration plan (terminal legacy stages mapped, rest → new)
update public.leads
set
  lead_status = case
    when crm_status = 'activated_warranty' then 'converted'
    when crm_status = 'canceled_lost' then 'failed'
    else 'new'
  end,
  lead_status_changed_at = coalesce(crm_status_changed_at, created_at)
where lead_status is null;

alter table public.leads
  alter column lead_status set default 'new',
  alter column lead_status set not null;

alter table public.leads
  add constraint leads_lead_status_fkey
  foreign key (lead_status) references public.lead_statuses (code);

alter table public.leads drop column if exists crm_status;
alter table public.leads drop column if exists crm_status_changed_at;

create index if not exists leads_lead_status_idx on public.leads (lead_status);

-- Projects
create table public.projects (
  id uuid primary key default gen_random_uuid(),
  lead_id uuid not null unique references public.leads (id) on delete restrict,
  office_id uuid not null references public.offices (id),
  status text not null default 'needs_discovery' references public.project_stages (code),
  status_changed_at timestamptz not null default now(),
  last_activity_at timestamptz not null default now(),
  product_type text,
  product_details text,
  estimated_budget numeric(12, 2),
  our_quote numeric(12, 2),
  is_only_measurement boolean not null default false,
  advance_paid numeric(12, 2),
  final_paid numeric(12, 2),
  loss_reason text references public.loss_reasons (code),
  assigned_to uuid references public.profiles (id) on delete set null,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create index projects_office_id_idx on public.projects (office_id);
create index projects_status_idx on public.projects (status);
create index projects_last_activity_at_idx on public.projects (last_activity_at);

alter table public.leads
  add constraint leads_converted_project_id_fkey
  foreign key (converted_project_id) references public.projects (id) on delete set null;

-- Project comments (activity for inactivity trigger)
create table public.project_comments (
  id uuid primary key default gen_random_uuid(),
  project_id uuid not null references public.projects (id) on delete cascade,
  author_id uuid not null references public.profiles (id) on delete cascade,
  body text not null,
  created_at timestamptz not null default now()
);

create index project_comments_project_id_idx
  on public.project_comments (project_id, created_at desc);

-- Project attachments
create type public.project_document_type as enum ('contract', 'act', 'other');

create table public.project_attachments (
  id uuid primary key default gen_random_uuid(),
  project_id uuid not null references public.projects (id) on delete cascade,
  uploaded_by uuid not null references public.profiles (id) on delete cascade,
  document_type public.project_document_type not null default 'other',
  file_name text not null,
  storage_path text not null,
  mime_type text not null,
  size_bytes int not null check (size_bytes > 0 and size_bytes <= 5242880),
  created_at timestamptz not null default now()
);

create index project_attachments_project_id_idx
  on public.project_attachments (project_id, created_at desc);

-- Tasks
create type public.task_entity_type as enum ('lead', 'project');
create type public.task_priority as enum ('normal', 'high');
create type public.task_source as enum ('manual', 'auto_no_answer', 'auto_inactivity');
create type public.task_status as enum ('open', 'done', 'canceled');

create table public.tasks (
  id uuid primary key default gen_random_uuid(),
  entity_type public.task_entity_type not null,
  entity_id uuid not null,
  assignee_id uuid references public.profiles (id) on delete set null,
  title text not null,
  due_at timestamptz not null,
  priority public.task_priority not null default 'normal',
  source public.task_source not null default 'manual',
  status public.task_status not null default 'open',
  created_at timestamptz not null default now(),
  completed_at timestamptz
);

create index tasks_assignee_status_idx on public.tasks (assignee_id, status, due_at);
create index tasks_entity_idx on public.tasks (entity_type, entity_id);
create index tasks_open_due_idx on public.tasks (status, due_at) where status = 'open';

-- Lead comments: retarget to lead_status
alter table public.lead_comments rename column pipeline_stage to lead_status;
update public.lead_comments set lead_status = 'in_progress' where lead_status not in (
  select code from public.lead_statuses
);
alter table public.lead_comments
  add constraint lead_comments_lead_status_fkey
  foreign key (lead_status) references public.lead_statuses (code);

-- Drop legacy pipeline_stages
drop table if exists public.pipeline_stages cascade;

-- Triggers
create trigger projects_updated_at before update on public.projects
  for each row execute function public.set_updated_at();

-- RLS
alter table public.lead_statuses enable row level security;
alter table public.project_stages enable row level security;
alter table public.loss_reasons enable row level security;
alter table public.projects enable row level security;
alter table public.project_comments enable row level security;
alter table public.project_attachments enable row level security;
alter table public.tasks enable row level security;

create policy lead_statuses_select on public.lead_statuses
  for select to authenticated using (true);

create policy project_stages_select on public.project_stages
  for select to authenticated using (true);

create policy loss_reasons_select on public.loss_reasons
  for select to authenticated using (true);

create policy projects_select on public.projects
  for select to authenticated using (public.can_access_office(office_id));

create policy projects_insert on public.projects
  for insert to authenticated
  with check (public.can_access_office(office_id));

create policy projects_update on public.projects
  for update to authenticated
  using (public.can_access_office(office_id))
  with check (public.can_access_office(office_id));

create policy project_comments_select on public.project_comments
  for select to authenticated
  using (
    exists (
      select 1 from public.projects p
      where p.id = project_id and public.can_access_office(p.office_id)
    )
  );

create policy project_comments_insert on public.project_comments
  for insert to authenticated
  with check (
    author_id = auth.uid()
    and exists (
      select 1 from public.projects p
      where p.id = project_id and public.can_access_office(p.office_id)
    )
  );

create policy project_attachments_select on public.project_attachments
  for select to authenticated
  using (
    exists (
      select 1 from public.projects p
      where p.id = project_id and public.can_access_office(p.office_id)
    )
  );

create policy project_attachments_insert on public.project_attachments
  for insert to authenticated
  with check (
    uploaded_by = auth.uid()
    and exists (
      select 1 from public.projects p
      where p.id = project_id and public.can_access_office(p.office_id)
    )
  );

create policy tasks_select on public.tasks
  for select to authenticated
  using (
    assignee_id = auth.uid()
    or public.is_super_admin()
    or (
      entity_type = 'lead' and exists (
        select 1 from public.leads l
        where l.id = entity_id and public.can_access_office(l.office_id)
      )
    )
    or (
      entity_type = 'project' and exists (
        select 1 from public.projects p
        where p.id = entity_id and public.can_access_office(p.office_id)
      )
    )
  );

create policy tasks_insert on public.tasks
  for insert to authenticated
  with check (true);

create policy tasks_update on public.tasks
  for update to authenticated
  using (
    assignee_id = auth.uid()
    or public.is_super_admin()
    or exists (
      select 1 from public.leads l
      where entity_type = 'lead' and l.id = entity_id
        and public.can_access_office(l.office_id)
    )
    or exists (
      select 1 from public.projects p
      where entity_type = 'project' and p.id = entity_id
        and public.can_access_office(p.office_id)
    )
  );

-- Storage bucket for project attachments
insert into storage.buckets (id, name, public, file_size_limit, allowed_mime_types)
values (
  'project-attachments',
  'project-attachments',
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
