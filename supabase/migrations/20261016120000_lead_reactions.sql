-- Per-user lead reactions (like / star / question) for the CRM v1 leads list.
-- Unlike lead_markers they are personal, never reset by lead changes and do
-- not count as lead activity.

begin;

create table if not exists public.lead_reactions (
  lead_id uuid not null references public.leads (id) on delete cascade,
  kind text not null,
  user_id uuid not null references public.profiles (id) on delete cascade,
  created_at timestamptz not null default now(),
  primary key (lead_id, kind, user_id),
  constraint lead_reactions_kind_check check (kind in ('like', 'star', 'question'))
);

create index if not exists lead_reactions_user_idx
  on public.lead_reactions (user_id, created_at desc);

alter table public.lead_reactions enable row level security;

revoke all privileges on table public.lead_reactions from public, anon, authenticated;

grant select, insert, delete on public.lead_reactions to kolss_api;

drop policy if exists lead_reactions_api_runtime on public.lead_reactions;
create policy lead_reactions_api_runtime on public.lead_reactions
  for all to kolss_api
  using (true)
  with check (true);

commit;
