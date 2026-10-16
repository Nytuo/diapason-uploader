// Command diapason-uploader is a small sidecar service that receives tracks from
// Diapason clients and places them into a music library scanned by Navidrome or
// Plex, then triggers a rescan. It solves the fact that neither server exposes a
// music-ingest API — files must land on the server's disk.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/http"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/nytuo/diapason-uploader/internal/config"
	"github.com/nytuo/diapason-uploader/internal/devices"
	"github.com/nytuo/diapason-uploader/internal/discovery"
	"github.com/nytuo/diapason-uploader/internal/library"
	"github.com/nytuo/diapason-uploader/internal/rescan"
	"github.com/nytuo/diapason-uploader/internal/server"
	"github.com/nytuo/diapason-uploader/internal/token"
	"github.com/nytuo/diapason-uploader/internal/ytdlp"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("diapason-uploader ")

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	if cfg.AuthToken == "" {
		tok, generated, err := token.LoadOrCreate(filepath.Join(cfg.StateDir, config.TokenFileName))
		if err != nil {
			log.Fatalf("token: %v", err)
		}
		cfg.AuthToken = tok
		if generated {
			log.Printf("no DIAPASON_AUTH_TOKEN set — generated and saved one. Copy it from the mounted "+
				"music dir into the Diapason app: %s", filepath.Join(cfg.StateDir, config.TokenFileName))
		} else {
			log.Printf("no DIAPASON_AUTH_TOKEN set — reusing previously generated token")
		}
	}

	lib, err := library.Open(cfg.MusicDir, cfg.PathTemplate)
	if err != nil {
		log.Fatalf("library: %v", err)
	}

	if cfg.MDNSEnabled {
		adv := discovery.NewAdvertiser(cfg.MDNSName, listenPort(cfg.ListenAddr), cfg.TLSEnabled(), cfg.AuthToken != "", cfg.Pairing != "off")
		if err := adv.Start(); err != nil {
			log.Printf("discovery: mDNS advertising disabled: %v", err)
		} else {
			defer adv.Stop()
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var yt *ytdlp.Manager
	if cfg.YtDlp.Enabled {
		yt, err = ytdlp.New(cfg.YtDlp, cfg.WorkDir)
		if err != nil {
			log.Fatalf("yt-dlp proxy: %v", err)
		}
		log.Printf("yt-dlp proxy enabled (yt-dlp %s, tagging=%v, auto-update=%v)", yt.Version(), yt.HasFFmpeg(), cfg.YtDlp.AutoUpdate)
		go yt.RunUpdater(ctx)
	}

	devs, err := devices.Open(filepath.Join(cfg.StateDir, devices.FileName))
	if err != nil {
		log.Fatalf("devices: %v", err)
	}
	srv := server.New(ctx, cfg, lib, rescan.New(cfg), yt, devs)
	if cfg.Pairing != "off" && (cfg.Pairing == "open" || devs.Count() == 0) {
		log.Printf("pairing is open (%s): Diapason apps on your local network can pair with one tap", cfg.Pairing)
	}

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	if cfg.TLSEnabled() {
		httpSrv.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			CipherSuites: []uint16{
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
				tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			},
		}
	} else if !isLoopback(cfg.ListenAddr) && !cfg.PlainHTTPOK {
		log.Printf("WARNING: serving plain HTTP on %s. That is fine on a trusted LAN, but the token and "+
			"your files travel unencrypted — for internet access set DIAPASON_TLS_CERT/KEY or put a "+
			"TLS reverse proxy in front. Over a VPN/tunnel you trust (Tailscale, WireGuard, Cloudflare "+
			"Tunnel…) plain HTTP is fine: set DIAPASON_PLAIN_HTTP_OK=true to silence this.", cfg.ListenAddr)
	}

	scheme := "http"
	if cfg.TLSEnabled() {
		scheme = "https"
	}
	log.Printf("listening on %s://%s (music dir: %s)", scheme, cfg.ListenAddr, cfg.MusicDir)

	serveErr := make(chan error, 1)
	go func() {
		if cfg.TLSEnabled() {
			serveErr <- httpSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			serveErr <- httpSrv.ListenAndServe()
		}
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	case <-ctx.Done():
		log.Printf("shutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}
}

func listenPort(addr string) int {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0
	}
	return port
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}
