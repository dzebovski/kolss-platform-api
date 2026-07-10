# Storage CORS (manual — Supabase Dashboard)

The expand migration creates private bucket `lead-uploads-quarantine` but **does not** set Storage CORS.
Configure CORS in Supabase Dashboard → Storage → Configuration → CORS, or via Management API.

## Required origins

Allow `PUT` and `HEAD` (and `GET` only if needed for debugging — prefer omit for quarantine):

| Origin | Purpose |
|--------|---------|
| `http://localhost:4200` | PL local |
| `http://localhost:4201` | UA local |
| `http://127.0.0.1:4200` | PL local alt |
| `http://127.0.0.1:4201` | UA local alt |
| `https://kolss-web-pl.vercel.app` | PL Vercel project |
| `https://kolss-web-ua.vercel.app` | UA Vercel project |
| `https://kolss.pl` | PL production (adjust if different) |
| `https://www.kolss.pl` | PL www |
| `https://kolss.com.ua` | UA production (adjust if different) |
| `https://www.kolss.com.ua` | UA www |

Also add any controlled Vercel preview hostnames you use (exact origins, no `*`).

## Suggested CORS JSON

```json
[
  {
    "allowedOrigins": [
      "http://localhost:4200",
      "http://localhost:4201",
      "http://127.0.0.1:4200",
      "http://127.0.0.1:4201",
      "https://kolss-web-pl.vercel.app",
      "https://kolss-web-ua.vercel.app",
      "https://kolss.pl",
      "https://www.kolss.pl",
      "https://kolss.com.ua",
      "https://www.kolss.com.ua"
    ],
    "allowedMethods": ["PUT", "HEAD"],
    "allowedHeaders": ["*"],
    "exposedHeaders": ["ETag", "Content-Type", "Content-Length"],
    "maxAgeSeconds": 600
  }
]
```

## S3-compatible keys

Enable S3 access keys in Dashboard → Storage → S3 access / Connection info.
Set on DigitalOcean App (API + worker):

- `SUPABASE_S3_ENDPOINT` — e.g. `https://fpqolqiivzokwpmymqsr.storage.supabase.co/storage/v1/s3`
- `SUPABASE_S3_REGION` — project region (often `eu-central-1`)
- `SUPABASE_S3_ACCESS_KEY_ID`
- `SUPABASE_S3_SECRET_ACCESS_KEY`
- Quarantine bucket name: `lead-uploads-quarantine`
