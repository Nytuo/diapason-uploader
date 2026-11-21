package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nytuo/diapason-uploader/internal/ytdlp"
)

const maxDownloadBody = 8 << 10

// ytError turns a yt-dlp failure into a classified response. The user-facing
// sentence goes in "error"; the technical cause is returned as "detail" and
// logged with the request id so it can be traced.
func (s *Server) ytError(w http.ResponseWriter, err error) {
	e, ok := ytdlp.AsError(err)
	if !ok {
		e = &ytdlp.Error{Kind: ytdlp.KindFailed, Msg: "yt-dlp failed on the server.", Detail: err.Error()}
	}
	status := http.StatusBadGateway
	switch e.Kind {
	case ytdlp.KindBusy:
		w.Header().Set("Retry-After", "5")
		status = http.StatusServiceUnavailable
	case ytdlp.KindBadTarget:
		status = http.StatusBadRequest
	case ytdlp.KindUnavailable, ytdlp.KindAgeRestrict, ytdlp.KindTooLong, ytdlp.KindTooLarge, ytdlp.KindNothing:
		status = http.StatusUnprocessableEntity
	case ytdlp.KindRateLimited:
		w.Header().Set("Retry-After", "300")
		status = http.StatusServiceUnavailable
	case ytdlp.KindNetwork, ytdlp.KindTimeout:
		status = http.StatusGatewayTimeout
	}
	log.Printf("ytdlp error kind=%s detail=%q", e.Kind, e.Detail)
	writeErrCode(w, status, "ytdlp", string(e.Kind), e.Msg, e.Detail)
}

func (s *Server) handleYtSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if strings.TrimSpace(q) == "" {
		writeErr(w, http.StatusBadRequest, "q is required")
		return
	}
	if id, ok := ytdlp.ParseTarget(q); ok {
		entry, err := s.yt.Lookup(r.Context(), id)
		if err != nil {
			s.ytError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": []ytdlp.Entry{entry}})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := s.yt.Search(r.Context(), q, limit)
	if err != nil {
		s.ytError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": entries})
}

func (s *Server) handleYtTrending(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := s.yt.Trending(r.Context(), limit)
	if err != nil {
		s.ytError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": entries})
}

func (s *Server) handleYtRelated(w http.ResponseWriter, r *http.Request) {
	id, ok := ytdlp.ParseTarget(r.URL.Query().Get("id"))
	if !ok {
		s.ytError(w, ytdlp.ErrBadTarget)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 {
		limit = 15
	}
	entries, err := s.yt.Related(r.Context(), id, limit)
	if err != nil {
		s.ytError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": entries})
}

func (s *Server) handleYtThumb(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !ytdlp.ValidID(id) {
		s.ytError(w, ytdlp.ErrBadTarget)
		return
	}
	body, ctype, err := s.yt.Thumbnail(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "thumbnail unavailable")
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(body)
}

func (s *Server) handleYtDownload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxDownloadBody)
	var req struct {
		ID     string `json:"id"`
		URL    string `json:"url"`
		Artist string `json:"artist"`
		Album  string `json:"album"`
		Title  string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	target := req.ID
	if target == "" {
		target = req.URL
	}
	id, ok := ytdlp.ParseTarget(target)
	if !ok {
		s.ytError(w, ytdlp.ErrBadTarget)
		return
	}

	job, fresh := s.jobs.start(id)
	if fresh {
		ov := override{Artist: clean(req.Artist), Album: clean(req.Album), Title: clean(req.Title)}
		go s.runDownload(ytdlp.WithOwner(s.bgCtx, ytdlp.Owner(r.Context())), job.ID, id, ov)
	}
	writeJSON(w, http.StatusAccepted, job)
}

// handleYtPrecache starts downloading a track into the stream cache so it is ready
// when the player gets to it. It answers at once: 200 if already cached, 202 if started.
func (s *Server) handleYtPrecache(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ready, err := s.yt.Precache(r.Context(), id)
	if err != nil {
		s.ytError(w, err)
		return
	}
	status := http.StatusAccepted
	if ready {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]bool{"ready": ready})
}

func clean(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

func (s *Server) handleYtJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.jobs.get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown job")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// streamProxy fetches resolved media URLs on behalf of clients.
type streamProxy struct{ client *http.Client }

func newStreamProxy() *streamProxy {
	return &streamProxy{client: &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			// IPv4 only, matching yt-dlp's --force-ipv4: the resolved URL is bound to one IP family.
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp4", addr)
			},
			// HTTP/1.1 and no gzip negotiation: mirrors what yt-dlp/wget send, which googlevideo accepts.
			ForceAttemptHTTP2:     false,
			TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
			DisableCompression:    true,
			ResponseHeaderTimeout: 20 * time.Second,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if req.URL.Scheme != "https" || !mediaHost(req.URL.Hostname()) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}}
}

func mediaHost(h string) bool {
	h = strings.ToLower(h)
	return strings.HasSuffix(h, ".googlevideo.com") || strings.HasSuffix(h, ".youtube.com")
}

// handleYtStream plays a video's audio through the server. The direct URL
// yt-dlp resolves is tied to the server's IP, so clients can't fetch it
// themselves. Range requests are forwarded so seeking works.
func (s *Server) handleYtStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !ytdlp.ValidID(id) {
		s.ytError(w, ytdlp.ErrBadTarget)
		return
	}
	// Preferred path: let yt-dlp download the audio, then serve the file (with
	// seeking). Only if that fails do we fall back to relaying the signed URL.
	if path, err := s.yt.CachedAudio(r.Context(), id); err == nil {
		serveAudioFile(w, r, path)
		return
	} else if r.Context().Err() != nil {
		return
	} else {
		log.Printf("stream %s: cache download failed (%v), relaying instead", id, err)
	}
	// A cached address can have gone stale; if the media host refuses it,
	// resolve a fresh one once before giving up.
	for attempt := 0; ; attempt++ {
		u, err := s.yt.StreamURL(r.Context(), id)
		if err != nil {
			s.ytError(w, err)
			return
		}
		reason := s.stream.copy(w, r, u)
		if reason == "" {
			return
		}
		s.yt.ForgetStream(r.Context(), id)
		log.Printf("stream %s: %s (attempt %d)", id, reason, attempt+1)
		if attempt == 1 || r.Context().Err() != nil {
			writeErr(w, http.StatusBadGateway, "YouTube refused the stream ("+reason+")")
			return
		}
	}
}

// copy relays the upstream body. It returns a non-empty reason, having
// written nothing, when the media host couldn't serve the request.
func (p *streamProxy) copy(w http.ResponseWriter, r *http.Request, u *url.URL) string {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, r.Method, u.String(), nil)
	if err != nil {
		return "bad upstream request"
	}
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0")
	resp, err := p.client.Do(req)
	if err != nil {
		return "upstream unreachable: " + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		log.Printf("stream upstream %s: proto=%s server=%q via=%q content-type=%q range=%q", resp.Status, resp.Proto,
			resp.Header.Get("Server"), resp.Header.Get("Via"), resp.Header.Get("Content-Type"), r.Header.Get("Range"))
		return "upstream HTTP " + resp.Status
	}
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return ""
}

func serveAudioFile(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cached audio unavailable")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cached audio unavailable")
		return
	}
	ctype := map[string]string{".m4a": "audio/mp4", ".mp4": "audio/mp4", ".webm": "audio/webm", ".opus": "audio/ogg", ".ogg": "audio/ogg", ".mp3": "audio/mpeg"}[strings.ToLower(filepath.Ext(path))]
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	http.ServeContent(w, r, "", info.ModTime(), f)
}
