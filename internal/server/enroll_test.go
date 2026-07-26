package server

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"arcatum/pkg/crypto"
)

// enrollTestServer returns a server with a CA, so enrollment requests can be signed.
func enrollTestServer(t *testing.T, requireClientCert bool) (*Server, *crypto.CA) {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "test.db"), filepath.Join(dir, "backup"), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ca, err := crypto.CreateCA("Test CA", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateCA: %v", err)
	}
	srv := &Server{
		store:             st,
		log:               log.New(io.Discard, "", 0),
		requireClientCert: requireClientCert,
		ca:                ca,
		sched:             NewScheduler(time.UTC),
		catalog:           &Catalog{byName: map[string]*ScriptEntry{}},
	}
	return srv, ca
}

// enroll posts a certificate request through the bootstrap handler, which is where a
// certificate-less host reaches the server.
func enroll(t *testing.T, srv *Server, runnerID, csrPEM string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(EnrollRequest{
		RunnerID: runnerID, Hostname: runnerID, OS: "linux", Arch: "amd64", CSR: csrPEM,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/enroll", bytes.NewReader(body))
	r.RemoteAddr = "10.1.2.3:44444"
	rec := httptest.NewRecorder()
	srv.BootstrapHandler(BootstrapConfig{APIURL: "https://arcatum:8443"}).ServeHTTP(rec, r)
	return rec
}

func enrollStatus(t *testing.T, srv *Server, runnerID string) (int, EnrollResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.BootstrapHandler(BootstrapConfig{}).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/api/v1/enroll/"+runnerID, nil))
	var out EnrollResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return rec.Code, out
}

// adminPost drives an admin action over the mTLS mux.
func adminPost(t *testing.T, srv *Server, path string, cert *x509.Certificate) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, nil)
	if cert != nil {
		r.TLS = requestWithCert(cert).TLS
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	return rec
}

// The whole point of enrollment: the private key is created on the host and never sent,
// and the certificate only arrives after an operator approves.
func TestEnrollmentFlow(t *testing.T) {
	srv, ca := enrollTestServer(t, false)
	csrPEM, keyPEM, err := crypto.CreateCSR("web-01")
	if err != nil {
		t.Fatalf("CreateCSR: %v", err)
	}
	if len(keyPEM) == 0 {
		t.Fatal("CreateCSR must return the private key, which stays on the host")
	}

	// 1. Submit: recorded as pending, nothing issued yet.
	if rec := enroll(t, srv, "web-01", string(csrPEM)); rec.Code != http.StatusAccepted {
		t.Fatalf("enroll = %d, want 202 (body %q)", rec.Code, rec.Body.String())
	}
	code, res := enrollStatus(t, srv, "web-01")
	if code != http.StatusOK || res.Status != EnrollPending || res.CertPEM != "" {
		t.Fatalf("status = %d/%+v, want 200/pending with no certificate", code, res)
	}

	// The operator sees where the request came from, so a swapped request is detectable.
	runners, err := srv.store.Runners()
	if err != nil || len(runners) != 1 {
		t.Fatalf("Runners: %v (%d)", err, len(runners))
	}
	if runners[0].Status != EnrollPending || runners[0].EnrollIP != "10.1.2.3" {
		t.Errorf("runner = %+v, want pending from 10.1.2.3", runners[0])
	}

	// 2. Approve as admin.
	rec := adminPost(t, srv, "/api/v1/runners/web-01/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	// 3. The runner picks up a certificate that chains to the CA and carries the
	//    runner role with its own id.
	code, res = enrollStatus(t, srv, "web-01")
	if code != http.StatusOK || res.Status != EnrollApproved || res.CertPEM == "" {
		t.Fatalf("status = %d/%+v, want 200/approved with a certificate", code, res)
	}
	block, _ := pem.Decode([]byte(res.CertPEM))
	if block == nil {
		t.Fatal("issued certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse issued certificate: %v", err)
	}
	if cert.Subject.CommonName != "web-01" {
		t.Errorf("CN = %q, want web-01", cert.Subject.CommonName)
	}
	if role := crypto.CertRole(cert); role != crypto.RoleRunner {
		t.Errorf("role = %q, want %q", role, crypto.RoleRunner)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("issued certificate does not chain to the CA: %v", err)
	}
}

func TestEnrollRejectsBadRequests(t *testing.T) {
	srv, _ := enrollTestServer(t, false)
	goodCSR, _, err := crypto.CreateCSR("web-01")
	if err != nil {
		t.Fatalf("CreateCSR: %v", err)
	}
	otherCSR, _, err := crypto.CreateCSR("db-01")
	if err != nil {
		t.Fatalf("CreateCSR: %v", err)
	}

	tests := []struct {
		name     string
		runnerID string
		csr      string
	}{
		{"garbage csr", "web-01", "not a pem"},
		{"empty csr", "web-01", ""},
		// A request whose subject disagrees with the claimed id would give a
		// certificate for a different identity than the operator approves.
		{"csr common name mismatch", "web-01", string(otherCSR)},
		{"invalid runner id", "../etc/passwd", string(goodCSR)},
		{"empty runner id", "", string(goodCSR)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if rec := enroll(t, srv, tc.runnerID, tc.csr); rec.Code != http.StatusBadRequest {
				t.Errorf("enroll = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
		})
	}
}

// Re-running install.sh must not require an operator to reject the previous request.
func TestEnrollTwiceWhilePendingIsAllowed(t *testing.T) {
	srv, _ := enrollTestServer(t, false)
	first, _, _ := crypto.CreateCSR("web-01")
	second, _, _ := crypto.CreateCSR("web-01")

	if rec := enroll(t, srv, "web-01", string(first)); rec.Code != http.StatusAccepted {
		t.Fatalf("first enroll = %d", rec.Code)
	}
	if rec := enroll(t, srv, "web-01", string(second)); rec.Code != http.StatusAccepted {
		t.Fatalf("second enroll = %d, want 202", rec.Code)
	}
	// The latest request is the one that gets signed.
	csr, err := srv.store.PendingCSR("web-01")
	if err != nil {
		t.Fatalf("PendingCSR: %v", err)
	}
	if csr != string(second) {
		t.Error("the most recent request should be the pending one")
	}
	runners, _ := srv.store.Runners()
	if len(runners) != 1 {
		t.Errorf("got %d runners, want 1", len(runners))
	}
}

// Once approved, a host on the network must not be able to replace the certificate of a
// working runner just by enrolling again.
func TestEnrollRefusedOnceApproved(t *testing.T) {
	srv, _ := enrollTestServer(t, false)
	csrPEM, _, _ := crypto.CreateCSR("web-01")
	enroll(t, srv, "web-01", string(csrPEM))
	adminPost(t, srv, "/api/v1/runners/web-01/approve", nil)

	attacker, _, _ := crypto.CreateCSR("web-01")
	rec := enroll(t, srv, "web-01", string(attacker))
	if rec.Code != http.StatusConflict {
		t.Fatalf("re-enroll of an approved runner = %d, want 409", rec.Code)
	}
	// The original certificate must still be the one on offer.
	_, res := enrollStatus(t, srv, "web-01")
	if res.Status != EnrollApproved || res.CertPEM == "" {
		t.Errorf("status = %+v, want the original approval intact", res)
	}
}

func TestRejectEnrollment(t *testing.T) {
	srv, _ := enrollTestServer(t, false)
	csrPEM, _, _ := crypto.CreateCSR("web-01")
	enroll(t, srv, "web-01", string(csrPEM))

	if rec := adminPost(t, srv, "/api/v1/runners/web-01/reject", nil); rec.Code != http.StatusOK {
		t.Fatalf("reject = %d, want 200", rec.Code)
	}
	_, res := enrollStatus(t, srv, "web-01")
	if res.Status != EnrollRejected || res.CertPEM != "" {
		t.Errorf("status = %+v, want rejected with no certificate", res)
	}
	// Approving afterwards must fail: the request is gone.
	if rec := adminPost(t, srv, "/api/v1/runners/web-01/approve", nil); rec.Code == http.StatusOK {
		t.Error("approving a rejected runner should fail")
	}
}

func TestApproveWithoutPendingRequest(t *testing.T) {
	srv, _ := enrollTestServer(t, false)
	if rec := adminPost(t, srv, "/api/v1/runners/nobody/approve", nil); rec.Code == http.StatusOK {
		t.Errorf("approve of an unknown runner = %d, want a failure", rec.Code)
	}
}

// Approval and rejection are operator actions, so a runner certificate must not do them.
func TestApproveRequiresAdminCertificate(t *testing.T) {
	srv, _ := enrollTestServer(t, true)
	csrPEM, _, _ := crypto.CreateCSR("web-01")
	enroll(t, srv, "web-01", string(csrPEM))

	tests := []struct {
		name     string
		cert     *x509.Certificate
		wantCode int
	}{
		{"admin", certWithCNAndRole("petr", crypto.RoleAdmin), http.StatusOK},
		{"runner", certWithCNAndRole("web-01", crypto.RoleRunner), http.StatusForbidden},
		{"none", nil, http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := adminPost(t, srv, "/api/v1/runners/web-01/approve", tc.cert)
			if rec.Code != tc.wantCode {
				t.Errorf("approve = %d, want %d", rec.Code, tc.wantCode)
			}
		})
	}
}

// A rejected runner is refused even while it still holds a valid certificate, so
// rejecting one takes effect without waiting for revocation.
func TestCheckinRefusesRejectedRunner(t *testing.T) {
	srv, _ := enrollTestServer(t, true)
	csrPEM, _, _ := crypto.CreateCSR("web-01")
	enroll(t, srv, "web-01", string(csrPEM))
	// With client certificates required, the rejection itself needs an admin one.
	admin := certWithCNAndRole("petr", crypto.RoleAdmin)
	if rec := adminPost(t, srv, "/api/v1/runners/web-01/reject", admin); rec.Code != http.StatusOK {
		t.Fatalf("reject = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	body := strings.NewReader(`{"runner_id":"web-01","hostname":"web-01","os":"linux","arch":"amd64"}`)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/checkin", body)
	r.TLS = requestWithCert(certWithCNAndRole("web-01", crypto.RoleRunner)).TLS
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("checkin of a rejected runner = %d, want 403", rec.Code)
	}
}

// Runners whose certificate was issued by hand have no enrollment record; their
// check-ins must keep working after the upgrade that added enrollment.
func TestManuallyIssuedRunnerIsApprovedByDefault(t *testing.T) {
	srv, _ := enrollTestServer(t, true)
	body := strings.NewReader(`{"runner_id":"legacy-01","hostname":"legacy-01","os":"linux","arch":"amd64"}`)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/checkin", body)
	r.TLS = requestWithCert(certWithCNAndRole("legacy-01", crypto.RoleRunner)).TLS
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("checkin = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	status, err := srv.store.RunnerStatus("legacy-01")
	if err != nil {
		t.Fatalf("RunnerStatus: %v", err)
	}
	if status != EnrollApproved {
		t.Errorf("status = %q, want approved — a runner that already holds a valid certificate is authorized by it", status)
	}
}

func TestEnrollStatusUnknownRunner(t *testing.T) {
	srv, _ := enrollTestServer(t, false)
	if code, _ := enrollStatus(t, srv, "nobody"); code != http.StatusNotFound {
		t.Errorf("status of unknown runner = %d, want 404", code)
	}
}
