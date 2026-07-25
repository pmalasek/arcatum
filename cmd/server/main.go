// Command server is the arcatum-server: scheduler, API, storage and (later) web UI.
//
// Phase B: it serves the check-in / results API over plain HTTP. mTLS and dispatch
// signing come later.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"arcatum/internal/server"
	"arcatum/pkg/config"
)

func main() {
	configPath := flag.String("config", "config/server.toml", "path to server config")
	instancesPath := flag.String("instances", "data/instances.json", "path to instances file")
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Fatalf("loading config: %v", err)
	}
	loc, err := cfg.Location()
	if err != nil {
		logger.Fatalf("config: %v", err)
	}

	srv, err := server.New(cfg.Server.Scripts, *instancesPath, cfg.Storage.BackupDir, loc, logger)
	if err != nil {
		logger.Fatalf("init server: %v", err)
	}

	logger.Printf("arcatum-server listening on %s", cfg.Server.Listen)
	logger.Printf("  scripts=%s  backup_dir=%s  instances=%s", cfg.Server.Scripts, cfg.Storage.BackupDir, *instancesPath)
	if err := http.ListenAndServe(cfg.Server.Listen, srv.Handler()); err != nil {
		logger.Fatalf("http: %v", err)
	}
}
