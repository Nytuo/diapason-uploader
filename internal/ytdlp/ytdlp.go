// Package ytdlp wraps the yt-dlp executable so the uploader can act as a
// search/download proxy for Diapason clients.
package ytdlp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nytuo/diapason-uploader/internal/config"
)

var (
	ErrBusy      error = &Error{Kind: KindBusy, Msg: "The server is busy with other YouTube requests. Try again in a moment."}
	ErrBadTarget error = &Error{Kind: KindBadTarget, Msg: "That isn't a YouTube video ID or link."}

	idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
)

const (
	maxOutputBytes = 8 << 20
	searchTimeout  = 45 * time.Second
	infoTimeout    = 45 * time.Second
	downloadTO     = 10 * time.Minute
	maxQueryLen    = 200
	maxResults     = 30
)

// Entry is one video in a search or feed result.
type Entry struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Duration int    `json:"durationSecs"`
}

// Meta is the tag-like information gathered while downloading a video.
type Meta struct {
	Title    string
	Artist   string
	Album    string
	Track    int
	Year     int
	Duration int
}

// Downloaded is a finished download sitting in a temporary directory.
type Downloaded struct {
	Path string
	Ext  string
	Meta Meta
	dir  string
}

// Cleanup removes the temporary download directory.
func (d *Downloaded) Cleanup() {
	if d != nil && d.dir != "" {
		_ = os.RemoveAll(d.dir)
	}
}

// Manager runs yt-dlp commands within the configured limits.
type Manager struct {
	cfg     config.YtDlp
	workDir string
	sem     chan struct{}
	ffmpeg  bool
	jsArg   string
	jsName  string
	client  *http.Client

	mu       sync.Mutex
	version  string
	updated  time.Time
	feed     []Entry
	feedTime time.Time
}

// New prepares a Manager. workDir is a scratch directory owned by the
// uploader (downloads land in per-job subdirectories and are always removed).
func New(cfg config.YtDlp, workDir string) (*Manager, error) {
	if err := os.MkdirAll(filepath.Join(workDir, "home"), 0o700); err != nil {
		return nil, fmt.Errorf("create yt-dlp work dir: %w", err)
	}
	m := &Manager{
		cfg:     cfg,
		workDir: workDir,
		sem:     make(chan struct{}, cfg.MaxConcurrent),
		client: &http.Client{
			Timeout: 15 * time.Second,
			// Thumbnails never legitimately redirect.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	_, err := exec.LookPath("ffmpeg")
	m.ffmpeg = err == nil
	m.jsName, m.jsArg = detectJSRuntime(cfg.JSRuntime)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := m.run(ctx, "--version")
	if err != nil {
		return nil, fmt.Errorf("yt-dlp not usable (%s): %w", cfg.Bin, err)
	}
	if m.jsName == "" && cfg.JSRuntime != "none" {
		log.Printf("WARNING: no JavaScript runtime (deno/node/bun) found — recent YouTube changes need one; some videos will fail")
	}
	m.version = strings.TrimSpace(string(out))
	return m, nil
}

// Version returns the installed yt-dlp version.
func (m *Manager) Version() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.version
}

// JSRuntime names the JavaScript runtime handed to yt-dlp ("" if none found).
func (m *Manager) JSRuntime() string { return m.jsName }

// detectJSRuntime finds a runtime yt-dlp can use to solve YouTube's player
// challenges. want is "auto", "none", or a runtime name.
func detectJSRuntime(want string) (name, arg string) {
	if want == "none" {
		return "", ""
	}
	candidates := []string{"deno", "node", "bun", "quickjs"}
	bin := map[string]string{"deno": "deno", "node": "node", "bun": "bun", "quickjs": "qjs"}
	if want != "" && want != "auto" {
		candidates = []string{want}
	}
	for _, c := range candidates {
		b, ok := bin[c]
		if !ok {
			continue
		}
		if path, err := exec.LookPath(b); err == nil {
			return c, c + ":" + path
		}
	}
	return "", ""
}

// HasFFmpeg reports whether tag/cover embedding is available.
func (m *Manager) HasFFmpeg() bool { return m.ffmpeg }

// ParseTarget extracts a video ID from a bare ID or a YouTube link.
func ParseTarget(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if idRe.MatchString(s) {
		return s, true
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", false
	}
	first := func(prefix string) string {
		rest := strings.TrimPrefix(u.Path, prefix)
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			rest = rest[:i]
		}
		return rest
	}
	var id string
	switch strings.ToLower(u.Hostname()) {
	case "youtu.be":
		id = first("/")
	case "youtube.com", "www.youtube.com", "m.youtube.com", "music.youtube.com":
		switch {
		case u.Path == "/watch":
			id = u.Query().Get("v")
		case strings.HasPrefix(u.Path, "/shorts/"):
			id = first("/shorts/")
		case strings.HasPrefix(u.Path, "/embed/"):
			id = first("/embed/")
		case strings.HasPrefix(u.Path, "/live/"):
			id = first("/live/")
		}
	}
	if idRe.MatchString(id) {
		return id, true
	}
	return "", false
}

