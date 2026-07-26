package server

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"arcatum/pkg/crypto"
)

// enrolledRunner puts an approved runner in the store and returns its certificate.
func enrolledRunner(t *testing.T, srv *Server, runnerID string) string {
	t.Helper()
	csrPEM, _, err := crypto.CreateCSR(runnerID)
	if err != nil {
		t.Fatalf("CreateCSR: %v", err)
	}
	if rec := enroll(t, srv, runnerID, string(csrPEM)); rec.Code != http.StatusAccepted {
		t.Fatalf("enroll = %d", rec.Code)
	}
	if rec := adminPost(t, srv, "/api/v1/runners/"+runnerID+"/approve", adminCert()); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d (%s)", rec.Code, rec.Body.String())
	}
	_, res := enrollStatus(t, srv, runnerID)
	if res.CertPEM == "" {
		t.Fatal("no certificate issued")
	}
	return res.CertPEM
}

func adminCert() *x509.Certificate {
	return certWithCNAndRole("petr", crypto.RoleAdmin)
}

// runnerRequest builds a request as a runner would send it over mTLS.
func runnerRequest(method, path, body, runnerID string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.TLS = requestWithCert(certWithCNAndRole(runnerID, crypto.RoleRunner)).TLS
	return r
}

// checkinAs performs a check-in as a runner and returns the recorder.
func checkinAs(srv *Server, runnerID string) *httptest.ResponseRecorder {
	body := `{"runner_id":"` + runnerID + `","hostname":"` + runnerID + `","os":"linux","arch":"amd64"}`
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, runnerRequest(http.MethodPost, "/api/v1/checkin", body, runnerID))
	return rec
}

// Revocation is the compromise response: the certificate must stop working everywhere,
// and the runner must be told to obtain a new one.
func TestRevokeSendsRunnerBackToPending(t *testing.T) {
	srv, _ := enrollTestServer(t, true)
	enrolledRunner(t, srv, "web-01")

	// It works before revocation.
	if rec := checkinAs(srv, "web-01"); rec.Code != http.StatusOK {
		t.Fatalf("checkin before revoke = %d, want 200", rec.Code)
	}

	if rec := adminPost(t, srv, "/api/v1/runners/web-01/revoke", adminCert()); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	// Status is pending again and the stored certificate is gone.
	status, err := srv.store.RunnerStatus("web-01")
	if err != nil {
		t.Fatalf("RunnerStatus: %v", err)
	}
	if status != EnrollPending {
		t.Errorf("status = %q, want pending", status)
	}
	_, res := enrollStatus(t, srv, "web-01")
	if res.CertPEM != "" {
		t.Error("the revoked certificate is still being handed out")
	}

	// The old certificate is refused, with a reason the runner can act on.
	rec := checkinAs(srv, "web-01")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("checkin after revoke = %d, want 403", rec.Code)
	}
	var body struct{ Reason string }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Reason != ReasonEnrollRequired {
		t.Errorf("reason = %q, want %q so the runner knows to enrol again", body.Reason, ReasonEnrollRequired)
	}
}

// Revocation must close the backup repository too, not just new work.
func TestRevokedRunnerLosesRepositoryAccess(t *testing.T) {
	srv, _ := enrollTestServer(t, true)
	// The fixture's instance targets web-01.
	path := writeInstances(t, t.TempDir(), []*Instance{{
		ID: "files-web01", Script: "files-backup", RunnerID: "web-01",
		Schedule: ScheduleJSON{Frequency: "daily", Time: "01:30"},
	}})
	if _, err := srv.store.ImportInstances(path); err != nil {
		t.Fatalf("ImportInstances: %v", err)
	}
	enrolledRunner(t, srv, "web-01")
	runnerCert := certWithCNAndRole("web-01", crypto.RoleRunner)

	// Allowed while approved.
	if rec := resticRequest(srv, http.MethodPost, "/restic/files-web01/?create=true", nil, runnerCert); rec.Code != http.StatusOK {
		t.Fatalf("repo access while approved = %d, want 200", rec.Code)
	}

	if rec := adminPost(t, srv, "/api/v1/runners/web-01/revoke", adminCert()); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d", rec.Code)
	}

	name := strings.Repeat("ab", 32)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/restic/files-web01/data/" + name},
		{http.MethodPost, "/restic/files-web01/data/" + name},
		{http.MethodGet, "/restic/files-web01/data/"},
		{http.MethodDelete, "/restic/files-web01/data/" + name},
	} {
		rec := resticRequest(srv, tc.method, tc.path, strings.NewReader("x"), runnerCert)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s after revoke = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
}

