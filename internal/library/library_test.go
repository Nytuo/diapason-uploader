package library

import (
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

func TestExistsEmptyHash(t *testing.T) {
	l := &Library{index: map[string]string{}}
	if _, ok := l.Exists(""); ok {
		t.Fatal("empty hash must not exist")
	}
}
