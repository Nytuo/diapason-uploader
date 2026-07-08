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
| `DIAPASON_AUTH_TOKEN` | *(empty)* | Bearer token required on all API calls. Strongly recommended. |
| `DIAPASON_PATH_TEMPLATE` | `{artist}/{album}/{track2} - {title}.{ext}` | Placement template. Placeholders: `{artist} {album} {title} {track} {track2} {disc} {year} {ext}`. |
| `DIAPASON_TLS_CERT` / `DIAPASON_TLS_KEY` | *(empty)* | Enable HTTPS when both set. |
| `DIAPASON_MAX_UPLOAD_BYTES` | `536870912` (512 MiB) | Reject larger uploads. `0` = unlimited. |
| `DIAPASON_RESCAN_DEBOUNCE_SECS` | `5` | Wait this long after the last upload before rescanning (batches collapse to one). |
| `DIAPASON_NAVIDROME_URL` / `_USER` / `_PASSWORD` | *(empty)* | Trigger Navidrome (Subsonic `startScan`). |
| `DIAPASON_PLEX_URL` / `_TOKEN` / `_SECTION` | *(empty)* | Trigger a Plex section refresh instead. |

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
