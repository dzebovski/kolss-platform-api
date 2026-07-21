# KOLSS Meta Lead Ads: setup and production cutover

This runbook connects the Kyiv and Warsaw Facebook Pages directly to
`https://api.kolss.eu` through one Meta Business App. Never paste real access
tokens into this file, Git, issue trackers, screenshots, or application logs.

Official references:

- [Retrieving Meta Lead Ads leads](https://developers.facebook.com/docs/marketing-api/guides/lead-ads/retrieving)
- [Meta Webhooks](https://developers.facebook.com/docs/graph-api/webhooks/getting-started)
- [Meta Lead Ads Testing Tool](https://developers.facebook.com/tools/lead-ads-testing)
- [Access Token Debugger](https://developers.facebook.com/tools/debug/accesstoken)

Meta changes dashboard labels regularly. If a label differs, use the equivalent
Business Settings / App Dashboard section and verify the resulting token and Graph
API calls rather than relying only on the UI state.

## 1. Prerequisites

The operator must be an administrator of:

- the KOLSS Meta Business Portfolio;
- the Kyiv Facebook Page and its ad account;
- the Warsaw Facebook Page and its ad account;
- the Meta Developer account that will own the App.

Before switching the App to Live mode, provide real public URLs for:

- Privacy Policy;
- Data Deletion Instructions;
- App domain/contact email.

The current `/privacy` pages in the website repositories contain placeholder copy
and are not suitable for production App Review. Legal wording is outside this
technical runbook.

Record these non-secret identifiers:

```text
BUSINESS_ID=<business portfolio id>
KYIV_PAGE_ID=<facebook page id>
WARSAW_PAGE_ID=<facebook page id>
KYIV_AD_ACCOUNT_ID=act_<id>
WARSAW_AD_ACCOUNT_ID=act_<id>
```

## 2. Create one Business Meta App

1. Open [Meta for Developers](https://developers.facebook.com/apps/).
2. Create an App for a Business use case and attach it to the KOLSS Business Portfolio.
3. Use a clear name such as `KOLSS CRM Lead Sync`.
4. In **Settings → Basic**, configure the app domain, contact email, Privacy Policy URL,
   and Data Deletion Instructions URL.
5. Add the **Marketing API** product/use case.
6. Add **Webhooks** and choose the **Page** object.
7. In **Settings → Advanced**, enable App Secret Proof for server API calls when available.
8. Complete Business Verification and request Advanced Access/App Review for permissions
   that the dashboard marks as requiring review.
9. Keep the App in Development mode until both Page tests pass, then switch it to Live.

Record the credentials only in a password manager:

```text
META_APP_ID=<app id>
META_APP_SECRET=<app secret>
```

## 3. Create the System User and assign assets

1. Open **Business Settings → Users → System Users**.
2. Create one admin System User named `KOLSS CRM Meta Leads`.
3. Assign the Meta App to this System User.
4. Assign both Facebook Pages with permissions to manage the Page, advertise, and
   analyze Page activity.
5. Assign both advertising accounts with at least campaign/ad read access.
6. Generate a System User token for the KOLSS Meta App with:

```text
ads_management
leads_retrieval
pages_show_list
pages_read_engagement
pages_manage_ads
pages_manage_metadata
```

If Meta does not show one of these permissions, confirm that the corresponding
Marketing API/Page use case is added to the App and that the System User owns the
required assets.

## 4. Leads Access Manager

For each Page, open **Business Settings → Integrations → Leads Access** (sometimes
shown as **Leads Access Manager**):

1. Select the Page.
2. Grant lead access to the System User.
3. Add the KOLSS Meta App/CRM integration.
4. Confirm access for the advertising account or people who manage the campaigns.

Missing Leads Access Manager permission can allow webhook delivery while the
subsequent `/{leadgen_id}` request still fails.

## 5. Obtain separate Page Access Tokens

Use the System User token with the App selected and request:

```http
GET /me/accounts?fields=id,name,access_token
```

The response must contain the Kyiv and Warsaw Page IDs. Store the corresponding
Page tokens separately:

```text
META_PAGE_ACCESS_TOKEN_KYIV=<token returned for KYIV_PAGE_ID>
META_PAGE_ACCESS_TOKEN_WARSAW=<token returned for WARSAW_PAGE_ID>
```

Check each token in the
[Access Token Debugger](https://developers.facebook.com/tools/debug/accesstoken):

- token type is Page;
- App ID is the KOLSS Meta App;
- Page ID is correct;
- required permissions are present;
- expiry is acceptable for a server integration.

Do not use a token generated for the generic Graph API Explorer App.

## 6. Prepare API configuration

Generate a long random webhook verification token. It is a shared setup secret and
is different from the App Secret and Page tokens.

Set these values as encrypted/runtime variables on the DigitalOcean `api` component:

```env
META_INTEGRATION_ENABLED=true
META_GRAPH_API_VERSION=v25.0
META_APP_ID=<app id>
META_APP_SECRET=<app secret>
META_WEBHOOK_VERIFY_TOKEN=<long random value>
META_PAGE_ID_KYIV=<Kyiv Page id>
META_PAGE_ACCESS_TOKEN_KYIV=<Kyiv Page token>
META_PAGE_ID_WARSAW=<Warsaw Page id>
META_PAGE_ACCESS_TOKEN_WARSAW=<Warsaw Page token>
META_INGEST_AFTER=<RFC3339 UTC cutover, for example 2026-07-21T18:00:00Z>
META_RECONCILIATION_INTERVAL_MINUTES=15
META_RECONCILIATION_LOOKBACK_HOURS=72
META_ALERT_TELEGRAM_CHAT_ID=<technical alerts chat id>
TELEGRAM_CHAT_ID_KYIV=<Kyiv lead notifications chat id>
TELEGRAM_CHAT_ID_WARSAW=<Warsaw lead notifications chat id>
```

`TELEGRAM_BOT_TOKEN` must also be configured because it sends lead notifications
and integration alerts. The API refuses to start with Meta enabled, with the
notification dispatcher disabled, or with an incomplete configuration.

`META_INGEST_AFTER` is permanent for the first connection record. Set it before
the first production start. Reconciliation never imports an older lead.

Apply the canonical migration using the existing production migration workflow:

```text
supabase/migrations/20260721120000_meta_lead_ads_direct.sql
```

Deploy the API and confirm:

```text
GET https://api.kolss.eu/health/live  -> 200
GET https://api.kolss.eu/health/ready -> 200
```

## 7. Configure the webhook callback

In the Meta App Webhooks dashboard, configure:

```text
Object: Page
Callback URL: https://api.kolss.eu/v1/integrations/meta/webhook
Verify Token: the exact META_WEBHOOK_VERIFY_TOKEN value
Field: leadgen
```

Meta performs a GET verification request. A successful setup means the dashboard
accepts the callback and the API returns the exact `hub.challenge` value.

## 8. Subscribe both Pages to the App

The App-level webhook is not enough; subscribe each Page to `leadgen`.

Calculate `appsecret_proof` separately for each Page token:

```bash
export META_APP_SECRET='<app-secret>'
export META_PAGE_TOKEN='<page-access-token>'
printf '%s' "$META_PAGE_TOKEN" | openssl dgst -sha256 -hmac "$META_APP_SECRET"
```

Copy only the hex digest from the output, then call Graph API for each Page:

```bash
export META_GRAPH_VERSION='v25.0'
export META_PAGE_ID='<page-id>'
export META_APPSECRET_PROOF='<hex digest>'

curl -X POST "https://graph.facebook.com/${META_GRAPH_VERSION}/${META_PAGE_ID}/subscribed_apps" \
  -H "Authorization: Bearer ${META_PAGE_TOKEN}" \
  --data-urlencode 'subscribed_fields=leadgen' \
  --data-urlencode "appsecret_proof=${META_APPSECRET_PROOF}"
```

Expected response:

```json
{"success": true}
```

Verify each subscription:

```bash
curl "https://graph.facebook.com/${META_GRAPH_VERSION}/${META_PAGE_ID}/subscribed_apps?appsecret_proof=${META_APPSECRET_PROOF}" \
  -H "Authorization: Bearer ${META_PAGE_TOKEN}"
```

The KOLSS App ID must be listed with `leadgen` subscribed.

## 9. Validate Pages, forms, and lead access

For each Page token, verify that forms can be listed:

```bash
curl "https://graph.facebook.com/${META_GRAPH_VERSION}/${META_PAGE_ID}/leadgen_forms?fields=id,name,status,locale&appsecret_proof=${META_APPSECRET_PROOF}" \
  -H "Authorization: Bearer ${META_PAGE_TOKEN}"
```

After one reconciliation cycle, inspect production:

```sql
select o.code, c.page_id, c.page_name, c.token_status, c.health_status,
       c.consecutive_failures, c.last_success_at, c.last_error
from public.meta_page_connections c
join public.offices o on o.id = c.office_id
order by o.code;

select f.form_id, f.name, f.status, f.last_seen_at
from public.meta_forms f
order by f.last_seen_at desc;
```

Both connections must be `healthy`, and all active forms must be present. A new
form on either known Page is accepted automatically; no whitelist is required.

## 10. Test lead delivery

Use the [Meta Lead Ads Testing Tool](https://developers.facebook.com/tools/lead-ads-testing):

1. Select the Kyiv Page and one active form; create a test lead.
2. Confirm one `meta_lead_events` row becomes `processed`.
3. Confirm one CRM lead exists with `external_lead_id = 'l:<leadgen_id>'`.
4. Confirm one Telegram message is delivered.
5. Repeat for Warsaw.
6. Test a form with email but no phone.
7. Resend the same webhook/test lead and confirm no duplicate CRM lead or notification.
8. Create a new temporary form and confirm it appears after the next 15-minute discovery cycle.

Useful queries:

```sql
select leadgen_id, page_id, form_id, status, attempts, last_error, lead_id
from public.meta_lead_events
order by received_at desc
limit 50;

select sync_type, status, forms_processed, leads_seen, events_created, error_message, started_at
from public.meta_sync_runs
order by started_at desc
limit 50;
```

## 11. Production cutover

1. Run Meta and the existing Google Apps Script flow in parallel for 48 hours.
2. Compare Meta lead IDs with CRM `external_lead_id` values after `META_INGEST_AFTER`.
3. The difference must be zero for Kyiv and Warsaw.
4. Disable and delete the time triggers in both Apps Script projects.
5. Wait at least 15 minutes and create one new lead per Page.
6. Confirm both leads reach CRM and Telegram without a new spreadsheet row.
7. Deploy the final release that removes the Google Sheets endpoint and secrets.
8. Confirm the removed endpoint returns `404`.

Historical `lead_import_sources` and `lead_import_runs` remain in Postgres for audit
but are disabled and read-only to the API runtime.

## 12. Monitoring and token rotation

Healthy production state:

- both connections reconciled successfully within 30 minutes;
- no unexplained `dead_letter` events;
- regular successful `active_reconcile` runs;
- no OAuth alerts in the technical Telegram chat.

Rotate a Page token one Page at a time:

1. Generate and debug the replacement token.
2. Update the corresponding encrypted DigitalOcean secret.
3. Redeploy/restart the API.
4. Verify forms and create a test lead for that Page.
5. Revoke the old token only after the test passes.

## 13. Troubleshooting

### OAuth error code 190 or 102

The token is invalid, expired, issued for the wrong App, or revoked. Debug the token,
replace the DigitalOcean secret, redeploy, and create a test lead.

### Permission error code 200

Confirm Page asset assignment, System User tasks, `pages_manage_metadata`, Leads
Access Manager, App mode, Business Verification, and Advanced Access.

### Webhook is verified but no lead events arrive

Confirm the Page appears in `/{PAGE_ID}/subscribed_apps` and includes `leadgen`.
Also confirm the campaign uses a form owned by that Page.

### Event exists but repeatedly retries

Read `meta_lead_events.last_error`. Check the Page token, `leads_retrieval`, Leads
Access Manager, and whether the lead is available through `GET /{leadgen_id}`.

### New form is missing

Call `/{PAGE_ID}/leadgen_forms` with the same Page token. If Meta returns the form,
the next discovery cycle will persist it. If Meta does not return it, fix Page/token
asset access rather than adding a database whitelist.

### Reconciliation is stale

Check `meta_sync_runs.error_message`, DigitalOcean logs, outbound connectivity to
`graph.facebook.com`, and the technical Telegram alert. Do not delete retry events;
they are the recovery source after the external issue is fixed.
