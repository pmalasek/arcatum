// Command runner is the arcatum-runner: a small service on each backed-up host. It
// initiates all communication (pull model) — nothing listens for inbound connections.
//
// Phase B: it checks in over plain HTTP, runs due jobs, and streams output back.
// mTLS and dispatch-signature verification come later.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"arcatum/internal/runner"
	"arcatum/pkg/config"
	"arcatum/pkg/crypto"
	"arcatum/pkg/proto"
)

func main() {
	configPath := flag.String("config", "config/runner.toml", "path to runner config")
	serverFlag := flag.String("server", "", "override arcatum server base URL")
	once := flag.Bool("once", false, "check in once and exit (for testing)")
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags)

	cfg, err := config.LoadRunner(*configPath)
	if err != nil {
		logger.Fatalf("loading config: %v", err)
	}
	if *serverFlag != "" {
		cfg.Runner.Server = *serverFlag
	}
	if err := cfg.Validate(); err != nil {
		logger.Fatalf("config: %v", err)
	}
	interval, _ := cfg.Interval()

	host, _ := os.Hostname()
	req := proto.CheckinRequest{RunnerID: host, Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// First start after install.sh: no certificate yet. Generate a key locally, ask for
	// a certificate, and wait until an operator approves it.
	if runner.NeedsEnrollment(cfg.TLS.Cert, cfg.TLS.Key) {
		enrollCfg := runner.EnrollConfig{
			RunnerID:     req.RunnerID,
			Hostname:     req.Hostname,
			OS:           req.OS,
			Arch:         req.Arch,
			EnrollServer: cfg.Runner.EnrollServer,
			CertPath:     cfg.TLS.Cert,
			KeyPath:      cfg.TLS.Key,
			PollInterval: interval,
		}
		if err := runner.Enroll(ctx, enrollCfg, logger); err != nil {
			logger.Fatalf("enrollment: %v", err)
		}
	}

	// Where rotated trust material is kept. The CA bundle is the file the config already
	// points at, so an adopted bundle is picked up on the next start; the signing-key set
	// sits next to the runner's state.
	trustPaths := runner.TrustPaths{
		SigningKeys: filepath.Join(cfg.Runner.DataDir, "pki", "signing-keys.pem"),
		CACert:      cfg.TLS.CACert,
	}

	// With mTLS configured, the server identifies this runner by its certificate's
	// common name, so the certificate must be issued for this runner_id.
	httpClient := &http.Client{Timeout: 0} // no global timeout: runs stream for as long as they take
	var verifier crypto.Verifier
	if cfg.TLS.Enabled() {
		tlsConfig, err := crypto.ClientTLSConfig(cfg.TLS.Cert, cfg.TLS.Key, cfg.TLS.CACert)
		if err != nil {
			logger.Fatalf("tls: %v", err)
		}
		httpClient.Transport = &http.Transport{
			TLSClientConfig:     tlsConfig,
			TLSHandshakeTimeout: 10 * time.Second,
		}
		// Prefer the set of keys adopted from the server over the single key install.sh
		// delivered, so a signing-key rotation survives restarts.
		if verifier, err = runner.LoadTrustedSigningKeys(trustPaths.SigningKeys, cfg.Signing.PublicKey); err != nil {
			logger.Fatalf("signing keys: %v", err)
		}
	}

	client := runner.NewClient(cfg.Runner.Server, httpClient)
	workBase := filepath.Join(cfg.Runner.DataDir, "work")
	tlsFiles := runner.TLSFiles{CACert: cfg.TLS.CACert, Cert: cfg.TLS.Cert, Key: cfg.TLS.Key}
	agent := runner.NewAgent(client, req, workBase, logger, verifier, tlsFiles, trustPaths)

	logger.Printf("arcatum-runner (protocol %s) — server=%s runner=%q (%s/%s)",
		proto.Version, cfg.Runner.Server, req.Hostname, req.OS, req.Arch)
	if verifier != nil {
		logger.Printf("mTLS enabled; job signatures are verified before execution")
	} else {
		logger.Printf("WARNING: no [tls] configured — plain HTTP and job signatures are NOT verified.")
		logger.Printf("         Development only. See README: Zabezpečení (mTLS a podpis úloh).")
	}

	if *once {
		// One full cycle, the same one the loop performs — including adopting rotated
		// trust material, so -once behaves like production rather than a shortcut.
		if err := agent.RunOnce(ctx); errors.Is(err, runner.ErrRestartRequired) {
			logger.Printf("trust material changed; run again to use it")
			return
		} else if err != nil {
			logger.Fatalf("checkin: %v", err)
		}
		return
	}
	// Exiting on a certificate change is deliberate: the service manager restarts us and
	// we pick the new material up cleanly, instead of swapping TLS state mid-flight.
	if err := agent.Run(ctx, interval); errors.Is(err, runner.ErrRestartRequired) {
		logger.Printf("restarting to pick up the new certificate")
		return
	} else if err != nil {
		logger.Fatalf("runner stopped: %v", err)
	}
}
