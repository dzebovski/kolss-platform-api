# KOLSS Platform API

Single-process Go API for the KOLSS CRM, direct Meta Lead Ads ingestion, public
text forms, and office-specific notification outbox delivery.

## Current phase

- CRM uses Supabase directly only for Auth/session.
- All CRM data, workflow, reports, user admin, archive, and file URL operations go through this API.
- Kyiv and Warsaw Facebook Pages send signed `leadgen` webhooks directly to this API.
- A durable Postgres inbox and periodic Meta Graph API reconciliation prevent lost leads.
- Kyiv Telegram and Warsaw Slack delivery run in-process immediately after commits and with an hourly recovery sweep.
- Public UA/PL site forms are feature-disabled (`PUBLIC_SITE_FORMS_ENABLED=false`).

## Local run

```bash
cp .env.example .env
set -a && source .env && set +a
go run ./cmd/api
```

S3 credentials are optional and retained only for historical CRM attachment URLs.
Health endpoints are `GET /health/live` and `GET /health/ready`.

## Contract, migrations, and Meta setup

- OpenAPI 2.4: [`api/openapi.yaml`](./api/openapi.yaml)
- Canonical Supabase migrations: [`supabase/migrations`](./supabase/migrations)
- Meta App setup and cutover: [`docs/META-LEAD-ADS-SETUP.md`](./docs/META-LEAD-ADS-SETUP.md)
- Manual browser grant revocation: [`deploy/post-cutover-revoke-browser-data.sql`](./deploy/post-cutover-revoke-browser-data.sql)

Do not run the browser grant revocation until the Go-backed CRM has been stable in
production for 24 hours.

### Meta Lead Ads

The public callback is `GET|POST /v1/integrations/meta/webhook`. GET performs Meta
subscription verification; POST requires `X-Hub-Signature-256`. The webhook only
persists events. Contact retrieval, retry, form discovery, and reconciliation run
after commit.

`META_INGEST_AFTER` is a hard historical cutoff. Reconciliation never imports an
older lead, including from archived forms.

### Kyiv Telegram delivery

`TELEGRAM_CHAT_ID_KYIV` remains the primary Kyiv chat. Set
`TELEGRAM_ADDITIONAL_CHAT_IDS_KYIV=-1002833157899` to also deliver each Kyiv lead
to the **Kolss Kyiv** supergroup. Each destination has independent outbox retry state.

### Warsaw Slack delivery

Set `SLACK_BOT_TOKEN_WARSAW` to the installed app's `xoxb-…` Bot User OAuth
Token and `SLACK_CHANNEL_ID_WARSAW` to the target channel ID. The app needs the
`chat:write` scope and must be a member of private target channels. Warsaw lead
notifications and the morning daily report (Mon–Sat at `DAILY_REPORT_HOUR_LOCAL`
in Europe/Warsaw) are delivered only to Slack.

### Daily report

Kyiv receives the morning report on Telegram; Warsaw receives it on Slack. Both
use the same local hour (`DAILY_REPORT_HOUR_LOCAL`, default 9) and skip Sundays.
Leads with a future `callback_due_at` are excluded until that calendar day.

Set `CRM_SITE_URL_PUBLIC=https://crm.kolss.eu` without `/crm/leads/:id`.

## Deploy

- [`Dockerfile`](./Dockerfile) — API-only image
- [`deploy/digitalocean-app.yaml`](./deploy/digitalocean-app.yaml)
- [`deploy/CUTOVER.md`](./deploy/CUTOVER.md)

Production backend: `https://api.kolss.eu`.

## Verification

```bash
go test ./...
go vet ./...
go build -o /tmp/kolss-platform-api ./cmd/api
```

The explicit output path avoids colliding with the repository's `api/` OpenAPI directory.
