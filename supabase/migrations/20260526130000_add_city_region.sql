alter table public.leads
  add column if not exists city_region text;
