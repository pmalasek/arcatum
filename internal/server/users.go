package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"arcatum/pkg/crypto"
)

// Password login for the web UI.
//
// Runners are identified by a certificate and always will be: they are machines, they
// are installed once, and a certificate is what lets the server refuse an unknown host
// during the TLS handshake. People are the opposite case — a client certificate has to
// be generated, exported as PKCS#12 and imported into every browser someone uses, and
// it silently expires. So the web listener asks for a username and a password instead
// (see WebHandler in http.go), and this file is that mechanism:
//
//   - POST /api/v1/login   → verifies the password, opens a session, sets a cookie
//   - the cookie           → resolved on every request by webRead/webWrite
//   - POST /api/v1/logout  → ends the session
//   - /api/v1/users/…      → account management, admins only
//
// The cookie is HttpOnly and SameSite=Strict, and every state-changing request must
// also come from this origin (see sameOrigin) — so another site cannot make an
// operator's browser act on their behalf.

// sessionCookie is the browser cookie holding the session token.
const sessionCookie = "arcatum_session"

// DefaultSessionTTL is how long a session lives without activity. It slides forward
// while the UI is being used, so a working day does not get interrupted, and an
// abandoned browser is logged out.
const DefaultSessionTTL = 12 * time.Hour

// touchInterval is how often an active session's expiry is written back. Without it
// every poll of the run list (every few seconds) would be a database write.
const touchInterval = 5 * time.Minute

// WebOptions configures the plain-HTTP web listener's authentication.
type WebOptions struct {
	// SessionTTL is the inactivity window before a session expires. Zero means
	// DefaultSessionTTL.
	SessionTTL time.Duration
	// SecureCookie marks the session cookie Secure, so the browser only ever sends it
	// over HTTPS. Off by default because the listener is plain HTTP by design; turn it on
	// when a reverse proxy terminates TLS in front of it.
	SecureCookie bool
}

// sessionTTL resolves the configured inactivity window.
func (s *Server) sessionTTL() time.Duration {
	if s.web.SessionTTL > 0 {
		return s.web.SessionTTL
	}
	return DefaultSessionTTL
}

// --- current user -------------------------------------------------------------

// userContextKey carries the logged-in operator through the request context, so a
// handler does not repeat the session lookup its guard already did.
type userContextKey struct{}

// userFrom returns the operator a request was authenticated as, or nil when the request
// came in over the mTLS listener (where identity is a certificate).
func userFrom(r *http.Request) *User {
	u, _ := r.Context().Value(userContextKey{}).(*User)
	return u
}

// withUser returns r carrying u as its authenticated operator.
func withUser(r *http.Request, u *User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userContextKey{}, u))
}

// sessionFromRequest resolves the session cookie to an account. It returns the token as
// well, so the guard can slide the expiry forward.
func (s *Server) sessionFromRequest(r *http.Request) (*User, string) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, ""
	}
	now := time.Now()
	u, sess, err := s.store.SessionUser(c.Value, now)
	if err != nil {
		s.log.Printf("session lookup: %v", err)
		return nil, ""
	}
	if u == nil {
		return nil, c.Value
	}
	// Slide the expiry forward, but not on every single request.
	if sess.LastSeen.IsZero() || now.Sub(sess.LastSeen) > touchInterval {
		if err := s.store.TouchSession(c.Value, now, now.Add(s.sessionTTL())); err != nil {
			s.log.Printf("session touch: %v", err)
		}
	}
	return u, c.Value
}

// --- guards -------------------------------------------------------------------

// webRead wraps a handler so any logged-in operator may reach it. Used for everything
// that only reads: run history, output, instance listings, restore browsing.
func (s *Server) webRead(next http.HandlerFunc) http.HandlerFunc {
	return s.webGuard(next, false)
}

// webAdmin wraps a handler so only an administrator may reach it: everything that
// changes something (triggering runs, editing instances, approving runners, rekeying
// secrets) plus account management, which a viewer has no business reading either.
func (s *Server) webAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.webGuard(next, true)
}

