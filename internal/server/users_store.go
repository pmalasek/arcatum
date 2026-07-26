package server

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"arcatum/pkg/crypto"
)

// Persistence for the two things password login needs: operator accounts and their
// sessions. The handlers and middleware are in users.go, the same split as
// enroll.go / enroll_store.go.

// User roles for the web UI. Runners are not users — their identity is a certificate
// (auth.go); these roles only decide what a person may do through the web.
const (
	// UserRoleAdmin may change anything, including other accounts.
	UserRoleAdmin = "admin"
	// UserRoleViewer may look but not touch: no triggering runs, no editing instances,
	// no approving runners. Handy for someone who only needs to see whether backups ran.
	UserRoleViewer = "viewer"
)

// Errors the login and account handlers turn into HTTP responses.
var (
	// ErrBadCredentials covers both "no such user" and "wrong password" on purpose: the
	// login endpoint must not reveal which usernames exist.
	ErrBadCredentials = errors.New("invalid username or password")
	// ErrUserDisabled is separate because it is not a guessing signal — the caller has
	// already proven the password.
	ErrUserDisabled = errors.New("account is disabled")
	// ErrUserExists is returned when creating an account that is already there.
	ErrUserExists = errors.New("user already exists")
	// ErrNoSuchUser is returned when an account operation names an unknown user.
	ErrNoSuchUser = errors.New("no such user")
	// ErrLastAdmin refuses the change that would leave nobody able to administer the
	// system — the one mistake that cannot be undone through the web UI.
	ErrLastAdmin = errors.New("this is the last enabled administrator")
	// ErrBadUsername rejects a username that would be awkward or ambiguous.
	ErrBadUsername = errors.New("username must be 2-32 characters of a-z, 0-9, dot, dash or underscore")
	// ErrBadRole rejects an unknown role.
	ErrBadRole = fmt.Errorf("role must be %q or %q", UserRoleAdmin, UserRoleViewer)
)

// User is a web operator account. The password hash is deliberately not a field: it
// never leaves the store, so it cannot end up in an API response by accident.
type User struct {
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	Disabled  bool      `json:"disabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	LastLogin time.Time `json:"last_login,omitempty"`
}

// IsAdmin reports whether the account may make changes.
func (u *User) IsAdmin() bool { return u != nil && u.Role == UserRoleAdmin }

// usernamePattern keeps usernames unambiguous: lowercase, no spaces, nothing that has
// to be quoted in a URL or a shell.
var usernamePattern = regexp.MustCompile(`^[a-z0-9._-]{2,32}$`)

// NormalizeUsername lowercases and trims a username, so "Petr" and "petr " are the same
// account rather than two.
func NormalizeUsername(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// validUsername checks a normalized username.
func validUsername(name string) error {
	if !usernamePattern.MatchString(name) {
		return ErrBadUsername
	}
	return nil
}

// validRole checks a role name.
func validRole(role string) error {
	switch role {
	case UserRoleAdmin, UserRoleViewer:
		return nil
	}
	return ErrBadRole
}

const userCols = `username, role, disabled, created_at, updated_at, last_login`

func scanUser(sc interface{ Scan(...any) error }) (*User, error) {
	var u User
	var disabled int
	var created, updated, login int64
	if err := sc.Scan(&u.Username, &u.Role, &disabled, &created, &updated, &login); err != nil {
		return nil, err
	}
	u.Disabled = disabled != 0
	u.CreatedAt, u.UpdatedAt, u.LastLogin = fromMillis(created), fromMillis(updated), fromMillis(login)
	return &u, nil
}

// Users lists accounts alphabetically.
func (s *Store) Users() ([]*User, error) {
	rows, err := s.db.Query(`SELECT ` + userCols + ` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// User returns one account, or nil when it does not exist.
func (s *Store) User(username string) (*User, error) {
	row := s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE username = ?`, NormalizeUsername(username))
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

// UserCount returns how many accounts exist. The server uses it at startup to decide
// whether it has to create the first administrator.
func (s *Store) UserCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// enabledAdmins counts accounts that can still administer the system.
func (s *Store) enabledAdmins(excluding string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users
		WHERE role = ? AND disabled = 0 AND username != ?`,
		UserRoleAdmin, NormalizeUsername(excluding)).Scan(&n)
	return n, err
}

