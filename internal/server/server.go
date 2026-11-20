// Package server exposes the HTTP API used by Diapason clients to push tracks.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nytuo/diapason-uploader/internal/config"
	"github.com/nytuo/diapason-uploader/internal/devices"
	"github.com/nytuo/diapason-uploader/internal/library"
	"github.com/nytuo/diapason-uploader/internal/rescan"
	"github.com/nytuo/diapason-uploader/internal/ytdlp"
)

const (
	maxMetadataBytes     = 64 << 10
	maxConcurrentUploads = 4

	apiDeadline    = 30 * time.Second
	uploadDeadline = 30 * time.Minute
)

type Server struct {
	cfg     *config.Config
	lib     *library.Library
	rescan  *rescan.Trigger
	maxSize int64

	limiter     *authLimiter
	tokenDigest [sha256.Size]byte
	uploadSem   chan struct{}

	devices   *devices.Store
	pairMu    sync.Mutex
	pairUntil time.Time

	yt     *ytdlp.Manager
	jobs   *jobStore
	bgCtx  context.Context
	stream *streamProxy
}

// New builds the server. yt may be nil when the yt-dlp proxy is disabled.
// bgCtx scopes background work (downloads) to the process lifetime.
func New(bgCtx context.Context, cfg *config.Config, lib *library.Library, r *rescan.Trigger, yt *ytdlp.Manager, devs *devices.Store) *Server {
	s := &Server{
		devices:     devs,
		cfg:         cfg,
		lib:         lib,
		rescan:      r,
		maxSize:     cfg.MaxUploadBytes,
		limiter:     newAuthLimiter(),
		tokenDigest: sha256.Sum256([]byte(cfg.AuthToken)),
		uploadSem:   make(chan struct{}, maxConcurrentUploads),
		yt:          yt,
		jobs:        newJobStore(),
		bgCtx:       bgCtx,
		stream:      newStreamProxy(),
	}
	if cfg.Pairing == "window" && devs.Count() == 0 {
		s.openPairingWindow()
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/v1/capabilities", s.auth(s.handleCapabilities))
	mux.HandleFunc("POST /api/v1/pair", s.handlePair)
	mux.HandleFunc("POST /api/v1/pair/open", s.auth(s.handlePairOpen))
	mux.HandleFunc("GET /api/v1/devices", s.auth(s.handleDevices))
	mux.HandleFunc("DELETE /api/v1/devices/{id}", s.auth(s.handleRevokeDevice))
	mux.HandleFunc("GET /api/v1/exists", s.auth(s.handleExists))
	mux.HandleFunc("POST /api/v1/upload", s.auth(withDeadline(uploadDeadline, s.handleUpload)))
	if s.yt == nil {
		mux.HandleFunc("/api/v1/ytdlp/", s.auth(func(w http.ResponseWriter, _ *http.Request) {
			writeErrCode(w, http.StatusNotImplemented, "ytdlp", "ytdlp_disabled",
				"The YouTube proxy is turned off on this server (set DIAPASON_YTDLP_ENABLED=true).", "")
		}))
	} else {
		mux.HandleFunc("GET /api/v1/ytdlp/search", s.auth(s.handleYtSearch))
		mux.HandleFunc("GET /api/v1/ytdlp/trending", s.auth(s.handleYtTrending))
		mux.HandleFunc("GET /api/v1/ytdlp/related", s.auth(s.handleYtRelated))
		mux.HandleFunc("GET /api/v1/ytdlp/thumb/{id}", s.auth(s.handleYtThumb))
		mux.HandleFunc("GET /api/v1/ytdlp/stream/{id}", s.auth(withDeadline(6*time.Hour, s.handleYtStream)))
		mux.HandleFunc("POST /api/v1/ytdlp/download", s.auth(s.handleYtDownload))
		mux.HandleFunc("GET /api/v1/ytdlp/jobs/{id}", s.auth(s.handleYtJob))
	}
	return s.secure(logging(s.cfg.TrustedProxies, withDeadline(apiDeadline, mux.ServeHTTP)))
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleCapabilities lets the app discover which optional features exist
// without guessing from 404s.
func (s *Server) handleCapabilities(w http.ResponseWriter, _ *http.Request) {
	caps := map[string]any{
		"version": 1,
		"upload":  true,
		"ytdlp":   s.yt != nil,
		"pairing": s.cfg.Pairing != "off",
	}
	if s.cfg.PublicURL != "" {
		caps["publicUrl"] = s.cfg.PublicURL
	}
	if s.yt != nil {
		caps["ytdlpVersion"] = s.yt.Version()
		caps["ytdlpTagging"] = s.yt.HasFFmpeg()
		caps["ytdlpJsRuntime"] = s.yt.JSRuntime()
	}
	writeJSON(w, http.StatusOK, caps)
}

func (s *Server) handleExists(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToLower(r.URL.Query().Get("hash"))
	if !library.ValidHash(hash) {
		writeErr(w, http.StatusBadRequest, "hash must be a hex sha256")
		return
	}
	rel, ok := s.lib.Exists(hash)
	writeJSON(w, http.StatusOK, map[string]any{"exists": ok, "path": rel})
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	select {
	case s.uploadSem <- struct{}{}:
		defer func() { <-s.uploadSem }()
	default:
		writeErr(w, http.StatusServiceUnavailable, "too many uploads in progress")
		return
	}

	if s.maxSize > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, s.maxSize)
	}
	reader, err := r.MultipartReader()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "expected multipart/form-data")
		return
	}

	var meta library.Metadata
	haveMeta := false
	var res library.Result
	stored := false

	for {
		part, err := reader.NextPart()
		if err != nil {
			if tooLarge(err) {
				writeErrCode(w, http.StatusRequestEntityTooLarge, "uploader", "too_large",
					"That file is larger than this server accepts (DIAPASON_MAX_UPLOAD_BYTES).", err.Error())
				return
			}
			break
		}
		switch part.FormName() {
		case "metadata":
			if err := json.NewDecoder(io.LimitReader(part, maxMetadataBytes)).Decode(&meta); err != nil {
				writeErr(w, http.StatusBadRequest, "invalid metadata json")
				return
			}
			if !library.AllowedExt(meta.Ext) {
				writeErr(w, http.StatusUnsupportedMediaType, "unsupported audio format")
				return
			}
			haveMeta = true
		case "file":
			if !haveMeta {
				writeErr(w, http.StatusBadRequest, "metadata field must precede file field")
				return
			}
			res, err = s.lib.Store(meta, part)
			if err != nil {
				if tooLarge(err) {
					writeErrCode(w, http.StatusRequestEntityTooLarge, "uploader", "too_large",
						"That file is larger than this server accepts (DIAPASON_MAX_UPLOAD_BYTES).", err.Error())
					return
				}
				log.Printf("store: %v", err)
				writeErrCode(w, http.StatusInternalServerError, "uploader", "storage_failed",
					"The server couldn't save the file to the library — check the folder's permissions and free space.", err.Error())
				return
			}
			stored = true
		default:
			_, _ = io.Copy(io.Discard, io.LimitReader(part, maxMetadataBytes))
		}
	}

	if !stored {
		writeErr(w, http.StatusBadRequest, "missing file field")
		return
	}
	if res.Duplicate {
		writeJSON(w, http.StatusConflict, res)
		return
	}
	s.rescan.Schedule()
	writeJSON(w, http.StatusCreated, res)
}

func tooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AuthToken == "" {
			next(w, r)
			return
		}
		ip := clientIP(r, s.cfg.TrustedProxies)
		if s.limiter.blocked(ip) {
			w.Header().Set("Retry-After", "60")
			writeErr(w, http.StatusTooManyRequests, "too many failed attempts, slow down")
			return
		}
		got, ok := bearer(r.Header.Get("Authorization"))
		digest := sha256.Sum256([]byte(got))
		authorized := ok && subtle.ConstantTimeCompare(digest[:], s.tokenDigest[:]) == 1
		who := "master"
		if ok && !authorized {
			var dev devices.View
			dev, authorized = s.devices.Verify(got)
			who = dev.ID
		}
		if !authorized {
			s.limiter.fail(ip)
			writeErr(w, http.StatusUnauthorized, "invalid or missing token")
			return
		}
		s.limiter.reset(ip)
		next(w, r.WithContext(ytdlp.WithOwner(r.Context(), who)))
	}
}

// bearer extracts the credential from an "Authorization: Bearer <token>" value.
func bearer(h string) (string, bool) {
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

func clientIP(r *http.Request, trusted []*net.IPNet) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil || !inNets(peer, trusted) {
		return peer
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(parts[i]))
		if ip == nil {
			return peer
		}
		if !inNets(ip, trusted) {
			return ip
		}
	}
	return peer
}

func inNets(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// secure adds defensive response headers and, over TLS, HSTS.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		if s.cfg.TLSEnabled() || r.Header.Get("X-Forwarded-Proto") == "https" {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// withDeadline replaces the connection's read/write deadlines for one handler,
// so ordinary API calls stay tight while big uploads/streams get room to finish.
func withDeadline(d time.Duration, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		deadline := time.Now().Add(d)
		_ = rc.SetReadDeadline(deadline)
		_ = rc.SetWriteDeadline(deadline)
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type errorBody struct {
	Error     string `json:"error"`
	Code      string `json:"code"`
	Component string `json:"component"`
	Detail    string `json:"detail,omitempty"`
}

var statusCodes = map[int]string{
	http.StatusBadRequest:            "bad_request",
	http.StatusUnauthorized:          "unauthorized",
	http.StatusForbidden:             "forbidden",
	http.StatusNotFound:              "not_found",
	http.StatusConflict:              "conflict",
	http.StatusRequestEntityTooLarge: "too_large",
	http.StatusUnsupportedMediaType:  "unsupported_type",
	http.StatusTooManyRequests:       "too_many_requests",
	http.StatusInternalServerError:   "internal",
	http.StatusBadGateway:            "bad_gateway",
	http.StatusServiceUnavailable:    "unavailable",
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	code := statusCodes[status]
	if code == "" {
		code = "error"
	}
	component := "uploader"
	switch status {
	case http.StatusUnauthorized:
		component = "auth"
	}
	writeJSON(w, status, errorBody{Error: msg, Code: code, Component: component})
}

func writeErrCode(w http.ResponseWriter, status int, component, code, msg, detail string) {
	writeJSON(w, status, errorBody{Error: msg, Code: code, Component: component, Detail: detail})
}

func logging(trusted []*net.IPNet, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := newRequestID()
		w.Header().Set("X-Request-Id", id)
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next(sw, r)
		ip := "?"
		if c := clientIP(r, trusted); c != nil {
			ip = c.String()
		}
		log.Printf("[%s] %s %s %s %d %s", id, ip, r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
	})
}

func newRequestID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// statusWriter captures the response status code for logging.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
