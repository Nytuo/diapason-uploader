package devices

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPairVerifyRevokePersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tok, v, err := s.Add("Pixel")
	if err != nil || tok == "" || v.Name != "Pixel" {
		t.Fatalf("add: %v %v %v", tok, v, err)
	}
	if _, ok := s.Verify(tok); !ok {
		t.Fatal("fresh token should verify")
	}
	if _, ok := s.Verify(tok + "x"); ok {
		t.Fatal("wrong token verified")
	}

	// The token itself must never hit the disk.
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), tok) {
		t.Fatal("plaintext token persisted")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode %v", info.Mode().Perm())
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Verify(tok); !ok {
		t.Fatal("token should survive restart")
	}
	if !s2.Revoke(v.ID) {
		t.Fatal("revoke failed")
	}
	if _, ok := s2.Verify(tok); ok {
		t.Fatal("revoked token still verifies")
	}
}