// requireAnotherAdmin refuses a change to username that would remove the last enabled
// administrator. Locking everybody out is recoverable only from a shell on the server
// (arcatum-server -passwd), so it is worth preventing.
func (s *Store) requireAnotherAdmin(username string) error {
	n, err := s.enabledAdmins(username)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLastAdmin
	}
	return nil
}

// CreateUser adds an account with the given password and role.
func (s *Store) CreateUser(username, password, role string) (*User, error) {
	username = NormalizeUsername(username)
	if err := validUsername(username); err != nil {
		return nil, err
	}
	if err := validRole(role); err != nil {
		return nil, err
	}
	hash, err := crypto.HashPassword(password)
	if err != nil {
		return nil, err
	}
	now := toMillis(time.Now())
	_, err = s.db.Exec(`
		INSERT INTO users (username, pass_hash, role, disabled, created_at, updated_at)
		VALUES (?, ?, ?, 0, ?, ?)`, username, hash, role, now, now)
	if err != nil {
		// modernc/sqlite reports the constraint in the message; mapping it here keeps the
		// driver's wording out of the API response.
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrUserExists
		}
		return nil, err
	}
	return s.User(username)
}

// SetUserPassword replaces an account's password. Every session of that account is
// dropped: changing a password is also how you throw out someone who is already logged
// in, and it would be surprising if the old cookie kept working.
func (s *Store) SetUserPassword(username, password string) error {
	username = NormalizeUsername(username)
	hash, err := crypto.HashPassword(password)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE users SET pass_hash = ?, updated_at = ? WHERE username = ?`,
		hash, toMillis(time.Now()), username)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNoSuchUser
	}
	return s.DeleteUserSessions(username)
}

// SetUserRole changes what an account may do.
func (s *Store) SetUserRole(username, role string) error {
	username = NormalizeUsername(username)
	if err := validRole(role); err != nil {
		return err
	}
	if role != UserRoleAdmin {
		if err := s.requireAdminUnaffected(username); err != nil {
			return err
		}
	}
	res, err := s.db.Exec(`UPDATE users SET role = ?, updated_at = ? WHERE username = ?`,
		role, toMillis(time.Now()), username)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNoSuchUser
	}
	return nil
}

// SetUserDisabled blocks or restores an account. Disabling also ends its sessions, so
// it takes effect at once rather than at the end of the cookie's life.
func (s *Store) SetUserDisabled(username string, disabled bool) error {
	username = NormalizeUsername(username)
	if disabled {
		if err := s.requireAdminUnaffected(username); err != nil {
			return err
		}
	}
	flag := 0
	if disabled {
		flag = 1
	}
	res, err := s.db.Exec(`UPDATE users SET disabled = ?, updated_at = ? WHERE username = ?`,
		flag, toMillis(time.Now()), username)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNoSuchUser
	}
	if disabled {
		return s.DeleteUserSessions(username)
	}
	return nil
}

// DeleteUser removes an account and everything it is logged in with.
func (s *Store) DeleteUser(username string) error {
	username = NormalizeUsername(username)
	if err := s.requireAdminUnaffected(username); err != nil {
		return err
	}
	res, err := s.db.Exec(`DELETE FROM users WHERE username = ?`, username)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNoSuchUser
	}
	return s.DeleteUserSessions(username)
}

// requireAdminUnaffected allows a change to username only when it does not remove the
// last enabled administrator. A change to a viewer is always fine.
func (s *Store) requireAdminUnaffected(username string) error {
	u, err := s.User(username)
	if err != nil {
		return err
	}
	if u == nil {
		return ErrNoSuchUser
	}
	if !u.IsAdmin() || u.Disabled {
		return nil // removing this one does not reduce the number of working admins
	}
	return s.requireAnotherAdmin(username)
}

// Authenticate checks a username and password. A wrong password and an unknown user
// are both ErrBadCredentials, so a failed login says nothing about which accounts
// exist.
func (s *Store) Authenticate(username, password string) (*User, error) {
	username = NormalizeUsername(username)
	var hash string
	err := s.db.QueryRow(`SELECT pass_hash FROM users WHERE username = ?`, username).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		// Check the password against a decoy anyway: a faster answer for a name that does
		// not exist would tell a caller which accounts are worth guessing at.
		crypto.VerifyDecoyPassword(password)
		return nil, ErrBadCredentials
	}
	if err != nil {
		return nil, err
	}
	ok, err := crypto.VerifyPassword(hash, password)
	if err != nil {
		// A hash we cannot parse is an operator problem, not a guess: say so in the log
		// but answer the caller with the same generic failure.
		return nil, fmt.Errorf("user %q: %w", username, err)
	}
	if !ok {
		return nil, ErrBadCredentials
	}
	u, err := s.User(username)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrBadCredentials
	}
	if u.Disabled {
		return nil, ErrUserDisabled
	}
	return u, nil
}

// MarkUserLogin records a successful login, which is what the UI shows as "last seen"
// for an account.
func (s *Store) MarkUserLogin(username string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE users SET last_login = ? WHERE username = ?`,
		toMillis(at), NormalizeUsername(username))
	return err
}

