package crypto

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"path/filepath"
	"testing"
	"time"
)

const testValidity = 24 * time.Hour

func newTestCA(t *testing.T) *CA {
	t.Helper()
	ca, err := CreateCA("Test CA", testValidity)
	if err != nil {
		t.Fatalf("CreateCA: %v", err)
	}
	return ca
}

func parseCert(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("no PEM block in certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert
}

// verifyAgainst checks a leaf chains up to the CA for the given usage.
func verifyAgainst(t *testing.T, ca *CA, leaf *x509.Certificate, usage x509.ExtKeyUsage) error {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	_, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{usage}})
	return err
}

func TestCreateCAIsUsableAuthority(t *testing.T) {
	ca := newTestCA(t)
	if !ca.Cert.IsCA {
		t.Error("CA certificate must have IsCA set")
	}
	if ca.Cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("CA certificate must be allowed to sign certificates")
	}
	// NotBefore is backdated so a slightly slow clock on a runner still accepts it.
	if !ca.Cert.NotBefore.Before(time.Now()) {
		t.Error("NotBefore should be backdated for clock skew")
	}
}

func TestIssueServerCertChainsAndCoversHosts(t *testing.T) {
	ca := newTestCA(t)
	certPEM, keyPEM, err := ca.IssueServer("arcatum-server", []string{"arcatum.xtuning.local", "172.24.0.60"}, testValidity)
	if err != nil {
		t.Fatalf("IssueServer: %v", err)
	}
	if len(keyPEM) == 0 {
		t.Error("no private key returned")
	}
	cert := parseCert(t, certPEM)

	if err := verifyAgainst(t, ca, cert, x509.ExtKeyUsageServerAuth); err != nil {
		t.Errorf("server certificate does not chain to the CA: %v", err)
	}
	// A hostname and an IP must land in the right SAN field, or TLS verification fails.
	if err := cert.VerifyHostname("arcatum.xtuning.local"); err != nil {
		t.Errorf("VerifyHostname(dns): %v", err)
	}
	if err := cert.VerifyHostname("172.24.0.60"); err != nil {
		t.Errorf("VerifyHostname(ip): %v", err)
	}
	if len(cert.IPAddresses) != 1 || !cert.IPAddresses[0].Equal(net.ParseIP("172.24.0.60")) {
		t.Errorf("IP SAN = %v, want 172.24.0.60", cert.IPAddresses)
	}
	if err := cert.VerifyHostname("evil.example.com"); err == nil {
		t.Error("certificate must not be valid for an unlisted host")
	}
}

func TestIssueRunnerAndAdminCarryRoles(t *testing.T) {
	ca := newTestCA(t)

	runnerPEM, _, err := ca.IssueRunner("web-01", testValidity)
	if err != nil {
		t.Fatalf("IssueRunner: %v", err)
	}
	runnerCert := parseCert(t, runnerPEM)
	if got := runnerCert.Subject.CommonName; got != "web-01" {
		t.Errorf("runner CN = %q, want web-01 (the server trusts this as the runner id)", got)
	}
	if got := CertRole(runnerCert); got != RoleRunner {
		t.Errorf("runner role = %q, want %q", got, RoleRunner)
	}
	if err := verifyAgainst(t, ca, runnerCert, x509.ExtKeyUsageClientAuth); err != nil {
		t.Errorf("runner certificate does not chain to the CA: %v", err)
	}

	adminPEM, _, err := ca.IssueAdmin("petr", testValidity)
	if err != nil {
		t.Fatalf("IssueAdmin: %v", err)
	}
	adminCert := parseCert(t, adminPEM)
	if got := CertRole(adminCert); got != RoleAdmin {
		t.Errorf("admin role = %q, want %q", got, RoleAdmin)
	}

	// Roles must be distinguishable: a runner certificate must never read as admin.
	if CertRole(runnerCert) == CertRole(adminCert) {
		t.Error("runner and admin certificates must carry different roles")
	}
}

