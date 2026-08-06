// Package discovery advertises this uploader instance over mDNS so Diapason
// clients can find it automatically.
//
// mDNS is LAN-only by construction (multicast doesn't route off the local
// segment), so the auth token is broadcast in the TXT record whenever one is
// configured — anything able to receive the advertisement is, by definition,
// already on the same local network as the uploader itself.
package discovery

import (
	"sync"

	"github.com/libp2p/zeroconf/v2"
)

// ServiceType is the mDNS service type Diapason clients browse for.
const ServiceType = "_diapason-uploader._tcp"

const serviceDomain = "local."

type Advertiser struct {
	name  string
	port  int
	tls   bool
	token string

	mu     sync.Mutex
	server *zeroconf.Server
}

// NewAdvertiser prepares (but does not start) mDNS advertising for this
// instance. name is used as the mDNS instance name (e.g. hostname). token,
// if non-empty, is broadcast in the TXT record so clients on the LAN can
// pair automatically.
func NewAdvertiser(name string, port int, tlsEnabled bool, token string) *Advertiser {
	return &Advertiser{name: name, port: port, tls: tlsEnabled, token: token}
}

// Start registers the mDNS service. Safe to call once at startup.
func (a *Advertiser) Start() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.registerLocked()
}

// Stop unregisters the mDNS service.
func (a *Advertiser) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server != nil {
		a.server.Shutdown()
		a.server = nil
	}
}

func (a *Advertiser) registerLocked() error {
	if a.server != nil {
		a.server.Shutdown()
		a.server = nil
	}
	txt := []string{
		"v=1",
		"tls=" + boolFlag(a.tls),
		"auth=" + boolFlag(a.token != ""),
	}
	if a.token != "" {
		txt = append(txt, "pair=1", "token="+a.token)
	} else {
		txt = append(txt, "pair=0")
	}

	server, err := zeroconf.Register(a.name, ServiceType, serviceDomain, a.port, txt, nil)
	if err != nil {
		return err
	}
	a.server = server
	return nil
}

func boolFlag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
