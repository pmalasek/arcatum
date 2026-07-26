package server

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

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

// Reasons a runner is turned away, sent as a machine-readable code so the runner can
// tell "ask for a new certificate" apart from "an operator refused you".
const (
	// ReasonEnrollRequired means the certificate was revoked: the runner should enrol
	// again and wait for approval.
	ReasonEnrollRequired = "enroll_required"
	// ReasonRejected means an operator refused this runner. Enrolling again would only
	// fill the queue with requests nobody wants.
	ReasonRejected = "rejected"
)

// notApprovedError carries the reason a runner was refused.
type notApprovedError struct {
	reason  string
	message string
}

func (e *notApprovedError) Error() string { return e.message }

// activeRunnerIdentity resolves the calling runner and refuses any runner that is not
// approved. Every path a runner can take must go through this rather than
// runnerIdentity: a revoked or rejected host still holds a cryptographically valid
// certificate until it expires, so refusing it is an application-level decision that has
// to be made consistently. Checking it only at check-in would leave the backup
// repository open.
func (s *Server) activeRunnerIdentity(r *http.Request, claimed string) (string, error) {
	id, err := s.runnerIdentity(r, claimed)
	if err != nil {
		return "", err
	}
	if err := s.requireApprovedRunner(id); err != nil {
		return "", err
	}
	return id, nil
}

// requireApprovedRunner allows only an approved runner through.
func (s *Server) requireApprovedRunner(runnerID string) error {
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
	switch status {
	case EnrollApproved:
		return nil
	case "":
		// No record yet. The runner holds a certificate this CA issued, which is itself
		// the authorization — a hand-issued runner has no row until its first check-in
		// creates one (as approved).
		return nil
	case EnrollRejected:
		return &notApprovedError{ReasonRejected,
			fmt.Sprintf("runner %q has been rejected by an operator", runnerID)}
	default:
		// Pending: either enrolled and not approved yet, or its certificate was
		// revoked. Either way the certificate it is holding is no longer trusted.
		return &notApprovedError{ReasonEnrollRequired,
			fmt.Sprintf("runner %q is not approved (%s) — enrol again", runnerID, status)}
	}
}

// denyRunner answers a refused runner with the reason in a form it can act on.
func (s *Server) denyRunner(w http.ResponseWriter, err error) {
	reason := ""
	var nae *notApprovedError
	if errors.As(err, &nae) {
		reason = nae.reason
	}
	w.Header().Set("Content-Type", "application/json")
	if reason != "" {
		w.Header().Set("X-Arcatum-Reason", reason)
	}
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error(), "reason": reason})
}

// Identity describes the caller and, when they authenticated with a certificate, the
// state of that certificate. The web UI reads it to know who is logged in, what they may
// do, and whether a certificate is about to expire — including the server's own, whose
// lapse would stop every runner.
type Identity struct {
	Name string `json:"name"`
	Role string `json:"role"`
	// Auth is how the caller proved who they are: "password" on the web listener,
	// "certificate" on the mTLS one, empty in development mode where nothing is checked.
	Auth         string    `json:"auth,omitempty"`
	CertNotAfter time.Time `json:"cert_not_after,omitempty"`
	DaysLeft     int       `json:"days_left,omitempty"`
	// ServerCertNotAfter is when the server's own certificate expires. Runners stop
	// trusting the server after that, so it needs watching too.
	ServerCertNotAfter time.Time `json:"server_cert_not_after,omitempty"`
	ServerDaysLeft     int       `json:"server_days_left,omitempty"`
	// Secured reports whether mTLS is on at all; false means development mode.
	Secured bool `json:"secured"`
}

// handleWhoAmI reports the caller's identity and certificate expiry. It is served on
// both listeners, so it answers for whichever identity the caller actually used: a
// logged-in operator on the web listener, a certificate on the mTLS one.
func (s *Server) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	id := Identity{Secured: s.requireClientCert}
	if u := userFrom(r); u != nil {
		id.Name = u.Username
		id.Role = u.Role
		id.Auth = "password"
	} else if cert := peerCert(r); cert != nil {
		id.Name = cert.Subject.CommonName
		id.Role = crypto.CertRole(cert)
		id.Auth = "certificate"
		id.CertNotAfter = cert.NotAfter
		id.DaysLeft = daysUntil(cert.NotAfter)
	}
	if !s.serverCertNotAfter.IsZero() {
		id.ServerCertNotAfter = s.serverCertNotAfter
		id.ServerDaysLeft = daysUntil(s.serverCertNotAfter)
	}
	writeJSON(w, id)
}

// daysUntil rounds down, so "0 days left" means it expires today rather than tomorrow.
func daysUntil(t time.Time) int {
	if t.IsZero() {
		return 0
	}
	return int(time.Until(t).Hours() / 24)
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
