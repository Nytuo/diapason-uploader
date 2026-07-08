// Command diapason-uploader is a small sidecar service that receives tracks from
// Diapason clients and places them into a music library scanned by Navidrome or
// Plex, then triggers a rescan. It solves the fact that neither server exposes a
// music-ingest API — files must land on the server's disk.
package main

import (
	"log"
	"net/http"
	"time"

	"github.com/nytuo/diapason-uploader/internal/config"
	"github.com/nytuo/diapason-uploader/internal/library"
	"github.com/nytuo/diapason-uploader/internal/rescan"
	"github.com/nytuo/diapason-uploader/internal/server"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("diapason-uploader ")

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	lib, err := library.Open(cfg.MusicDir, cfg.PathTemplate)
	if err != nil {
		log.Fatalf("library: %v", err)
	}

	srv := server.New(cfg, lib, rescan.New(cfg))

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	scheme := "http"
	if cfg.TLSEnabled() {
		scheme = "https"
	}
	log.Printf("listening on %s://%s (music dir: %s)", scheme, cfg.ListenAddr, cfg.MusicDir)

	if cfg.TLSEnabled() {
		err = httpSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	} else {
		err = httpSrv.ListenAndServe()
	}
	if err != nil {
		log.Fatalf("server: %v", err)
	}
}
