package crypto

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Web operators log in with a username and a password, so the database holds a
// verifier rather than the password itself: PBKDF2-HMAC-SHA256 with a random salt per
// user. A stolen arcatum.db therefore does not hand over anyone's password, and the
// deliberate cost of the derivation is what makes guessing the stolen hashes slow.
//
// PBKDF2 comes from the standard library (crypto/pbkdf2), which keeps the server free
// of dependencies — the same reason the SQLite driver is the pure-Go one.
//
// Runners keep authenticating with certificates; passwords are only for people. See
// auth.go for how the two identities meet.

// pbkdf2Iterations is the work factor. OWASP's current guidance for
// PBKDF2-HMAC-SHA256 is 600 000 iterations; a login costs a fraction of a second,
// which nobody notices, while a brute-force pass over a stolen hash does.
const pbkdf2Iterations = 600_000

// PasswordIterations is the work factor new hashes are created with. It is a variable
// only so tests can lower it — deriving a production-strength hash takes a noticeable
// fraction of a second, and test suites create a lot of accounts. Existing hashes are
// unaffected either way: each one records the count it was made with.
var PasswordIterations = pbkdf2Iterations

// pbkdf2KeyLen is the derived key length, matching the hash output.
const pbkdf2KeyLen = 32

// pbkdf2SaltLen is the per-user salt length. A random salt means two operators with
// the same password still get different hashes, so one cracked hash reveals nothing
// about the other.
const pbkdf2SaltLen = 16

// passwordScheme labels the stored format, so a future move to a different KDF can
// recognise and re-hash old values instead of locking everyone out.
const passwordScheme = "pbkdf2-sha256"

// MinPasswordLen is the shortest password accepted, counted in characters. Short enough
// not to be a nuisance on an internal system, long enough that the KDF's cost is what an
// attacker faces.
const MinPasswordLen = 8

// ErrPasswordTooShort is returned by HashPassword for a password below MinPasswordLen.
var ErrPasswordTooShort = fmt.Errorf("password must be at least %d characters", MinPasswordLen)

// ErrBadPasswordHash marks a stored hash that cannot be parsed — a corrupted or
// hand-edited row. It is deliberately distinct from "wrong password" so the two are
// not confused when diagnosing a failed login.
var ErrBadPasswordHash = errors.New("stored password hash is not in a known format")

// HashPassword derives a storable verifier for password. The result is a single
// self-describing string:
//
//	pbkdf2-sha256$600000$<salt-base64>$<key-base64>
//
// Keeping the parameters inside the value is what lets the work factor be raised later
// without invalidating existing users: an old hash still verifies with the iteration
// count it was made with.
func HashPassword(password string) (string, error) {
	return hashPasswordWith(password, PasswordIterations)
}

// hashPasswordWith is HashPassword with an explicit work factor, so tests do not pay
// the full cost of a production-strength derivation on every case.
func hashPasswordWith(password string, iterations int) (string, error) {
	// Counted in characters, not bytes: "heslíčko" is eight characters to whoever types
	// it, whatever its UTF-8 length happens to be.
	if utf8.RuneCountInString(password) < MinPasswordLen {
		return "", ErrPasswordTooShort
	}
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("password salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, pbkdf2KeyLen)
	if err != nil {
		return "", fmt.Errorf("derive password hash: %w", err)
	}
	return strings.Join([]string{
		passwordScheme,
		strconv.Itoa(iterations),
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	}, "$"), nil
}

// parsePasswordHash splits a stored verifier into its parts. Everything it can reject
// is a malformed hash rather than a wrong password, so the caller answers with
// ErrBadPasswordHash.
func parsePasswordHash(stored string) (iterations int, salt, key []byte, err error) {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != passwordScheme {
		return 0, nil, nil, ErrBadPasswordHash
	}
	iterations, err = strconv.Atoi(parts[1])
	if err != nil || iterations <= 0 {
		return 0, nil, nil, ErrBadPasswordHash
	}
	salt, err = base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return 0, nil, nil, ErrBadPasswordHash
	}
	key, err = base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(key) == 0 {
		return 0, nil, nil, ErrBadPasswordHash
	}
	return iterations, salt, key, nil
}

// ValidPasswordHash checks that a stored verifier is one this package can verify
// against, without knowing the password. A configuration archive carries hashes rather
// than passwords, so importing one has to be able to tell a usable account from a row
// that would only ever refuse to log in.
func ValidPasswordHash(stored string) error {
	_, _, _, err := parsePasswordHash(stored)
	return err
}

// VerifyPassword reports whether password matches a hash produced by HashPassword. It
// returns an error only when the stored hash itself is unusable; a plain mismatch is
// (false, nil), because the caller must treat "wrong password" and "no such user"
// identically.
func VerifyPassword(stored, password string) (bool, error) {
	iterations, salt, want, err := parsePasswordHash(stored)
	if err != nil {
		return false, err
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(want))
	if err != nil {
		return false, fmt.Errorf("derive password hash: %w", err)
	}
	// Constant time: the comparison must not leak how much of the hash matched.
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// decoyHash is a genuine verifier for a random password nobody knows. It is built on
// first use rather than at init, so a server that never sees a failed login does not pay
// for it. See VerifyDecoyPassword.
var decoyHash = sync.OnceValue(func() string {
	password, err := GeneratePassword()
	if err != nil {
		return ""
	}
	hash, err := hashPasswordWith(password, PasswordIterations)
	if err != nil {
		return ""
	}
	return hash
})

// VerifyDecoyPassword performs a verification that cannot succeed. Callers use it when
// there is no stored hash to check against — an unknown username — so that a failed login
// costs the same time whether or not the account exists. Without it, the quick rejection
// would tell an attacker which usernames are worth guessing passwords for.
func VerifyDecoyPassword(password string) {
	if hash := decoyHash(); hash != "" {
		_, _ = VerifyPassword(hash, password)
	}
}

// passwordAlphabet is base32 without padding: no case ambiguity, no characters that a
// shell or a chat client would mangle, and safe to read out over the phone.
var passwordAlphabet = base32.StdEncoding.WithPadding(base32.NoPadding)

// GeneratePassword returns a random password with about 80 bits of entropy. It is used
// for the first administrator (printed once into the log) and for resetting a password
// an operator has lost, where the point is that nobody has to invent one.
func GeneratePassword() (string, error) {
	buf := make([]byte, 10) // 10 bytes → 16 base32 characters
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	return strings.ToLower(passwordAlphabet.EncodeToString(buf)), nil
}
