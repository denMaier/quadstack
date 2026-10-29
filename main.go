// Quadstack is a small web UI for rootless Podman Quadlets: stacks of unit files you can
// edit, validate, start and stop, with logs, image pulls and container shells.
package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"time"
)

//go:embed all:web/dist
var webDist embed.FS

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatal(err)
	}
	static, _ := fs.Sub(webDist, "web/dist")
	s := &Server{
		cfg:     cfg,
		auth:    &Authenticator{cfg: cfg},
		files:   &Files{cfg: cfg},
		systemd: &Systemd{},
		static:  static,
	}
	if cfg.QuadletBin == "" {
		log.Print("quadlet generator not found: files are saved without validation")
	}
	log.Printf("quadstack listening on %s, managing %s", cfg.Listen, cfg.QuadletDir)
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
