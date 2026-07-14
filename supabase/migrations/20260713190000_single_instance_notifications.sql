-- Consolidate public submissions and Telegram delivery into the API runtime.
-- Historical upload metadata and storage objects are intentionally preserved.

update public.lead_submissions
set status = 'expired'
where status = 'awaiting_upload';

alter table public.lead_submissions
  alter column completion_token_hash drop not null,
  alter column expires_at drop not null;

-- Slack is retired. Preserve delivered history for audit purposes.
delete from public.lead_notifications
where channel = 'slack'
  and status in ('pending', 'failed');

-- The API is now both notification producer and consumer, and creates the
-- text-only submission record in the same transaction as the lead/outbox rows.
grant select on public.sites to kolss_api;
grant select, insert, update on public.lead_submissions to kolss_api;
grant select, insert, update on public.lead_notifications to kolss_api;

-- Keep the role for audit/rollback, but remove its active table privileges.
revoke all privileges on table public.lead_submissions from kolss_worker;
revoke all privileges on table public.lead_submission_uploads from kolss_worker;
revoke all privileges on table public.lead_attachments from kolss_worker;
revoke all privileges on table public.lead_notifications from kolss_worker;
