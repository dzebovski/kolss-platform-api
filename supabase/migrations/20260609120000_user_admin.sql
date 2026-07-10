-- User admin: curator role, deactivation, RLS for super_admin CRUD

alter type public.user_role add value if not exists 'curator' after 'super_admin';

alter table public.profiles
  add column if not exists is_active boolean not null default true,
  add column if not exists deactivated_at timestamptz;

-- Block non-super-admins from changing role or is_active on their own profile
create or replace function public.profiles_guard_sensitive_fields()
returns trigger
language plpgsql
security definer
set search_path = public
as $$
begin
  if public.is_super_admin() then
    return new;
  end if;

  if new.role is distinct from old.role then
    raise exception 'Cannot change role';
  end if;

  if new.is_active is distinct from old.is_active then
    raise exception 'Cannot change active status';
  end if;

  if new.deactivated_at is distinct from old.deactivated_at then
    raise exception 'Cannot change deactivation status';
  end if;

  return new;
end;
$$;

drop trigger if exists profiles_guard_sensitive_fields on public.profiles;
create trigger profiles_guard_sensitive_fields
  before update on public.profiles
  for each row execute function public.profiles_guard_sensitive_fields();

-- Super admin can update any profile
create policy profiles_update_admin on public.profiles
  for update to authenticated
  using (public.is_super_admin())
  with check (public.is_super_admin());

-- Super admin membership management
create policy memberships_insert_admin on public.user_office_memberships
  for insert to authenticated
  with check (public.is_super_admin());

create policy memberships_delete_admin on public.user_office_memberships
  for delete to authenticated
  using (public.is_super_admin());
