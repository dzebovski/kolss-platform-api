# Meta Lead Ads production cutover

Use the complete setup instructions in
[`../docs/META-LEAD-ADS-SETUP.md`](../docs/META-LEAD-ADS-SETUP.md).

## Release A — parallel validation

- Back up production and apply migrations through `20260721120000_meta_lead_ads_direct.sql`.
- Deploy the API with all required `META_*` variables.
- Subscribe the Kyiv and Warsaw Pages to the `leadgen` webhook.
- Keep the existing Google Apps Script triggers running for 48 hours.
- Verify one lead per Page, an email-only lead, a duplicate webhook, and a newly created form.
- Compare Meta lead IDs with `public.leads.external_lead_id`; the difference must be zero after `META_INGEST_AFTER`.

## Release B — Google Sheets shutdown

- Disable and delete both Apps Script time triggers.
- Confirm direct Meta leads continue to reach CRM and Telegram for at least 15 minutes.
- Deploy the final code that removes the Sheets endpoint and secrets.
- Confirm `POST /v1/integrations/google-sheets/lead-imports` returns `404`.
- Keep `lead_import_sources` and `lead_import_runs` as read-only history.

## Runtime checks

- `meta_page_connections.last_success_at` is newer than 30 minutes for both Pages.
- No `meta_lead_events` rows remain in `dead_letter` without investigation.
- `meta_sync_runs` shows successful `active_reconcile` runs every 15 minutes.
- Token and sync failures reach `META_ALERT_TELEGRAM_CHAT_ID`.

Rollback does not delete leads: deploy the previous API image and, if necessary,
restore the old Apps Script endpoint from the previous release. Existing Meta inbox
events and Telegram outbox rows remain durable.
