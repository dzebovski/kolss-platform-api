-- Run only after the Go-backed CRM has been stable for the agreed observation window.
-- Supabase Auth remains available; this revokes PostgREST/RPC/Storage business access.

revoke all on all tables in schema public from authenticated;
revoke all on all sequences in schema public from authenticated;
revoke all on all functions in schema public from authenticated;

drop policy if exists lead_attachments_storage_insert on storage.objects;
drop policy if exists lead_attachments_storage_select on storage.objects;
drop policy if exists project_attachments_storage_insert on storage.objects;
drop policy if exists project_attachments_storage_select on storage.objects;