// A revoked runner must not be able to report results either.
func TestRevokedRunnerCannotPostUpdates(t *testing.T) {
	srv, _ := enrollTestServer(t, true)
	enrolledRunner(t, srv, "web-01")
	if rec := adminPost(t, srv, "/api/v1/runners/web-01/revoke", adminCert()); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, runnerRequest(http.MethodPost, "/api/v1/runs/updates",
		`{"run_id":"run-1","kind":"finished","exit_code":0}`, "web-01"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("updates after revoke = %d, want 403", rec.Code)
	}
}

// After revocation the runner enrols again, which is what "automatically back to
// pending" means in practice.
func TestRevokedRunnerCanEnrollAgain(t *testing.T) {
	srv, _ := enrollTestServer(t, true)
	first := enrolledRunner(t, srv, "web-01")

	if rec := adminPost(t, srv, "/api/v1/runners/web-01/revoke", adminCert()); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d", rec.Code)
	}
	// Enrolling is allowed again precisely because the runner is pending, not approved.
	csrPEM, _, _ := crypto.CreateCSR("web-01")
	if rec := enroll(t, srv, "web-01", string(csrPEM)); rec.Code != http.StatusAccepted {
		t.Fatalf("re-enroll after revoke = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	if rec := adminPost(t, srv, "/api/v1/runners/web-01/approve", adminCert()); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d", rec.Code)
	}
	_, res := enrollStatus(t, srv, "web-01")
	if res.CertPEM == "" || res.CertPEM == first {
		t.Error("re-approval should issue a new, different certificate")
	}
	if rec := checkinAs(srv, "web-01"); rec.Code != http.StatusOK {
		t.Errorf("checkin after re-approval = %d, want 200", rec.Code)
	}
}

func TestRevokeAll(t *testing.T) {
	srv, _ := enrollTestServer(t, true)
	enrolledRunner(t, srv, "web-01")
	enrolledRunner(t, srv, "db-01")

	rec := adminPost(t, srv, "/api/v1/runners/revoke-all", adminCert())
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke-all = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var res struct{ Revoked int }
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Revoked != 2 {
		t.Errorf("revoked = %d, want 2", res.Revoked)
	}
	for _, id := range []string{"web-01", "db-01"} {
		if status, _ := srv.store.RunnerStatus(id); status != EnrollPending {
			t.Errorf("%s status = %q, want pending", id, status)
		}
	}
}

func TestRevokeRequiresAdmin(t *testing.T) {
	srv, _ := enrollTestServer(t, true)
	enrolledRunner(t, srv, "web-01")

	tests := []struct {
		name     string
		cert     *x509.Certificate
		wantCode int
	}{
		{"runner cannot revoke itself", certWithCNAndRole("web-01", crypto.RoleRunner), http.StatusForbidden},
		{"no certificate", nil, http.StatusUnauthorized},
		{"admin", adminCert(), http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := adminPost(t, srv, "/api/v1/runners/web-01/revoke", tc.cert)
			if rec.Code != tc.wantCode {
				t.Errorf("revoke = %d, want %d", rec.Code, tc.wantCode)
			}
		})
	}
}

func TestRevokePendingRunnerIsRejected(t *testing.T) {
	srv, _ := enrollTestServer(t, true)
	csrPEM, _, _ := crypto.CreateCSR("web-01")
	enroll(t, srv, "web-01", string(csrPEM)) // pending, nothing issued

	if rec := adminPost(t, srv, "/api/v1/runners/web-01/revoke", adminCert()); rec.Code == http.StatusOK {
		t.Error("revoking a runner that has no certificate should fail — use reject instead")
	}
}

// --- renewal ----------------------------------------------------------------

// renewAs asks for a new certificate as a runner would, over its authenticated
// connection.
func renewAs(t *testing.T, srv *Server, runnerID, csrPEM string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"csr": csrPEM})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/renew", bytes.NewReader(body))
	r.TLS = requestWithCert(certWithCNAndRole(runnerID, crypto.RoleRunner)).TLS
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	return rec
}

