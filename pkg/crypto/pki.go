package crypto

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Roles carried in a certificate's Organizational Unit. The server uses them to
// separate "a runner asking for work" from "an operator driving the API".
const (
	RoleRunner = "runner"
	RoleAdmin  = "admin"
)

// CA is the Arcatum certificate authority: it issues the server's certificate and
// one client certificate per runner, so both sides can authenticate each other.
//
// Keys are ECDSA P-256 (not Ed25519) because browsers must also be able to open the
// web UI served with the same certificate.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
}

// CreateCA generates a new self-signed CA.
func CreateCA(commonName string, validity time.Duration) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"Arcatum"}},
		NotBefore:             now.Add(-5 * time.Minute), // tolerate small clock skew
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("self-sign CA: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key}, nil
}

// LoadCA reads a CA certificate and its private key from PEM files.
func LoadCA(certPath, keyPath string) (*CA, error) {
	cert, err := loadCertPEM(certPath)
	if err != nil {
		return nil, err
	}
	key, err := loadECKeyPEM(keyPath)
	if err != nil {
		return nil, err
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("%s is not a CA certificate", certPath)
	}
	return &CA{Cert: cert, Key: key}, nil
}

// Save writes the CA certificate and key as PEM. The key is written 0600.
func (ca *CA) Save(certPath, keyPath string) error {
	if err := writePEMFile(certPath, "CERTIFICATE", ca.Cert.Raw, 0o644); err != nil {
		return err
	}
	return writeECKey(keyPath, ca.Key)
}

// IssueServer issues a server certificate valid for the given DNS names and IPs.
func (ca *CA) IssueServer(commonName string, hosts []string, validity time.Duration) (certPEM, keyPEM []byte, err error) {
	dnsNames, ips := splitHosts(hosts)
	return ca.issue(commonName, nil, validity, x509.ExtKeyUsageServerAuth, dnsNames, ips)
}

// IssueRunner issues a client certificate identifying one runner. The common name is
// the runner id the server will trust — it must match the instance's runner_id.
func (ca *CA) IssueRunner(runnerID string, validity time.Duration) (certPEM, keyPEM []byte, err error) {
	return ca.issue(runnerID, []string{RoleRunner}, validity, x509.ExtKeyUsageClientAuth, nil, nil)
}

// IssueAdmin issues a client certificate for an operator driving the API/web UI.
func (ca *CA) IssueAdmin(name string, validity time.Duration) (certPEM, keyPEM []byte, err error) {
	return ca.issue(name, []string{RoleAdmin}, validity, x509.ExtKeyUsageClientAuth, nil, nil)
}

func (ca *CA) issue(commonName string, ous []string, validity time.Duration,
	usage x509.ExtKeyUsage, dnsNames []string, ips []net.IP) (certPEM, keyPEM []byte, err error) {

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:         commonName,
			OrganizationalUnit: ous,
			Organization:       []string{"Arcatum"},
		},
		NotBefore:   now.Add(-5 * time.Minute),
		NotAfter:    now.Add(validity),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{usage},
		DNSNames:    dnsNames,
		IPAddresses: ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return nil, nil, fmt.Errorf("sign certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// SignCSR signs a certificate request, producing a runner client certificate. This is
// the building block for enrollment, where the runner keeps its private key and only
// sends a CSR.
func (ca *CA) SignCSR(csrPEM []byte, validity time.Duration) (certPEM []byte, err error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in CSR")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("CSR signature: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:         csr.Subject.CommonName,
			OrganizationalUnit: []string{RoleRunner},
			Organization:       []string{"Arcatum"},
		},
		NotBefore:   now.Add(-5 * time.Minute),
		NotAfter:    now.Add(validity),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.Key)
	if err != nil {
		return nil, fmt.Errorf("sign CSR: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// CreateCSR generates a private key and a certificate request for a runner id. The
// key never leaves the host; only the CSR is sent to the server.
func CreateCSR(runnerID string) (csrPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: runnerID}}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create CSR: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	csrPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return csrPEM, keyPEM, nil
}

// CertRole reports the role carried in a certificate's OU, or "" if none.
func CertRole(cert *x509.Certificate) string {
	for _, ou := range cert.Subject.OrganizationalUnit {
		if ou == RoleRunner || ou == RoleAdmin {
			return ou
		}
	}
	return ""
}

// --- helpers ----------------------------------------------------------------

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("serial: %w", err)
	}
	return n, nil
}

// splitHosts sorts host entries into DNS names and IP addresses.
func splitHosts(hosts []string) ([]string, []net.IP) {
	var dnsNames []string
	var ips []net.IP
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip)
			continue
		}
		dnsNames = append(dnsNames, h)
	}
	return dnsNames, ips
}

func writePEMFile(path, blockType string, der []byte, mode os.FileMode) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	return os.WriteFile(path, data, mode)
}

func writeECKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return writePEMFile(path, "EC PRIVATE KEY", der, 0o600)
}

func loadCertPEM(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	return x509.ParseCertificate(block.Bytes)
}

func loadECKeyPEM(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	// Also accept PKCS#8, which some tools emit.
	any, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: parse EC key: %w", path, err)
	}
	key, ok := any.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an ECDSA key", path)
	}
	return key, nil
}
