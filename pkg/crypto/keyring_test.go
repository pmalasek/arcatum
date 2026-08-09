package crypto

import (
	"path/filepath"
	"strings"
	"testing"
)

// writeMasterKeyFile creates a fresh master key file and returns its path.
func writeMasterKeyFile(t *testing.T, dir, name string) string {
	t.Helper()
	key, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	path := filepath.Join(dir, name)
	writeFile(t, path, key)
	return path
}

func TestKeyringRoundTrip(t *testing.T) {
	dir := t.TempDir()
	kr, err := LoadKeyring(writeMasterKeyFile(t, dir, "primary.key"), nil)
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	sealed, err := kr.SealString("hunter2", "mysql-web01", "password")
	if err != nil {
		t.Fatalf("SealString: %v", err)
	}
	if strings.Contains(sealed, "hunter2") {
		t.Error("stored value must not contain the plaintext")
	}
	// The key id is stored with the value so decryption picks the right key directly.
	if !strings.HasPrefix(sealed, "enc:v2:"+kr.PrimaryID()+":") {
		t.Errorf("stored value %q does not carry the primary key id", sealed)
	}
	got, err := kr.OpenString(sealed, "mysql-web01", "password")
	if err != nil || got != "hunter2" {
		t.Fatalf("OpenString = %q, %v; want hunter2", got, err)
	}
}

// The point of a keyring: after adding a new key, values written under the old one are
// still readable, and new values use the new key.
func TestKeyringReadsPreviousKey(t *testing.T) {
	dir := t.TempDir()
	oldPath := writeMasterKeyFile(t, dir, "old.key")
	newPath := writeMasterKeyFile(t, dir, "new.key")

	oldOnly, err := LoadKeyring(oldPath, nil)
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	sealedOld, err := oldOnly.SealString("old-secret", "i", "password")
	if err != nil {
		t.Fatalf("SealString: %v", err)
	}

	// Rotation: new key is primary, old one stays available.
	rotated, err := LoadKeyring(newPath, []string{oldPath})
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if rotated.Len() != 2 {
		t.Fatalf("keyring has %d keys, want 2", rotated.Len())
	}
	got, err := rotated.OpenString(sealedOld, "i", "password")
	if err != nil || got != "old-secret" {
		t.Fatalf("value sealed with the old key is unreadable: %q, %v", got, err)
	}
	// It is not yet on the current key, which is what tells a rekey pass to do work.
	if rotated.IsOnPrimary(sealedOld) {
		t.Error("a value sealed with the old key must not report as current")
	}
	// New values go to the new key.
	sealedNew, err := rotated.SealString("new-secret", "i", "password")
	if err != nil {
		t.Fatalf("SealString: %v", err)
	}
	if !rotated.IsOnPrimary(sealedNew) {
		t.Error("a freshly sealed value must be on the primary key")
	}
	if !strings.Contains(sealedNew, rotated.PrimaryID()) {
		t.Error("freshly sealed value should carry the new key id")
	}
}

// Once the old key is dropped from the config, values still sealed with it must produce a
// clear error naming the missing key rather than a vague failure.
func TestKeyringMissingKeyIsExplained(t *testing.T) {
	dir := t.TempDir()
	oldPath := writeMasterKeyFile(t, dir, "old.key")
	newPath := writeMasterKeyFile(t, dir, "new.key")

	oldOnly, _ := LoadKeyring(oldPath, nil)
	sealedOld, err := oldOnly.SealString("old-secret", "i", "password")
	if err != nil {
		t.Fatalf("SealString: %v", err)
	}
	newOnly, _ := LoadKeyring(newPath, nil)

	_, err = newOnly.OpenString(sealedOld, "i", "password")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "previous_keys") {
		t.Errorf("error %q should point at the fix (previous_keys)", err)
	}
	if !strings.Contains(err.Error(), oldOnly.PrimaryID()) {
		t.Errorf("error %q should name the key the value was sealed with", err)
	}
}

// Values written before key ids existed carry no id, so every key is tried for them.
func TestKeyringReadsLegacyV1Values(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeMasterKeyFile(t, dir, "master.key")

	// Produce a v1 value with the single-key box, as an older server would have.
	box, err := LoadSecretBox(keyPath)
	if err != nil {
		t.Fatalf("LoadSecretBox: %v", err)
	}
	v1, err := SealToString(box, "legacy", "i", "password")
	if err != nil {
		t.Fatalf("SealToString: %v", err)
	}
	if !strings.HasPrefix(v1, "enc:v1:") {
		t.Fatalf("expected a v1 value, got %q", v1)
	}

	kr, err := LoadKeyring(keyPath, nil)
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	got, err := kr.OpenString(v1, "i", "password")
	if err != nil || got != "legacy" {
		t.Fatalf("v1 value unreadable through the keyring: %q, %v", got, err)
	}
	// It is not on the current format, so a rekey pass will upgrade it.
	if kr.IsOnPrimary(v1) {
		t.Error("a v1 value must not report as being on the primary key")
	}
}