// webGuard authenticates the session cookie and, when needAdmin is set, checks the role.
func (s *Server) webGuard(next http.HandlerFunc, needAdmin bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// A cross-site request must not be able to act with the operator's cookie. The
		// cookie is already SameSite=Strict; this is the second, explicit check.
		if isStateChanging(r) && !sameOrigin(r) {
			s.log.Printf("denied: cross-origin %s %s from %q", r.Method, r.URL.Path, r.Header.Get("Origin"))
			writeAuthError(w, http.StatusForbidden, "request must come from the Arcatum web UI")
			return
		}
		u, _ := s.sessionFromRequest(r)
		if u == nil {
			// 401 with a machine-readable reason: the UI shows the login form on this.
			writeAuthError(w, http.StatusUnauthorized, "not logged in")
			return
		}
		if needAdmin && !u.IsAdmin() {
			s.log.Printf("denied: %q (role %q) tried %s %s", u.Username, u.Role, r.Method, r.URL.Path)
			writeAuthError(w, http.StatusForbidden, "this action needs the admin role")
			return
		}
		next(w, withUser(r, u))
	}
}

// writeAuthError answers with JSON, since every caller of the web API is the UI's
// fetch() and a JSON body is what it can act on.
func writeAuthError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// isStateChanging reports whether a request would alter state, and therefore needs the
// origin check. GET and HEAD are exempt: they change nothing, and the restore view
// downloads files through plain links and iframes that carry no Origin header.
func isStateChanging(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// sameOrigin reports whether a request originates from the page the server itself
// served. Browsers send Origin on every state-changing fetch, so an absent header means
// the caller is not a browser form or script — curl, for instance — and is allowed.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		// Sec-Fetch-Site is sent by current browsers even when Origin is not; when it says
		// the request is cross-site, refuse regardless.
		return r.Header.Get("Sec-Fetch-Site") != "cross-site"
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// --- login --------------------------------------------------------------------

// loginRequest is the body of POST /api/v1/login.
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLogin verifies a password and opens a session.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeAuthError(w, http.StatusForbidden, "request must come from the Arcatum web UI")
		return
	}
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAuthError(w, http.StatusBadRequest, "bad request")
		return
	}
	username := NormalizeUsername(req.Username)

	// Verifying a password is deliberately expensive, so an unauthenticated caller must
	// not be able to ask for it without limit.
	if wait := s.logins.blocked(username, time.Now()); wait > 0 {
		s.log.Printf("login for %q throttled for another %s", username, wait.Round(time.Second))
		writeAuthError(w, http.StatusTooManyRequests,
			"too many failed attempts, try again in "+wait.Round(time.Second).String())
		return
	}

	u, err := s.store.Authenticate(username, req.Password)
	if err != nil {
		s.logins.fail(username, time.Now())
		switch {
		case errors.Is(err, ErrBadCredentials):
			s.log.Printf("login failed for %q from %s", username, clientIP(r))
			writeAuthError(w, http.StatusUnauthorized, "invalid username or password")
		case errors.Is(err, ErrUserDisabled):
			s.log.Printf("login refused for disabled account %q", username)
			writeAuthError(w, http.StatusForbidden, "account is disabled")
		default:
			s.log.Printf("login for %q: %v", username, err)
			writeAuthError(w, http.StatusUnauthorized, "invalid username or password")
		}
		return
	}
	s.logins.success(username)

	now := time.Now()
	if n, err := s.store.PurgeExpiredSessions(now); err != nil {
		s.log.Printf("purge sessions: %v", err)
	} else if n > 0 {
		s.log.Printf("purged %d expired session(s)", n)
	}
	token, err := s.store.CreateSession(u.Username, clientIP(r), now, now.Add(s.sessionTTL()))
	if err != nil {
		s.log.Printf("create session for %q: %v", u.Username, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.store.MarkUserLogin(u.Username, now); err != nil {
		s.log.Printf("record login for %q: %v", u.Username, err)
	}
	s.setSessionCookie(w, token)
	s.log.Printf("login: %q (%s) from %s", u.Username, u.Role, clientIP(r))
	writeJSON(w, u)
}

// handleLogout ends the calling session.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := s.store.DeleteSession(c.Value); err != nil {
			s.log.Printf("logout: %v", err)
		}
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// setSessionCookie sends the session cookie. HttpOnly keeps it away from scripts,
// SameSite=Strict keeps it off cross-site requests, and no Expires makes it a session
// cookie — closing the browser drops it.
func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.web.SecureCookie,
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.web.SecureCookie,
		SameSite: http.SameSiteStrictMode,
	})
}

