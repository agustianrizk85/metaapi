#!/usr/bin/env bash
# Deploy metaapi (Meta Graph proxy: Ads / WhatsApp / Instagram + DM inbox).
# Pull, build, run/-restart via PM2 as meta-be (:8098). Same pattern as the
# other Greenpark backends. Run on the server from inside the repo: ./deploy.sh
set -euo pipefail
cd "$(dirname "$0")"

echo "==> git pull"
git pull --ff-only

echo "==> go build"
export PATH="$PATH:/usr/local/go/bin"
CGO_ENABLED=0 go build -trimpath -o meta-server .

# Env (Meta token + SHARED JWT secret) from outside git: /opt/apps/meta.env
# JWT_SECRET must equal the marketing backend's so its login tokens validate.
set -a; [ -f /opt/apps/meta.env ] && . /opt/apps/meta.env; set +a

echo "==> (re)start PM2: meta-be"
pm2 restart meta-be --update-env 2>/dev/null || pm2 start ./meta-server --name meta-be --update-env
pm2 save
echo "==> selesai. status:"
pm2 status meta-be
