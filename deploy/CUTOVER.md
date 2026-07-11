# CRM production cutover checklist

## Database

- [x] Production custom backup and schema dump created before migrations.
- [x] July migrations applied through `20260710180000`.
- [x] Kyiv and Warsaw import sources point to separate confirmed spreadsheets.
- [x] Last 20 rows reconciled without notifications; Warsaw gap of 19 leads imported.
- [x] Pending/failed outbox count is zero after reconcile.
- [ ] Keep `post-cutover-revoke-browser-data.sql` unapplied until 24 stable hours.

## DigitalOcean App Platform

Deploy [`digitalocean-app.yaml`](./digitalocean-app.yaml) to the existing application.

API requirements:

- `DATABASE_URL` for `kolss_api` (or the current production DB user during transition);
- `SUPABASE_URL`, JWKS URL, JWT issuer, and API-only `SUPABASE_SECRET_KEY`;
- separate Kyiv/Warsaw Google Sheets import secrets;
- `CORS_ALLOWED_ORIGINS=https://crm.kolss.eu`;
- `CRM_SITE_URL_PUBLIC=https://crm.kolss.eu`;
- `PUBLIC_SITE_FORMS_ENABLED=false`.

Worker requirements:

- `DATABASE_URL` for `kolss_worker`;
- `WORKER_SITE_JOBS_ENABLED=false`;
- separate Kyiv/Warsaw bot tokens/chat IDs;
- no Supabase Auth secret and no mandatory S3 credentials.

Verify the starter `*.ondigitalocean.app` ingress first, then add `api.kolss.eu` and create the DigitalOcean-provided CNAME in GoDaddy.

## CRM / Vercel

- Set `API_BASE_URL=https://api.kolss.eu`, Supabase Auth URL/anon key, and `SITE_URL_PUBLIC=https://crm.kolss.eu`.
- Deploy preview and test all four roles.
- Confirm browser network traffic has no Supabase Data API, Functions, RPC, or Storage business calls.
- Promote the verified deployment; do not dual-write.

## Google Apps Script

Source IDs:

- Kyiv: `f21b5c38-ed09-4cf1-ab23-7e6e854f3f4d`
- Warsaw: `31b5aa24-7b79-48a3-b3fe-178cf2aea0b2`

In both spreadsheets paste the canonical script, preserve the existing `LAST_ROW`, set the office-specific secret, change the URL to `https://api.kolss.eu/v1/integrations/google-sheets/lead-imports`, and verify one five-minute trigger.

Current Sheet last rows at reconciliation time:

- Kyiv: 81
- Warsaw: 24

## Final 24-hour lock-down

After 24 stable hours:

1. Apply the reviewed browser Data API grant revocation.
2. Confirm CRM still operates entirely through Go.
3. Undeploy `import-lead`, `site-lead`, `process-notifications`, and `admin-users` Edge Functions.
