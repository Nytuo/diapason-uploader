package server

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nytuo/diapason-uploader/internal/config"
	"github.com/nytuo/diapason-uploader/internal/devices"
	"github.com/nytuo/diapason-uploader/internal/library"
)

const testToken = "0123456789abcdef0123456789abcdef"

func newTestServer(t *testing.T, trusted ...string) (*Server, http.Handler) {
	t.Helper()
	lib, err := library.Open(t.TempDir(), library.DefaultPathTemplate)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthToken: testToken, MaxUploadBytes: 1 << 20}
	for _, c := range trusted {
		_, n, _ := net.ParseCIDR(c)
		cfg.TrustedProxies = append(cfg.TrustedProxies, n)
	}
	cfg.Pairing, cfg.PairingWindow = "window", 15*time.Minute
	devs, _ := devices.Open(filepath.Join(t.TempDir(), devices.FileName))
	s := New(context.Background(), cfg, lib, nil, nil, devs)
	return s, s.Handler()
}

func do(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAuthRequired(t *testing.T) {
	_, h := newTestServer(t)
	hash := strings.Repeat("a", 64)

	cases := map[string]int{
		"":                          http.StatusUnauthorized,
		"Bearer ":                   http.StatusUnauthorized,
		"Bearer wrong":              http.StatusUnauthorized,
		"Basic " + testToken:        http.StatusUnauthorized,
		testToken:                   http.StatusUnauthorized, // missing scheme
		"Bearer " + testToken:       http.StatusOK,
		"bearer " + testToken:       http.StatusOK,
		"Bearer " + testToken + "x": http.StatusUnauthorized,
	}
	for header, want := range cases {
		req := httptest.NewRequest("GET", "/api/v1/exists?hash="+hash, nil)
		req.RemoteAddr = "10.0.0.5:1234"
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		if got := do(h, req).Code; got != want {
			t.Errorf("Authorization %q: got %d want %d", header, got, want)
		}
		// reset limiter between attempts
		h2 := h
		_ = h2
	}
}

func TestBruteForceIsThrottled(t *testing.T) {
	_, h := newTestServer(t)
	var last int
	for i := 0; i < 12; i++ {
		req := httptest.NewRequest("GET", "/api/v1/capabilities", nil)
		req.RemoteAddr = "203.0.113.9:5555"
		req.Header.Set("Authorization", "Bearer nope")
		last = do(h, req).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after repeated failures, got %d", last)
	}
	// Even the right token is refused while blocked.
	req := httptest.NewRequest("GET", "/api/v1/capabilities", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	req.Header.Set("Authorization", "Bearer "+testToken)
	if got := do(h, req).Code; got != http.StatusTooManyRequests {
		t.Fatalf("blocked client got %d", got)
	}
}

func TestForwardedForOnlyTrustedFromProxy(t *testing.T) {
	// Untrusted peer forging X-Forwarded-For must not dodge the limiter.
	_, h := newTestServer(t)
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest("GET", "/api/v1/capabilities", nil)
		req.RemoteAddr = "198.51.100.7:1"
		req.Header.Set("X-Forwarded-For", "1.2.3."+string(rune('0'+i)))
		req.Header.Set("Authorization", "Bearer nope")
		do(h, req)
	}
	req := httptest.NewRequest("GET", "/api/v1/capabilities", nil)
	req.RemoteAddr = "198.51.100.7:1"
	req.Header.Set("X-Forwarded-For", "9.9.9.9")
	req.Header.Set("Authorization", "Bearer nope")
	if got := do(h, req).Code; got != http.StatusTooManyRequests {
		t.Fatalf("forged XFF bypassed limiter: %d", got)
	}

	// Behind a trusted proxy, two clients are throttled independently.
	_, h = newTestServer(t, "10.0.0.0/8")
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest("GET", "/api/v1/capabilities", nil)
		req.RemoteAddr = "10.0.0.2:1"
		req.Header.Set("X-Forwarded-For", "203.0.113.1")
		req.Header.Set("Authorization", "Bearer nope")
		do(h, req)
	}
	req = httptest.NewRequest("GET", "/api/v1/capabilities", nil)
	req.RemoteAddr = "10.0.0.2:1"
	req.Header.Set("X-Forwarded-For", "203.0.113.2")
	req.Header.Set("Authorization", "Bearer "+testToken)
	if got := do(h, req).Code; got != http.StatusOK {
		t.Fatalf("innocent client behind proxy got %d", got)
	}
}

func TestSecurityHeaders(t *testing.T) {
	_, h := newTestServer(t)
	rec := do(h, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("missing security headers: %v", rec.Header())
	}
}

