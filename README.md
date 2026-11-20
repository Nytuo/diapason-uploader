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
| `DIAPASON_AUTH_TOKEN` | *(auto-generated)* | Bearer token required on all API calls (min. 24 chars). If unset, a random token is generated on first run and persisted to `<DIAPASON_MUSIC_DIR>/.diapason-uploader-token` (mode `0600`) so it survives restarts — the service never runs unauthenticated by accident. |
| `DIAPASON_TRUSTED_PROXIES` | *(empty)* | Comma-separated IPs/CIDRs of reverse proxies whose `X-Forwarded-For` is trusted, so brute-force throttling is per real client rather than per proxy. |
| `DIAPASON_PATH_TEMPLATE` | `{artist}/{album}/{track2} - {title}.{ext}` | Placement template. Placeholders: `{artist} {album} {title} {track} {track2} {disc} {year} {ext}`. |
| `DIAPASON_TLS_CERT` / `DIAPASON_TLS_KEY` | *(empty)* | Enable HTTPS when both set (TLS 1.2+). |
| `DIAPASON_MAX_UPLOAD_BYTES` | `536870912` (512 MiB) | Reject larger uploads. `0` = unlimited. |
| `DIAPASON_RESCAN_DEBOUNCE_SECS` | `5` | Wait this long after the last upload before rescanning (batches collapse to one). |
| `DIAPASON_NAVIDROME_URL` / `_USER` / `_PASSWORD` | *(empty)* | Trigger Navidrome (Subsonic `startScan`). |
| `DIAPASON_PLEX_URL` / `_TOKEN` / `_SECTION` | *(empty)* | Trigger a Plex section refresh instead. |
| `DIAPASON_MDNS_ENABLED` | `true` | Advertise on the LAN via mDNS (`_diapason-uploader._tcp`) so the app can auto-discover this instance. See [Auto-discovery & pairing](#auto-discovery--pairing). |
| `DIAPASON_MDNS_NAME` | machine hostname | Name Diapason clients see when browsing for uploaders. |
| `DIAPASON_PAIRING` | `window` | `window` / `open` / `off` — see [Pairing](#auto-discovery--pairing). |
| `DIAPASON_PAIRING_WINDOW_MINUTES` | `15` | How long the pairing window stays open. |
| `DIAPASON_PAIRING_ALLOW_PUBLIC` | `false` | Also allow pairing from non-private addresses / via proxies (not advised). |
| `DIAPASON_STATE_DIR` | `DIAPASON_MUSIC_DIR` | Where the master token and paired-device list live. |
| `DIAPASON_PUBLIC_URL` | *(empty)* | Address to reach this server from outside your LAN (reverse proxy, Tailscale name…). Paired apps use it automatically when the `.local` address isn't reachable. |
| `DIAPASON_PLAIN_HTTP_OK` | `false` | Silence the plain-HTTP warning when your network is already secure (LAN, Tailscale, WireGuard, tunnel). |
| `DIAPASON_WORK_DIR` | `$TMPDIR/diapason-uploader` | Scratch space for yt-dlp (kept outside the music dir). |
| `DIAPASON_YTDLP_ENABLED` | `false` | Enable the [yt-dlp proxy](#yt-dlp-proxy-optional). |
| `DIAPASON_YTDLP_BIN` | `yt-dlp` | Path of the yt-dlp executable. |
| `DIAPASON_YTDLP_JS_RUNTIME` | `auto` | JavaScript runtime yt-dlp uses for YouTube's player challenges: `auto` (deno, node, bun or quickjs, whichever is found), a name, or `none`. The Docker image ships Node. |
| `DIAPASON_YTDLP_AUTO_UPDATE` | `true` | Run `yt-dlp -U` at start and every interval. |
| `DIAPASON_YTDLP_UPDATE_INTERVAL_HOURS` | `24` | Self-update period. |
| `DIAPASON_YTDLP_MAX_DURATION_SECS` | `3600` | Refuse downloads of longer videos (`0` = no limit). |
| `DIAPASON_YTDLP_MAX_FILE_BYTES` | `268435456` | Per-download size cap. |
| `DIAPASON_YTDLP_MAX_CONCURRENT` | `2` | Simultaneous yt-dlp processes **per paired device**: each device runs its own, so several devices never queue behind each other. |
| `DIAPASON_YTDLP_MAX_TOTAL` | `16` | Ceiling on yt-dlp processes across all devices. |
| `DIAPASON_YTDLP_PROXY` | *(empty)* | Route all yt-dlp traffic via this proxy (e.g. `socks5://vpn:1080`). |
| `DIAPASON_YTDLP_TRENDING_URL` | YouTube Top 100 Music (global) | Playlist behind the "Popular on YouTube" feed. |

## Robustness & security notes

- **Path traversal is blocked**: client-supplied metadata (including the file
  extension) is sanitized and every resolved write path is verified to stay
  under `DIAPASON_MUSIC_DIR` before anything is written.
- **Only audio is accepted**: uploads whose extension isn't a known audio
  container are refused (`415`), so the endpoint can't drop arbitrary files.
- **Auth is on by default**: a token is generated and persisted automatically
  if you don't set one — there's no accidental "no auth" mode. Tokens are
  compared via SHA-256 digests in constant time; supplied tokens must be ≥ 24
  characters.
- **Brute-force throttling**: repeated bad tokens from the same address are
  rate-limited and temporarily blocked. `X-Forwarded-For` is only believed from
  `DIAPASON_TRUSTED_PROXIES`, so it can't be forged to dodge the limiter.
- **Secrets are never broadcast**: mDNS advertises the address only; apps pair
  for their own revocable token.
- **The library is never modified by uploads**: an upload is staged in
  `.diapason-uploader-incoming/` and hashed first. If it's a duplicate, only
  that staged copy is deleted — existing files, folders and timestamps are left
  exactly as they were. A new file is only ever written to a path that doesn't
  exist (a numeric suffix is added otherwise); nothing is overwritten.
- **Hardened responses**: `nosniff`, `no-store`, a locked-down CSP and (over
  TLS) HSTS; request/metadata sizes are bounded and uploads are concurrency
  limited. Query strings (search terms) are never written to the log.
- **TLS**: when enabled, TLS 1.2+ with AEAD/ECDHE suites only. The service
  warns at start-up when it serves plain HTTP on a non-loopback address.
  HTTPS is *not* required when your network already encrypts the traffic
  (Tailscale, WireGuard, a tunnel): the app accepts plain HTTP to private/VPN
  addresses (including `*.ts.net`) and has an explicit "my connection is
  already secure" switch for anything else.
- **Timeouts everywhere**: header, idle and per-route read/write deadlines
  prevent slow clients from tying up the server while still letting big
  uploads finish on slow links.
- **Graceful shutdown**: `SIGINT`/`SIGTERM` drain in-flight requests before
  exiting (10s budget), which matters for `docker compose restart` /
  `docker stop`.

## Auto-discovery & pairing

The uploader advertises itself on the local network over mDNS
(`_diapason-uploader._tcp`) with its address and whether TLS/pairing are on —
**never a credential**. The app then pairs in one tap:

1. App → `POST /api/v1/pair {"name": "Pixel 9"}`.
2. The server checks that pairing is **open** and that the request comes
   **directly from a private / VPN address** (not relayed by a reverse proxy,
   not from the internet).
3. It mints a fresh token for that device alone and returns it once. Only a
   SHA-256 digest is stored (`.diapason-uploader-devices.json`, mode `0600`).

Pairing is open for `DIAPASON_PAIRING_WINDOW_MINUTES` after the first start
(while nobody is paired yet). Afterwards it's closed until a paired app
reopens it (Settings → Uploader → *Allow another device to pair*, or
`POST /api/v1/pair/open`). `DIAPASON_PAIRING=open` keeps it always available to
the local network; `off` disables it and you paste the master token instead.

Each device can be listed and revoked on its own:

```sh
curl -H "Authorization: Bearer $TOKEN" http://nas:8688/api/v1/devices
curl -X DELETE -H "Authorization: Bearer $TOKEN" http://nas:8688/api/v1/devices/<id>
```

On Docker's default bridge network mDNS won't reach your LAN: use
`network_mode: host` (see the examples) or enter the address manually — pairing
itself still works against a typed-in LAN address.

## Errors

Every failure is JSON: `{"error": "<sentence for people>", "code": "<stable reason>", "component": "uploader|ytdlp|auth|pairing", "detail": "<technical cause>"}`,
plus an `X-Request-Id` header that also appears in the server log line — quote it in a bug report.
yt-dlp failures are classified so the app can say what really went wrong:

| `code` | Meaning |
|---|---|
| `ytdlp_disabled` | The proxy is off on this server. |
| `youtube_blocked` | YouTube wants the server to prove it isn't a bot. |
| `youtube_rate_limited` | HTTP 429 from YouTube. |
| `video_unavailable` / `age_restricted` | Private, removed, geo-blocked / needs sign-in. |
| `server_network` | The server can't reach YouTube (DNS, outbound firewall, proxy). |
| `ytdlp_js_runtime` | No JavaScript runtime installed for yt-dlp. |
| `ytdlp_outdated` | yt-dlp can't parse YouTube's response; update it. |
| `ytdlp_timeout`, `ytdlp_busy`, `file_too_large`, `nothing_downloaded`, `ytdlp_failed` | Self-explanatory / catch-all. |
| `storage_failed`, `too_large`, `unsupported_type` | The uploader itself (disk, size limit, file type). |

## Example compose files

[`examples/`](./examples) has ready-to-edit files, every option commented:

| File | Scenario |
|---|---|
| `docker-compose.minimal.yaml` | Uploader beside an existing music server, LAN only |
| `docker-compose.full.yaml` | Navidrome + every feature and env var |
| `docker-compose.plex.yaml` | Plex instead of Navidrome |
| `docker-compose.caddy.yaml` | Internet access with automatic HTTPS (Caddy) |
| `docker-compose.tailscale.yaml` | Private access anywhere via Tailscale, no certificates |
| `docker-compose.cloudflared.yaml` | Cloudflare Tunnel |
| `docker-compose.ytdlp-vpn.yaml` | yt-dlp proxy whose traffic exits through a VPN |

## yt-dlp proxy (optional)

With `DIAPASON_YTDLP_ENABLED=true` the uploader becomes a YouTube proxy for the
Diapason app. Instead of every phone running its own scraping code (which
breaks whenever YouTube changes something until a new app version ships) the
**server** runs [yt-dlp](https://github.com/yt-dlp/yt-dlp), keeps it updated
itself (`yt-dlp -U` at start and every 24 h) and the app just asks it:

```
app ──HTTPS──▶ uploader ──yt-dlp──▶ YouTube
        (search · stream · download straight into the library)
```

What it gives you:

- **Search / feeds / related** — results come from yt-dlp, not the app.
- **Download to library** — one request makes the *server* fetch the audio,
  tag it (with ffmpeg, if installed), de-duplicate it and trigger a rescan;
  nothing is downloaded to or re-uploaded from the phone.
- **Streaming** — `GET /api/v1/ytdlp/stream/{id}` relays audio (with `Range`
  support), needed because stream URLs are bound to the server's IP.
- **Privacy** — the phone never talks to Google (thumbnails are relayed too),
  and `DIAPASON_YTDLP_PROXY` can send the server's own traffic through a VPN.

Security properties (inputs come from the network, so they are treated as hostile):

- Off by default; every endpoint needs the bearer token.
- Clients can only name a **YouTube video ID or youtube.com / youtu.be /
  music.youtube.com link**; anything else is rejected, so the server can't be
  turned into a general fetcher (no SSRF to your LAN or cloud metadata).
- yt-dlp is exec'd directly (never via a shell) with a fixed argument list,
  user text only after `--`, `--ignore-config`, a private `HOME`, and a
  stripped environment — it cannot read the uploader's token or your Navidrome/
  Plex credentials.
- Bounded: concurrent processes, runtime, video duration, file size, and the
  amount of process output kept in memory. The stream relay only follows
  `*.googlevideo.com` / `*.youtube.com`.
- No cookie support and no logging of what you search or download — only
  that a request happened.
- Please respect YouTube's terms and your local copyright law; this is meant
  for your own library.

The Docker image bundles a standalone yt-dlp and ffmpeg. For a native install,
put `yt-dlp` (the standalone binary from the yt-dlp releases page, so `-U`
works) on `PATH` or set `DIAPASON_YTDLP_BIN`.

## Local vs Internet

- **Local-only**: bind `DIAPASON_LISTEN_ADDR=127.0.0.1:8688` (or a LAN IP) and
  leave TLS off. The Diapason app, set to *Local network only*, uploads only when
  it can reach the LAN address.
- **Private overlay (Tailscale, WireGuard)**: plain HTTP is fine — the tunnel
  encrypts it. No certificates needed. See `examples/docker-compose.tailscale.yaml`.
- **Public internet**: terminate TLS (reverse proxy such as Caddy, a
  Cloudflare Tunnel, or `DIAPASON_TLS_CERT`/`_KEY`) and set the app to
  *Anywhere*. The app refuses plain HTTP to public hosts unless you tick
  "my connection is already secure".

## HTTP API

All endpoints require `Authorization: Bearer <token>` when a token is configured.

### `GET /healthz`
`200 {"status":"ok"}`

### `GET /api/v1/exists?hash=<sha256>`
Cheap pre-check so clients can skip re-uploading.
`200 {"exists":true,"path":"Artist/Album/01 - Song.mp3"}`

### `GET /api/v1/capabilities`
`200 {"version":1,"upload":true,"ytdlp":true,"ytdlpVersion":"2026.08.19","ytdlpTagging":true}`
— lets the app see which optional features this server has.

### Pairing & devices
| Endpoint | Auth | Description |
|---|---|---|
| `POST /api/v1/pair` | none (LAN + open window only) | `{"name"}` → `201 {"token","deviceId","name"}` |
| `POST /api/v1/pair/open` | token | Reopen the pairing window. |
| `GET /api/v1/devices` | token | List paired devices. |
| `DELETE /api/v1/devices/{id}` | token | Revoke one device. |

### `POST /api/v1/upload`
`multipart/form-data` with two parts, **in order**:
1. `metadata` — JSON: `{"artist","album","title","trackNumber","discNumber","year","ext"}`
2. `file` — the audio bytes.

Responses:
- `201 {"path","hash","duplicate":false}` — stored.
- `409 {"path","hash","duplicate":true}` — identical content already present.
- `400` / `401` / `500` on error.

- `415` — extension isn't an audio format.

Example:

```sh
curl -sS -X POST http://localhost:8688/api/v1/upload \
  -H "Authorization: Bearer $TOKEN" \
  -F 'metadata={"artist":"Artist","album":"Album","title":"Song","trackNumber":1,"ext":"mp3"};type=application/json' \
  -F 'file=@song.mp3'
```

### yt-dlp endpoints (only when `DIAPASON_YTDLP_ENABLED=true`)

| Endpoint | Description |
|---|---|
| `GET /api/v1/ytdlp/search?q=&limit=` | Search. A pasted YouTube link resolves to that one video. `{"results":[{"id","title","artist","durationSecs"}]}` |
| `GET /api/v1/ytdlp/trending?limit=` | Popular-music feed (cached 30 min). |
| `GET /api/v1/ytdlp/related?id=&limit=` | Videos similar to `id`. |
| `GET /api/v1/ytdlp/thumb/{id}` | Cover image, relayed. |
| `GET /api/v1/ytdlp/stream/{id}` | Audio stream relay (`Range` supported). |
| `POST /api/v1/ytdlp/download` | `{"id" or "url", "artist"?, "album"?, "title"?}` → `202 {"id": jobId, "state": "queued"}`. |
| `GET /api/v1/ytdlp/jobs/{jobId}` | `{"state":"queued|running|done|error","path","hash","duplicate","error"}` |

## Development

```sh
go test ./...
go run . # with the env vars above
```
