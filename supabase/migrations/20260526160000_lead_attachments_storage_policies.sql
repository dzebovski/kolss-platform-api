-- RLS for lead-attachments storage bucket (path: {office_id}/{lead_id}/{file})
-- Fallback when uploads use the authenticated client (no service role key in app).

drop policy if exists lead_attachments_storage_insert on storage.objects;
drop policy if exists lead_attachments_storage_select on storage.objects;

create policy lead_attachments_storage_insert on storage.objects
  for insert to authenticated
  with check (
    bucket_id = 'lead-attachments'
    and public.can_access_office(split_part(name, '/', 1)::uuid)
    and exists (
      select 1
      from public.leads l
      where l.id = split_part(name, '/', 2)::uuid
        and l.office_id = split_part(name, '/', 1)::uuid
    )
  );

create policy lead_attachments_storage_select on storage.objects
  for select to authenticated
  using (
    bucket_id = 'lead-attachments'
    and exists (
      select 1
      from public.lead_attachments la
      join public.leads l on l.id = la.lead_id
      where la.storage_path = storage.objects.name
        and public.can_access_office(l.office_id)
    )
  );
