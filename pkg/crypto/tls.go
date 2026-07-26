package crypto

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// ServerTLSConfig builds a TLS config that presents the server certificate and
// *requires* a client certificate signed by the Arcatum CA. That requirement is what
// makes the authentication mutual: an unknown host cannot even complete the handshake.
func ServerTLSConfig(certPath, keyPath, caPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("server key pair: %w", err)
	}
	pool, err := LoadCAPool(caPath)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ClientTLSConfig builds a TLS config that presents the runner certificate and
// verifies the server against the Arcatum CA.
func ClientTLSConfig(certPath, keyPath, caPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("client key pair: %w", err)
	}
	pool, err := LoadCAPool(caPath)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// LoadCAPool reads a PEM bundle into a certificate pool. The file may hold several
// certificates, which is what makes a CA rotation possible: both the outgoing and the
// incoming authority are trusted during the overlap.
func LoadCAPool(caPath string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool, err := ParseCAPool(pem)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", caPath, err)
	}
	return pool, nil
}

// ParseCAPool builds a certificate pool from a PEM bundle in memory. It is used to check
// a freshly received bundle is usable *before* it replaces a working one.
func ParseCAPool(bundlePEM []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundlePEM) {
		return nil, fmt.Errorf("no certificates found")
	}
	return pool, nil
}
