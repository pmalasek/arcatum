package crypto

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
)

// newKeyPair writes a signing keypair and returns the signer plus its public PEM.
func newKeyPair(t *testing.T, dir, name string) (*Ed25519Signer, []byte) {
	t.Helper()
	privPEM, pubPEM, err := GenerateSigningKey()
	if err != nil {
		t.Fatalf("GenerateSigningKey: %v", err)
	}
	privPath := filepath.Join(dir, name+".key")
	writeFile(t, privPath, privPEM)
	writeFile(t, filepath.Join(dir, name+".pub"), pubPEM)
	signer, err := LoadSigner(privPath)
	if err != nil {
		t.Fatalf("LoadSigner: %v", err)
	}
	return signer, pubPEM
}

// During a rotation both keys are trusted, so dispatches signed with either are accepted.
func TestSigningSetAcceptsAnyKey(t *testing.T) {
	dir := t.TempDir()
	oldSigner, oldPub := newKeyPair(t, dir, "old")
	newSigner, newPub := newKeyPair(t, dir, "new")

	set, err := NewSigningSet([][]byte{newPub, oldPub})
	if err != nil {
		t.Fatalf("NewSigningSet: %v", err)
	}
	if set.Len() != 2 {
		t.Fatalf("set has %d keys, want 2", set.Len())
	}
	data := []byte("job dispatch")
	for name, signer := range map[string]*Ed25519Signer{"old": oldSigner, "new": newSigner} {
		sig, err := signer.Sign(data)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}
		if err := set.Verify(data, sig); err != nil {
			t.Errorf("%s key's signature rejected: %v", name, err)
		}
	}

	// A key outside the set must still be refused.
	strangerSigner, _ := newKeyPair(t, dir, "stranger")
	sig, _ := strangerSigner.Sign(data)
	if err := set.Verify(data, sig); err == nil {
		t.Error("a signature from a key outside the set must be refused")
	}
}

// Dropping the old key from the set must stop accepting its signatures — that is what
// completes the rotation.
func TestSigningSetAfterDroppingOldKey(t *testing.T) {
	dir := t.TempDir()
	oldSigner, _ := newKeyPair(t, dir, "old")
	_, newPub := newKeyPair(t, dir, "new")

	set, err := NewSigningSet([][]byte{newPub})
	if err != nil {
		t.Fatalf("NewSigningSet: %v", err)
	}
	sig, _ := oldSigner.Sign([]byte("payload"))
	if err := set.Verify([]byte("payload"), sig); err == nil {
		t.Error("after the old key is dropped its signatures must be refused")
	}
}

func TestSigningSetRoundTripThroughBytes(t *testing.T) {
	dir := t.TempDir()
	signer, pub := newKeyPair(t, dir, "k1")
	_, pub2 := newKeyPair(t, dir, "k2")

	set, err := NewSigningSet([][]byte{pub, pub2})
	if err != nil {
		t.Fatalf("NewSigningSet: %v", err)
	}
	restored, err := ParseSigningSetBytes(set.Bytes())
	if err != nil {
		t.Fatalf("ParseSigningSetBytes: %v", err)
	}
	if restored.Len() != 2 {
		t.Errorf("restored set has %d keys, want 2", restored.Len())
	}
	sig, _ := signer.Sign([]byte("payload"))
	if err := restored.Verify([]byte("payload"), sig); err != nil {
		t.Errorf("restored set does not verify: %v", err)
	}
}

func TestSigningSetDeduplicatesAndRejectsJunk(t *testing.T) {
	dir := t.TempDir()
	_, pub := newKeyPair(t, dir, "k1")

	set, err := NewSigningSet([][]byte{pub, pub})
	if err != nil {
		t.Fatalf("NewSigningSet: %v", err)
	}
	if set.Len() != 1 {
		t.Errorf("set has %d keys, want 1 after dedup", set.Len())
	}
	if _, err := NewSigningSet([][]byte{[]byte("not a pem")}); err == nil {
		t.Error("NewSigningSet must reject junk")
	}
	if _, err := NewSigningSet(nil); err == nil {
		t.Error("an empty set must be refused")
	}
	if _, err := ParseSigningSetBytes([]byte("nothing here")); err == nil {
		t.Error("ParseSigningSetBytes must reject input with no PEM blocks")
	}
}

