# metaapi

Standalone Go service that proxies the Meta (Facebook/Instagram) Graph API for
the Greenpark marketing dashboard. Self-contained: **no database**, a single
**System User token** from the environment, JWT auth, and optional static SPA
serving — so one binary can power `meta.greenparkgroup.cloud`.

## Endpoints

```
POST /api/auth/login            {email,password} -> {token, expires_at, user}
GET  /api/auth/me               (Bearer)         -> user
GET  /api/health                                 -> {ok:true}

GET  /api/meta/ads              ?range=today|7d|30d|90d|this_year|last_year|max
GET  /api/meta/ads/detail       ?range=...
GET  /api/meta/ads/campaign     ?id=...&range=...
GET  /api/meta/whatsapp
GET  /api/meta/instagram
GET  /api/meta/instagram/conversations          # IG DM inbox list
GET  /api/meta/instagram/messages   ?conversation_id=..&page_id=..
POST /api/meta/instagram/send       {page_id,recipient_id,text}
```

All `/api/meta/*` routes require a Bearer token (from `/api/auth/login`).

## Configuration

Copy `.env.example` to `.env` and set at least `META_ACCESS_TOKEN` (the System
User token from Business Settings → System Users, with `ads_read`,
`instagram_manage_messages`, `pages_show_list`). See `.env.example` for the rest.

Seeded logins: `kadep@greenpark.id` / `KADEP_PASSWORD`, `viewer@greenpark.id` /
`VIEWER_PASSWORD`.

## Run

```bash
cp .env.example .env      # then fill META_ACCESS_TOKEN
go run .
# or
go build -o metaapi . && ./metaapi
```

## Serve the SPA too

Set `FRONTEND_DIR=/path/to/dist` to serve the built dashboard from this binary
(SPA fallback to `index.html`). Leave empty for API-only.

## Deploy

Cross-compile for the Linux server, ship the binary + `.env`, run behind Apache
(reverse-proxy `/api` or everything to the service port). A `deploy.sh` will be
added next.
```bash
GOOS=linux GOARCH=amd64 go build -o metaapi .
```
