// Package rescan triggers a library rescan on the configured music server after
// uploads settle, so newly added files appear without waiting for a periodic scan.
package rescan

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/nytuo/diapason-uploader/internal/config"
)

type Trigger struct {
	cfg    *config.Config
	client *http.Client

	mu    sync.Mutex
	timer *time.Timer
}

func New(cfg *config.Config) *Trigger {
	if cfg.NavidromeURL == "" && cfg.PlexURL == "" {
		return nil
	}
	return &Trigger{cfg: cfg, client: &http.Client{Timeout: 20 * time.Second}}
}

// Schedule requests a rescan after the debounce window. Repeated calls within
// the window collapse into a single rescan.
func (t *Trigger) Schedule() {
	if t == nil {
		return
	}
	d := time.Duration(t.cfg.RescanDebounceSecs) * time.Second
	if d <= 0 {
		d = time.Second
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer != nil {
		t.timer.Stop()
	}
	t.timer = time.AfterFunc(d, t.run)
}

func (t *Trigger) run() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var err error
	switch {
	case t.cfg.NavidromeURL != "":
		err = t.navidrome(ctx)
	case t.cfg.PlexURL != "":
		err = t.plex(ctx)
	}
	if err != nil {
		log.Printf("rescan: %v", err)
	} else {
		log.Printf("rescan: triggered")
	}
}

// navidrome calls the Subsonic startScan endpoint using salted-token auth.
func (t *Trigger) navidrome(ctx context.Context) error {
	salt := randomHex(8)
	token := md5hex(t.cfg.NavidromePassword + salt)
	q := url.Values{}
	q.Set("u", t.cfg.NavidromeUser)
	q.Set("t", token)
	q.Set("s", salt)
	q.Set("v", "1.16.1")
	q.Set("c", "diapason-uploader")
	q.Set("f", "json")
	u := t.cfg.NavidromeURL + "/rest/startScan?" + q.Encode()
	return t.get(ctx, u)
}

// plex refreshes the configured library section.
func (t *Trigger) plex(ctx context.Context) error {
	q := url.Values{}
	q.Set("X-Plex-Token", t.cfg.PlexToken)
	u := fmt.Sprintf("%s/library/sections/%s/refresh?%s", t.cfg.PlexURL, t.cfg.PlexSection, q.Encode())
	return t.get(ctx, u)
}

func (t *Trigger) get(ctx context.Context, u string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("rescan status %d", resp.StatusCode)
	}
	return nil
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
