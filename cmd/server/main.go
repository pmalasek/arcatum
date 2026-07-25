// Command server is the arcatum-server: scheduler, API, storage and (later) web UI.
//
// With [tls] and [signing] configured it serves mTLS and signs every dispatched job.
// Without them it falls back to plain HTTP for local development — unauthenticated,
// so it must not be used on a real network.
package main

import (
	"crypto/tls"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"arcatum/internal/server"
	"arcatum/pkg/config"
	"arcatum/pkg/crypto"
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

	opts := server.Options{RequireClientCert: cfg.TLS.Enabled()}
	var tlsConfig *tls.Config
	if cfg.TLS.Enabled() {
		if tlsConfig, err = crypto.ServerTLSConfig(cfg.TLS.Cert, cfg.TLS.Key, cfg.TLS.CACert); err != nil {
			logger.Fatalf("tls: %v", err)
		}
		signer, err := crypto.LoadSigner(cfg.Signing.Key)
		if err != nil {
			logger.Fatalf("signing key: %v", err)
		}
		opts.Signer = signer
	}

	srv, err := server.New(store, cfg.Server.Scripts, loc, logger, opts)
	if err != nil {
		logger.Fatalf("init server: %v", err)
	}

	httpSrv := &http.Server{
		Addr:      cfg.Server.Listen,
		Handler:   srv.Handler(),
		TLSConfig: tlsConfig,
	}

	logger.Printf("arcatum-server listening on %s", cfg.Server.Listen)
	logger.Printf("  scripts=%s  db=%s  backup_dir=%s", cfg.Server.Scripts, dbPath, cfg.Storage.BackupDir)
	if tlsConfig != nil {
		logger.Printf("  mTLS enabled (CA %s); job dispatches are signed", cfg.TLS.CACert)
		err = httpSrv.ListenAndServeTLS("", "") // certificates come from TLSConfig
	} else {
		logger.Printf("  WARNING: no [tls] configured — plain HTTP, callers are not authenticated.")
		logger.Printf("           Development only. See README: Zabezpečení (mTLS a podpis úloh).")
		err = httpSrv.ListenAndServe()
	}
	if err != nil {
		logger.Fatalf("http: %v", err)
	}
}