// ValidID reports whether s is a well-formed video ID.
func ValidID(s string) bool { return idRe.MatchString(s) }

func watchURL(id string) string { return "https://www.youtube.com/watch?v=" + id }

func (m *Manager) acquire(ctx context.Context) error {
	select {
	case m.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ErrBusy
	}
}

func (m *Manager) release() { <-m.sem }

func (m *Manager) baseArgs() []string {
	args := []string{"--ignore-config", "--no-cache-dir", "--no-warnings", "--no-colors"}
	if m.cfg.OutboundProxy != "" {
		args = append(args, "--proxy", m.cfg.OutboundProxy)
	}
	if m.jsArg != "" {
		args = append(args, "--js-runtimes", m.jsArg)
	}
	return args
}

func (m *Manager) env() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(m.workDir, "home"),
		"TMPDIR=" + m.workDir,
		"LANG=C.UTF-8",
		"PYTHONNOUSERSITE=1",
	}
	for _, k := range []string{"SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// run executes yt-dlp with args (already including base flags if wanted) and
// returns bounded stdout. No slot is taken; callers do that.
func (m *Manager) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, m.cfg.Bin, args...)
	cmd.Env = m.env()
	cmd.Dir = m.workDir
	var stdout, stderr limitedBuffer
	stdout.max, stderr.max = maxOutputBytes, 64<<10
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, &Error{Kind: KindTimeout, Msg: "yt-dlp took too long on the server and was stopped.", Detail: ctx.Err().Error()}
		}
		if errors.Is(err, exec.ErrNotFound) {
			return nil, &Error{Kind: KindFailed, Msg: "yt-dlp isn't installed on the server.", Detail: err.Error()}
		}
		e := classify(stderr.String())
		if e.Detail == "" {
			e.Detail = err.Error()
		}
		return nil, e
	}
	return stdout.Bytes(), nil
}

func (m *Manager) runLimited(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := m.acquire(ctx); err != nil {
		return nil, err
	}
	defer m.release()
	return m.run(ctx, args...)
}

type flatEntry struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Channel  string   `json:"channel"`
	Uploader string   `json:"uploader"`
	Duration *float64 `json:"duration"`
}

func parseEntries(raw []byte) ([]Entry, error) {
	var doc struct {
		Entries []flatEntry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, &Error{Kind: KindOutdated, Msg: "yt-dlp returned something unexpected.", Detail: err.Error()}
	}
	out := make([]Entry, 0, len(doc.Entries))
	for _, e := range doc.Entries {
		if !idRe.MatchString(e.ID) || e.Title == "" {
			continue // channels, playlists, removed videos
		}
		d := 0
		if e.Duration != nil {
			d = int(*e.Duration)
		}
		artist := e.Channel
		if artist == "" {
			artist = e.Uploader
		}
		out = append(out, Entry{ID: e.ID, Title: e.Title, Artist: cleanArtist(artist), Duration: d})
	}
	return out, nil
}

// Search returns up to limit YouTube results for query.
func (m *Manager) Search(ctx context.Context, query string, limit int) ([]Entry, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("empty query")
	}
	if len(query) > maxQueryLen {
		query = query[:maxQueryLen]
	}
	if limit < 1 || limit > maxResults {
		limit = 15
	}
	args := append(m.baseArgs(), "--flat-playlist", "-J", "--skip-download", "--",
		"ytsearch"+strconv.Itoa(limit)+":"+query)
	out, err := m.runLimited(ctx, searchTimeout, args...)
	if err != nil {
		return nil, err
	}
	return parseEntries(out)
}

