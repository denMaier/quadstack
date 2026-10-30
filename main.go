// Quadstack is a small web UI for rootless Podman Quadlets: stacks of unit files you can
// edit, validate, start and stop, with logs, image pulls and container shells.
package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"time"
)

//go:embed all:web/dist
var webDist embed.FS

// version is set at build time with -ldflags "-X main.version=…"
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
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
		grants:  newGrants(cfg),
		audit:   newAudit(cfg),
	}
	if cfg.QuadletBin == "" {
		log.Print("quadlet generator not found: files are saved without validation")
	}
	log.Printf("quadstack %s listening on %s, managing %s", version, cfg.Listen, cfg.QuadletDir)
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
