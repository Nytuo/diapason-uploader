// Package discovery advertises this uploader instance over mDNS so Diapason
// clients can find it automatically.
package discovery

import (
	"sync"

	"github.com/libp2p/zeroconf/v2"
)

// ServiceType is the mDNS service type Diapason clients browse for.
const ServiceType = "_diapason-uploader._tcp"

const serviceDomain = "local."

type Advertiser struct {
	name string
	port int
	tls  bool
	auth bool
	pair bool

	mu     sync.Mutex
	server *zeroconf.Server
}

func NewAdvertiser(name string, port int, tlsEnabled, authRequired, pairing bool) *Advertiser {
	return &Advertiser{name: name, port: port, tls: tlsEnabled, auth: authRequired, pair: pairing}
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
		"auth=" + boolFlag(a.auth),
	}
	txt = append(txt, "pair="+boolFlag(a.pair))

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
