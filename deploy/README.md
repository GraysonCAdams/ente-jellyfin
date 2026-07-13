# Deploying Ente -> Jellyfin to Hetzner

Single CAX11 (ARM, EU) running Jellyfin + the gateway in Docker, fronted by a
Cloudflare Tunnel (no public ports).

## Prereqs
- Hetzner Cloud API token (Read & Write) -> `hcloud context create ente-jellyfin`
- Cloudflare token/creds (for the tunnel + DNS)
- Ente session exported locally: `gateway export-secrets > deploy/.env.ente`

## 1. Provision
```
hcloud server create --name ente-jellyfin --type cax11 --image docker-ce \
  --location hel1 --ssh-key <key>
hcloud firewall create --name ej-fw   # allow 22 from your IP only; 443 via CF only (no inbound needed)
```

## 2. Ship code + secrets
```
scp -r . root@<ip>:/opt/ente-jellyfin       # or git clone
# .env on the box holds ENTE_* (from export-secrets) + CF_TUNNEL_TOKEN + Jellyfin admin
```

## 3. Bring up + populate
```
cd /opt/ente-jellyfin/deploy
docker compose up -d gateway
docker compose run --rm -e GATEWAY_URL=http://gateway:8092 gateway strm   /library
docker compose run --rm gateway thumbs /library
docker compose up -d jellyfin cloudflared
```
Then create the Jellyfin "Ente Home Movies" (Home videos and photos) library
pointed at `/media`, scan.

## Secrets
Everything sensitive lives only in `deploy/.env` on the box (git-ignored) and in
1Password. `.env` keys: `ENTE_*`, `CF_TUNNEL_TOKEN`, `JELLYFIN_ADMIN_*`.

## Notes
- The gateway advertises `GATEWAY_PUBLIC_URL=http://gateway:8092` so Jellyfin's
  server-side ffmpeg can reach it over the compose network; clients never touch
  the gateway directly.
- Fallback decrypt cache capped at 5 GiB (`/cache`), LRU-evicted.
