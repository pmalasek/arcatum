// Package runner … (see executor.go). This file implements the runner's side of
// enrollment: obtaining a client certificate on first start.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"arcatum/pkg/crypto"
)

// Enrollment lets a freshly installed host obtain its client certificate without an
// operator copying private keys around. The private key is generated here and stays
// here; only a certificate request is sent.
//
// The request goes to the server's plain-HTTP bootstrap address, because this host has
// no certificate yet and the mTLS listener would reject it during the handshake. That
// is safe: a CSR carries only a public key, and the certificate the server returns is
// public too. Nothing is trusted until an operator approves the request.

// EnrollConfig is what the runner needs to enrol.
type EnrollConfig struct {
	RunnerID     string
	Hostname     string
	OS           string
	Arch         string
	EnrollServer string // plain-HTTP bootstrap base URL
	CertPath     string // where to write the signed certificate
	KeyPath      string // where to write the private key
	PollInterval time.Duration
}

// NeedsEnrollment reports whether the certificate or key is missing, which is what
// makes the runner enrol on first start and stay quiet on every later start.
func NeedsEnrollment(certPath, keyPath string) bool {
	if certPath == "" || keyPath == "" {
		return false // mTLS not configured at all: development mode
	}
	for _, p := range []string{certPath, keyPath} {
		if _, err := os.Stat(p); err != nil {
			return true
		}
	}
	return false
}

// Enroll requests a certificate and waits until an operator approves it. It returns
// once the certificate has been written, or when ctx is cancelled.
func Enroll(ctx context.Context, cfg EnrollConfig, logger *log.Logger) error {
	if cfg.EnrollServer == "" {
		return fmt.Errorf("no certificate at %s and runner.enroll_server is not set — "+
			"either install via install.sh or issue a certificate with 'arcatum-ca runner'", cfg.CertPath)
	}
	interval := cfg.PollInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}

	// Reuse the key if a previous attempt already created one, so re-running does not
	// invalidate a request an operator may be about to approve.
	csrPEM, err := loadOrCreateKey(cfg, logger)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 30 * time.Second}
	if err := submitCSR(ctx, client, cfg, csrPEM, logger); err != nil {
		return err
	}

	logger.Printf("enrollment: waiting for an operator to approve %q in the web UI", cfg.RunnerID)
	for {
		certPEM, status, err := pollEnrollment(ctx, client, cfg)
		switch {
		case err != nil:
			logger.Printf("enrollment: %v (retrying in %s)", err, interval)
		case status == "approved" && len(certPEM) > 0:
			if err := writeFileMode(cfg.CertPath, certPEM, 0o644); err != nil {
				return fmt.Errorf("write certificate: %w", err)
			}
			logger.Printf("enrollment: approved, certificate written to %s", cfg.CertPath)
			return nil
		case status == "rejected":
			return fmt.Errorf("enrollment was rejected by an operator")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// loadOrCreateKey returns a CSR for the existing key, or creates a new keypair.
func loadOrCreateKey(cfg EnrollConfig, logger *log.Logger) (csrPEM []byte, err error) {
	csrPath := cfg.KeyPath + ".csr"
	if _, err := os.Stat(cfg.KeyPath); err == nil {
		if existing, err := os.ReadFile(csrPath); err == nil && len(existing) > 0 {
			logger.Printf("enrollment: reusing the existing key and request (%s)", cfg.KeyPath)
			return existing, nil
		}
	}
	logger.Printf("enrollment: generating a private key for %q (it stays on this host)", cfg.RunnerID)
	csrPEM, keyPEM, err := crypto.CreateCSR(cfg.RunnerID)
	if err != nil {
		return nil, fmt.Errorf("create certificate request: %w", err)
	}
	if err := writeFileMode(cfg.KeyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("write private key: %w", err)
	}
	// Keeping the CSR next to the key means a restart resumes the same request rather
	// than asking the operator to approve a new one.
	if err := writeFileMode(csrPath, csrPEM, 0o644); err != nil {
		return nil, fmt.Errorf("write certificate request: %w", err)
	}
	return csrPEM, nil
}

func submitCSR(ctx context.Context, client *http.Client, cfg EnrollConfig, csrPEM []byte, logger *log.Logger) error {
	body, err := json.Marshal(map[string]string{
		"runner_id": cfg.RunnerID,
		"hostname":  cfg.Hostname,
		"os":        cfg.OS,
		"arch":      cfg.Arch,
		"csr":       string(csrPEM),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		cfg.EnrollServer+"/api/v1/enroll", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("submit certificate request to %s: %w", cfg.EnrollServer, err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	switch resp.StatusCode {
	case http.StatusAccepted, http.StatusOK:
		logger.Printf("enrollment: request submitted for %q", cfg.RunnerID)
		return nil
	case http.StatusConflict:
		// The server already approved this runner id: a certificate exists, so carry on
		// to the pickup step instead of failing.
		logger.Printf("enrollment: server reports %q is already approved, fetching the certificate", cfg.RunnerID)
		return nil
	default:
		return fmt.Errorf("enrollment refused (%s): %s", resp.Status, bytes.TrimSpace(payload))
	}
}

// pollEnrollment asks whether the request has been approved yet.
func pollEnrollment(ctx context.Context, client *http.Client, cfg EnrollConfig) (certPEM []byte, status string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		cfg.EnrollServer+"/api/v1/enroll/"+cfg.RunnerID, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("check enrollment status: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("enrollment status: %s", resp.Status)
	}
	var out struct {
		Status  string `json:"status"`
		CertPEM string `json:"cert_pem"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256*1024)).Decode(&out); err != nil {
		return nil, "", fmt.Errorf("decode enrollment status: %w", err)
	}
	return []byte(out.CertPEM), out.Status, nil
}

// writeFileMode writes a file, creating its directory, with the given permissions.
func writeFileMode(path string, data []byte, mode os.FileMode) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, mode)
}