// --- sessions ----------------------------------------------------------------

// Session is a logged-in browser. The token itself is not stored — see hashToken.
type Session struct {
	Username  string
	CreatedAt time.Time
	ExpiresAt time.Time
	LastSeen  time.Time
	IP        string
}

// sessionTokenLen is the size of the random cookie value: 256 bits, so guessing one is
// not a strategy.
const sessionTokenLen = 32

// newSessionToken returns a fresh cookie value.
func newSessionToken() (string, error) {
	buf := make([]byte, sessionTokenLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("session token: %w", err)
	}
	// URL-safe so it needs no escaping in a Set-Cookie header.
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashToken maps a cookie value to what the database holds. Storing the hash rather
// than the token means a leaked database (or a backup of it) cannot be replayed as
// somebody's live session — the same reasoning as passwords, one step further in.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateSession opens a session for username and returns the cookie value. The token
// is returned once and never again: only its hash is kept.
func (s *Store) CreateSession(username, ip string, now, expires time.Time) (string, error) {
	token, err := newSessionToken()
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(`
		INSERT INTO sessions (token_hash, username, created_at, expires_at, last_seen, ip)
		VALUES (?, ?, ?, ?, ?, ?)`,
		hashToken(token), NormalizeUsername(username), toMillis(now), toMillis(expires),
		toMillis(now), ip)
	if err != nil {
		return "", err
	}
	return token, nil
}

// SessionUser resolves a cookie value to the account behind it. It returns (nil, nil,
// nil) when the token is unknown, expired, or belongs to an account that has since
// been disabled — all of which mean "log in again" to the caller.
func (s *Store) SessionUser(token string, now time.Time) (*User, *Session, error) {
	if token == "" {
		return nil, nil, nil
	}
	var (
		sess                       Session
		created, expires, lastSeen int64
	)
	err := s.db.QueryRow(`SELECT username, created_at, expires_at, last_seen, ip
		FROM sessions WHERE token_hash = ?`, hashToken(token)).
		Scan(&sess.Username, &created, &expires, &lastSeen, &sess.IP)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	sess.CreatedAt, sess.ExpiresAt, sess.LastSeen = fromMillis(created), fromMillis(expires), fromMillis(lastSeen)
	if !sess.ExpiresAt.IsZero() && now.After(sess.ExpiresAt) {
		// Clean up as we go, so expired rows do not accumulate for a browser that keeps
		// presenting the same dead cookie.
		_ = s.DeleteSession(token)
		return nil, nil, nil
	}
	u, err := s.User(sess.Username)
	if err != nil {
		return nil, nil, err
	}
	if u == nil || u.Disabled {
		_ = s.DeleteSession(token)
		return nil, nil, nil
	}
	return u, &sess, nil
}

// TouchSession extends a session's life and records the activity. Sessions slide
// forward while they are being used, so working in the UI does not get interrupted by a
// fixed deadline, while an abandoned session still expires.
func (s *Store) TouchSession(token string, now, expires time.Time) error {
	_, err := s.db.Exec(`UPDATE sessions SET last_seen = ?, expires_at = ? WHERE token_hash = ?`,
		toMillis(now), toMillis(expires), hashToken(token))
	return err
}

// DeleteSession ends one session (logout).
func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
	return err
}

// DeleteUserSessions ends every session of an account.
func (s *Store) DeleteUserSessions(username string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE username = ?`, NormalizeUsername(username))
	return err
}

// PurgeExpiredSessions removes sessions that have run out. Called at startup and on
// every login, which is often enough for a table that only grows by one row per login.
func (s *Store) PurgeExpiredSessions(now time.Time) (int, error) {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at > 0 AND expires_at < ?`, toMillis(now))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
