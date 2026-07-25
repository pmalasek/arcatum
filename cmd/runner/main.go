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
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"arcatum/internal/runner"
	"arcatum/pkg/config"
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

	client := runner.NewClient(cfg.Runner.Server, nil)
	workBase := filepath.Join(cfg.Runner.DataDir, "work")
	agent := runner.NewAgent(client, req, workBase, logger)

	logger.Printf("arcatum-runner (protocol %s) — server=%s runner=%q (%s/%s)",
		proto.Version, cfg.Runner.Server, req.Hostname, req.OS, req.Arch)

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
