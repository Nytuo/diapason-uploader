# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/diapason-uploader .

# ---- runtime ----
# Alpine (not distroless) because the optional yt-dlp proxy needs ffmpeg and a
# writable tool directory. The yt-dlp feature stays OFF unless
# DIAPASON_YTDLP_ENABLED=true.
FROM alpine:3.22
ARG TARGETARCH
# nodejs: yt-dlp needs a JavaScript runtime to solve YouTube's player challenges.
RUN apk add --no-cache ca-certificates ffmpeg nodejs \
 && adduser -D -u 10001 diapason \
 && mkdir -p /opt/yt-dlp /work /music \
 && chown -R diapason /opt/yt-dlp /work /music
# Standalone yt-dlp build (bundles its own Python) in a directory the service
# user can write, so `yt-dlp -U` can replace it in place — the uploader runs
# that on a schedule, which is the whole point: extractor fixes land within a
# day, with no app or image release.
RUN set -eux; \
    case "$TARGETARCH" in \
      arm64) asset=yt-dlp_musllinux_aarch64 ;; \
      *)     asset=yt-dlp_musllinux ;; \
    esac; \
    wget -q -O /opt/yt-dlp/yt-dlp "https://github.com/yt-dlp/yt-dlp/releases/latest/download/$asset"; \
    chmod 0755 /opt/yt-dlp/yt-dlp; chown diapason /opt/yt-dlp/yt-dlp

COPY --from=build /out/diapason-uploader /usr/local/bin/diapason-uploader
# Music library is expected to be mounted here (shared with Navidrome/Plex).
ENV DIAPASON_MUSIC_DIR=/music \
    DIAPASON_LISTEN_ADDR=0.0.0.0:8688 \
    DIAPASON_WORK_DIR=/work \
    DIAPASON_YTDLP_BIN=/opt/yt-dlp/yt-dlp
EXPOSE 8688
USER diapason
ENTRYPOINT ["/usr/local/bin/diapason-uploader"]
