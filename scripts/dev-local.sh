#!/usr/bin/env bash
# Runs the API against the local Supabase stack (`supabase start`) only.
# Ignores .env/.env.local on purpose: those point at the hosted project.
# Background workers and external integrations are disabled.
set -euo pipefail
cd "$(dirname "$0")/.."

status="$(supabase status -o env 2>/dev/null)" || {
  echo "Local Supabase is not running. Start it with: supabase start" >&2
  exit 1
}
value() { printf '%s\n' "$status" | sed -n "s/^$1=\"\{0,1\}\([^\"]*\)\"\{0,1\}$/\1/p"; }

api_url="$(value API_URL)"
case "$api_url" in
  http://127.0.0.1:*|http://localhost:*) ;;
  *) echo "Refusing to run: API_URL '$api_url' is not local" >&2; exit 1 ;;
esac

exec env -i PATH="$PATH" HOME="$HOME" GOPATH="${GOPATH:-}" GOCACHE="${GOCACHE:-}" \
  HTTP_ADDR=":8080" \
  DATABASE_URL="$(value DB_URL)" \
  SUPABASE_URL="$api_url" \
  SUPABASE_SECRET_KEY="$(value SECRET_KEY)" \
  CORS_ALLOWED_ORIGINS="http://localhost:4202,http://127.0.0.1:4202" \
  PUBLIC_SITE_FORMS_ENABLED=false \
  NOTIFICATION_DISPATCHER_ENABLED=false \
  DAILY_REPORT_ENABLED=false \
  META_INTEGRATION_ENABLED=false \
  BOTCHECK_DISABLED=true \
  go run ./cmd/api
