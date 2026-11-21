package ytdlp

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Streaming plays from a small on-disk cache: yt-dlp downloads the audio itself
// (so it handles everything YouTube demands of a download), and the server then
// serves the file with normal range support. This avoids refetching the signed
// googlevideo URL from Go, which YouTube sometimes refuses with 403.

const streamCacheMaxBytes = 1 << 30 // 1 GiB

type cacheFlight struct {
	done chan struct{}
	path string
	err  error
}

var (
	cacheMu      sync.Mutex
	cacheFlights = map[string]*cacheFlight{}
)

func (m *Manager) cacheDir() string { return filepath.Join(m.workDir, "streamcache") }

// cached returns the finished cache file for id, if any.
func (m *Manager) cached(id string) string {
	entries, err := os.ReadDir(m.cacheDir())
	if err != nil {
		return ""
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, id+".") {
			continue
		}
		switch strings.ToLower(strings.TrimPrefix(filepath.Ext(name), ".")) {
		case "part", "ytdl", "json", "tmp":
			continue
		}
		return filepath.Join(m.cacheDir(), name)
	}
	return ""
}

// CachedAudio returns a local file holding id's audio, downloading it with
// yt-dlp first if needed. Concurrent callers for the same track share one download.
func (m *Manager) CachedAudio(ctx context.Context, id string) (string, error) {
	if !idRe.MatchString(id) {
		return "", ErrBadTarget
	}
	if p := m.cached(id); p != "" {
		now := time.Now()
		_ = os.Chtimes(p, now, now) // LRU: recently played stays
		return p, nil
	}
	cacheMu.Lock()
	f := cacheFlights[id]
	if f == nil {
		f = &cacheFlight{done: make(chan struct{})}
		cacheFlights[id] = f
		go func() {
			// Detached: one listener giving up must not abort the download for others.
			f.path, f.err = m.downloadToCache(context.WithoutCancel(ctx), id)
			cacheMu.Lock()
			delete(cacheFlights, id)
			cacheMu.Unlock()
			close(f.done)
		}()
	}
	cacheMu.Unlock()
	select {
	case <-f.done:
		return f.path, f.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (m *Manager) downloadToCache(ctx context.Context, id string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTO)
	defer cancel()
	release, err := m.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()

	dir := m.cacheDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	args := append(m.baseArgs(),
		"--no-playlist", "--no-progress", "--no-mtime",
		"-f", "bestaudio[ext=m4a]/bestaudio",
		"--write-info-json", // lets a played track be moved into the library with its tags
		"-o", filepath.Join(dir, "%(id)s.%(ext)s"),
	)
	if m.ffmpeg {
		args = append(args, "--embed-metadata")
	}
	if m.cfg.MaxFileBytes > 0 {
		args = append(args, "--max-filesize", strconv.FormatInt(m.cfg.MaxFileBytes, 10))
	}
	filter := "!is_live"
	if m.cfg.MaxDurationSecs > 0 {
		filter += " & duration<=" + strconv.Itoa(m.cfg.MaxDurationSecs)
	}
	args = append(args, "--match-filters", filter, "--", watchURL(id))
	if _, err := m.run(ctx, args...); err != nil {
		return "", err
	}
	p := m.cached(id)
	if p == "" {
		return "", &Error{Kind: KindNothing, Msg: "Nothing was downloaded — the video may be a live stream or longer than this server allows."}
	}
	m.pruneCache(p)
	return p, nil
}

// pruneCache deletes the least recently played files until the cache fits, never touching keep.
func (m *Manager) pruneCache(keep string) {
	entries, err := os.ReadDir(m.cacheDir())
	if err != nil {
		return
	}
	type file struct {
		path string
		size int64
		mod  time.Time
	}
	var files []file
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || e.IsDir() {
			continue
		}
		p := filepath.Join(m.cacheDir(), e.Name())
		files = append(files, file{p, info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files {
		if total <= streamCacheMaxBytes {
			break
		}
		if f.path == keep || strings.HasSuffix(f.path, ".part") {
			continue
		}
		if os.Remove(f.path) == nil {
			total -= f.size
			log.Printf("stream cache: evicted %s", filepath.Base(f.path))
		}
	}
}

// Precache starts downloading id in the background (if it isn't cached already) and
// returns at once. ready reports whether the file is already there.
func (m *Manager) Precache(ctx context.Context, id string) (ready bool, err error) {
	if !idRe.MatchString(id) {
		return false, ErrBadTarget
	}
	if m.cached(id) != "" {
		return true, nil
	}
	bg := context.WithoutCancel(ctx)
	go func() {
		if _, err := m.CachedAudio(bg, id); err != nil {
			log.Printf("precache %s: %v", id, err)
		}
	}()
	return false, nil
}

// TakeCached hands over id's cached download for the library, if there is one. The
// caller moves it into place and then calls Drop; until then the files stay put.
func (m *Manager) TakeCached(id string) *Downloaded {
	if !idRe.MatchString(id) {
		return nil
	}
	p := m.cached(id)
	if p == "" {
		return nil
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(p), "."))
	return &Downloaded{Path: p, Ext: ext, Meta: readMeta(filepath.Join(m.cacheDir(), id+".info.json"))}
}

// DropCached removes id's cache files (after they were moved into the library).
func (m *Manager) DropCached(id string) {
	if !idRe.MatchString(id) {
		return
	}
	if p := m.cached(id); p != "" {
		_ = os.Remove(p)
	}
	_ = os.Remove(filepath.Join(m.cacheDir(), id+".info.json"))
}
