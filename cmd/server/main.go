// Command server is the arcatum-server: scheduler, API, storage and (later) web UI.
//
// With [tls] and [signing] configured it serves mTLS and signs every dispatched job.
// Without them it falls back to plain HTTP for local development — unauthenticated,
// so it must not be used on a real network.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"arcatum/internal/server"
	"arcatum/pkg/config"
	"arcatum/pkg/crypto"
)

// signingKeysFor loads every dispatch-signing key the server holds: the one it signs
// with, plus any predecessor still inside the rotation window. The public parts become
// the set runners accept; the private parts all co-sign the published trust material, so
// a runner on any generation of key can verify it.
func signingKeysFor(cfg *config.Config) (*crypto.SigningSet, []crypto.Signer, error) {
	var (
		pems    [][]byte
		signers []crypto.Signer
	)
	for i, path := range append([]string{cfg.Signing.Key}, cfg.Signing.PreviousKeys...) {
		signer, err := crypto.LoadSigner(path)
		if err != nil {
			if i == 0 {
				return nil, nil, err
			}
			return nil, nil, fmt.Errorf("previous signing key %s: %w", path, err)
		}
		pub, err := signer.Public()
		if err != nil {
			return nil, nil, err
		}
		pems = append(pems, pub)
		signers = append(signers, signer)
	}
	set, err := crypto.NewSigningSet(pems)
	if err != nil {
		return nil, nil, err
	}
	return set, signers, nil
}

func main() {
	configPath := flag.String("config", "config/server.toml", "path to server config")
	instancesPath := flag.String("instances", "data/instances.json", "instances JSON to seed from on start")
	importForce := flag.Bool("import-force", false,
		"let the seed file overwrite instances that already exist (they are normally left alone, "+
			"so edits made through the web UI survive a restart)")
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

	// Secrets are encrypted at rest whenever a master key is configured. Previous keys
	// stay loaded so a rotation can still read values sealed before it.
	var box *crypto.Keyring
	if cfg.Secrets.MasterKey != "" {
		if box, err = crypto.LoadKeyring(cfg.Secrets.MasterKey, cfg.Secrets.PreviousKeys); err != nil {
			logger.Fatalf("secrets master key: %v", err)
		}
	}

	dbPath := filepath.Join(cfg.Server.DataDir, "arcatum.db")
	store, err := server.Open(dbPath, cfg.Storage.BackupDir, box)
	if err != nil {
		logger.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// The JSON file seeds instances that do not exist yet. Instances are managed through
	// the API, so re-importing over them on every start would undo those edits.
	n, err := store.ImportInstances(*instancesPath, *importForce)
	if err != nil {
		logger.Fatalf("import instances: %v", err)
	}
	if n > 0 {
		logger.Printf("seeded %d new instance(s) from %s", n, *instancesPath)
	}

	opts := server.Options{
		RequireClientCert: cfg.TLS.Enabled(),
		// The same directory the bootstrap listener installs from is what auto-update
		// publishes; a VERSION file next to the binaries is what makes them an update.
		DistDir: cfg.Bootstrap.DistDir,
	}
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
		// Surface our own certificate's expiry: once it lapses, runners stop trusting
		// this server, and that should not come as a surprise.
		if len(tlsConfig.Certificates) > 0 && len(tlsConfig.Certificates[0].Certificate) > 0 {
			if leaf, err := x509.ParseCertificate(tlsConfig.Certificates[0].Certificate[0]); err == nil {
				opts.ServerCertNotAfter = leaf.NotAfter
				opts.ServerCertIssuer = leaf.Issuer.CommonName
				logger.Printf("  server certificate valid until %s", leaf.NotAfter.Format(time.RFC3339))
			}
		}
	}
	// The CA is only needed to sign enrollment requests from new runners. During a CA
	// rotation the signing authority differs from the trust bundle, so it is configured
	// separately.
	if cfg.Bootstrap.Enabled() {
		signingCA := cfg.Bootstrap.SigningCA(cfg.TLS.CACert)
		ca, err := crypto.LoadCA(signingCA, cfg.Bootstrap.CAKey)
		if err != nil {
			logger.Fatalf("bootstrap CA: %v", err)
		}
		opts.CA = ca
		opts.Rotation.SigningCAName = ca.Cert.Subject.CommonName
		logger.Printf("  new certificates are issued under %q", ca.Cert.Subject.CommonName)
	}
	// Trust material published to runners: which signing keys to accept and which
	// authorities to trust. Both are served signed, so rotation propagates by itself.
	if cfg.TLS.Enabled() {
		set, signers, err := signingKeysFor(cfg)
		if err != nil {
			logger.Fatalf("signing keys: %v", err)
		}
		opts.Rotation.SigningSet = set
		opts.Rotation.Signers = signers
		opts.Rotation.CABundlePEM = server.ReadTrustBundle(cfg.TLS.CACert)
		if set.Len() > 1 {
			logger.Printf("  %d dispatch-signing keys trusted (rotation in progress)", set.Len())
		}
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
