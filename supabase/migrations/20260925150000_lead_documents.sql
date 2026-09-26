-- CRM v2 (W11): document upload + list per lead (Add documents board). Reuses the existing
-- public.lead_attachments table (20260526140000, extended with status/storage_bucket/source in
-- 20260710125233) and its "status = 'ready' is visible" convention already used by
-- GET /v1/files/{fileId}/download-url — no new table.

-- Additive: a nullable file type tag, the board's 5 codes.
alter table public.lead_attachments
  add column if not exists tag text;

alter table public.lead_attachments
  drop constraint if exists lead_attachments_tag_check,
  add constraint lead_attachments_tag_check check (
    tag is null or tag in ('plan', 'photo', 'drawing', 'estimate', 'other')
  );

-- Wider CHECK (Rule zero: every existing row already satisfies a larger bound): the board's
-- "up to 25 MB each", up from the original 5 MB. The constraint was inline and unnamed in
-- 20260526140000, so Postgres auto-named it lead_attachments_size_bytes_check
-- (<table>_<column>_check, its standard default-naming rule).
alter table public.lead_attachments
  drop constraint if exists lead_attachments_size_bytes_check,
  add constraint lead_attachments_size_bytes_check check (
    size_bytes > 0 and size_bytes <= 26214400
  );

-- New status: a two-step upload (task W11) needs a row to exist, with a stable id, before the
-- browser's direct PUT to storage completes and is confirmed. No Go code and no CRM code reads
-- lead_attachments.status by an exhaustive switch today (checked): the only reader is
-- GET /v1/files/{fileId}/download-url's "status != 'ready' -> unavailable" gate, which a new
-- value only strengthens (an awaiting_upload row is correctly treated as unavailable, same as
-- pending_scan/blocked/deleted already are). The CRM v1 mapper hardcodes leads.attachments to
-- [] (no v1 attachments UI exists), and the only CRM reader, the v2 Documents card, has no
-- upload endpoint yet either, so nothing consumes an unconfirmed row anywhere today.
alter type public.lead_attachment_status add value if not exists 'awaiting_upload';

-- Storage bucket: widen the size limit to match, and add the content types the board's file
-- picker accepts that the original 5 did not. Existing entries (including the historical
-- docx/xlsx pair, unrelated to this board) are kept, not replaced.
update storage.buckets set
  file_size_limit = 26214400,
  allowed_mime_types = array[
    'application/pdf',
    'image/jpeg',
    'image/png',
    'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
    'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
    'image/heic',
    'application/octet-stream'
  ]
where id = 'lead-attachments';

-- kolss_api can now create the pending row (upload step 1) and mark it ready (confirm step 2).
-- Previously select, delete only (20260710170000, 20260715120000).
grant insert, update on public.lead_attachments to kolss_api;