// Trending returns the configured popular-music chart, cached for 30 minutes.
func (m *Manager) Trending(ctx context.Context, limit int) ([]Entry, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	m.mu.Lock()
	if time.Since(m.feedTime) < 30*time.Minute && len(m.feed) > 0 {
		feed := m.feed
		m.mu.Unlock()
		return clip(feed, limit), nil
	}
	m.mu.Unlock()

	args := append(m.baseArgs(), "--flat-playlist", "-J", "--skip-download", "--playlist-end", "100", "--", m.cfg.TrendingURL)
	out, err := m.runLimited(ctx, infoTimeout, args...)
	if err != nil {
		return nil, err
	}
	entries, err := parseEntries(out)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.feed, m.feedTime = entries, time.Now()
	m.mu.Unlock()
	return clip(entries, limit), nil
}

// Lookup returns one video's details.
func (m *Manager) Lookup(ctx context.Context, id string) (Entry, error) {
	if !idRe.MatchString(id) {
		return Entry{}, ErrBadTarget
	}
	args := append(m.baseArgs(), "--no-playlist", "--skip-download", "-J", "--", watchURL(id))
	out, err := m.runLimited(ctx, infoTimeout, args...)
	if err != nil {
		return Entry{}, err
	}
	var info struct {
		flatEntry
		Track  string `json:"track"`
		Artist string `json:"artist"`
	}
	if err := json.Unmarshal(out, &info); err != nil || !idRe.MatchString(info.ID) {
		return Entry{}, &Error{Kind: KindOutdated, Msg: "yt-dlp returned something unexpected.", Detail: "unparseable video info"}
	}
	d := 0
	if info.Duration != nil {
		d = int(*info.Duration)
	}
	return Entry{
		ID:       info.ID,
		Title:    firstNonEmpty(info.Track, info.Title),
		Artist:   cleanArtist(firstNonEmpty(info.Artist, info.Channel, info.Uploader)),
		Duration: d,
	}, nil
}

// Related returns videos similar to id, found by searching on the video's own
// artist and title (yt-dlp has no stable "related" extractor).
func (m *Manager) Related(ctx context.Context, id string, limit int) ([]Entry, error) {
	seed, err := m.Lookup(ctx, id)
	if err != nil {
		return nil, err
	}
	results, err := m.Search(ctx, strings.TrimSpace(seed.Artist+" "+seed.Title), limit+1)
	if err != nil {
		return nil, err
	}
	filtered := results[:0]
	for _, e := range results {
		if e.ID != id {
			filtered = append(filtered, e)
		}
	}
	return clip(filtered, limit), nil
}

func clip(e []Entry, n int) []Entry {
	if len(e) > n {
		return e[:n]
	}
	return e
}

// Download fetches the best audio for id into a fresh temporary directory.
// The caller must call Cleanup on the result.
func (m *Manager) Download(ctx context.Context, id string) (*Downloaded, error) {
	if !idRe.MatchString(id) {
		return nil, ErrBadTarget
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTO)
	defer cancel()
	if err := m.acquire(ctx); err != nil {
		return nil, err
	}
	defer m.release()

	dir, err := os.MkdirTemp(m.workDir, "dl-*")
	if err != nil {
		return nil, err
	}
	d := &Downloaded{dir: dir}
	ok := false
	defer func() {
		if !ok {
			d.Cleanup()
		}
	}()

	args := append(m.baseArgs(),
		"--no-playlist", "--no-progress", "--no-mtime",
		"-f", "bestaudio[ext=m4a]/bestaudio",
		"--write-info-json",
		"-o", filepath.Join(dir, "%(id)s.%(ext)s"),
	)
	if m.cfg.MaxFileBytes > 0 {
		args = append(args, "--max-filesize", strconv.FormatInt(m.cfg.MaxFileBytes, 10))
	}
	filter := "!is_live"
	if m.cfg.MaxDurationSecs > 0 {
		filter += " & duration<=" + strconv.Itoa(m.cfg.MaxDurationSecs)
	}
	args = append(args, "--match-filters", filter)
	if m.ffmpeg {
		args = append(args, "--embed-metadata", "--embed-thumbnail", "--convert-thumbnails", "jpg")
	}
	args = append(args, "--", watchURL(id))

	if _, err := m.run(ctx, args...); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasSuffix(name, ".info.json") || strings.HasPrefix(name, ".") {
			continue
		}
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
		switch ext {
		case "jpg", "jpeg", "png", "webp", "part", "ytdl", "json":
			continue
		}
		d.Path = filepath.Join(dir, name)
		d.Ext = ext
		break
	}
	if d.Path == "" {
		return nil, &Error{Kind: KindNothing, Msg: "Nothing was downloaded — the video may be a live stream or longer than this server allows."}
	}
	d.Meta = readMeta(filepath.Join(dir, id+".info.json"))
	ok = true
	return d, nil
}

