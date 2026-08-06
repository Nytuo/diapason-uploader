// Package config loads Diapason Uploader configuration from the environment.
package config

import (
	"errors"
	"os"
	"strconv"
	"strings"

	"github.com/nytuo/diapason-uploader/internal/library"
)

const TokenFileName = ".diapason-uploader-token"

// Config holds the runtime configuration for the uploader service.
type Config struct {
	// MusicDir is the root of the server's music library. Uploaded files are
	// written here and this is the directory Navidrome/Plex scan.
	MusicDir string
	// ListenAddr is the bind address, e.g. "0.0.0.0:8688" (internet/LAN) or
	// "127.0.0.1:8688" (local host only).
	ListenAddr string
	// AuthToken is the shared bearer token required on every request. Empty
	// disables auth (not recommended; only for trusted local networks).
	AuthToken string
	// PathTemplate controls where files are placed under MusicDir. Supported
	// placeholders: {artist} {album} {title} {track} {track2} {disc} {year} {ext}.
	PathTemplate   string
	TLSCert        string
	TLSKey         string
	MaxUploadBytes int64

	NavidromeURL       string
	NavidromeUser      string
	NavidromePassword  string
	PlexURL            string
	PlexToken          string
	PlexSection        string
	RescanDebounceSecs int

	// MDNSEnabled controls whether the service advertises itself on the LAN
	// so Diapason clients can discover it automatically.
	MDNSEnabled bool
	// MDNSName is the mDNS instance name clients see when browsing.
	MDNSName string
}

func Load() (*Config, error) {
	c := &Config{
		MusicDir:           env("DIAPASON_MUSIC_DIR", ""),
		ListenAddr:         env("DIAPASON_LISTEN_ADDR", "0.0.0.0:8688"),
		AuthToken:          env("DIAPASON_AUTH_TOKEN", ""),
		PathTemplate:       env("DIAPASON_PATH_TEMPLATE", library.DefaultPathTemplate),
		TLSCert:            env("DIAPASON_TLS_CERT", ""),
		TLSKey:             env("DIAPASON_TLS_KEY", ""),
		MaxUploadBytes:     envInt64("DIAPASON_MAX_UPLOAD_BYTES", 512<<20),
		NavidromeURL:       strings.TrimRight(env("DIAPASON_NAVIDROME_URL", ""), "/"),
		NavidromeUser:      env("DIAPASON_NAVIDROME_USER", ""),
		NavidromePassword:  env("DIAPASON_NAVIDROME_PASSWORD", ""),
		PlexURL:            strings.TrimRight(env("DIAPASON_PLEX_URL", ""), "/"),
		PlexToken:          env("DIAPASON_PLEX_TOKEN", ""),
		PlexSection:        env("DIAPASON_PLEX_SECTION", ""),
		RescanDebounceSecs: int(envInt64("DIAPASON_RESCAN_DEBOUNCE_SECS", 5)),
		MDNSEnabled:        envBool("DIAPASON_MDNS_ENABLED", true),
		MDNSName:           env("DIAPASON_MDNS_NAME", hostname()),
	}

	if c.MusicDir == "" {
		return nil, errors.New("DIAPASON_MUSIC_DIR is required")
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, errors.New("DIAPASON_TLS_CERT and DIAPASON_TLS_KEY must be set together")
	}
	if c.PathTemplate == "" {
		c.PathTemplate = library.DefaultPathTemplate
	}
	return c, nil
}

func (c *Config) TLSEnabled() bool { return c.TLSCert != "" && c.TLSKey != "" }

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "diapason-uploader"
	}
	return h
}
