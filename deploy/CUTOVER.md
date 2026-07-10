# Cutover checklist — kolss-platform (API + worker)

Manual steps to move public site lead submissions from Supabase Edge to DigitalOcean App Platform + Vercel Angular sites.

## 1. DigitalOcean App Platform

1. Create app from [`digitalocean-app.yaml`](./digitalocean-app.yaml) (region `fra` or `ams`).
2. Connect the `kolss-platform-api` GitHub repo; confirm Dockerfile builds stage `both`.
3. Set **secret** values in the DO UI (names only in the YAML):
   - `DATABASE_URL` — Supabase pooler or direct Postgres URL (prefer transaction pooler for API; session/direct for worker if using `LISTEN` later; both work with pgx pool for this MVP)
   - `TURNSTILE_SECRET_KEY`
   - `SUBMISSION_TOKEN_PEPPER` — long random string; must stay stable across deploys
   - `SUPABASE_S3_ACCESS_KEY_ID` / `SUPABASE_S3_SECRET_ACCESS_KEY`
   - `TELEGRAM_BOT_TOKEN` (optional fallback), `TELEGRAM_BOT_TOKEN_KYIV`, `TELEGRAM_BOT_TOKEN_WARSAW`
   - `TELEGRAM_CHAT_ID_KYIV`, `TELEGRAM_CHAT_ID_WARSAW`
   - `SLACK_WEBHOOK_URL_KYIV`, `SLACK_WEBHOOK_URL_WARSAW` (optional)
4. Set **non-secret** values:
   - `CORS_ALLOWED_ORIGINS` — comma-separated production + Vercel origins for PL/UA
   - `TURNSTILE_ALLOWED_HOSTNAMES` — hostnames allowed by Turnstile (no scheme), e.g. `kolss.pl,www.kolss.pl,kolss.com.ua,www.kolss.com.ua,kolss-web-pl.vercel.app,kolss-web-ua.vercel.app`
   - `SUPABASE_S3_ENDPOINT` — from Supabase Dashboard → Storage → S3 connection
   - `SUPABASE_S3_REGION` — often `eu-central-1` or `auto`
   - `CRM_SITE_URL_PUBLIC` — public CRM origin used in notification links (`…/crm/leads/{id}`)
5. Confirm `BOTCHECK_DISABLED=false` in production.
6. Deploy; verify:
   - API `GET /health/live` and `GET /health/ready`
   - Worker process running (`run_command: worker`)

## 2. Vercel — Angular sites

Projects:

| Project | Repo | Notes |
|---------|------|--------|
| `kolss-web-pl` | `kolss-web-pl-angular` | PL site |
| `kolss-web-ua` | `kolss-web-ua-angular` | UA site |

For each project:

1. Framework: Angular / Node **24** (`NODE_VERSION=24` in project env or `vercel.json` build env).
2. Build command: `npm run build`
3. Output: `dist/<project>/browser` (SSR server under `dist/<project>/server` — confirm Vercel Angular SSR preset or custom server entry).
4. Set env pointing public forms at the DO API base URL (e.g. `API_BASE_URL` / `PUBLIC_API_URL` — match whatever the Angular apps expect).
5. Attach production domains (`kolss.pl`, `kolss.com.ua`) and keep `*.vercel.app` previews if used in CORS/Turnstile.

## 3. Cloudflare Turnstile

1. Create widgets for PL and UA production hostnames (and Vercel preview hosts if needed).
2. Put **site keys** in the Angular apps; put the **secret key** in DO as `TURNSTILE_SECRET_KEY`.
3. Align `TURNSTILE_ALLOWED_HOSTNAMES` with widget hostnames.
4. Expected action: `lead_submission` (must match client widget / API config).

## 4. Supabase Storage

1. Follow [`STORAGE_CORS.md`](./STORAGE_CORS.md) — allow `PUT`/`HEAD` from PL/UA origins.
2. Enable **S3 access keys** in Dashboard → Storage → S3.
3. Confirm private bucket `lead-uploads-quarantine` exists (migration `20260710140000_lead_submissions_quarantine.sql`).
4. Paste endpoint + keys into DO env (`SUPABASE_S3_*`).

## 5. Database

1. Apply platform migrations to the target Supabase project (including quarantine / submission tables). Do **not** use local `supabase db push` casually against prod — use a controlled migration path.
2. Confirm `sites` rows `kolss-pl` / `kolss-ua` and `allowed_origins`.

## 6. Smoke checklist

- [ ] `POST /v1/public/sites/kolss-pl/lead-submissions` creates submission (Turnstile OK)
- [ ] Presigned upload PUT to quarantine succeeds from browser origin
- [ ] Complete endpoint accepts token and moves submission forward
- [ ] Worker marks upload `pending_scan` → `ready` (or `blocked` for bad magic bytes)
- [ ] Expired `awaiting_upload` → `expired`; quarantine objects deleted; uploads `deleted`
- [ ] Telegram (and Slack if configured) receives:

```text
🔔 Нова заявка!
👤 Ім'я: ...
📞 Тел: ...
🌐 Джерело: Site Form
🔗 Посилання на CRM: ...
```

- [ ] CORS rejects unknown origins
- [ ] `BOTCHECK_DISABLED` is **false** in prod
- [ ] Old Edge `site-lead` path disabled or traffic cut over

## Worker env reference

| Variable | Default |
|----------|---------|
| `WORKER_HEALTH_ADDR` | `:8081` |
| `WORKER_CLEANUP_INTERVAL_SECONDS` | `60` |
| `WORKER_SCAN_INTERVAL_SECONDS` | `15` |
| `WORKER_NOTIFY_INTERVAL_SECONDS` | `10` |
