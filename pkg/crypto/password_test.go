package crypto

import (
	"errors"
	"strings"
	"testing"
)

// testIterations keeps the derivation cheap in tests; the format is what is under test,
// not the work factor.
const testIterations = 1000

func TestHashPasswordVerifies(t *testing.T) {
	hash, err := hashPasswordWith("correct horse", testIterations)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	ok, err := VerifyPassword(hash, "correct horse")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("the password used to create the hash must verify")
	}
}

func TestVerifyPasswordRejectsWrongPassword(t *testing.T) {
	hash, err := hashPasswordWith("correct horse", testIterations)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	// A mismatch is not an error: the caller must not be able to tell "wrong password"
	// from "no such user".
	ok, err := VerifyPassword(hash, "correct horsf")
	if err != nil {
		t.Fatalf("VerifyPassword: unexpected error %v", err)
	}
	if ok {
		t.Error("a different password must not verify")
	}
}

// The password itself must not be recoverable from what is stored.
func TestHashDoesNotContainPassword(t *testing.T) {
	const password = "hunter2hunter2"
	hash, err := hashPasswordWith(password, testIterations)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if strings.Contains(hash, password) {
		t.Errorf("stored hash %q contains the password", hash)
	}
}

// Two users with the same password must not end up with the same hash, or cracking one
// would crack both.
func TestHashPasswordUsesRandomSalt(t *testing.T) {
	a, err := hashPasswordWith("same password", testIterations)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	b, err := hashPasswordWith("same password", testIterations)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if a == b {
		t.Error("the same password must hash differently (missing salt?)")
	}
	// Both must still verify.
	for _, h := range []string{a, b} {
		if ok, err := VerifyPassword(h, "same password"); err != nil || !ok {
			t.Errorf("VerifyPassword(%q) = %v, %v; want true, nil", h, ok, err)
		}
	}
}

// The stored value carries its own work factor, so raising the production iteration
// count must not lock out users hashed under the old one.
func TestVerifyPasswordHonoursStoredIterations(t *testing.T) {
	cheap, err := hashPasswordWith("iterations matter", 500)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.Contains(cheap, "$500$") {
		t.Fatalf("hash %q does not record its iteration count", cheap)
	}
	if ok, err := VerifyPassword(cheap, "iterations matter"); err != nil || !ok {
		t.Errorf("VerifyPassword = %v, %v; want true, nil", ok, err)
	}
}

func TestHashPasswordRejectsShortPassword(t *testing.T) {
	// "krátké" is six characters but eight bytes: the limit is about what someone typed,
	// not about how UTF-8 encodes it.
	for _, password := range []string{"", "short", "krátké"} {
		if _, err := hashPasswordWith(password, testIterations); !errors.Is(err, ErrPasswordTooShort) {
			t.Errorf("hashPasswordWith(%q) err = %v; want ErrPasswordTooShort", password, err)
		}
	}
	// Exactly at the limit is accepted, in characters as well as in bytes.
	for _, password := range []string{"12345678", "heslíčko"} {
		if _, err := hashPasswordWith(password, testIterations); err != nil {
			t.Errorf("hashPasswordWith(%q) = %v; want it accepted", password, err)
		}
	}
}

// A corrupted or hand-edited row must be reported as such rather than silently
// behaving like a wrong password — and must never verify.
func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	for _, stored := range []string{
		"",
		"plaintext-password",
		"bcrypt$10$abc$def",
		"pbkdf2-sha256$notanumber$c2FsdA$aGFzaA",
		"pbkdf2-sha256$1000$!!!$aGFzaA",
		"pbkdf2-sha256$1000$c2FsdA",
	} {
		ok, err := VerifyPassword(stored, "any password")
		if ok {
			t.Errorf("VerifyPassword(%q) verified", stored)
		}
		if !errors.Is(err, ErrBadPasswordHash) {
			t.Errorf("VerifyPassword(%q) err = %v; want ErrBadPasswordHash", stored, err)
		}
	}
}

// The decoy exists so an unknown username costs the same as a wrong password. It must
// therefore actually do the work — and never behave like a match.
func TestVerifyDecoyPassword(t *testing.T) {
	PasswordIterations = testIterations
	t.Cleanup(func() { PasswordIterations = pbkdf2Iterations })

	VerifyDecoyPassword("whatever was typed") // builds the decoy
	hash := decoyHash()
	if hash == "" {
		t.Fatal("no decoy hash was built")
	}
	if ok, err := VerifyPassword(hash, "whatever was typed"); err != nil || ok {
		t.Errorf("the decoy verified a caller-supplied password: %v, %v", ok, err)
	}
	// Repeated calls reuse the same decoy rather than rebuilding it.
	VerifyDecoyPassword("something else")
	if decoyHash() != hash {
		t.Error("the decoy hash changed between calls")
	}
}

func TestGeneratePassword(t *testing.T) {
	a, err := GeneratePassword()
	if err != nil {
		t.Fatalf("GeneratePassword: %v", err)
	}
	b, err := GeneratePassword()
	if err != nil {
		t.Fatalf("GeneratePassword: %v", err)
	}
	if a == b {
		t.Error("generated passwords must differ")
	}
	// It has to be usable: long enough for HashPassword to accept it, and printable so
	// it can be read from a log or dictated over the phone.
	if len(a) < MinPasswordLen {
		t.Errorf("generated password %q is shorter than MinPasswordLen", a)
	}
	for _, r := range a {
		if !(r >= 'a' && r <= 'z') && !(r >= '2' && r <= '7') {
			t.Errorf("generated password %q contains awkward character %q", a, r)
			break
		}
	}
	if _, err := hashPasswordWith(a, testIterations); err != nil {
		t.Errorf("generated password is not accepted by HashPassword: %v", err)
	}
}
