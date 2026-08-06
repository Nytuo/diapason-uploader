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
	"github.com/nytuo/diapason-uploader/internal/discovery"
	"github.com/nytuo/diapason-uploader/internal/library"
	"github.com/nytuo/diapason-uploader/internal/rescan"
	"github.com/nytuo/diapason-uploader/internal/server"
	"github.com/nytuo/diapason-uploader/internal/token"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("diapason-uploader ")

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	if cfg.AuthToken == "" {
		tok, generated, err := token.LoadOrCreate(filepath.Join(cfg.MusicDir, config.TokenFileName))
		if err != nil {
			log.Fatalf("token: %v", err)
		}
		cfg.AuthToken = tok
		if generated {
			log.Printf("no DIAPASON_AUTH_TOKEN set — generated and saved one. The Diapason app will pick "+
				"it up automatically via mDNS on the local network, or read it from the mounted "+
				"music dir: %s", filepath.Join(cfg.MusicDir, config.TokenFileName))
		} else {
			log.Printf("no DIAPASON_AUTH_TOKEN set — reusing previously generated token")
		}
	}

	lib, err := library.Open(cfg.MusicDir, cfg.PathTemplate)
	if err != nil {
		log.Fatalf("library: %v", err)
	}

	if cfg.MDNSEnabled {
		adv := discovery.NewAdvertiser(cfg.MDNSName, listenPort(cfg.ListenAddr), cfg.TLSEnabled(), cfg.AuthToken)
		if err := adv.Start(); err != nil {
			log.Printf("discovery: mDNS advertising disabled: %v", err)
		} else {
			defer adv.Stop()
		}
	}

	srv := server.New(cfg, lib, rescan.New(cfg))

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if cfg.TLSEnabled() {
		httpSrv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	scheme := "http"
	if cfg.TLSEnabled() {
		scheme = "https"
	}
	log.Printf("listening on %s://%s (music dir: %s)", scheme, cfg.ListenAddr, cfg.MusicDir)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
