// Package config loads Diapason Uploader configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
	MDNSName      string
	StateDir      string
	Pairing       string
	PairingWindow time.Duration
	PairingPublic bool
	// PublicURL is the address clients should use away from the LAN (reverse
	// proxy, Tailscale name…). Handed to paired apps so they can fall back to it.
	PublicURL      string
	PlainHTTPOK    bool
	TrustedProxies []*net.IPNet
	WorkDir        string

	YtDlp YtDlp
}

// YtDlp configures the optional yt-dlp proxy feature.
type YtDlp struct {
	Enabled         bool
	Bin             string
	AutoUpdate      bool
	UpdateInterval  time.Duration
	MaxDurationSecs int
	MaxFileBytes    int64
	MaxConcurrent   int // simultaneous yt-dlp processes per paired device
	MaxTotal        int // ceiling across all devices
	OutboundProxy   string
	JSRuntime       string
	TrendingURL     string
}

// DefaultTrendingURL is YouTube's "Top 100 Music Videos Global" chart playlist.
const DefaultTrendingURL = "https://www.youtube.com/playlist?list=PL4fGSI1pDJn6puJdseH2Rt9sMvt9E2M4i"

// MinTokenLength is the shortest user-supplied auth token accepted.
const MinTokenLength = 24

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
		WorkDir:            env("DIAPASON_WORK_DIR", filepath.Join(os.TempDir(), "diapason-uploader")),
		StateDir:           env("DIAPASON_STATE_DIR", ""),
		Pairing:            strings.ToLower(env("DIAPASON_PAIRING", "window")),
		PairingWindow:      time.Duration(envInt64("DIAPASON_PAIRING_WINDOW_MINUTES", 15)) * time.Minute,
		PairingPublic:      envBool("DIAPASON_PAIRING_ALLOW_PUBLIC", false),
		PublicURL:          strings.TrimRight(strings.TrimSpace(env("DIAPASON_PUBLIC_URL", "")), "/"),
		PlainHTTPOK:        envBool("DIAPASON_PLAIN_HTTP_OK", false),
		YtDlp: YtDlp{
			Enabled:         envBool("DIAPASON_YTDLP_ENABLED", false),
			Bin:             env("DIAPASON_YTDLP_BIN", "yt-dlp"),
			AutoUpdate:      envBool("DIAPASON_YTDLP_AUTO_UPDATE", true),
			UpdateInterval:  time.Duration(envInt64("DIAPASON_YTDLP_UPDATE_INTERVAL_HOURS", 24)) * time.Hour,
			MaxDurationSecs: int(envInt64("DIAPASON_YTDLP_MAX_DURATION_SECS", 3600)),
			MaxFileBytes:    envInt64("DIAPASON_YTDLP_MAX_FILE_BYTES", 256<<20),
			MaxConcurrent:   int(envInt64("DIAPASON_YTDLP_MAX_CONCURRENT", 2)),
			MaxTotal:        int(envInt64("DIAPASON_YTDLP_MAX_TOTAL", 16)),
			OutboundProxy:   env("DIAPASON_YTDLP_PROXY", ""),
			JSRuntime:       strings.ToLower(env("DIAPASON_YTDLP_JS_RUNTIME", "auto")),
			TrendingURL:     env("DIAPASON_YTDLP_TRENDING_URL", DefaultTrendingURL),
		},
	}

	if c.StateDir == "" {
		c.StateDir = c.MusicDir
	}
	switch c.Pairing {
	case "off", "window", "open":
	default:
		return nil, fmt.Errorf("DIAPASON_PAIRING must be off, window or open (got %q)", c.Pairing)
	}
	if c.PairingWindow < time.Minute {
		c.PairingWindow = time.Minute
	}

	proxies, err := parseCIDRs(env("DIAPASON_TRUSTED_PROXIES", ""))
	if err != nil {
		return nil, err
	}
	c.TrustedProxies = proxies

	if c.AuthToken != "" && len(c.AuthToken) < MinTokenLength {
		return nil, fmt.Errorf("DIAPASON_AUTH_TOKEN must be at least %d characters (try `openssl rand -hex 24`)", MinTokenLength)
	}
	if c.YtDlp.MaxTotal < c.YtDlp.MaxConcurrent {
		c.YtDlp.MaxTotal = c.YtDlp.MaxConcurrent
	}
	if c.YtDlp.MaxConcurrent < 1 {
		c.YtDlp.MaxConcurrent = 1
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

// parseCIDRs parses a comma-separated list of CIDRs or bare IPs.
func parseCIDRs(list string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, part := range strings.Split(list, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			ip := net.ParseIP(part)
			if ip == nil {
				return nil, fmt.Errorf("DIAPASON_TRUSTED_PROXIES: invalid address %q", part)
			}
			bits := 128
			if ip.To4() != nil {
				bits = 32
			}
			part = fmt.Sprintf("%s/%d", ip, bits)
		}
		_, n, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("DIAPASON_TRUSTED_PROXIES: %w", err)
		}
		out = append(out, n)
	}
	return out, nil
}
