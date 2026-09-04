-- Immutable currency-rate snapshots used by lead budgets and signed contracts.

create table public.currency_rate_sets (
  id uuid primary key default gen_random_uuid(),
  version bigint generated always as identity unique,
  effective_from timestamptz not null default now(),
  pln_per_eur numeric(18, 8) not null check (pln_per_eur > 0),
  uah_per_eur numeric(18, 8) not null check (uah_per_eur > 0),
  uah_per_usd numeric(18, 8) not null check (uah_per_usd > 0),
  created_by uuid references public.profiles (id) on delete set null,
  created_at timestamptz not null default now()
);

create unique index currency_rate_sets_effective_from_idx
  on public.currency_rate_sets (effective_from);

alter table public.currency_rate_sets enable row level security;
revoke all on table public.currency_rate_sets from anon, authenticated;

insert into public.currency_rate_sets (
  effective_from,
  pln_per_eur,
  uah_per_eur,
  uah_per_usd
) values ('1970-01-01 00:00:00+00', 4.2, 52, 44.2);

alter table public.leads
  add column estimated_budget_currency text not null default 'EUR',
  add column estimated_budget_rate_set_id uuid references public.currency_rate_sets (id) on delete restrict,
  add constraint leads_estimated_budget_currency_check check (
    estimated_budget_currency in ('UAH', 'USD', 'EUR', 'PLN')
  );

update public.leads
set estimated_budget_rate_set_id = (
  select id
  from public.currency_rate_sets
  order by effective_from asc
  limit 1
)
where estimated_budget is not null;

alter table public.lead_contracts
  add column currency_rate_set_id uuid references public.currency_rate_sets (id) on delete restrict;

update public.lead_contracts
set currency_rate_set_id = (
  select id
  from public.currency_rate_sets
  order by effective_from asc
  limit 1
)
where amount is not null and currency is not null;

create or replace function private.prevent_currency_rate_set_mutation()
returns trigger
language plpgsql
security definer
set search_path = ''
as $$
begin
  raise exception 'currency rate sets are immutable';
end
$$;

revoke all on function private.prevent_currency_rate_set_mutation() from public;

create trigger currency_rate_sets_immutable
before update or delete on public.currency_rate_sets
for each row execute function private.prevent_currency_rate_set_mutation();
