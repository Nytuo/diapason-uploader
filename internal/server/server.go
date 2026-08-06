// Package server exposes the HTTP API used by Diapason clients to push tracks.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/nytuo/diapason-uploader/internal/config"
	"github.com/nytuo/diapason-uploader/internal/library"
	"github.com/nytuo/diapason-uploader/internal/rescan"
)

type Server struct {
	cfg     *config.Config
	lib     *library.Library
	rescan  *rescan.Trigger
	maxSize int64

	limiter *authLimiter
}

func New(cfg *config.Config, lib *library.Library, r *rescan.Trigger) *Server {
	return &Server{cfg: cfg, lib: lib, rescan: r, maxSize: cfg.MaxUploadBytes, limiter: newAuthLimiter()}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/v1/exists", s.auth(s.handleExists))
	mux.HandleFunc("POST /api/v1/upload", s.auth(s.handleUpload))
	return logging(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleExists(w http.ResponseWriter, r *http.Request) {
	hash := r.URL.Query().Get("hash")
	rel, ok := s.lib.Exists(hash)
	writeJSON(w, http.StatusOK, map[string]any{"exists": ok, "path": rel})
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
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
			break
		}
		switch part.FormName() {
		case "metadata":
			if err := json.NewDecoder(part).Decode(&meta); err != nil {
				writeErr(w, http.StatusBadRequest, "invalid metadata json")
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
				log.Printf("store: %v", err)
				writeErr(w, http.StatusInternalServerError, "failed to store file")
				return
			}
			stored = true
		default:
			// ignore unknown parts (e.g. reserved "cover")
			_, _ = io.Copy(io.Discard, part)
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

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AuthToken == "" {
			next(w, r)
			return
		}
		ip := clientIP(r)
		if s.limiter.blocked(ip) {
			writeErr(w, http.StatusTooManyRequests, "too many failed attempts, slow down")
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.AuthToken)) != 1 {
			s.limiter.fail(ip)
			writeErr(w, http.StatusUnauthorized, "invalid or missing token")
			return
		}
		s.limiter.reset(ip)
		next(w, r)
	}
}

func clientIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s %s %d %s", clientIPString(r), r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
	})
}

func clientIPString(r *http.Request) string {
	if ip := clientIP(r); ip != nil {
		return ip.String()
	}
	return r.RemoteAddr
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
