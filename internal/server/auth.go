package server

import (
	"crypto/x509"
	"fmt"
	"net/http"

	"arcatum/pkg/crypto"
)

// Authentication is certificate-based. With mTLS enabled the TLS handshake has
// already proven the peer holds a key whose certificate our CA signed, so the
// certificate — not anything in the request body — decides who the caller is:
//
//   - Common Name      → identity (a runner's id, or an admin's name)
//   - Organizational Unit → role ("runner" or "admin")
//
// Runners may only fetch their own work and report on their own runs; driving the
// API (triggering jobs, listing instances, reading output) requires an admin
// certificate. Without mTLS the server runs in development mode: no identity is
// available, so every check is skipped — see cmd/server's warning.

// peerCert returns the verified client certificate, if the connection had one.
func peerCert(r *http.Request) *x509.Certificate {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return nil
	}
	// PeerCertificates[0] is the leaf, already verified against ClientCAs by the
	// TLS stack (RequireAndVerifyClientCert).
	return r.TLS.PeerCertificates[0]
}

// runnerIdentity resolves which runner is calling. The claimed id (from the request
// body) is accepted only in development mode; with mTLS the certificate wins and a
// mismatch is rejected loudly rather than silently returning no work.
func (s *Server) runnerIdentity(r *http.Request, claimed string) (string, error) {
	if !s.requireClientCert {
		if claimed == "" {
			return "", fmt.Errorf("runner_id is required")
		}
		return claimed, nil
	}
	cert := peerCert(r)
	if cert == nil {
		return "", fmt.Errorf("client certificate required")
	}
	if role := crypto.CertRole(cert); role != crypto.RoleRunner {
		return "", fmt.Errorf("certificate %q has role %q, need %q", cert.Subject.CommonName, role, crypto.RoleRunner)
	}
	id := cert.Subject.CommonName
	if id == "" {
		return "", fmt.Errorf("client certificate has no common name")
	}
	if claimed != "" && claimed != id {
		return "", fmt.Errorf("runner_id %q does not match certificate %q", claimed, id)
	}
	return id, nil
}

// activeRunnerIdentity resolves the calling runner and refuses one that an operator has
// rejected. Every path a runner can take must go through this rather than
// runnerIdentity: a rejected host still holds a cryptographically valid certificate
// until it expires, so refusing it is an application-level decision that has to be made
// consistently. Checking it only at check-in would leave the backup repository open.
func (s *Server) activeRunnerIdentity(r *http.Request, claimed string) (string, error) {
	id, err := s.runnerIdentity(r, claimed)
	if err != nil {
		return "", err
	}
	if err := s.checkRunnerNotRejected(id); err != nil {
		return "", err
	}
	return id, nil
}

// checkRunnerNotRejected refuses a runner that has been rejected.
func (s *Server) checkRunnerNotRejected(runnerID string) error {
	if !s.requireClientCert {
		return nil // development mode: no identity to act on
	}
	status, err := s.store.RunnerStatus(runnerID)
	if err != nil {
		// Failing closed would break every runner on a database hiccup; log and allow,
		// since the certificate itself was already verified.
		s.log.Printf("runner status lookup for %q failed: %v", runnerID, err)
		return nil
	}
	if status == EnrollRejected {
		return fmt.Errorf("runner %q has been rejected", runnerID)
	}
	return nil
}

// adminOnly wraps a handler so it can only be reached with an admin certificate.
func (s *Server) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.requireClientCert {
			cert := peerCert(r)
			if cert == nil {
				http.Error(w, "client certificate required", http.StatusUnauthorized)
				return
			}
			if role := crypto.CertRole(cert); role != crypto.RoleAdmin {
				s.log.Printf("denied: %q (role %q) tried %s %s", cert.Subject.CommonName, role, r.Method, r.URL.Path)
				http.Error(w, "admin certificate required", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}
