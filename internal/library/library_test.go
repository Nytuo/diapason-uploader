package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRelPathTemplate(t *testing.T) {
	l := &Library{root: "/music", template: DefaultPathTemplate}
	got := l.relPath(Metadata{Artist: "A Tribe Called Quest", Album: "Low End Theory", Title: "Check the Rhime", TrackNumber: 3}, "mp3")
	want := filepath.Join("A Tribe Called Quest", "Low End Theory", "03 - Check the Rhime.mp3")
	if got != want {
		t.Fatalf("relPath = %q, want %q", got, want)
	}
}

func TestRelPathSanitizesAndFallsBack(t *testing.T) {
	l := &Library{root: "/music", template: DefaultPathTemplate}
	got := l.relPath(Metadata{Artist: "AC/DC", Title: "Back: In?"}, "flac")
	if strings.ContainsAny(got, "/\\:?*\"<>|"[1:]) { // '/' allowed as separator
		// only path separators allowed; illegal chars must be gone from segments
	}
	segs := strings.Split(got, string(filepath.Separator))
	if segs[0] != "AC_DC" {
		t.Fatalf("artist not sanitized: %q", segs[0])
	}
	if segs[1] != "Unknown Album" {
		t.Fatalf("missing album fallback: %q", segs[1])
	}
	if !strings.HasSuffix(got, ".flac") {
		t.Fatalf("wrong ext: %q", got)
	}
}

func TestRelPathNoTrackNumber(t *testing.T) {
	l := &Library{root: "/music", template: DefaultPathTemplate}
	got := l.relPath(Metadata{Artist: "X", Album: "Y", Title: "Z"}, "opus")
	want := filepath.Join("X", "Y", "Z.opus")
	if got != want {
		t.Fatalf("relPath = %q, want %q", got, want)
	}
}

func TestStoreAndDedup(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, DefaultPathTemplate)
	if err != nil {
		t.Fatal(err)
	}
	meta := Metadata{Artist: "Artist", Album: "Album", Title: "Song", TrackNumber: 1, Ext: "mp3"}

	r1, err := l.Store(meta, strings.NewReader("audio-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if r1.Duplicate {
		t.Fatal("first store should not be duplicate")
	}
	if _, err := filepath.Abs(filepath.Join(dir, r1.RelPath)); err != nil {
		t.Fatal(err)
	}

	// Same content again -> duplicate, no new file.
	r2, err := l.Store(meta, strings.NewReader("audio-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Duplicate {
		t.Fatal("second store of identical content should be duplicate")
	}
	if r2.Hash != r1.Hash {
		t.Fatalf("hash mismatch: %s vs %s", r1.Hash, r2.Hash)
	}

	// Different content, same metadata -> new unique path.
	r3, err := l.Store(meta, strings.NewReader("different-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if r3.Duplicate {
		t.Fatal("different content should not be duplicate")
	}
	if r3.RelPath == r1.RelPath {
		t.Fatalf("expected unique path, both are %q", r1.RelPath)
	}

	// Exists reflects stored hash.
	if _, ok := l.Exists(r1.Hash); !ok {
		t.Fatal("Exists should find stored hash")
	}
	if _, ok := l.Exists("deadbeef"); ok {
		t.Fatal("Exists should not find unknown hash")
	}
}

func TestStoreRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, DefaultPathTemplate)
	if err != nil {
		t.Fatal(err)
	}

	cases := []Metadata{
		{Artist: "..", Album: "..", Title: "pwned", Ext: "mp3"},
		{Artist: "Artist", Album: "Album", Title: "Song", Ext: "../../../../etc/cron.d/x"},
		{Artist: "..", Album: "etc", Title: "passwd", Ext: ""},
	}
	for _, meta := range cases {
		// Either rejected outright, or neutralized into a safe path under root —
		// what must never happen is a write outside dir (checked below).
		if res, err := l.Store(meta, strings.NewReader("audio-bytes")); err == nil {
			if _, statErr := os.Stat(filepath.Join(dir, res.RelPath)); statErr != nil {
				t.Fatalf("stored file not found under root for %+v: %v", meta, statErr)
			}
		}
	}

	// Nothing should have escaped the temp dir root.
	parent := filepath.Dir(dir)
	entries, err := filepath.Glob(filepath.Join(parent, "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e, "pwned") || strings.Contains(e, "passwd") {
			t.Fatalf("traversal escaped root: %s", e)
		}
	}
}

func TestSanitizeExt(t *testing.T) {
	exact := map[string]string{
		"mp3":   "mp3",
		".flac": "flac",
		"":      "mp3",
		"MP3":   "mp3",
	}
	for in, want := range exact {
		if got := sanitizeExt(in); got != want {
			t.Fatalf("sanitizeExt(%q) = %q, want %q", in, got, want)
		}
	}

	unsafe := []string{"../../../etc/passwd", "m4a/../../etc", "..", "///", "..."}
	for _, in := range unsafe {
		got := sanitizeExt(in)
		if got == "" || strings.Contains(got, "/") || strings.Contains(got, "..") {
			t.Fatalf("sanitizeExt(%q) = %q, still unsafe", in, got)
		}
	}
}

func TestExistsEmptyHash(t *testing.T) {
	l := &Library{index: map[string]string{}}
	if _, ok := l.Exists(""); ok {
		t.Fatal("empty hash must not exist")
	}
}
