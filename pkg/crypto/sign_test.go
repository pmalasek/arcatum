package crypto

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newTestKeypair writes a signing keypair to disk and returns loaded signer/verifier.
func newTestKeypair(t *testing.T) (*Ed25519Signer, *Ed25519Verifier) {
	t.Helper()
	dir := t.TempDir()
	privPEM, pubPEM, err := GenerateSigningKey()
	if err != nil {
		t.Fatalf("GenerateSigningKey: %v", err)
	}
	privPath := filepath.Join(dir, "signing.key")
	pubPath := filepath.Join(dir, "signing.pub")
	writeFile(t, privPath, privPEM)
	writeFile(t, pubPath, pubPEM)

	signer, err := LoadSigner(privPath)
	if err != nil {
		t.Fatalf("LoadSigner: %v", err)
	}
	verifier, err := LoadVerifier(pubPath)
	if err != nil {
		t.Fatalf("LoadVerifier: %v", err)
	}
	return signer, verifier
}

func TestSignVerifyRoundTrip(t *testing.T) {
	signer, verifier := newTestKeypair(t)
	data := []byte("job dispatch bytes")

	sig, err := signer.Sign(data)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := verifier.Verify(data, sig); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestVerifyRejectsTamperedData(t *testing.T) {
	signer, verifier := newTestKeypair(t)
	data := []byte("run this script")
	sig, err := signer.Sign(data)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := verifier.Verify([]byte("run that script"), sig); !errors.Is(err, ErrBadSignature) {
		t.Errorf("Verify(tampered data) = %v, want ErrBadSignature", err)
	}
}

func TestVerifyRejectsTamperedSignature(t *testing.T) {
	signer, verifier := newTestKeypair(t)
	data := []byte("payload")
	sig, err := signer.Sign(data)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	sig[0] ^= 0xff
	if err := verifier.Verify(data, sig); !errors.Is(err, ErrBadSignature) {
		t.Errorf("Verify(tampered signature) = %v, want ErrBadSignature", err)
	}
}

func TestVerifyRejectsEmptySignature(t *testing.T) {
	_, verifier := newTestKeypair(t)
	if err := verifier.Verify([]byte("payload"), nil); !errors.Is(err, ErrBadSignature) {
		t.Errorf("Verify(no signature) = %v, want ErrBadSignature", err)
	}
}

// A signature from another server's key must not pass — this is what stops an
// impostor from handing a runner arbitrary code.
func TestVerifyRejectsForeignKey(t *testing.T) {
	signer, _ := newTestKeypair(t)
	_, otherVerifier := newTestKeypair(t)
	data := []byte("payload")
	sig, err := signer.Sign(data)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := otherVerifier.Verify(data, sig); !errors.Is(err, ErrBadSignature) {
		t.Errorf("Verify(foreign key) = %v, want ErrBadSignature", err)
	}
}

func TestSignerPublicMatchesVerifier(t *testing.T) {
	signer, _ := newTestKeypair(t)
	pubPEM, err := signer.Public()
	if err != nil {
		t.Fatalf("Public: %v", err)
	}
	dir := t.TempDir()
	pubPath := filepath.Join(dir, "derived.pub")
	writeFile(t, pubPath, pubPEM)

	verifier, err := LoadVerifier(pubPath)
	if err != nil {
		t.Fatalf("LoadVerifier: %v", err)
	}
	data := []byte("payload")
	sig, err := signer.Sign(data)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := verifier.Verify(data, sig); err != nil {
		t.Errorf("public key derived from signer does not verify its own signature: %v", err)
	}
}

func TestLoadSignerRejectsWrongKeyType(t *testing.T) {
	dir := t.TempDir()
	// An EC key is valid PEM but not an Ed25519 signing key.
	ca, err := CreateCA("Test CA", testValidity)
	if err != nil {
		t.Fatalf("CreateCA: %v", err)
	}
	keyPath := filepath.Join(dir, "ca.key")
	if err := ca.Save(filepath.Join(dir, "ca.pem"), keyPath); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := LoadSigner(keyPath); err == nil {
		t.Error("LoadSigner must reject a non-Ed25519 key")
	}
}

func TestLoadVerifierRejectsMissingFile(t *testing.T) {
	if _, err := LoadVerifier(filepath.Join(t.TempDir(), "absent.pub")); err == nil {
		t.Error("LoadVerifier must fail on a missing file")
	}
}
