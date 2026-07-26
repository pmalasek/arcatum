// Command runner is the arcatum-runner: a small service on each backed-up host. It
// initiates all communication (pull model) — nothing listens for inbound connections.
//
// Phase B: it checks in over plain HTTP, runs due jobs, and streams output back.
// mTLS and dispatch-signature verification come later.
package main

import (
	"context"
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
		if verifier, err = crypto.LoadVerifier(cfg.Signing.PublicKey); err != nil {
			logger.Fatalf("signing public key: %v", err)
		}
	}

	client := runner.NewClient(cfg.Runner.Server, httpClient)
	workBase := filepath.Join(cfg.Runner.DataDir, "work")
	tlsFiles := runner.TLSFiles{CACert: cfg.TLS.CACert, Cert: cfg.TLS.Cert, Key: cfg.TLS.Key}
	agent := runner.NewAgent(client, req, workBase, logger, verifier, tlsFiles)

	logger.Printf("arcatum-runner (protocol %s) — server=%s runner=%q (%s/%s)",
		proto.Version, cfg.Runner.Server, req.Hostname, req.OS, req.Arch)
	if verifier != nil {
		logger.Printf("mTLS enabled; job signatures are verified before execution")
	} else {
		logger.Printf("WARNING: no [tls] configured — plain HTTP and job signatures are NOT verified.")
		logger.Printf("         Development only. See README: Zabezpečení (mTLS a podpis úloh).")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *once {
		if err := agent.Tick(ctx); err != nil {
			logger.Fatalf("checkin: %v", err)
		}
		return
	}
	agent.Run(ctx, interval)
}
