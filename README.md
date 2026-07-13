# ente-jellyfin

Stream an [Ente Photos](https://ente.io) library into [Jellyfin](https://jellyfin.org) — from the **official Ente cloud** (`api.ente.io`), end-to-end-encrypted, decrypted on demand, with **nothing stored** but a small rolling cache.

Ente is end-to-end encrypted, so there is no server-side API that hands out playable media — decryption has to happen client-side with your key. This gateway does exactly that: it reuses the session created by the official `ente` CLI, talks to the real Ente API, decrypts video/thumbnails on the fly, and serves them to Jellyfin as a normal filesystem library via `.strm` stubs.

## What it does

- **Reuses your `ente` CLI session** (macOS Keychain + `~/.ente/ente-cli.db`) or injected env vars — never re-implements login, never stores your password.
- **Video**: serves Ente's transcoded HLS preview, but **decrypts the AES-128 layer server-side** and repackages it as a plain, seekable MPEG-TS stream (`/stream/{id}.ts`) that *any* player direct-plays — Infuse, Swiftfin, AVPlayer, the Jellyfin web player.
- **Fallback**: for videos without a preview, decrypts the original on demand with Range/seek support (`/media/{id}.mp4`).
- **Thumbnails**: decrypts Ente's own thumbnails (no frame extraction).
- Optional: generate Ente HLS previews (`generate`), pre-build a flat date-sorted library, emit `.strm` + poster + `.nfo` per clip.

## Commands

```
gateway list      # smoke test: recover session, print albums
gateway probe     # check which videos have HLS previews
gateway serve     # run the HTTP gateway (what Jellyfin reads from)
gateway strm  <d> # write .strm stubs (HLS-preferred, original fallback) into <d>
gateway thumbs <d># write Ente thumbnails as <clip>-poster.jpg
gateway flat  <d> # flat, date-sorted library: .strm + poster + .nfo per clip
gateway generate  # (opt) transcode+upload HLS previews for videos lacking one
gateway manifest  # JSON of every clip's metadata (date/album/GPS) for enrichment
gateway export-secrets  # print ENTE_* env vars to run headless on a server
```

## Auth

On macOS it reads the session that `ente account add` creates. On a headless
server, inject the secrets as env vars (`ENTE_MASTER_KEY`, `ENTE_SECRET_KEY`,
`ENTE_PUBLIC_KEY`, `ENTE_TOKEN`, `ENTE_USER_ID`, `ENTE_EMAIL`) — produce them
with `gateway export-secrets` on a machine that has the CLI session.

Set `GATEWAY_TOKEN` to require `?t=<token>` on every request (so the gateway can
be exposed publicly behind a reverse proxy without leaking decrypted content).

## Deploy

See [`deploy/`](deploy/) for a Docker Compose stack (gateway + Jellyfin +
Cloudflare Tunnel) and a cloud-init for a fresh VM.

## License

AGPL-3.0. Vendors the libsodium-compatible crypto from
[`ente-io/ente`](https://github.com/ente-io/ente) (`cli/internal/crypto`), which
is AGPL-3.0.

## Status

Personal project. Not affiliated with Ente or Jellyfin.
