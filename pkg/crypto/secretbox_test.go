package crypto

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func newTestBox(t *testing.T) *AESSecretBox {
	t.Helper()
	dir := t.TempDir()
	key, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	path := filepath.Join(dir, "secrets-master.key")
	writeFile(t, path, key)
	box, err := LoadSecretBox(path)
	if err != nil {
		t.Fatalf("LoadSecretBox: %v", err)
	}
	return box
}

func TestSealOpenRoundTrip(t *testing.T) {
	box := newTestBox(t)
	ctx := SecretContext("mysql-web01", "password")
	plaintext := []byte("hunter2")

	sealed, err := box.Seal(plaintext, ctx)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if strings.Contains(string(sealed), "hunter2") {
		t.Error("ciphertext must not contain the plaintext")
	}
	opened, err := box.Open(sealed, ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(opened) != "hunter2" {
		t.Errorf("Open = %q, want hunter2", opened)
	}
}

// A random nonce per value means the same secret encrypts differently each time, so
// an observer cannot tell that two instances share a password.
func TestSealProducesDistinctCiphertexts(t *testing.T) {
	box := newTestBox(t)
	ctx := SecretContext("i", "password")
	a, err := box.Seal([]byte("same"), ctx)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	b, err := box.Seal([]byte("same"), ctx)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if string(a) == string(b) {
		t.Error("encrypting the same value twice must not produce identical ciphertext")
	}
}

func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	box := newTestBox(t)
	ctx := SecretContext("i", "password")
	sealed, err := box.Seal([]byte("hunter2"), ctx)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	sealed[len(sealed)-1] ^= 0xff
	if _, err := box.Open(sealed, ctx); err == nil {
		t.Error("a tampered ciphertext must not decrypt")
	}
}

func TestOpenRejectsForeignKey(t *testing.T) {
	box := newTestBox(t)
	other := newTestBox(t)
	ctx := SecretContext("i", "password")
	sealed, err := box.Seal([]byte("hunter2"), ctx)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := other.Open(sealed, ctx); err == nil {
		t.Error("a ciphertext must not decrypt under a different master key")
	}
}

// The context binding is the point: someone with write access to the database must not
// be able to move a ciphertext to another instance or another parameter name.
func TestOpenRejectsRelocatedCiphertext(t *testing.T) {
	box := newTestBox(t)
	sealed, err := box.Seal([]byte("hunter2"), SecretContext("mysql-web01", "password"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	tests := []struct {
		name string
		ctx  []byte
	}{
		{"other instance", SecretContext("mysql-web02", "password")},
		{"other secret name", SecretContext("mysql-web01", "token")},
		{"no context", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := box.Open(sealed, tc.ctx); err == nil {
				t.Error("ciphertext must not decrypt under a different context")
			}
		})
	}
}

func TestOpenRejectsShortCiphertext(t *testing.T) {
	box := newTestBox(t)
	if _, err := box.Open([]byte("xx"), nil); err == nil {
		t.Error("a ciphertext shorter than the nonce must be rejected")
	}
}

func TestLoadSecretBoxRejectsBadKeyFiles(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		content string
	}{
		{"not base64", "@@@ not base64 @@@"},
		{"too short", "c2hvcnQ="}, // "short"
		{"empty", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".key")
			writeFile(t, path, []byte(tc.content))
			if _, err := LoadSecretBox(path); err == nil {
				t.Error("LoadSecretBox must reject an invalid key file")
			}
		})
	}
	if _, err := LoadSecretBox(filepath.Join(dir, "absent.key")); err == nil {
		t.Error("LoadSecretBox must fail on a missing file")
	}
}

func TestSealToStringRoundTrip(t *testing.T) {
	box := newTestBox(t)
	stored, err := SealToString(box, "hunter2", "mysql-web01", "password")
	if err != nil {
		t.Fatalf("SealToString: %v", err)
	}
	if !IsSealed(stored) {
		t.Errorf("stored value %q is not marked as sealed", stored)
	}
	if strings.Contains(stored, "hunter2") {
		t.Error("stored value must not contain the plaintext")
	}
	opened, err := OpenFromString(box, stored, "mysql-web01", "password")
	if err != nil {
		t.Fatalf("OpenFromString: %v", err)
	}
	if opened != "hunter2" {
		t.Errorf("OpenFromString = %q, want hunter2", opened)
	}
}

// Without a key configured the value is stored as-is, so development mode keeps working.
func TestSealToStringWithoutBoxIsPassthrough(t *testing.T) {
	stored, err := SealToString(nil, "hunter2", "i", "password")
	if err != nil {
		t.Fatalf("SealToString: %v", err)
	}
	if stored != "hunter2" {
		t.Errorf("stored = %q, want the plaintext unchanged", stored)
	}
	if IsSealed(stored) {
		t.Error("a plaintext value must not be marked as sealed")
	}
}

// Databases written before encryption was enabled must stay readable.
func TestOpenFromStringPassesThroughLegacyPlaintext(t *testing.T) {
	box := newTestBox(t)
	got, err := OpenFromString(box, "legacy-plaintext", "i", "password")
	if err != nil {
		t.Fatalf("OpenFromString: %v", err)
	}
	if got != "legacy-plaintext" {
		t.Errorf("OpenFromString = %q, want legacy-plaintext", got)
	}
}

// An encrypted value with no key must be a loud error, not a silently empty password.
func TestOpenFromStringWithoutBoxFailsOnSealedValue(t *testing.T) {
	box := newTestBox(t)
	stored, err := SealToString(box, "hunter2", "i", "password")
	if err != nil {
		t.Fatalf("SealToString: %v", err)
	}
	if _, err := OpenFromString(nil, stored, "i", "password"); !errors.Is(err, ErrSealed) {
		t.Errorf("OpenFromString(no box) = %v, want ErrSealed", err)
	}
}

func TestOpenFromStringRejectsCorruptedStoredValue(t *testing.T) {
	box := newTestBox(t)
	if _, err := OpenFromString(box, sealedPrefix+"!!!not base64!!!", "i", "password"); err == nil {
		t.Error("a sealed value that is not valid base64 must be rejected")
	}
}
