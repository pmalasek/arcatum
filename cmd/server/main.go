// Command server is the arcatum-server: scheduler, API, storage and (later) web UI.
//
// Phase C: state is persisted in SQLite under the configured data_dir. The API is
// still plain HTTP; mTLS and dispatch signing come later.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"arcatum/internal/server"
	"arcatum/pkg/config"
)

func main() {
	configPath := flag.String("config", "config/server.toml", "path to server config")
	instancesPath := flag.String("instances", "data/instances.json", "instances JSON to import on start")
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

	dbPath := filepath.Join(cfg.Server.DataDir, "arcatum.db")
	store, err := server.Open(dbPath, cfg.Storage.BackupDir)
	if err != nil {
		logger.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// Instances are seeded from JSON for now; the web UI will manage them in the DB.
	n, err := store.ImportInstances(*instancesPath)
	if err != nil {
		logger.Fatalf("import instances: %v", err)
	}
	if n > 0 {
		logger.Printf("imported %d instance(s) from %s", n, *instancesPath)
	}

	srv, err := server.New(store, cfg.Server.Scripts, loc, logger)
	if err != nil {
		logger.Fatalf("init server: %v", err)
	}

	logger.Printf("arcatum-server listening on %s", cfg.Server.Listen)
	logger.Printf("  scripts=%s  db=%s  backup_dir=%s", cfg.Server.Scripts, dbPath, cfg.Storage.BackupDir)
	if err := http.ListenAndServe(cfg.Server.Listen, srv.Handler()); err != nil {
		logger.Fatalf("http: %v", err)
	}
}
