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

- OpenAPI 2.10: [`api/openapi.yaml`](./api/openapi.yaml)
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

Kyiv receives the morning report on Telegram (Ukrainian); Warsaw receives it
on Slack (Polish). Both use the same local hour (`DAILY_REPORT_HOUR_LOCAL`,
default 9) and skip Sundays.

The message has one line per non-empty group: emoji, group name, count, and a
CRM deep link. There is no per-lead detail — the count on the line always
equals what the manager sees after clicking through, because both the digest
and the CRM read the same [`internal/leadcohorts`](./internal/leadcohorts)
queries. If every group is empty, a single "nothing needs attention" message
is sent instead. Six groups, in display order (non-archived, not
`closed_lost` / `contract_signed`):

1. **New leads** (🆕) — `call_status` is null.
2. **No answer + undated callback** (📵) — `call_status = no_answer`, or
   `callback_requested` with no due date yet.
3. **Callbacks due today** (⏰) — a callback reminder due on the office-local
   report date.
4. **Visits due today** (🏠) — a scheduled showroom/measurement visit due on
   the office-local report date.
5. **Other reminders due today** (💬) — a `thinking` or comment reminder due
   on the office-local report date.
6. **Overdue reminders** (⚠️) — any reminder (callback, visit, or other) due
   strictly before the office-local report date.

Each line links into the CRM: groups 1-2 to the leads list
(`/crm/leads?office=<code>&callStatus=…&clientStatus=…&days=all`), groups 3-5
to that day's calendar (`/crm/calendar?office=<code>&date=<YYYY-MM-DD>&kind=…`),
and group 6 to the calendar filtered to overdue items
(`/crm/calendar?office=<code>&due=overdue`). Set
`CRM_SITE_URL_PUBLIC=https://crm.kolss.eu` without a path; an invalid or empty
value degrades a line to plain text instead of emitting a broken link.

Use `cmd/dailyreport-preview` to inspect a composed message (including the six
raw counts) for a given office and date without waiting for the schedule or
sending anything — see below.

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

## Daily report preview

Compose and print the morning digest for one office and date to stdout,
without sending anything:

```bash
go build -o /tmp/kolss-dailyreport-preview ./cmd/dailyreport-preview
set -a && source .env.local && set +a   # or .env, or export DATABASE_URL yourself
/tmp/kolss-dailyreport-preview -office=warsaw -date=2026-08-17
```

`-date` defaults to today in the office's timezone when omitted. It prints
the six raw group counts and the exact rendered message (Slack for `warsaw`,
Telegram for `kyiv`).

Unlike `cmd/api`, this tool only needs `DATABASE_URL` (required) and
optionally `CRM_SITE_URL_PUBLIC` (falling back to `SITE_URL_PUBLIC`) for the
CRM deep links — see `config.LoadPreview` in
[`internal/config/config.go`](./internal/config/config.go). It does **not**
need `SUPABASE_URL`/`SUPABASE_SECRET_KEY` or any Telegram/Slack token: it
only runs read-only `SELECT`s through `internal/leadcohorts` and prints text,
so it works with exactly the environment a local checkout's `.env`/`.env.local`
already provide. Leave `CRM_SITE_URL_PUBLIC` unset to see how a line degrades
to plain text without a link. There is no `.env` auto-loading in this
repository (no dotenv dependency) — export the variables yourself, e.g. via
`source` as shown above, before running the binary or `go run`. It never
sends a message and never prints secrets.

## One-off Biuro Leads Status import

Build the importer separately from the API:

```bash
go build -o /tmp/import-biuro-leads ./cmd/import-biuro-leads
```

Export `Sheet1` as CSV and stream it directly to stdin. Run a read-only dry-run
first and retain its `sourceSha256`:

```bash
export DATABASE_URL='postgresql://...'
drive-export-command | /tmp/import-biuro-leads --mode dry-run --input -
```

Apply only the unchanged snapshot by passing the dry-run hash:

```bash
drive-export-command | /tmp/import-biuro-leads \
  --mode apply \
  --input - \
  --expected-sha256 '<dry-run sourceSha256>'
```

The apply runs in one database transaction. The CSV must not be committed or
written to repository files.
