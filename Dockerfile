# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
# No third-party deps: go.sum is not needed. Download is a no-op but keeps cache warm.
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/diapason-uploader .

# ---- runtime ----
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/diapason-uploader /diapason-uploader
# Music library is expected to be mounted here (shared with Navidrome/Plex).
ENV DIAPASON_MUSIC_DIR=/music
ENV DIAPASON_LISTEN_ADDR=0.0.0.0:8688
EXPOSE 8688
USER nonroot:nonroot
ENTRYPOINT ["/diapason-uploader"]
