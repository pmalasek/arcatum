package runner

import (
	"encoding/base64"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"arcatum/pkg/crypto"
)

// signingKeyPair returns a signer and its public PEM.
func signingKeyPair(t *testing.T) (*crypto.Ed25519Signer, []byte) {
	t.Helper()
	dir := t.TempDir()
	privPEM, pubPEM, err := crypto.GenerateSigningKey()
	if err != nil {
		t.Fatalf("GenerateSigningKey: %v", err)
	}
	path := filepath.Join(dir, "k.key")
	if err := os.WriteFile(path, privPEM, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	signer, err := crypto.LoadSigner(path)
	if err != nil {
		t.Fatalf("LoadSigner: %v", err)
	}
	return signer, pubPEM
}

// trustAgent builds an agent that currently trusts the given keys.
func trustAgent(t *testing.T, trustedPubs ...[]byte) *Agent {
	t.Helper()
	set, err := crypto.NewSigningSet(trustedPubs)
	if err != nil {
		t.Fatalf("NewSigningSet: %v", err)
	}
	return &Agent{log: log.New(io.Discard, "", 0), verifier: set}
}

// A rotation only happens if the new set is signed by a key the runner already trusts.
func TestAdoptSigningKeysAcceptsSignedRotation(t *testing.T) {
	currentSigner, currentPub := signingKeyPair(t)
	_, incomingPub := signingKeyPair(t)
	a := trustAgent(t, currentPub)

	proposed := [][]byte{incomingPub, currentPub}
	sig, err := currentSigner.Sign(crypto.SigningSetBytesToSign(proposed))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	path := filepath.Join(t.TempDir(), "signing-keys.pem")
	fetched := &trustFetch{
		SigningKeys:           []string{string(incomingPub), string(currentPub)},
		SigningKeysSignatures: []string{base64.StdEncoding.EncodeToString(sig)},
	}
	if !a.adoptSigningKeys(fetched, path) {
		t.Fatal("a set signed by the currently trusted key must be adopted")
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read stored set: %v", err)
	}
	set, err := crypto.ParseSigningSetBytes(stored)
	if err != nil {
		t.Fatalf("stored set unusable: %v", err)
	}
	if set.Len() != 2 {
		t.Errorf("stored set has %d keys, want 2", set.Len())
	}
	// Adopting the same set again is not a change, so the runner does not restart in a loop.
	if a.adoptSigningKeys(fetched, path) {
		t.Error("adopting an unchanged set must not report a change")
	}
}

// This is the attack the signature exists to stop: whoever controls the server must not be
// able to introduce their own signing key and get code executed.
func TestAdoptSigningKeysRefusesUntrustedSigner(t *testing.T) {
	_, currentPub := signingKeyPair(t)
	attackerSigner, attackerPub := signingKeyPair(t)
	a := trustAgent(t, currentPub)

	hostile := [][]byte{attackerPub}
	sig, err := attackerSigner.Sign(crypto.SigningSetBytesToSign(hostile))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	path := filepath.Join(t.TempDir(), "signing-keys.pem")
	if a.adoptSigningKeys(&trustFetch{
		SigningKeys:           []string{string(attackerPub)},
		SigningKeysSignatures: []string{base64.StdEncoding.EncodeToString(sig)},
	}, path) {
		t.Fatal("a set signed by an untrusted key must be refused")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a refused set must not be written to disk")
	}
}

func TestAdoptSigningKeysRefusesTamperedSet(t *testing.T) {
	currentSigner, currentPub := signingKeyPair(t)
	_, extraPub := signingKeyPair(t)
	a := trustAgent(t, currentPub)

	// Signature covers only the current key…
	signed := [][]byte{currentPub}
	sig, err := currentSigner.Sign(crypto.SigningSetBytesToSign(signed))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// …but an extra key is spliced into what is delivered.
	path := filepath.Join(t.TempDir(), "signing-keys.pem")
	if a.adoptSigningKeys(&trustFetch{
		SigningKeys:           []string{string(currentPub), string(extraPub)},
		SigningKeysSignatures: []string{base64.StdEncoding.EncodeToString(sig)},
	}, path) {
		t.Fatal("a set with a spliced-in key must be refused")
	}
}

func TestAdoptSigningKeysRefusesGarbage(t *testing.T) {
	_, currentPub := signingKeyPair(t)
	a := trustAgent(t, currentPub)
	path := filepath.Join(t.TempDir(), "signing-keys.pem")

	for _, tc := range []struct {
		name    string
		fetched *trustFetch
	}{
		{"no keys", &trustFetch{}},
		{"signature not base64", &trustFetch{
			SigningKeys: []string{string(currentPub)}, SigningKeysSignatures: []string{"!!!"}}},
		{"no signature", &trustFetch{SigningKeys: []string{string(currentPub)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if a.adoptSigningKeys(tc.fetched, path) {
				t.Error("must not adopt")
			}
		})
	}
}

// The CA bundle is signed for the same reason: a compromised server must not be able to
// redirect which authorities a runner trusts.
func TestAdoptCABundle(t *testing.T) {
	currentSigner, currentPub := signingKeyPair(t)
	a := trustAgent(t, currentPub)

	ca, err := crypto.CreateCA("Arcatum CA", 24*60*60*1e9)
	if err != nil {
		t.Fatalf("CreateCA: %v", err)
	}
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := ca.Save(caPath, filepath.Join(dir, "ca.key")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	bundle, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	sig, err := currentSigner.Sign(crypto.CABundleBytesToSign(bundle))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	target := filepath.Join(dir, "trust", "ca.pem")

	if !a.adoptCABundle(&trustFetch{
		CABundle:           string(bundle),
		CABundleSignatures: []string{base64.StdEncoding.EncodeToString(sig)},
	}, target) {
		t.Fatal("a signed CA bundle must be adopted")
	}
	stored, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read stored bundle: %v", err)
	}
	if _, err := crypto.ParseCAPool(stored); err != nil {
		t.Errorf("stored bundle is not usable as a trust store: %v", err)
	}
}

func TestAdoptCABundleRefusesUntrustedOrUnusable(t *testing.T) {
	_, currentPub := signingKeyPair(t)
	attackerSigner, _ := signingKeyPair(t)
	currentSigner, _ := signingKeyPair(t)
	_ = currentSigner
	a := trustAgent(t, currentPub)
	target := filepath.Join(t.TempDir(), "ca.pem")

	// Signed by someone else.
	bundle := []byte("-----BEGIN CERTIFICATE-----\nnot really\n-----END CERTIFICATE-----\n")
	sig, err := attackerSigner.Sign(crypto.CABundleBytesToSign(bundle))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if a.adoptCABundle(&trustFetch{
		CABundle:           string(bundle),
		CABundleSignatures: []string{base64.StdEncoding.EncodeToString(sig)},
	}, target) {
		t.Error("a bundle signed by an untrusted key must be refused")
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("a refused bundle must not be written")
	}
}

// A bundle that is signed correctly but unusable must not replace a working trust store —
// otherwise the runner would cut itself off from the server.
func TestAdoptCABundleRefusesUnusableBundle(t *testing.T) {
	currentSigner, currentPub := signingKeyPair(t)
	a := trustAgent(t, currentPub)

	dir := t.TempDir()
	target := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(target, []byte("EXISTING"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	bundle := []byte("this is signed but contains no certificate\n")
	sig, err := currentSigner.Sign(crypto.CABundleBytesToSign(bundle))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if a.adoptCABundle(&trustFetch{
		CABundle:           string(bundle),
		CABundleSignatures: []string{base64.StdEncoding.EncodeToString(sig)},
	}, target) {
		t.Error("an unusable bundle must be refused even when correctly signed")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "EXISTING" {
		t.Errorf("the working trust store was replaced: %q, %v", data, err)
	}
}

// A cached set takes precedence over the single bootstrap key, so a signing-key rotation
// survives a restart.
func TestLoadTrustedSigningKeysPrefersCachedSet(t *testing.T) {
	dir := t.TempDir()
	bootstrapSigner, bootstrapPub := signingKeyPair(t)
	rotatedSigner, rotatedPub := signingKeyPair(t)

	bootstrapPath := filepath.Join(dir, "dispatch-signing.pub")
	if err := os.WriteFile(bootstrapPath, bootstrapPub, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cachedPath := filepath.Join(dir, "signing-keys.pem")
	set, err := crypto.NewSigningSet([][]byte{rotatedPub, bootstrapPub})
	if err != nil {
		t.Fatalf("NewSigningSet: %v", err)
	}
	if err := os.WriteFile(cachedPath, set.Bytes(), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	v, err := LoadTrustedSigningKeys(cachedPath, bootstrapPath)
	if err != nil {
		t.Fatalf("LoadTrustedSigningKeys: %v", err)
	}
	// Both the rotated and the original key must verify.
	for name, signer := range map[string]*crypto.Ed25519Signer{
		"rotated": rotatedSigner, "bootstrap": bootstrapSigner,
	} {
		sig, err := signer.Sign([]byte("payload"))
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}
		if err := v.Verify([]byte("payload"), sig); err != nil {
			t.Errorf("%s key not accepted from the cached set: %v", name, err)
		}
	}
}

// A corrupt cache must not lock the runner out; it falls back to the bootstrap key.
func TestLoadTrustedSigningKeysFallsBackOnCorruptCache(t *testing.T) {
	dir := t.TempDir()
	signer, pub := signingKeyPair(t)
	bootstrapPath := filepath.Join(dir, "dispatch-signing.pub")
	if err := os.WriteFile(bootstrapPath, pub, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cachedPath := filepath.Join(dir, "signing-keys.pem")
	if err := os.WriteFile(cachedPath, []byte("garbage"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	v, err := LoadTrustedSigningKeys(cachedPath, bootstrapPath)
	if err != nil {
		t.Fatalf("LoadTrustedSigningKeys: %v", err)
	}
	sig, _ := signer.Sign([]byte("payload"))
	if err := v.Verify([]byte("payload"), sig); err != nil {
		t.Errorf("fallback to the bootstrap key failed: %v", err)
	}
}

// Development mode has no verifier, so there is nothing to check a change against and
// nothing is adopted.
func TestRefreshTrustSkippedWithoutVerifier(t *testing.T) {
	a := &Agent{log: log.New(io.Discard, "", 0)}
	if a.RefreshTrust(nil, TrustPaths{SigningKeys: "x", CACert: "y"}) {
		t.Error("without a verifier nothing must be adopted")
	}
}

var _ = io.Discard

// The rotation has to work for a runner that is still on the *old* key. The server
// therefore co-signs the published set with every key it holds, and the runner accepts it
// on the strength of whichever signature it can check.
//
// Signing only with the incoming key would deadlock the rotation: no runner would ever
// accept the set that introduces it. (This is what an end-to-end run caught.)
func TestAdoptSigningKeysWorksForRunnerOnOldKey(t *testing.T) {
	oldSigner, oldPub := signingKeyPair(t)
	newSigner, newPub := signingKeyPair(t)

	// This runner only knows the old key.
	a := trustAgent(t, oldPub)

	proposed := [][]byte{newPub, oldPub}
	toSign := crypto.SigningSetBytesToSign(proposed)
	newSig, err := newSigner.Sign(toSign)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	oldSig, err := oldSigner.Sign(toSign)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	keys := []string{string(newPub), string(oldPub)}

	// Signed only by the incoming key: the runner cannot verify it, so it must refuse.
	pathA := filepath.Join(t.TempDir(), "a.pem")
	if a.adoptSigningKeys(&trustFetch{
		SigningKeys:           keys,
		SigningKeysSignatures: []string{base64.StdEncoding.EncodeToString(newSig)},
	}, pathA) {
		t.Error("a set signed only with the incoming key must not be adopted")
	}

	// Co-signed with the old key as well: now the runner can verify and adopt it.
	pathB := filepath.Join(t.TempDir(), "b.pem")
	if !a.adoptSigningKeys(&trustFetch{
		SigningKeys: keys,
		SigningKeysSignatures: []string{
			base64.StdEncoding.EncodeToString(newSig),
			base64.StdEncoding.EncodeToString(oldSig),
		},
	}, pathB) {
		t.Fatal("a set co-signed with the key this runner trusts must be adopted")
	}
	stored, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	set, err := crypto.ParseSigningSetBytes(stored)
	if err != nil {
		t.Fatalf("stored set unusable: %v", err)
	}
	// Having adopted it, the runner now accepts dispatches signed with the new key.
	sig, err := newSigner.Sign([]byte("dispatch"))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := set.Verify([]byte("dispatch"), sig); err != nil {
		t.Errorf("after adopting the set, the new key's dispatches must verify: %v", err)
	}
}