func upload(t *testing.T, h http.Handler, ext string) int {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	w, _ := mw.CreateFormField("metadata")
	_, _ = w.Write([]byte(`{"artist":"A","album":"B","title":"C","ext":"` + ext + `"}`))
	f, _ := mw.CreateFormFile("file", "x")
	_, _ = f.Write([]byte("audio-bytes-" + ext))
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/api/v1/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.RemoteAddr = "10.0.0.1:1"
	return do(h, req).Code
}

func TestUploadExtensionAllowlist(t *testing.T) {
	_, h := newTestServer(t)
	if got := upload(t, h, "mp3"); got != http.StatusCreated {
		t.Fatalf("mp3 upload: %d", got)
	}
	for _, ext := range []string{"html", "php", "sh", "exe"} {
		if got := upload(t, h, ext); got != http.StatusUnsupportedMediaType {
			t.Errorf("%s upload: got %d, want 415", ext, got)
		}
	}
}

func TestExistsRejectsBadHash(t *testing.T) {
	_, h := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/v1/exists?hash=../../etc", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.RemoteAddr = "10.0.0.1:1"
	if got := do(h, req).Code; got != http.StatusBadRequest {
		t.Fatalf("got %d", got)
	}
}

func TestYtDlpDisabledSaysSoClearly(t *testing.T) {
	_, h := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/v1/ytdlp/search?q=x", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.RemoteAddr = "10.0.0.1:1"
	rec := do(h, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("got %d", rec.Code)
	}
	var body struct{ Code, Component string }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Code != "ytdlp_disabled" || body.Component != "ytdlp" {
		t.Fatalf("unexpected error body %s", rec.Body)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("missing request id")
	}
}

func pairReq(remote string, hdr map[string]string) *http.Request {
	req := httptest.NewRequest("POST", "/api/v1/pair", strings.NewReader(`{"name":"Pixel 9"}`))
	req.RemoteAddr = remote
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return req
}

func TestPairingIssuesPerDeviceToken(t *testing.T) {
	_, h := newTestServer(t)
	rec := do(h, pairReq("192.168.1.20:5000", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("pair: %d %s", rec.Code, rec.Body)
	}
	var out struct{ Token, DeviceId string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Token == "" || out.Token == testToken {
		t.Fatalf("bad device token %q", out.Token)
	}

	// The device token works, and is not the master token.
	req := httptest.NewRequest("GET", "/api/v1/capabilities", nil)
	req.RemoteAddr = "192.168.1.20:5000"
	req.Header.Set("Authorization", "Bearer "+out.Token)
	if got := do(h, req).Code; got != http.StatusOK {
		t.Fatalf("device token rejected: %d", got)
	}

	// Revoking it cuts access.
	del := httptest.NewRequest("DELETE", "/api/v1/devices/"+out.DeviceId, nil)
	del.RemoteAddr = "192.168.1.20:5000"
	del.Header.Set("Authorization", "Bearer "+testToken)
	if got := do(h, del).Code; got != http.StatusNoContent {
		t.Fatalf("revoke: %d", got)
	}
	if got := do(h, req).Code; got != http.StatusUnauthorized {
		t.Fatalf("revoked token still works: %d", got)
	}
}

func TestPairingRefusesOutsidersAndProxies(t *testing.T) {
	_, h := newTestServer(t)
	cases := map[string]*http.Request{
		"public internet peer":      pairReq("203.0.113.5:1", nil),
		"relayed by reverse proxy":  pairReq("10.0.0.2:1", map[string]string{"X-Forwarded-For": "203.0.113.5"}),
		"forged real-ip from proxy": pairReq("172.18.0.3:1", map[string]string{"X-Real-IP": "192.168.1.5"}),
	}
	for name, req := range cases {
		if got := do(h, req).Code; got != http.StatusForbidden {
			t.Errorf("%s: got %d, want 403", name, got)
		}
	}
	// Tailscale-style CGNAT peers are treated as local.
	if got := do(h, pairReq("100.101.102.103:1", nil)).Code; got != http.StatusCreated {
		t.Errorf("tailnet peer: got %d", got)
	}
}

func TestPairingWindowClosedUntilReopened(t *testing.T) {
	s, h := newTestServer(t)
	s.pairMu.Lock()
	s.pairUntil = time.Now().Add(-time.Minute)
	s.pairMu.Unlock()

	if got := do(h, pairReq("192.168.1.20:5000", nil)).Code; got != http.StatusForbidden {
		t.Fatalf("closed window allowed pairing: %d", got)
	}
	open := httptest.NewRequest("POST", "/api/v1/pair/open", nil)
	open.RemoteAddr = "192.168.1.9:1"
	open.Header.Set("Authorization", "Bearer "+testToken)
	if got := do(h, open).Code; got != http.StatusOK {
		t.Fatalf("open: %d", got)
	}
	if got := do(h, pairReq("192.168.1.20:5000", nil)).Code; got != http.StatusCreated {
		t.Fatalf("reopened window refused pairing: %d", got)
	}
}