func TestRunnerCertIsNotAcceptedAsServerCert(t *testing.T) {
	ca := newTestCA(t)
	runnerPEM, _, err := ca.IssueRunner("web-01", testValidity)
	if err != nil {
		t.Fatalf("IssueRunner: %v", err)
	}
	cert := parseCert(t, runnerPEM)
	if err := verifyAgainst(t, ca, cert, x509.ExtKeyUsageServerAuth); err == nil {
		t.Error("a client certificate must not be usable for server authentication")
	}
}

func TestCertFromDifferentCADoesNotVerify(t *testing.T) {
	ca := newTestCA(t)
	other, err := CreateCA("Other CA", testValidity)
	if err != nil {
		t.Fatalf("CreateCA: %v", err)
	}
	certPEM, _, err := other.IssueRunner("web-01", testValidity)
	if err != nil {
		t.Fatalf("IssueRunner: %v", err)
	}
	if err := verifyAgainst(t, ca, parseCert(t, certPEM), x509.ExtKeyUsageClientAuth); err == nil {
		t.Error("a certificate from a foreign CA must be rejected")
	}
}

func TestSaveAndLoadCARoundTrip(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca.key")

	ca := newTestCA(t)
	if err := ca.Save(certPath, keyPath); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := LoadCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	if !loaded.Cert.Equal(ca.Cert) {
		t.Error("loaded CA certificate differs from the saved one")
	}
	// The reloaded CA must still be able to issue certificates that chain to it.
	certPEM, _, err := loaded.IssueRunner("web-01", testValidity)
	if err != nil {
		t.Fatalf("IssueRunner after load: %v", err)
	}
	if err := verifyAgainst(t, loaded, parseCert(t, certPEM), x509.ExtKeyUsageClientAuth); err != nil {
		t.Errorf("certificate from reloaded CA does not verify: %v", err)
	}
}

func TestLoadCARejectsNonCACertificate(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t)
	certPEM, keyPEM, err := ca.IssueRunner("web-01", testValidity)
	if err != nil {
		t.Fatalf("IssueRunner: %v", err)
	}
	certPath := filepath.Join(dir, "leaf.pem")
	keyPath := filepath.Join(dir, "leaf.key")
	writeFile(t, certPath, certPEM)
	writeFile(t, keyPath, keyPEM)

	if _, err := LoadCA(certPath, keyPath); err == nil {
		t.Error("LoadCA must reject a certificate that is not a CA")
	}
}

func TestSignCSRProducesRunnerCert(t *testing.T) {
	ca := newTestCA(t)
	csrPEM, keyPEM, err := CreateCSR("db-01")
	if err != nil {
		t.Fatalf("CreateCSR: %v", err)
	}
	if len(keyPEM) == 0 {
		t.Error("CSR generation must return the private key, which stays on the runner")
	}
	certPEM, err := ca.SignCSR(csrPEM, testValidity)
	if err != nil {
		t.Fatalf("SignCSR: %v", err)
	}
	cert := parseCert(t, certPEM)
	if cert.Subject.CommonName != "db-01" {
		t.Errorf("CN = %q, want db-01", cert.Subject.CommonName)
	}
	if got := CertRole(cert); got != RoleRunner {
		t.Errorf("role = %q, want %q", got, RoleRunner)
	}
	if err := verifyAgainst(t, ca, cert, x509.ExtKeyUsageClientAuth); err != nil {
		t.Errorf("signed CSR does not chain to the CA: %v", err)
	}
}

func TestSignCSRRejectsGarbage(t *testing.T) {
	ca := newTestCA(t)
	if _, err := ca.SignCSR([]byte("not a pem"), testValidity); err == nil {
		t.Error("SignCSR must reject non-PEM input")
	}
}

func TestCertRoleEmptyWhenAbsent(t *testing.T) {
	ca := newTestCA(t)
	certPEM, _, err := ca.IssueServer("arcatum-server", []string{"localhost"}, testValidity)
	if err != nil {
		t.Fatalf("IssueServer: %v", err)
	}
	if got := CertRole(parseCert(t, certPEM)); got != "" {
		t.Errorf("CertRole = %q, want empty for a server certificate", got)
	}
}
