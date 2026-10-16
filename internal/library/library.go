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
	Ext         string `json:"ext"`
}

type Result struct {
	RelPath   string `json:"path"`
	Hash      string `json:"hash"`
	Duplicate bool   `json:"duplicate"`
}

type Library struct {
	root     string
	template string

	placeMu sync.Mutex

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
	_ = os.RemoveAll(filepath.Join(dir, IncomingDir))
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

// IncomingDir is the uploader-owned scratch directory (inside the library root,
// so a final rename stays on one filesystem) where uploads are staged.
const IncomingDir = ".diapason-uploader-incoming"

// Store streams src into the library at a path derived from meta.
func (l *Library) Store(meta Metadata, src io.Reader) (Result, error) {
	ext := sanitizeExt(meta.Ext)
	rel := l.relPath(meta, ext)
	abs := filepath.Join(l.root, rel)
	if err := l.assertWithinRoot(abs); err != nil {
		return Result{}, err
	}

	incoming := filepath.Join(l.root, IncomingDir)
	if err := os.MkdirAll(incoming, 0o755); err != nil {
		return Result{}, fmt.Errorf("create staging dir: %w", err)
	}
	tmp, err := os.CreateTemp(incoming, "upload-*")
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

	l.placeMu.Lock()
	defer l.placeMu.Unlock()

	if existing, ok := l.Exists(hash); ok {
		return Result{RelPath: existing, Hash: hash, Duplicate: true}, nil
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return Result{}, fmt.Errorf("mkdir: %w", err)
	}
	abs = uniquePath(abs)
	rel, _ = filepath.Rel(l.root, abs)
	if err := moveNew(tmpName, abs); err != nil {
		return Result{}, fmt.Errorf("move into place: %w", err)
	}

	l.mu.Lock()
	l.index[hash] = rel
	l.mu.Unlock()
	l.saveIndex()

	return Result{RelPath: rel, Hash: hash}, nil
}

func moveNew(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return os.Remove(src)
	} else if os.IsExist(err) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
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
		if s == "" || s == "." || s == ".." {
			s = "_"
		}
		segs[i] = s
	}
	return filepath.Join(segs...)
}

// assertWithinRoot rejects any path that resolves outside the library root,
// guarding against traversal via crafted metadata (e.g. "..").
func (l *Library) assertWithinRoot(abs string) error {
	root, err := filepath.Abs(l.root)
	if err != nil {
		return fmt.Errorf("resolve root: %w", err)
	}
	target, err := filepath.Abs(abs)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
		return fmt.Errorf("refusing to write outside library root: %q", abs)
	}
	return nil
}

// sanitizeExt strips any path-unsafe content from the client-supplied file
// extension, which is otherwise placed directly into the destination path.
func sanitizeExt(ext string) string {
	ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	if ext == "" {
		return "mp3"
	}
	ext = strings.ReplaceAll(sanitize(ext), ".", "")
	if ext == "" || ext == "_" {
		return "mp3"
	}
	return ext
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

var audioExts = map[string]bool{
	"mp3": true, "m4a": true, "aac": true, "flac": true, "ogg": true, "oga": true,
	"opus": true, "wav": true, "wma": true, "aiff": true, "aif": true, "alac": true,
	"webm": true, "mka": true, "mp4": true, "wv": true, "ape": true,
}

func AllowedExt(ext string) bool {
	ext = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
	return ext == "" || audioExts[ext]
}

// ValidHash reports whether h looks like a hex SHA-256 digest.
func ValidHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for _, c := range h {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
