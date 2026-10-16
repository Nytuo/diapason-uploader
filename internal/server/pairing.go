package server

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// Pairing handshake
//
// Goal: one-tap setup from the app without ever broadcasting a secret. The app
// finds the uploader over mDNS, then POSTs /api/v1/pair. If — and only if —
//   - pairing is currently open (a short window after first start, reopened by
//     an already-paired device, or permanently with DIAPASON_PAIRING=open), and
//   - the request comes straight from a private/loopback/VPN address (not via a
//     reverse proxy, not from the internet),
func (s *Server) pairingOpen() bool {
	switch s.cfg.Pairing {
	case "open":
		return true
	case "window":
		s.pairMu.Lock()
		defer s.pairMu.Unlock()
		return time.Now().Before(s.pairUntil)
	}
	return false
}

func (s *Server) openPairingWindow() time.Time {
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	s.pairUntil = time.Now().Add(s.cfg.PairingWindow)
	return s.pairUntil
}

// isPrivateIP is true for loopback, RFC 1918, link-local, unique-local IPv6 and
// the 100.64.0.0/10 range used by Tailscale and other overlay networks.
func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		return v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
	}
	return false
}

// directLocalPeer is true if the TCP peer itself is on a private network and
// the request wasn't relayed by a proxy (which would hide the real client).
func (s *Server) directLocalPeer(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" || r.Header.Get("X-Real-IP") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return isPrivateIP(net.ParseIP(host))
}

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r, s.cfg.TrustedProxies)
	if s.limiter.blocked(ip) {
		w.Header().Set("Retry-After", "60")
		writeErr(w, http.StatusTooManyRequests, "too many attempts, slow down")
		return
	}
	deny := func(code int, msg string) {
		s.limiter.fail(ip)
		writeErr(w, code, msg)
	}

	if s.cfg.Pairing == "off" {
		deny(http.StatusForbidden, "pairing is disabled on this server")
		return
	}
	if !s.pairingOpen() {
		deny(http.StatusForbidden, "pairing is closed — open it from an already-paired device or restart the uploader")
		return
	}
	if !s.cfg.PairingPublic && !s.directLocalPeer(r) {
		deny(http.StatusForbidden, "pairing is only available from the local network")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 2<<10)
	var req struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	name := strings.TrimSpace(req.Name)
	if len(name) > 64 {
		name = name[:64]
	}

	token, dev, err := s.devices.Add(name)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	log.Printf("paired device %q (id %s)", dev.Name, dev.ID)
	writeJSON(w, http.StatusCreated, map[string]string{"token": token, "deviceId": dev.ID, "name": dev.Name})
}

// handlePairOpen lets an authenticated device open the window for another one.
func (s *Server) handlePairOpen(w http.ResponseWriter, _ *http.Request) {
	if s.cfg.Pairing == "off" {
		writeErr(w, http.StatusForbidden, "pairing is disabled on this server")
		return
	}
	until := s.openPairingWindow()
	log.Printf("pairing window opened until %s", until.Format(time.TimeOnly))
	writeJSON(w, http.StatusOK, map[string]any{"open": true, "until": until})
}

func (s *Server) handleDevices(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"devices": s.devices.List()})
}

func (s *Server) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	if !s.devices.Revoke(r.PathValue("id")) {
		writeErr(w, http.StatusNotFound, "unknown device")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
