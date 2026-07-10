# KOLSS Platform API

Go API + worker for public PL/UA contact-form lead submissions against CRM Supabase.

## Local run

```bash
cp .env.example .env
# BOTCHECK_DISABLED=true and empty S3 is OK for contact-only local tests
set -a && source .env && set +a
go run ./cmd/api
# optional worker (requires S3 credentials):
go run ./cmd/worker
```

Health:

- API `GET /health/live`, `GET /health/ready`
- Worker `GET /health/live` on `WORKER_HEALTH_ADDR` (default `:8081`)

## Contract

OpenAPI: [`api/openapi.yaml`](./api/openapi.yaml)

1. `POST /v1/public/sites/{siteCode}/lead-submissions` — draft + signed uploads (or auto-complete when `files: []`)
2. Browser `PUT` to quarantine Storage
3. `POST .../complete` + `X-Submission-Token` — create lead, attachments `pending_scan`, enqueue notifications

## Schema

Remote ownership: `/Users/dzebski/Documents/kolss/kolss-crm/supabase/migrations`  
Local mirror: `supabase/migrations/` (do **not** `db push` baseline from this repo to remote).

## Deploy

- [`Dockerfile`](./Dockerfile) — targets `api`, `worker`, `both`
- [`compose.yaml`](./compose.yaml)
- [`deploy/digitalocean-app.yaml`](./deploy/digitalocean-app.yaml)
- [`deploy/CUTOVER.md`](./deploy/CUTOVER.md)
- [`deploy/STORAGE_CORS.md`](./deploy/STORAGE_CORS.md)

## Tests

```bash
go test ./...
go vet ./...
go build -o bin/api ./cmd/api
go build -o bin/worker ./cmd/worker
```