// clientIP is the caller's address without the port, recorded with a session and logged
// with a failed login so a password-guessing attempt can be traced.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- login throttling ---------------------------------------------------------

// Failed logins are throttled per account. Two reasons: guessing a password should be
// slow in wall-clock terms as well as in CPU terms, and each attempt costs a PBKDF2
// derivation, which an unauthenticated caller must not be able to demand in a loop.
const (
	// loginFailureGrace is how many wrong passwords are tolerated before locking.
	loginFailureGrace = 5
	// loginLockoutBase is the first lockout; it doubles with each further failure.
	loginLockoutBase = time.Minute
	// loginLockoutMax caps the lockout, so a locked-out operator is never stuck for long.
	loginLockoutMax = 15 * time.Minute
	// loginFailureTTL forgets old failures, so yesterday's typos do not count.
	loginFailureTTL = time.Hour
)

// loginLimiter tracks consecutive failed logins per account. In memory on purpose: a
// restart clearing it is not a weakness worth a database write per attempt.
type loginLimiter struct {
	mu    sync.Mutex
	state map[string]*loginFailures
}

type loginFailures struct {
	count int
	last  time.Time
	until time.Time // locked out until this moment
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{state: make(map[string]*loginFailures)}
}

// blocked returns how long the account must wait, or zero when it may try now.
func (l *loginLimiter) blocked(username string, now time.Time) time.Duration {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.state[username]
	if st == nil {
		return 0
	}
	if now.Sub(st.last) > loginFailureTTL {
		delete(l.state, username)
		return 0
	}
	if now.Before(st.until) {
		return st.until.Sub(now)
	}
	return 0
}

// fail records a wrong password and extends the lockout once the grace is used up.
func (l *loginLimiter) fail(username string, now time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.state[username]
	if st == nil || now.Sub(st.last) > loginFailureTTL {
		st = &loginFailures{}
		l.state[username] = st
	}
	st.count++
	st.last = now
	if st.count <= loginFailureGrace {
		return
	}
	lockout := loginLockoutBase << (st.count - loginFailureGrace - 1)
	if lockout > loginLockoutMax || lockout <= 0 {
		lockout = loginLockoutMax
	}
	st.until = now.Add(lockout)
}

// success clears the record: a correct password means the operator is who they say.
func (l *loginLimiter) success(username string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.state, username)
}

// --- account management -------------------------------------------------------

// handleListUsers returns the accounts. Admin-only: who has access is not something a
// viewer needs to enumerate.
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.Users()
	if err != nil {
		s.log.Printf("list users: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, users)
}

// userRequest is the body for creating or updating an account. Every field is optional
// on update: what is absent stays as it is.
type userRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
	Disabled *bool  `json:"disabled"`
	// GeneratePassword asks the server to invent one and return it, which is how a
	// password is reset for somebody who has forgotten theirs.
	GeneratePassword bool `json:"generate_password"`
}

// userResponse returns the account, plus a generated password when one was created —
// the only moment a password is ever sent back.
type userResponse struct {
	*User
	Password string `json:"password,omitempty"`
}

