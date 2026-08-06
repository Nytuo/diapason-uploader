# Diapason Uploader

A small sidecar service that lets the Diapason apps push tracks (e.g. songs
downloaded via yt-dlp / discovery) **into your music server's library**, instead
of keeping them per-device.

## Why this exists

Neither **Navidrome/Subsonic** nor **Plex** exposes a music *upload/ingest* API —
both only ingest by scanning a folder on the server's disk. This service runs
**next to** Navidrome/Plex, shares the same music volume, receives uploads over
HTTP(S), writes them into the library in a clean `Artist/Album/Track` layout,
de-duplicates by content hash, and then triggers a server rescan so the track
shows up right away.

```
Diapason app  ──HTTPS──▶  diapason-uploader  ──writes──▶  /music  ◀──scans──  Navidrome/Plex
```

## Install

### Docker (recommended)

See [`docker-compose.yaml`](./docker-compose.yaml) for a full example running
alongside Navidrome. Key point: the uploader and your music server must mount the
**same** music directory.

```sh
docker compose up -d --build
```

### Native binary

```sh
go build -o diapason-uploader .
DIAPASON_MUSIC_DIR=/srv/music \
DIAPASON_AUTH_TOKEN=$(openssl rand -hex 24) \
./diapason-uploader
```

A single static binary (`CGO_ENABLED=0`), no runtime dependencies.

## Configuration (environment variables)

| Variable | Default | Description |
|---|---|---|
| `DIAPASON_MUSIC_DIR` | *(required)* | Music library root, shared with Navidrome/Plex. |
| `DIAPASON_LISTEN_ADDR` | `0.0.0.0:8688` | Bind address. Use `127.0.0.1:8688` to serve localhost only. |
| `DIAPASON_AUTH_TOKEN` | *(auto-generated)* | Bearer token required on all API calls. If unset, a random token is generated on first run and persisted to `<DIAPASON_MUSIC_DIR>/.diapason-uploader-token` (mode `0600`) so it survives restarts — the service never runs unauthenticated by accident. |
| `DIAPASON_PATH_TEMPLATE` | `{artist}/{album}/{track2} - {title}.{ext}` | Placement template. Placeholders: `{artist} {album} {title} {track} {track2} {disc} {year} {ext}`. |
| `DIAPASON_TLS_CERT` / `DIAPASON_TLS_KEY` | *(empty)* | Enable HTTPS when both set (TLS 1.2+). |
| `DIAPASON_MAX_UPLOAD_BYTES` | `536870912` (512 MiB) | Reject larger uploads. `0` = unlimited. |
| `DIAPASON_RESCAN_DEBOUNCE_SECS` | `5` | Wait this long after the last upload before rescanning (batches collapse to one). |
| `DIAPASON_NAVIDROME_URL` / `_USER` / `_PASSWORD` | *(empty)* | Trigger Navidrome (Subsonic `startScan`). |
| `DIAPASON_PLEX_URL` / `_TOKEN` / `_SECTION` | *(empty)* | Trigger a Plex section refresh instead. |
| `DIAPASON_MDNS_ENABLED` | `true` | Advertise on the LAN via mDNS (`_diapason-uploader._tcp`) so the app can auto-discover this instance. See [Auto-discovery & pairing](#auto-discovery--pairing). |
| `DIAPASON_MDNS_NAME` | machine hostname | Name Diapason clients see when browsing for uploaders. |

## Robustness & security notes

- **Path traversal is blocked**: client-supplied metadata (including the file
  extension) is sanitized and every resolved write path is verified to stay
  under `DIAPASON_MUSIC_DIR` before anything is written.
- **Auth is on by default**: a token is generated and persisted automatically
  if you don't set one — there's no accidental "no auth" mode.
- **Brute-force throttling**: repeated bad tokens from the same address are
  rate-limited and temporarily blocked.
- **Timeouts everywhere**: request read/write/idle timeouts prevent slow
  clients from tying up the server; TLS, when enabled, requires 1.2+.
- **Graceful shutdown**: `SIGINT`/`SIGTERM` drain in-flight requests before
  exiting (10s budget), which matters for `docker compose restart` /
  `docker stop`.

## Auto-discovery & pairing

The uploader advertises itself on the local network over mDNS
(`_diapason-uploader._tcp`), the same mechanism Diapason's device-to-device
"Connect" feature already uses — so the app can find it without you typing an
IP address. Whenever an auth token is configured, it's included in the mDNS
TXT record: the app's uploader settings screen shows nearby uploaders and, if
one is found, offers to fill in the address and token with one tap.

This is safe specifically because mDNS multicast is LAN-local by
construction — it never crosses routers or reaches the internet, so anything
that can receive the advertisement is already on the same local network as
the uploader. If you'd rather not broadcast the token at all, disable
discovery (`DIAPASON_MDNS_ENABLED=false`) and paste the token shown in the
startup logs / token file into the app manually instead.

If you run the uploader in Docker on the default bridge network, mDNS won't
reach your LAN (multicast doesn't cross the bridge); use `network_mode: host`
(see `docker-compose.yaml`) or set `DIAPASON_MDNS_ENABLED=false` and enter the
address manually.

## Local vs Internet

- **Local-only**: bind `DIAPASON_LISTEN_ADDR=127.0.0.1:8688` (or a LAN IP) and
  leave TLS off. The Diapason app, set to *Local network only*, uploads only when
  it can reach the LAN address.
- **Internet**: put the service behind a reverse proxy with TLS (or set
  `DIAPASON_TLS_CERT`/`_KEY`), keep a strong `DIAPASON_AUTH_TOKEN`, and set the
  app to *Local + Internet*.

## HTTP API

All endpoints require `Authorization: Bearer <token>` when a token is configured.

### `GET /healthz`
`200 {"status":"ok"}`

### `GET /api/v1/exists?hash=<sha256>`
Cheap pre-check so clients can skip re-uploading.
`200 {"exists":true,"path":"Artist/Album/01 - Song.mp3"}`

### `POST /api/v1/upload`
`multipart/form-data` with two parts, **in order**:
1. `metadata` — JSON: `{"artist","album","title","trackNumber","discNumber","year","ext"}`
2. `file` — the audio bytes.

Responses:
- `201 {"path","hash","duplicate":false}` — stored.
- `409 {"path","hash","duplicate":true}` — identical content already present.
- `400` / `401` / `500` on error.

Example:

```sh
curl -sS -X POST http://localhost:8688/api/v1/upload \
  -H "Authorization: Bearer $TOKEN" \
  -F 'metadata={"artist":"Artist","album":"Album","title":"Song","trackNumber":1,"ext":"mp3"};type=application/json' \
  -F 'file=@song.mp3'
```

## Development

```sh
go test ./...
go run . # with the env vars above
```