// Renewal needs no operator: the current certificate is the proof of identity. Without
// it every runner would stop working on the day the originals expire.
func TestRenewNeedsNoApproval(t *testing.T) {
	srv, ca := enrollTestServer(t, true)
	first := enrolledRunner(t, srv, "web-01")

	csrPEM, _, err := crypto.CreateCSR("web-01")
	if err != nil {
		t.Fatalf("CreateCSR: %v", err)
	}
	rec := renewAs(t, srv, "web-01", string(csrPEM))
	if rec.Code != http.StatusOK {
		t.Fatalf("renew = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var res EnrollResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.CertPEM == "" || res.CertPEM == first {
		t.Fatal("renewal must return a new certificate")
	}
	cert := parseCertPEM(t, res.CertPEM)
	if cert.Subject.CommonName != "web-01" || crypto.CertRole(cert) != crypto.RoleRunner {
		t.Errorf("renewed certificate identity = %s/%s", cert.Subject.CommonName, crypto.CertRole(cert))
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("renewed certificate does not chain to the CA: %v", err)
	}
	// The runner stays approved, and the recorded expiry moves forward.
	if status, _ := srv.store.RunnerStatus("web-01"); status != EnrollApproved {
		t.Errorf("status = %q, want approved", status)
	}
	runners, _ := srv.store.Runners()
	for _, r := range runners {
		if r.ID == "web-01" && r.CertNotAfter.Before(time.Now().Add(800*24*time.Hour)) {
			t.Errorf("cert_not_after = %s, want it moved out to the new expiry", r.CertNotAfter)
		}
	}
}

// A runner must not be able to obtain a certificate for a different identity.
func TestRenewRefusesForeignIdentity(t *testing.T) {
	srv, _ := enrollTestServer(t, true)
	enrolledRunner(t, srv, "web-01")
	enrolledRunner(t, srv, "db-01")

	csrPEM, _, _ := crypto.CreateCSR("db-01") // asking for db-01's identity…
	rec := renewAs(t, srv, "web-01", string(csrPEM))
	if rec.Code != http.StatusForbidden {
		t.Errorf("renew with a foreign CSR = %d, want 403", rec.Code)
	}
}

// A revoked runner must not be able to renew its way back in.
func TestRenewRefusedAfterRevoke(t *testing.T) {
	srv, _ := enrollTestServer(t, true)
	enrolledRunner(t, srv, "web-01")
	if rec := adminPost(t, srv, "/api/v1/runners/web-01/revoke", adminCert()); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d", rec.Code)
	}
	csrPEM, _, _ := crypto.CreateCSR("web-01")
	rec := renewAs(t, srv, "web-01", string(csrPEM))
	if rec.Code != http.StatusForbidden {
		t.Errorf("renew after revoke = %d, want 403", rec.Code)
	}
}

func TestRenewRejectsBadCSR(t *testing.T) {
	srv, _ := enrollTestServer(t, true)
	enrolledRunner(t, srv, "web-01")
	if rec := renewAs(t, srv, "web-01", "not a pem"); rec.Code != http.StatusBadRequest {
		t.Errorf("renew with garbage = %d, want 400", rec.Code)
	}
}

// --- identity ---------------------------------------------------------------

func TestWhoAmIReportsExpiry(t *testing.T) {
	srv, _ := enrollTestServer(t, true)
	srv.serverCertNotAfter = time.Now().Add(10 * 24 * time.Hour)

	cert := adminCert()
	cert.NotAfter = time.Now().Add(5 * 24 * time.Hour)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	r.TLS = requestWithCert(cert).TLS
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("whoami = %d, want 200", rec.Code)
	}
	var id Identity
	if err := json.Unmarshal(rec.Body.Bytes(), &id); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if id.Name != "petr" || id.Role != crypto.RoleAdmin || !id.Secured {
		t.Errorf("identity = %+v", id)
	}
	// Rounded down, so "4" for something just under 5 days is right.
	if id.DaysLeft < 4 || id.DaysLeft > 5 {
		t.Errorf("days_left = %d, want about 5", id.DaysLeft)
	}
	if id.ServerDaysLeft < 9 || id.ServerDaysLeft > 10 {
		t.Errorf("server_days_left = %d, want about 10", id.ServerDaysLeft)
	}
}

func TestWhoAmIRequiresAdmin(t *testing.T) {
	srv, _ := enrollTestServer(t, true)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	r.TLS = requestWithCert(certWithCNAndRole("web-01", crypto.RoleRunner)).TLS
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("whoami as runner = %d, want 403", rec.Code)
	}
}

// A rejected runner gets a different reason than a revoked one, so it does not keep
// filing enrollment requests nobody wants.
func TestRejectedRunnerGetsRejectedReason(t *testing.T) {
	srv, _ := enrollTestServer(t, true)
	csrPEM, _, _ := crypto.CreateCSR("web-01")
	enroll(t, srv, "web-01", string(csrPEM))
	if rec := adminPost(t, srv, "/api/v1/runners/web-01/reject", adminCert()); rec.Code != http.StatusOK {
		t.Fatalf("reject = %d", rec.Code)
	}
	rec := checkinAs(srv, "web-01")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("checkin = %d, want 403", rec.Code)
	}
	var body struct{ Reason string }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Reason != ReasonRejected {
		t.Errorf("reason = %q, want %q", body.Reason, ReasonRejected)
	}
}

func parseCertPEM(t *testing.T, certPEM string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatal("no PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert
}