func readMeta(path string) Meta {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Meta{}
	}
	var info struct {
		Title       string   `json:"title"`
		Track       string   `json:"track"`
		Artist      string   `json:"artist"`
		Artists     []string `json:"artists"`
		Album       string   `json:"album"`
		Channel     string   `json:"channel"`
		Uploader    string   `json:"uploader"`
		TrackNumber int      `json:"track_number"`
		ReleaseYear int      `json:"release_year"`
		UploadDate  string   `json:"upload_date"`
		Duration    float64  `json:"duration"`
	}
	if json.Unmarshal(raw, &info) != nil {
		return Meta{}
	}
	meta := Meta{
		Title:    firstNonEmpty(info.Track, info.Title),
		Artist:   cleanArtist(firstNonEmpty(info.Artist, strings.Join(info.Artists, ", "), info.Channel, info.Uploader)),
		Album:    info.Album,
		Track:    info.TrackNumber,
		Year:     info.ReleaseYear,
		Duration: int(info.Duration),
	}
	if meta.Year == 0 && len(info.UploadDate) >= 4 {
		meta.Year, _ = strconv.Atoi(info.UploadDate[:4])
	}
	return meta
}

// StreamURL resolves a short-lived direct audio URL for id. The URL is bound
// to this server's IP, so clients must fetch it through the uploader.
func (m *Manager) StreamURL(ctx context.Context, id string) (*url.URL, error) {
	if !idRe.MatchString(id) {
		return nil, ErrBadTarget
	}
	args := append(m.baseArgs(), "--no-playlist", "-f", "bestaudio[ext=m4a]/bestaudio", "-g", "--", watchURL(id))
	out, err := m.runLimited(ctx, infoTimeout, args...)
	if err != nil {
		return nil, err
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	u, err := url.Parse(line)
	if err != nil || u.Scheme != "https" || !allowedMediaHost(u.Hostname()) {
		return nil, &Error{Kind: KindOutdated, Msg: "yt-dlp returned an unexpected stream address.", Detail: "host not on the media allow-list"}
	}
	return u, nil
}

func allowedMediaHost(h string) bool {
	h = strings.ToLower(h)
	return strings.HasSuffix(h, ".googlevideo.com") || strings.HasSuffix(h, ".youtube.com")
}

// Thumbnail fetches a video's cover through the server so the client's IP
// never reaches Google's image CDN.
func (m *Manager) Thumbnail(ctx context.Context, id string) ([]byte, string, error) {
	if !idRe.MatchString(id) {
		return nil, "", ErrBadTarget
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://i.ytimg.com/vi/"+id+"/mqdefault.jpg", nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("thumbnail: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return body, "image/jpeg", err
}

// RunUpdater keeps yt-dlp current until ctx is cancelled.
func (m *Manager) RunUpdater(ctx context.Context) {
	if !m.cfg.AutoUpdate || m.cfg.UpdateInterval <= 0 {
		return
	}
	m.update(ctx)
	t := time.NewTicker(m.cfg.UpdateInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.update(ctx)
		}
	}
}

func (m *Manager) update(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	for i := 0; i < cap(m.sem); i++ {
		select {
		case m.sem <- struct{}{}:
		case <-ctx.Done():
			for ; i > 0; i-- {
				<-m.sem
			}
			return
		}
	}
	defer func() {
		for i := 0; i < cap(m.sem); i++ {
			<-m.sem
		}
	}()

	before := m.Version()
	if _, err := m.run(ctx, "--ignore-config", "-U"); err != nil {
		log.Printf("yt-dlp self-update skipped: %v", err)
	}
	if v, err := m.run(ctx, "--version"); err == nil {
		now := strings.TrimSpace(string(v))
		m.mu.Lock()
		m.version, m.updated = now, time.Now()
		m.mu.Unlock()
		if now != before {
			log.Printf("yt-dlp updated %s -> %s", before, now)
		} else {
			log.Printf("yt-dlp is up to date (%s)", now)
		}
	}
}

func cleanArtist(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, " - Topic")
	s = strings.TrimSuffix(s, "VEVO")
	return strings.TrimSpace(s)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// limitedBuffer keeps at most max bytes and silently drops the rest.
type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *limitedBuffer) String() string { return b.buf.String() }
