# Deploy metaapi → meta.greenparkgroup.cloud

Server: cPanel/WHM, apps under PM2, Apache reverse-proxy via cPanel userdata
includes. metaapi runs as **meta-be** on **:8098**. The marketing backend
(:8086) provides login (shared JWT secret) and the marketing frontend (:8093,
which now has the Meta tabs) provides the SPA.

Routing on `meta.greenparkgroup.cloud`:

```
/api/meta -> 127.0.0.1:8098  (metaapi  — Ads/WhatsApp/Instagram + DM inbox)
/api      -> 127.0.0.1:8086  (marketing-be — login, work items)
/         -> 127.0.0.1:8093  (marketing-fe — SPA)
```

## One-time setup

```bash
# 1) Go (if missing) — metaapi needs Go 1.25
cd /tmp && wget https://go.dev/dl/go1.25.0.linux-amd64.tar.gz
rm -rf /usr/local/go && tar -C /usr/local -xzf go1.25.0.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin && echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc

# 2) Clone metaapi
cd /opt/apps && git clone https://github.com/agustianrizk85/metaapi.git && cd metaapi

# 3) Server env (token + shared secret) — outside git
cp deploy/meta.env.example /opt/apps/meta.env
nano /opt/apps/meta.env       # set META_ACCESS_TOKEN, keep JWT_SECRET=dev-secret

# 4) Build + start meta-be (:8098)
chmod +x deploy.sh && ./deploy.sh

# 5) Apache proxy for meta.greenparkgroup.cloud (both std + ssl)
for T in std ssl; do
  D=/etc/apache2/conf.d/userdata/$T/2_4/greenparkgroup/meta.greenparkgroup.cloud
  mkdir -p "$D" && cp deploy/proxy.conf "$D/proxy.conf"
done
/scripts/ensure_vhost_includes --user=greenparkgroup
apachectl graceful

# 6) Make sure the marketing frontend (SPA) has the Meta tabs
cd /opt/apps/greenparkmarketing && ./deploy.sh      # git pull + build + restart marketing-fe
```

## Update later

```bash
cd /opt/apps/metaapi && ./deploy.sh
```

## Verify

```bash
curl -s https://meta.greenparkgroup.cloud/api/health           # {"ok":true} via metaapi? (no, /api -> 8086) 
curl -s http://127.0.0.1:8098/api/health                       # {"ok":true}  (metaapi direct)
# In the browser: open https://meta.greenparkgroup.cloud, login, check Iklan + Instagram (Inbox).
```
