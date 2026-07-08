// Package library handles placement of uploaded tracks into the music library
// directory, including path templating, filesystem-safe naming, and dedup.
package library

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Metadata struct {
	Artist      string `json:"artist"`
	Album       string `json:"album"`
	Title       string `json:"title"`
	TrackNumber int    `json:"trackNumber"`
	DiscNumber  int    `json:"discNumber"`
	Year        int    `json:"year"`
	Ext         string `json:"ext"` // e.g. "mp3", "m4a", "opus" (no dot)
}

type Result struct {
	RelPath string `json:"path"`
	Hash string `json:"hash"`
	Duplicate bool `json:"duplicate"`
}

type Library struct {
	root     string
	template string

	mu    sync.Mutex
	index map[string]string
}

const indexFile = ".diapason-uploader-index.json"

// DefaultPathTemplate mirrors the layout most music servers expect.
const DefaultPathTemplate = "{artist}/{album}/{track2} - {title}.{ext}"

// Open loads (or creates) a library rooted at dir using the given path template.
func Open(dir, template string) (*Library, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create music dir: %w", err)
	}
	l := &Library{root: dir, template: template, index: map[string]string{}}
	l.loadIndex()
	return l, nil
}

// Exists reports whether content with the given hash was already stored. If hash
// is empty it is treated as unknown (not existing).
func (l *Library) Exists(hash string) (string, bool) {
	if hash == "" {
		return "", false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	rel, ok := l.index[hash]
	if ok {
		if _, err := os.Stat(filepath.Join(l.root, rel)); err == nil {
			return rel, true
		}
		delete(l.index, hash)
	}
	return "", false
}

// Store streams src into the library at a path derived from meta. It computes
// the content hash while streaming; if the hash was already stored, it discards
// the upload and returns Duplicate=true.
func (l *Library) Store(meta Metadata, src io.Reader) (Result, error) {
	ext := strings.ToLower(strings.TrimPrefix(meta.Ext, "."))
	if ext == "" {
		ext = "mp3"
	}
	rel := l.relPath(meta, ext)
	abs := filepath.Join(l.root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return Result{}, fmt.Errorf("mkdir: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(abs), ".upload-*")
	if err != nil {
		return Result{}, fmt.Errorf("temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	h := sha256.New()
	if _, err := io.Copy(tmp, io.TeeReader(src, h)); err != nil {
		tmp.Close()
		return Result{}, fmt.Errorf("write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Result{}, fmt.Errorf("close temp: %w", err)
	}
	hash := hex.EncodeToString(h.Sum(nil))

	if existing, ok := l.Exists(hash); ok {
		return Result{RelPath: existing, Hash: hash, Duplicate: true}, nil
	}

	abs = uniquePath(abs)
	rel, _ = filepath.Rel(l.root, abs)
	if err := os.Rename(tmpName, abs); err != nil {
		return Result{}, fmt.Errorf("move into place: %w", err)
	}

	l.mu.Lock()
	l.index[hash] = rel
	l.mu.Unlock()
	l.saveIndex()

	return Result{RelPath: rel, Hash: hash}, nil
}

func (l *Library) relPath(m Metadata, ext string) string {
	track2 := ""
	if m.TrackNumber > 0 {
		track2 = fmt.Sprintf("%02d", m.TrackNumber)
	}
	repl := strings.NewReplacer(
		"{artist}", sanitize(orUnknown(m.Artist, "Unknown Artist")),
		"{album}", sanitize(orUnknown(m.Album, "Unknown Album")),
		"{title}", sanitize(orUnknown(m.Title, "Untitled")),
		"{track}", itoaOrEmpty(m.TrackNumber),
		"{track2}", track2,
		"{disc}", itoaOrEmpty(m.DiscNumber),
		"{year}", itoaOrEmpty(m.Year),
		"{ext}", ext,
	)
	p := repl.Replace(l.template)
	p = strings.ReplaceAll(p, " - .", ".")
	segs := strings.Split(p, "/")
	for i, s := range segs {
		s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "- "))
		if s == "" {
			s = "_"
		}
		segs[i] = s
	}
	return filepath.Join(segs...)
}

func (l *Library) loadIndex() {
	data, err := os.ReadFile(filepath.Join(l.root, indexFile))
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &l.index)
}

func (l *Library) saveIndex() {
	l.mu.Lock()
	data, err := json.MarshalIndent(l.index, "", "  ")
	l.mu.Unlock()
	if err != nil {
		return
	}
	path := filepath.Join(l.root, indexFile)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}

func uniquePath(abs string) string {
	if _, err := os.Stat(abs); os.IsNotExist(err) {
		return abs
	}
	ext := filepath.Ext(abs)
	base := strings.TrimSuffix(abs, ext)
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
}

func sanitize(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			b.WriteByte('_')
		case 0:
			// drop
		default:
			if r < 0x20 {
				b.WriteByte('_')
			} else {
				b.WriteRune(r)
			}
		}
	}
	out := strings.TrimRight(b.String(), " .")
	if out == "" {
		out = "_"
	}
	if len(out) > 180 {
		out = out[:180]
	}
	return out
}

func orUnknown(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func itoaOrEmpty(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", n)
}
