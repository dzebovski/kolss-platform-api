-- Heal orphan lead assignments left after managers were deactivated
-- without clearing assigned_to (pre-fix behavior).

update public.leads l
set assigned_to = null,
    updated_at = now()
where l.assigned_to is not null
  and l.archived_at is null
  and exists (
    select 1
    from public.profiles p
    where p.id = l.assigned_to
      and p.is_active = false
  );