// handleCreateUser adds an account.
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req userRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	role := req.Role
	if role == "" {
		role = UserRoleViewer // the safe default: an account that cannot change anything
	}
	password := req.Password
	generated := ""
	if password == "" || req.GeneratePassword {
		var err error
		if password, err = crypto.GeneratePassword(); err != nil {
			s.log.Printf("generate password: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		generated = password
	}
	u, err := s.store.CreateUser(req.Username, password, role)
	if err != nil {
		s.userError(w, err)
		return
	}
	s.log.Printf("user %q created (role %s) by %s", u.Username, u.Role, actor(r))
	writeJSON(w, userResponse{User: u, Password: generated})
}

// handleUpdateUser changes a role, a password, or whether an account is disabled.
func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	name := NormalizeUsername(r.PathValue("name"))
	var req userRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	existing, err := s.store.User(name)
	if err != nil {
		s.log.Printf("user %q: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if existing == nil {
		s.userError(w, ErrNoSuchUser)
		return
	}
	generated := ""
	if req.Role != "" && req.Role != existing.Role {
		if err := s.store.SetUserRole(name, req.Role); err != nil {
			s.userError(w, err)
			return
		}
		s.log.Printf("user %q role -> %s by %s", name, req.Role, actor(r))
	}
	if req.Disabled != nil && *req.Disabled != existing.Disabled {
		if err := s.store.SetUserDisabled(name, *req.Disabled); err != nil {
			s.userError(w, err)
			return
		}
		s.log.Printf("user %q disabled=%v by %s", name, *req.Disabled, actor(r))
	}
	if req.Password != "" || req.GeneratePassword {
		password := req.Password
		if password == "" {
			if password, err = crypto.GeneratePassword(); err != nil {
				s.log.Printf("generate password: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			generated = password
		}
		if err := s.store.SetUserPassword(name, password); err != nil {
			s.userError(w, err)
			return
		}
		s.log.Printf("user %q password reset by %s (sessions ended)", name, actor(r))
	}
	u, err := s.store.User(name)
	if err != nil || u == nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, userResponse{User: u, Password: generated})
}

// handleDeleteUser removes an account.
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	name := NormalizeUsername(r.PathValue("name"))
	// Deleting the account you are logged in as would log you out mid-action; refusing is
	// clearer than doing it.
	if me := userFrom(r); me != nil && me.Username == name {
		writeAuthError(w, http.StatusConflict, "you cannot delete the account you are logged in as")
		return
	}
	if err := s.store.DeleteUser(name); err != nil {
		s.userError(w, err)
		return
	}
	s.log.Printf("user %q deleted by %s", name, actor(r))
	w.WriteHeader(http.StatusNoContent)
}

// passwordRequest is the body of POST /api/v1/password: an operator changing their own.
type passwordRequest struct {
	Current string `json:"current"`
	New     string `json:"new"`
}

// handleChangeOwnPassword lets any logged-in operator change their own password. The
// current one is required, so a borrowed session cannot lock the owner out.
func (s *Server) handleChangeOwnPassword(w http.ResponseWriter, r *http.Request) {
	me := userFrom(r)
	if me == nil {
		writeAuthError(w, http.StatusUnauthorized, "not logged in")
		return
	}
	var req passwordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := s.store.Authenticate(me.Username, req.Current); err != nil {
		writeAuthError(w, http.StatusForbidden, "current password is not correct")
		return
	}
	if err := s.store.SetUserPassword(me.Username, req.New); err != nil {
		s.userError(w, err)
		return
	}
	// SetUserPassword dropped every session, including this one — the UI has to log in
	// again, which is also the confirmation that the new password works.
	s.clearSessionCookie(w)
	s.log.Printf("user %q changed their password", me.Username)
	writeJSON(w, map[string]string{"status": "changed", "note": "log in again"})
}

// actor names whoever is making a change, for the log: a logged-in operator on the web
// listener, or the certificate's common name on the mTLS one.
func actor(r *http.Request) string {
	if u := userFrom(r); u != nil {
		return u.Username
	}
	if cert := peerCert(r); cert != nil {
		return cert.Subject.CommonName + " (certificate)"
	}
	return "unauthenticated"
}

// userError maps an account error to a status code, so the UI can show the reason
// rather than a bare 500.
func (s *Server) userError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNoSuchUser):
		writeAuthError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrUserExists):
		writeAuthError(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrLastAdmin):
		writeAuthError(w, http.StatusConflict,
			"refusing to leave the system without an administrator")
	case errors.Is(err, ErrBadUsername), errors.Is(err, ErrBadRole),
		errors.Is(err, crypto.ErrPasswordTooShort):
		writeAuthError(w, http.StatusBadRequest, err.Error())
	default:
		s.log.Printf("user operation: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
