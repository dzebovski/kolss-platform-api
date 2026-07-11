-- Slack is not part of the CRM cutover. Remove only exhausted legacy rows that
-- never delivered because the office webhook was not configured.
delete from public.lead_notifications
where channel = 'slack'
  and status = 'failed'
  and attempts >= 10
  and last_error = 'Missing Slack webhook for office';
