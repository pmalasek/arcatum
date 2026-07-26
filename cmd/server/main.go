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

	// Secrets are encrypted at rest whenever a master key is configured.
	var box crypto.SecretBox
	if cfg.Secrets.MasterKey != "" {
		if box, err = crypto.LoadSecretBox(cfg.Secrets.MasterKey); err != nil {
			logger.Fatalf("secrets master key: %v", err)
		}
	}

	dbPath := filepath.Join(cfg.Server.DataDir, "arcatum.db")
	store, err := server.Open(dbPath, cfg.Storage.BackupDir, box)
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
	var signingPubPEM []byte
	if cfg.TLS.Enabled() {
		if tlsConfig, err = crypto.ServerTLSConfig(cfg.TLS.Cert, cfg.TLS.Key, cfg.TLS.CACert); err != nil {
			logger.Fatalf("tls: %v", err)
		}
		signer, err := crypto.LoadSigner(cfg.Signing.Key)
		if err != nil {
			logger.Fatalf("signing key: %v", err)
		}
		opts.Signer = signer
		// Derived from the loaded key, so what runners verify with always matches what
		// the server signs with.
		if signingPubPEM, err = signer.Public(); err != nil {
			logger.Fatalf("signing public key: %v", err)
		}
	}
	// The CA is only needed to sign enrollment requests from new runners.
	if cfg.Bootstrap.Enabled() {
		ca, err := crypto.LoadCA(cfg.TLS.CACert, cfg.Bootstrap.CAKey)
		if err != nil {
			logger.Fatalf("bootstrap CA: %v", err)
		}
		opts.CA = ca
	}

	srv, err := server.New(store, cfg.Server.Scripts, loc, logger, opts)
	if err != nil {
		logger.Fatalf("init server: %v", err)
	}

	// The bootstrap listener is plain HTTP on purpose: a host that has no certificate
	// yet cannot get through the mTLS handshake, so this is what install.sh talks to.
	if cfg.Bootstrap.Enabled() {
		bootstrapSrv := &http.Server{
			Addr: cfg.Bootstrap.Listen,
			Handler: srv.BootstrapHandler(server.BootstrapConfig{
				DistDir:       cfg.Bootstrap.DistDir,
				CACert:        cfg.TLS.CACert,
				SigningPubPEM: signingPubPEM,
				APIURL:        cfg.Bootstrap.APIURL,
			}),
		}
		go func() {
			logger.Printf("  bootstrap (plain HTTP) on %s — install.sh and enrollment", cfg.Bootstrap.Listen)
			if err := bootstrapSrv.ListenAndServe(); err != nil {
				logger.Printf("bootstrap listener stopped: %v", err)
			}
		}()
	}

	httpSrv := &http.Server{
		Addr:      cfg.Server.Listen,
		Handler:   srv.Handler(),
		TLSConfig: tlsConfig,
	}

	logger.Printf("arcatum-server listening on %s", cfg.Server.Listen)
	logger.Printf("  scripts=%s  db=%s  backup_dir=%s", cfg.Server.Scripts, dbPath, cfg.Storage.BackupDir)
	if box != nil {
		logger.Printf("  instance secrets are encrypted at rest")
	} else {
		logger.Printf("  WARNING: no [secrets] master_key — credentials are stored in the database in plaintext.")
	}
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
