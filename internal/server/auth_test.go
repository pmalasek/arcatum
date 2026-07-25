package server

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"arcatum/pkg/crypto"
)

// certWithCNAndRole builds a certificate value good enough for the identity checks,
// which read only the subject. Chain validation is the TLS stack's job.
func certWithCNAndRole(cn, role string) *x509.Certificate {
	subject := pkix.Name{CommonName: cn}
	if role != "" {
		subject.OrganizationalUnit = []string{role}
	}
	return &x509.Certificate{Subject: subject}
}

// requestWithCert returns a request carrying a verified peer certificate.
func requestWithCert(cert *x509.Certificate) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	if cert != nil {
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	}
	return r
}

func testServer(requireClientCert bool) *Server {
	return &Server{
		log:               log.New(io.Discard, "", 0),
		requireClientCert: requireClientCert,
	}
}

func TestRunnerIdentityUsesCertificateNotClaim(t *testing.T) {
	s := testServer(true)
	r := requestWithCert(certWithCNAndRole("web-01", crypto.RoleRunner))

	// No claim: the certificate supplies the identity.
	got, err := s.runnerIdentity(r, "")
	if err != nil || got != "web-01" {
		t.Fatalf("runnerIdentity = %q, %v; want web-01, nil", got, err)
	}
	// Matching claim is fine.
	if got, err = s.runnerIdentity(r, "web-01"); err != nil || got != "web-01" {
		t.Fatalf("runnerIdentity(matching) = %q, %v; want web-01, nil", got, err)
	}
}

// A host holding a valid certificate must not be able to fetch another host's jobs
// by claiming a different runner_id.
func TestRunnerIdentityRejectsMismatchedClaim(t *testing.T) {
	s := testServer(true)
	r := requestWithCert(certWithCNAndRole("web-01", crypto.RoleRunner))
	if _, err := s.runnerIdentity(r, "db-01"); err == nil {
		t.Error("claiming another runner's id must be rejected")
	}
}

func TestRunnerIdentityRequiresCertificate(t *testing.T) {
	s := testServer(true)
	if _, err := s.runnerIdentity(requestWithCert(nil), "web-01"); err == nil {
		t.Error("a runner without a client certificate must be rejected")
	}
}

// An admin certificate is not a runner certificate: roles must not be interchangeable.
func TestRunnerIdentityRejectsWrongRole(t *testing.T) {
	s := testServer(true)
	r := requestWithCert(certWithCNAndRole("petr", crypto.RoleAdmin))
	if _, err := s.runnerIdentity(r, ""); err == nil {
		t.Error("an admin certificate must not authenticate as a runner")
	}
}

func TestRunnerIdentityRejectsRolelessCertificate(t *testing.T) {
	s := testServer(true)
	r := requestWithCert(certWithCNAndRole("web-01", ""))
	if _, err := s.runnerIdentity(r, ""); err == nil {
		t.Error("a certificate without a role must not authenticate as a runner")
	}
}

// In development mode there is no certificate, so the claimed id is used — but an
// empty claim is still an error.
func TestRunnerIdentityDevModeUsesClaim(t *testing.T) {
	s := testServer(false)
	got, err := s.runnerIdentity(requestWithCert(nil), "web-01")
	if err != nil || got != "web-01" {
		t.Fatalf("runnerIdentity = %q, %v; want web-01, nil", got, err)
	}
	if _, err := s.runnerIdentity(requestWithCert(nil), ""); err == nil {
		t.Error("an empty runner_id must be rejected even in development mode")
	}
}

func TestAdminOnly(t *testing.T) {
	tests := []struct {
		name       string
		cert       *x509.Certificate
		wantStatus int
	}{
		{"admin allowed", certWithCNAndRole("petr", crypto.RoleAdmin), http.StatusOK},
		{"runner forbidden", certWithCNAndRole("web-01", crypto.RoleRunner), http.StatusForbidden},
		{"no role forbidden", certWithCNAndRole("someone", ""), http.StatusForbidden},
		{"no certificate unauthorized", nil, http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(true)
			called := false
			h := s.adminOnly(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			})
			rec := httptest.NewRecorder()
			h(rec, requestWithCert(tc.cert))

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if called != (tc.wantStatus == http.StatusOK) {
				t.Errorf("handler called = %v, want %v", called, tc.wantStatus == http.StatusOK)
			}
		})
	}
}

// Without mTLS there is nothing to check, so development mode must stay usable.
func TestAdminOnlyDevModeAllowsEveryone(t *testing.T) {
	s := testServer(false)
	called := false
	h := s.adminOnly(func(w http.ResponseWriter, r *http.Request) { called = true })
	h(httptest.NewRecorder(), requestWithCert(nil))
	if !called {
		t.Error("development mode must not require a certificate")
	}
}
