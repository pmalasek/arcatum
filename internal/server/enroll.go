package server

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"arcatum/pkg/crypto"
)

// Enrollment is how a freshly installed runner gets its client certificate without an
// operator copying private keys around:
//
//  1. the runner generates its own keypair and sends only a CSR (the key never leaves
//     the host),
//  2. the server records it as "pending" — nothing is trusted yet,
//  3. an operator approves it in the web UI, having checked the request's fingerprint
//     and source address,
//  4. the server signs the CSR and the runner picks up its certificate.
//
// These two endpoints must be reachable by a host that has no certificate yet, so they
// live on the bootstrap listener (plain HTTP) rather than the mTLS one, which would
// reject such a client during the handshake. That is safe: a CSR and a certificate are
// both public by nature, and the approval step is the actual gate.

// enrollValidity is how long an enrolled runner certificate is valid.
const enrollValidity = 825 * 24 * time.Hour

// EnrollRequest is what a runner submits to ask for a certificate.
type EnrollRequest struct {
	RunnerID string `json:"runner_id"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	CSR      string `json:"csr"` // PEM
}

// EnrollResponse tells the runner where its request stands.
type EnrollResponse struct {
	Status  string `json:"status"`
	CertPEM string `json:"cert_pem,omitempty"`
	Message string `json:"message,omitempty"`
}

// handleEnroll accepts a certificate request from a host without a certificate.
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req EnrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateInstanceID(req.RunnerID); err != nil {
		http.Error(w, "invalid runner_id", http.StatusBadRequest)
		return
	}
	// Reject a malformed CSR here rather than at approval time, so the operator never
	// sees a request that could not be signed anyway.
	fingerprint, csrCN, err := inspectCSR(req.CSR)
	if err != nil {
		http.Error(w, "invalid csr: "+err.Error(), http.StatusBadRequest)
		return
	}
	if csrCN != req.RunnerID {
		http.Error(w, fmt.Sprintf("csr common name %q does not match runner_id %q", csrCN, req.RunnerID),
			http.StatusBadRequest)
		return
	}

	ip := remoteIP(r)
	err = s.store.SubmitEnrollment(req.RunnerID, req.Hostname, req.OS, req.Arch, req.CSR, ip, time.Now())
	if errors.Is(err, ErrAlreadyApproved) {
		// An approved runner already has a certificate; re-issuing must be deliberate.
		s.log.Printf("enroll refused: runner %q is already approved (from %s)", req.RunnerID, ip)
		writeJSONStatus(w, http.StatusConflict, EnrollResponse{
			Status:  EnrollApproved,
			Message: "runner is already approved; ask an operator to reject it first if you need a new certificate",
		})
		return
	}
	if err != nil {
		s.log.Printf("enroll %q: %v", req.RunnerID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.log.Printf("enroll: runner %q requested a certificate from %s (csr %s) — awaiting approval",
		req.RunnerID, ip, shortFingerprint(fingerprint))
	writeJSONStatus(w, http.StatusAccepted, EnrollResponse{
		Status:  EnrollPending,
		Message: "request recorded, waiting for an operator to approve it",
	})
}

// handleEnrollStatus lets a runner poll for its signed certificate.
func (s *Server) handleEnrollStatus(w http.ResponseWriter, r *http.Request) {
	runnerID := r.PathValue("id")
	if err := validateInstanceID(runnerID); err != nil {
		http.Error(w, "invalid runner id", http.StatusBadRequest)
		return
	}
	e, err := s.store.Enrollment(runnerID)
	if err != nil {
		s.log.Printf("enroll status %q: %v", runnerID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if e == nil {
		http.Error(w, "unknown runner", http.StatusNotFound)
		return
	}
	writeJSON(w, EnrollResponse{Status: e.Status, CertPEM: e.CertPEM})
}

// handleApproveRunner signs a pending request (admin only).
func (s *Server) handleApproveRunner(w http.ResponseWriter, r *http.Request) {
	runnerID := r.PathValue("id")
	if s.ca == nil {
		http.Error(w, "server has no CA configured, cannot sign certificates", http.StatusServiceUnavailable)
		return
	}
	csrPEM, err := s.store.PendingCSR(runnerID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if csrPEM == "" {
		http.Error(w, "no pending request for this runner", http.StatusNotFound)
		return
	}
	certPEM, err := s.ca.SignCSR([]byte(csrPEM), enrollValidity)
	if err != nil {
		s.log.Printf("approve %q: sign csr: %v", runnerID, err)
		http.Error(w, "signing failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	fingerprint, err := certFingerprint(certPEM)
	if err != nil {
		s.log.Printf("approve %q: fingerprint: %v", runnerID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.store.ApproveEnrollment(runnerID, string(certPEM), fingerprint, time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.log.Printf("approve: runner %q approved, certificate issued (%s)", runnerID, shortFingerprint(fingerprint))
	writeJSON(w, map[string]string{"status": EnrollApproved, "runner": runnerID, "fingerprint": fingerprint})
}

// handleRejectRunner refuses a pending request (admin only).
func (s *Server) handleRejectRunner(w http.ResponseWriter, r *http.Request) {
	runnerID := r.PathValue("id")
	if err := s.store.RejectEnrollment(runnerID, time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.log.Printf("reject: runner %q rejected", runnerID)
	writeJSON(w, map[string]string{"status": EnrollRejected, "runner": runnerID})
}

// --- helpers ----------------------------------------------------------------

// inspectCSR parses a certificate request and returns its public-key fingerprint and
// common name.
func inspectCSR(csrPEM string) (fingerprint, commonName string, err error) {
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil {
		return "", "", errors.New("no PEM block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return "", "", err
	}
	if err := csr.CheckSignature(); err != nil {
		return "", "", fmt.Errorf("signature: %w", err)
	}
	sum := sha256.Sum256(csr.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:]), csr.Subject.CommonName, nil
}

// certFingerprint is the SHA-256 of the DER certificate, the value normally shown by
// other tooling.
func certFingerprint(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", errors.New("no PEM block in certificate")
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}

// shortFingerprint abbreviates a fingerprint for log lines.
func shortFingerprint(fp string) string {
	if len(fp) <= 16 {
		return fp
	}
	return fp[:16] + "…"
}

// remoteIP extracts the client address, which the operator uses to sanity-check where a
// request came from.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// verify the CA type satisfies what we need at compile time.
var _ interface {
	SignCSR([]byte, time.Duration) ([]byte, error)
} = (*crypto.CA)(nil)
