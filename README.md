# KOLSS Platform API

Go API and worker for the KOLSS CRM, Google Sheets imports, and notification outbox.

## Current phase

- CRM uses Supabase directly only for Auth/session.
- All CRM data, workflow, reports, user admin, archive, and file URL operations go through this API.
- Kyiv and Warsaw Google Sheets use the office-secret import endpoint.
- Worker runs notification-only by default and sends Telegram per office.
- Public UA/PL site forms are feature-disabled (`PUBLIC_SITE_FORMS_ENABLED=false`).

## Local run

```bash
cp .env.example .env
set -a && source .env && set +a
go run ./cmd/api
go run ./cmd/worker
```

Notification-only worker does not require S3 credentials. Health endpoints:

- API: `GET /health/live`, `GET /health/ready`
- Worker: `GET /health/live` on `WORKER_HEALTH_ADDR` (default `:8081`)

## Contract and migrations

- OpenAPI 1.0: [`api/openapi.yaml`](./api/openapi.yaml)
- Canonical Supabase migrations: [`supabase/migrations`](./supabase/migrations)
- Manual post-stability grant revocation: [`deploy/post-cutover-revoke-browser-data.sql`](./deploy/post-cutover-revoke-browser-data.sql)
- Google Apps Script: [`integrations/google-apps-script/meta-leads-import.gs`](./integrations/google-apps-script/meta-leads-import.gs)

Do not run the browser grant revocation until the Go-backed CRM has been stable in production for 24 hours.

## Deploy

- [`Dockerfile`](./Dockerfile) — `api`, `worker`, and `both` targets
- [`deploy/digitalocean-app.yaml`](./deploy/digitalocean-app.yaml)
- [`deploy/CUTOVER.md`](./deploy/CUTOVER.md)

Production backend domain: `https://api.kolss.eu`.

## Verification

```bash
go test ./...
go vet ./...
go build ./cmd/api ./cmd/worker
```