func TestKeyringPlaintextPassthrough(t *testing.T) {
	dir := t.TempDir()
	kr, _ := LoadKeyring(writeMasterKeyFile(t, dir, "master.key"), nil)
	got, err := kr.OpenString("just-a-password", "i", "password")
	if err != nil || got != "just-a-password" {
		t.Errorf("plaintext passthrough = %q, %v", got, err)
	}
	if kr.IsOnPrimary("just-a-password") {
		t.Error("plaintext must not report as encrypted with the primary key")
	}
}

// Listing the same key twice must not create a duplicate entry.
func TestKeyringDeduplicates(t *testing.T) {
	dir := t.TempDir()
	path := writeMasterKeyFile(t, dir, "master.key")
	kr, err := LoadKeyring(path, []string{path})
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if kr.Len() != 1 {
		t.Errorf("keyring has %d keys, want 1", kr.Len())
	}
}

// The context binding survives the keyring: a ciphertext must not move between
// instances or parameter names.
func TestKeyringKeepsContextBinding(t *testing.T) {
	dir := t.TempDir()
	kr, _ := LoadKeyring(writeMasterKeyFile(t, dir, "master.key"), nil)
	sealed, err := kr.SealString("hunter2", "mysql-web01", "password")
	if err != nil {
		t.Fatalf("SealString: %v", err)
	}
	for _, tc := range []struct{ instance, name string }{
		{"mysql-web02", "password"},
		{"mysql-web01", "token"},
	} {
		if _, err := kr.OpenString(sealed, tc.instance, tc.name); err == nil {
			t.Errorf("value decrypted under %s/%s — the binding is not enforced", tc.instance, tc.name)
		}
	}
}

func TestKeyringRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadKeyring("", nil); err == nil {
		t.Error("LoadKeyring must require a primary key")
	}
	if _, err := LoadKeyring(filepath.Join(dir, "absent.key"), nil); err == nil {
		t.Error("LoadKeyring must fail on a missing file")
	}
	kr, _ := LoadKeyring(writeMasterKeyFile(t, dir, "master.key"), nil)
	for _, bad := range []string{
		"enc:v2:deadbeef", // no separator
		"enc:v2:" + kr.PrimaryID() + ":!!!not base64!!!",
		"enc:v1:!!!not base64!!!",
	} {
		if _, err := kr.OpenString(bad, "i", "password"); err == nil {
			t.Errorf("OpenString(%q) should fail", bad)
		}
	}
}

// SealedKeyID is what lets a configuration archive be checked before it is imported: the
// ciphertexts travel without the keys, so the importer has to be able to name the keys it
// would need without holding any of them.
func TestSealedKeyID(t *testing.T) {
	dir := t.TempDir()
	kr, err := LoadKeyring(writeMasterKeyFile(t, dir, "master.key"), nil)
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	sealed, err := kr.SealString("hunter2", "mysql-web01", "password")
	if err != nil {
		t.Fatalf("SealString: %v", err)
	}

	id, isSealed := SealedKeyID(sealed)
	if !isSealed || id != kr.PrimaryID() {
		t.Errorf("SealedKeyID = %q, %v; want %q, true", id, isSealed, kr.PrimaryID())
	}
	if !kr.Has(id) {
		t.Errorf("Has(%q) = false for the key that just sealed the value", id)
	}
	if kr.Has("00000000") {
		t.Error("Has reports a key that was never loaded")
	}

	// A v1 value names no key, which is reported as an empty id: any loaded key is a
	// candidate for it, so Has says yes as long as there is one.
	if id, isSealed := SealedKeyID("enc:v1:c29tZXRoaW5n"); !isSealed || id != "" {
		t.Errorf("SealedKeyID(v1) = %q, %v; want \"\", true", id, isSealed)
	}
	if !kr.Has("") {
		t.Error(`Has("") = false, but a v1 value can be tried against every loaded key`)
	}

	// Plaintext predates encryption and is not sealed with anything.
	if id, isSealed := SealedKeyID("hunter2"); isSealed || id != "" {
		t.Errorf("SealedKeyID(plaintext) = %q, %v; want \"\", false", id, isSealed)
	}
}
