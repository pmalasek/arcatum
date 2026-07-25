package crypto

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
)

// ErrBadSignature means the data was not signed by the expected key — the runner
// must refuse to execute such a dispatch.
var ErrBadSignature = errors.New("invalid signature")

// Dispatch signing uses Ed25519: small signatures, fast verification, no parameter
// choices to get wrong. It is deliberately separate from the TLS/PKI keys — mTLS
// proves who the peer is on the wire, the signature proves the *job* came from
// Arcatum and was not altered in transit or at rest.
type Ed25519Signer struct{ key ed25519.PrivateKey }

// Ed25519Verifier verifies dispatch signatures against the server's public key.
type Ed25519Verifier struct{ key ed25519.PublicKey }

// GenerateSigningKey creates a new dispatch-signing keypair as PEM.
func GenerateSigningKey() (privPEM, pubPEM []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, nil, err
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, nil, err
	}
	privPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})
	pubPEM = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	return privPEM, pubPEM, nil
}

// LoadSigner reads a dispatch-signing private key (server side).
func LoadSigner(path string) (*Ed25519Signer, error) {
	block, err := readPEM(path)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: parse signing key: %w", path, err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an Ed25519 private key", path)
	}
	return &Ed25519Signer{key: key}, nil
}

// LoadVerifier reads the server's dispatch-signing public key (runner side).
func LoadVerifier(path string) (*Ed25519Verifier, error) {
	block, err := readPEM(path)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: parse public key: %w", path, err)
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an Ed25519 public key", path)
	}
	return &Ed25519Verifier{key: key}, nil
}

// Sign signs data with the server's signing key.
func (s *Ed25519Signer) Sign(data []byte) ([]byte, error) {
	return ed25519.Sign(s.key, data), nil
}

// Public returns the matching public key as PEM, handy for provisioning runners.
func (s *Ed25519Signer) Public() ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(s.key.Public())
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// Verify checks a signature, returning ErrBadSignature when it does not match.
func (v *Ed25519Verifier) Verify(data, sig []byte) error {
	if !ed25519.Verify(v.key, data, sig) {
		return ErrBadSignature
	}
	return nil
}

func readPEM(path string) (*pem.Block, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	return block, nil
}
