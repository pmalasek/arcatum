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

// initialAdmin is the account created on a first start, so there is a way in before
// anything has been configured.
const initialAdmin = "admin"

// ensureAdminAccount creates the first administrator when the database has no accounts
// at all, and prints its generated password once. A server whose web UI nobody can log
// in to would be useless, and asking an operator to run a separate command before the
// first start is the kind of step that gets missed.
func ensureAdminAccount(store *server.Store, logger *log.Logger) error {
	n, err := store.UserCount()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	password, err := crypto.GeneratePassword()
	if err != nil {
		return err
	}
	if _, err := store.CreateUser(initialAdmin, password, server.UserRoleAdmin); err != nil {
		return err
	}
	// Printed once and never stored in the clear: the log is the only place it appears,
	// which is why it says to change it.
	logger.Printf("  ")
	logger.Printf("  ┌─ first start: created the web account ─────────────────────")
	logger.Printf("  │   user:     %s", initialAdmin)
	logger.Printf("  │   password: %s", password)
	logger.Printf("  │ Log in and change it (Účet → změnit heslo). A forgotten")
	logger.Printf("  │ password is reset with: arcatum-server -passwd %s", initialAdmin)
	logger.Printf("  └───────────────────────────────────────────────────────────")
	logger.Printf("  ")
	return nil
}

// setPassword backs the -passwd flag: it sets (or creates) an account's password and
// prints it when it had to be generated. This is the way back in when the last password
// is lost — everything else about accounts is done in the web UI.
func setPassword(store *server.Store, logger *log.Logger, username, role string) error {
	username = server.NormalizeUsername(username)
	password := os.Getenv("ARCATUM_PASSWORD")
	generated := password == ""
	if generated {
		var err error
		// Generated rather than prompted: a password typed on a command line ends up in
		// the shell history and in ps output.
		if password, err = crypto.GeneratePassword(); err != nil {
			return err
		}
	}
	existing, err := store.User(username)
	if err != nil {
		return err
	}
	switch {
	case existing == nil:
		if _, err := store.CreateUser(username, password, role); err != nil {
			return err
		}
		logger.Printf("created account %q with role %s", username, role)
	default:
		if err := store.SetUserPassword(username, password); err != nil {
			return err
		}
		// A disabled account would still not let anyone in, which is not what someone
		// resetting a password expects.
		if existing.Disabled {
			if err := store.SetUserDisabled(username, false); err != nil {
				return err
			}
			logger.Printf("account %q re-enabled", username)
		}
		logger.Printf("password of %q changed; its sessions were ended", username)
	}
	if generated {
		logger.Printf("password: %s", password)
	} else {
		logger.Printf("password taken from $ARCATUM_PASSWORD")
	}
	return nil
}

func main() {
	configPath := flag.String("config", "",
		"path to server config; without it ./server.toml is used, then /etc/arcatum/server.toml")
	instancesPath := flag.String("instances", "data/instances.json", "instances JSON to seed from on start")
	importForce := flag.Bool("import-force", false,
		"let the seed file overwrite instances that already exist (they are normally left alone, "+
			"so edits made through the web UI survive a restart)")
	passwd := flag.String("passwd", "",
		"set this web account's password (creating the account if needed) and exit — the way back "+
			"in when nobody can log in. Uses $ARCATUM_PASSWORD, or generates and prints one")
	passwdRole := flag.String("passwd-role", server.UserRoleAdmin,
		"role for an account created by -passwd: admin or viewer")
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags)

	// Say which file this is before anything can fail on its contents: a start that dies
	// on a certificate path is usually a start that read the wrong configuration.
	resolvedConfig, err := config.Resolve(*configPath)
	if err != nil {
		logger.Fatalf("%v", err)
	}
	if abs, absErr := filepath.Abs(resolvedConfig); absErr == nil {
		resolvedConfig = abs
	}
	logger.Printf("configuration from %s", resolvedConfig)

	cfg, err := config.Load(resolvedConfig)
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

	// -passwd is a maintenance mode, not a server start: it exists for the case where
	// nobody can log in to the web UI any more and the only access left is a shell.
	if *passwd != "" {
		if err := setPassword(store, logger, *passwd, *passwdRole); err != nil {
			logger.Fatalf("-passwd: %v", err)
		}
		return
	}

	// The JSON file seeds instances that do not exist yet. Instances are managed through
	// the API, so re-importing over them on every start would undo those edits.
	n, err := store.ImportInstances(*instancesPath, *importForce)
	if err != nil {
		logger.Fatalf("import instances: %v", err)
	}
	if n > 0 {
		logger.Printf("seeded %d new instance(s) from %s", n, *instancesPath)
	}

	sessionTTL, err := cfg.Web.TTL()
	if err != nil {
		logger.Fatalf("config: %v", err)
	}
	opts := server.Options{
		RequireClientCert: cfg.TLS.Enabled(),
		// The same directory the bootstrap listener installs from is what auto-update
		// publishes; a VERSION file next to the binaries is what makes them an update.
		DistDir: cfg.Bootstrap.DistDir,
		// Lets the web UI show the install command for a new runner, pointed at the port
		// install.sh is actually served from.
		BootstrapListen: cfg.Bootstrap.Listen,
		Web: server.WebOptions{
			SessionTTL:   sessionTTL,
			SecureCookie: cfg.Web.SecureCookie,
		},
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

	// The web UI listener is plain HTTP and authenticates people with a password, so
	// looking at the backups needs nothing but a browser. Runners are unaffected: they
	// keep talking mTLS to the listener below.
	if cfg.Web.Enabled() {
		if err := ensureAdminAccount(store, logger); err != nil {
			logger.Fatalf("web accounts: %v", err)
		}
		if n, err := store.PurgeExpiredSessions(time.Now()); err != nil {
			logger.Printf("purge sessions: %v", err)
		} else if n > 0 {
			logger.Printf("  purged %d expired web session(s)", n)
		}
		webSrv := &http.Server{
			Addr:    cfg.Web.Listen,
			Handler: srv.WebHandler(),
		}
		go func() {
			logger.Printf("  web UI (plain HTTP, password login) on %s", cfg.Web.Listen)
			if err := webSrv.ListenAndServe(); err != nil {
				logger.Printf("web listener stopped: %v", err)
			}
		}()
	} else {
		logger.Printf("  web UI disabled ([web] listen is empty) — the API is reachable with an admin certificate")
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
