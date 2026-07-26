package runner

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"arcatum/pkg/crypto"
)

// Certificates expire. Because they are all issued around the same time, they would all
// expire around the same time too — every runner going dark on the same day. Renewal
// avoids that by having each runner replace its own certificate well before the
// deadline.
//
// Renewal needs no operator action: the request travels over mTLS, so the runner has
// already proved it holds the certificate being replaced. That is a different situation
// from enrollment, where nothing is known about the host yet.

// renewBefore is how long before expiry a runner starts renewing. Generous on purpose:
// a runner that only wakes up occasionally still gets several chances.
const renewBefore = 30 * 24 * time.Hour

// certNotAfter reads the expiry from a certificate file.
func certNotAfter(path string) (time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return time.Time{}, fmt.Errorf("%s: no PEM block", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, err
	}
	return cert.NotAfter, nil
}

// NeedsRenewal reports whether the certificate is close enough to expiry to replace.
func NeedsRenewal(certPath string, now time.Time) (bool, time.Time, error) {
	notAfter, err := certNotAfter(certPath)
	if err != nil {
		return false, time.Time{}, err
	}
	return now.Add(renewBefore).After(notAfter), notAfter, nil
}

// RenewIfNeeded replaces the runner's certificate when it is nearing expiry. It reports
// whether a new certificate was written, in which case the caller must rebuild its TLS
// client to start using it.
//
// A failure here is deliberately not fatal: the current certificate still works, so the
// runner keeps going and tries again later.
func (a *Agent) RenewIfNeeded(ctx context.Context, certPath, keyPath string) (renewed bool) {
	if certPath == "" || keyPath == "" {
		return false // development mode
	}
	due, notAfter, err := NeedsRenewal(certPath, time.Now())
	if err != nil {
		a.log.Printf("renewal: cannot read %s: %v", certPath, err)
		return false
	}
	if !due {
		return false
	}
	daysLeft := int(time.Until(notAfter).Hours() / 24)
	a.log.Printf("renewal: certificate expires in %d day(s) (%s), requesting a new one",
		daysLeft, notAfter.Format(time.RFC3339))

	// A fresh keypair, so a renewal also rotates the key rather than just extending the
	// certificate over an old one.
	csrPEM, keyPEM, err := crypto.CreateCSR(a.req.RunnerID)
	if err != nil {
		a.log.Printf("renewal: create request: %v", err)
		return false
	}
	certPEM, err := a.requestRenewal(ctx, csrPEM)
	if err != nil {
		a.log.Printf("renewal: %v (will retry; current certificate is still valid)", err)
		return false
	}
	// Write the key first: a key without a matching certificate is recoverable (renew
	// again), a certificate without its key is not usable at all.
	if err := writeFileMode(keyPath, keyPEM, 0o600); err != nil {
		a.log.Printf("renewal: write key: %v", err)
		return false
	}
	if err := writeFileMode(certPath, certPEM, 0o644); err != nil {
		a.log.Printf("renewal: write certificate: %v", err)
		return false
	}
	if newNotAfter, err := certNotAfter(certPath); err == nil {
		a.log.Printf("renewal: done, new certificate valid until %s", newNotAfter.Format(time.RFC3339))
	}
	return true
}

// requestRenewal asks the server to sign a new certificate over the authenticated mTLS
// connection.
func (a *Agent) requestRenewal(ctx context.Context, csrPEM []byte) ([]byte, error) {
	body, err := json.Marshal(map[string]string{"csr": string(csrPEM)})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.client.BaseURL()+"/api/v1/renew", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.HTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("request renewal: %w", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("renewal refused (%s): %s", resp.Status, bytes.TrimSpace(payload))
	}
	var out struct {
		CertPEM string `json:"cert_pem"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("decode renewal response: %w", err)
	}
	if out.CertPEM == "" {
		return nil, fmt.Errorf("renewal response contained no certificate")
	}
	return []byte(out.CertPEM), nil
}
