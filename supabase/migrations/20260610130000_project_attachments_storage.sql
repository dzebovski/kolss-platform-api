-- Storage policies for project-attachments bucket

create policy project_attachments_storage_select on storage.objects
  for select to authenticated
  using (
    bucket_id = 'project-attachments'
    and (storage.foldername(name))[1]::uuid in (select public.user_office_ids())
  );

create policy project_attachments_storage_insert on storage.objects
  for insert to authenticated
  with check (
    bucket_id = 'project-attachments'
    and (storage.foldername(name))[1]::uuid in (select public.user_office_ids())
  );