// The published set is signed so only the holder of a trusted key can change it. The
// canonical form must therefore be order-independent but sensitive to membership.
func TestSigningSetBytesToSign(t *testing.T) {
	dir := t.TempDir()
	_, a := newKeyPair(t, dir, "a")
	_, b := newKeyPair(t, dir, "b")

	ab := SigningSetBytesToSign([][]byte{a, b})
	ba := SigningSetBytesToSign([][]byte{b, a})
	if string(ab) != string(ba) {
		t.Error("the signed form must not depend on the order keys are listed in")
	}
	justA := SigningSetBytesToSign([][]byte{a})
	if string(ab) == string(justA) {
		t.Error("removing a key must change the signed form, or a key could be stripped")
	}
	_, c := newKeyPair(t, dir, "c")
	abc := SigningSetBytesToSign([][]byte{a, b, c})
	if string(ab) == string(abc) {
		t.Error("adding a key must change the signed form, or a key could be spliced in")
	}
}

// A signed set is only adopted if a currently-trusted key signed it. This is what stops
// a compromised server from introducing its own signing key.
func TestSigningSetSignatureAuthorisesRotation(t *testing.T) {
	dir := t.TempDir()
	currentSigner, currentPub := newKeyPair(t, dir, "current")
	_, incomingPub := newKeyPair(t, dir, "incoming")
	attackerSigner, attackerPub := newKeyPair(t, dir, "attacker")

	trusted, err := NewSigningSet([][]byte{currentPub})
	if err != nil {
		t.Fatalf("NewSigningSet: %v", err)
	}

	// Legitimate rotation: signed with the key being replaced.
	proposed := [][]byte{incomingPub, currentPub}
	sig, err := currentSigner.Sign(SigningSetBytesToSign(proposed))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := trusted.Verify(SigningSetBytesToSign(proposed), sig); err != nil {
		t.Errorf("a set signed by the current key must be accepted: %v", err)
	}

	// Attacker's set, signed with their own key: must not be accepted.
	hostile := [][]byte{attackerPub}
	badSig, _ := attackerSigner.Sign(SigningSetBytesToSign(hostile))
	if err := trusted.Verify(SigningSetBytesToSign(hostile), badSig); err == nil {
		t.Error("a set signed by an untrusted key must be refused")
	}
}

func TestCABundleBytesToSign(t *testing.T) {
	a := CABundleBytesToSign([]byte("CA-A"))
	b := CABundleBytesToSign([]byte("CA-B"))
	if string(a) == string(b) {
		t.Error("different bundles must produce different signed forms")
	}
	if string(a) != string(CABundleBytesToSign([]byte("CA-A"))) {
		t.Error("the signed form must be deterministic")
	}
	// Length-prefixed, so concatenation cannot be rearranged undetected.
	ab := CABundleBytesToSign([]byte("CA-ACA-B"))
	if string(ab) == string(a)+"CA-B" {
		t.Error("the encoding must be length-prefixed, not plain concatenation")
	}
}

func TestParseCAPoolRejectsJunk(t *testing.T) {
	if _, err := ParseCAPool([]byte("not a certificate")); err == nil {
		t.Error("ParseCAPool must reject input with no certificates")
	}
}

// A bundle holding two authorities must be usable as a trust store — that is the whole
// mechanism behind a CA rotation.
func TestParseCAPoolAcceptsMultipleAuthorities(t *testing.T) {
	first, err := CreateCA("Arcatum CA", testValidity)
	if err != nil {
		t.Fatalf("CreateCA: %v", err)
	}
	second, err := CreateCA("Arcatum CA 2", testValidity)
	if err != nil {
		t.Fatalf("CreateCA: %v", err)
	}
	dir := t.TempDir()
	firstPath := filepath.Join(dir, "ca1.pem")
	secondPath := filepath.Join(dir, "ca2.pem")
	if err := first.Save(firstPath, filepath.Join(dir, "ca1.key")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := second.Save(secondPath, filepath.Join(dir, "ca2.key")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	bundle := append(readFileT(t, firstPath), readFileT(t, secondPath)...)

	pool, err := ParseCAPool(bundle)
	if err != nil {
		t.Fatalf("ParseCAPool: %v", err)
	}
	// A certificate from either authority must verify against the bundle.
	for name, ca := range map[string]*CA{"first": first, "second": second} {
		certPEM, _, err := ca.IssueRunner("web-01", testValidity)
		if err != nil {
			t.Fatalf("IssueRunner: %v", err)
		}
		leaf := parseCert(t, certPEM)
		if _, err := leaf.Verify(x509VerifyOpts(pool)); err != nil {
			t.Errorf("certificate from the %s authority does not verify against the bundle: %v", name, err)
		}
	}
}

func readFileT(t *testing.T, path string) []byte {
	t.Helper()
	return []byte(readTestFileContents(t, path))
}

func readTestFileContents(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// x509VerifyOpts builds verify options for a client certificate against a pool.
func x509VerifyOpts(pool *x509.CertPool) x509.VerifyOptions {
	return x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
}
