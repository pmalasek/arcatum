package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"arcatum/pkg/crypto"
)

// TestMain lowers the password work factor for the whole package. Whether the hashing
// itself is right is pkg/crypto's business; here it would only mean 600 000 iterations
// per account in every test that logs in.
func TestMain(m *testing.M) {
	crypto.PasswordIterations = 1000
	os.Exit(m.Run())
}

// webServer builds a server with an empty store, as the plain-HTTP web listener sees it.
func webServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "test.db"), filepath.Join(dir, "backup"), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &Server{
		store:   st,
		log:     log.New(io.Discard, "", 0),
		sched:   NewScheduler(time.UTC),
		catalog: &Catalog{byName: map[string]*ScriptEntry{}},
		logins:  newLoginLimiter(),
	}
}

// mustUser creates an account or fails the test.
func mustUser(t *testing.T, s *Server, name, password, role string) *User {
	t.Helper()
	u, err := s.store.CreateUser(name, password, role)
	if err != nil {
		t.Fatalf("CreateUser(%q): %v", name, err)
	}
	return u
}

// webCall performs a request against the web listener's router. Origin is set to the
// request host, as a browser on the UI's own page would.
func webCall(t *testing.T, s *Server, method, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = strings.NewReader(string(encoded))
	}
	r := httptest.NewRequest(method, path, reader)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://"+r.Host)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.WebHandler().ServeHTTP(rec, r)
	return rec
}

// login logs in over the web listener and returns the session cookie.
func login(t *testing.T, s *Server, username, password string) *http.Cookie {
	t.Helper()
	rec := webCall(t, s, http.MethodPost, "/api/v1/login",
		map[string]string{"username": username, "password": password}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login %q: status %d, body %s", username, rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	t.Fatalf("login %q: no %s cookie in response", username, sessionCookie)
	return nil
}

// --- store ------------------------------------------------------------------

func TestCreateAndAuthenticateUser(t *testing.T) {
	s := webServer(t)
	mustUser(t, s, "petr", "correct horse", UserRoleAdmin)

	u, err := s.store.Authenticate("petr", "correct horse")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if u.Username != "petr" || !u.IsAdmin() {
		t.Errorf("Authenticate = %+v; want admin petr", u)
	}
	// A wrong password and an unknown account must look the same from outside.
	if _, err := s.store.Authenticate("petr", "wrong password"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("wrong password err = %v; want ErrBadCredentials", err)
	}
	if _, err := s.store.Authenticate("nobody", "correct horse"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("unknown user err = %v; want ErrBadCredentials", err)
	}
}

// The database must not hold anything that can be replayed as a password.
func TestUserPasswordIsNotStoredInClear(t *testing.T) {
	s := webServer(t)
	mustUser(t, s, "petr", "correct horse", UserRoleAdmin)

	var hash string
	if err := s.store.db.QueryRow(`SELECT pass_hash FROM users WHERE username = 'petr'`).Scan(&hash); err != nil {
		t.Fatalf("select: %v", err)
	}
	if strings.Contains(hash, "correct horse") {
		t.Errorf("stored hash %q contains the password", hash)
	}
	if !strings.HasPrefix(hash, "pbkdf2-sha256$") {
		t.Errorf("stored hash %q is not a PBKDF2 verifier", hash)
	}
}

func TestUsernameIsCaseInsensitive(t *testing.T) {
	s := webServer(t)
	mustUser(t, s, "Petr ", "correct horse", UserRoleAdmin)

	u, err := s.store.User("PETR")
	if err != nil || u == nil {
		t.Fatalf("User(PETR) = %v, %v; want the account", u, err)
	}
	if u.Username != "petr" {
		t.Errorf("username stored as %q; want normalized %q", u.Username, "petr")
	}
	if _, err := s.store.CreateUser("PETR", "another password", UserRoleViewer); !errors.Is(err, ErrUserExists) {
		t.Errorf("creating the same account in different case: err = %v; want ErrUserExists", err)
	}
}

func TestCreateUserRejectsBadInput(t *testing.T) {
	s := webServer(t)
	cases := []struct {
		name, username, password, role string
		want                           error
	}{
		{"short password", "petr", "short", UserRoleAdmin, crypto.ErrPasswordTooShort},
		{"bad username", "petr!", "correct horse", UserRoleAdmin, ErrBadUsername},
		{"empty username", "", "correct horse", UserRoleAdmin, ErrBadUsername},
		{"unknown role", "petr", "correct horse", "root", ErrBadRole},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.store.CreateUser(tc.username, tc.password, tc.role); !errors.Is(err, tc.want) {
				t.Errorf("err = %v; want %v", err, tc.want)
			}
		})
	}
}

func TestDisabledUserCannotAuthenticate(t *testing.T) {
	s := webServer(t)
	mustUser(t, s, "petr", "correct horse", UserRoleAdmin)
	mustUser(t, s, "hana", "another password", UserRoleAdmin)

	if err := s.store.SetUserDisabled("hana", true); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	if _, err := s.store.Authenticate("hana", "another password"); !errors.Is(err, ErrUserDisabled) {
		t.Errorf("err = %v; want ErrUserDisabled", err)
	}
	// Re-enabling restores access without touching the password.
	if err := s.store.SetUserDisabled("hana", false); err != nil {
		t.Fatalf("SetUserDisabled(false): %v", err)
	}
	if _, err := s.store.Authenticate("hana", "another password"); err != nil {
		t.Errorf("Authenticate after re-enabling: %v", err)
	}
}

// Locking everyone out is only recoverable from a shell on the server, so the store
// refuses the change rather than allowing it.
func TestLastAdminIsProtected(t *testing.T) {
	s := webServer(t)
	mustUser(t, s, "petr", "correct horse", UserRoleAdmin)
	mustUser(t, s, "hana", "another password", UserRoleViewer)

	if err := s.store.DeleteUser("petr"); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("deleting the last admin: err = %v; want ErrLastAdmin", err)
	}
	if err := s.store.SetUserRole("petr", UserRoleViewer); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("demoting the last admin: err = %v; want ErrLastAdmin", err)
	}
	if err := s.store.SetUserDisabled("petr", true); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("disabling the last admin: err = %v; want ErrLastAdmin", err)
	}
	// A viewer is not an admin, so removing one is always allowed.
	if err := s.store.DeleteUser("hana"); err != nil {
		t.Errorf("deleting a viewer: %v", err)
	}
	// With a second admin, the first may go.
	mustUser(t, s, "hana", "another password", UserRoleAdmin)
	if err := s.store.DeleteUser("petr"); err != nil {
		t.Errorf("deleting an admin while another exists: %v", err)
	}
}

// --- sessions ---------------------------------------------------------------

func TestSessionLifecycle(t *testing.T) {
	s := webServer(t)
	mustUser(t, s, "petr", "correct horse", UserRoleAdmin)

	now := time.Now()
	token, err := s.store.CreateSession("petr", "10.0.0.1", now, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	u, sess, err := s.store.SessionUser(token, now)
	if err != nil || u == nil {
		t.Fatalf("SessionUser = %v, %v", u, err)
	}
	if u.Username != "petr" || sess.IP != "10.0.0.1" {
		t.Errorf("session = %+v / %+v; want petr from 10.0.0.1", u, sess)
	}
	// The token must not be stored in a form that can be replayed.
	var stored string
	if err := s.store.db.QueryRow(`SELECT token_hash FROM sessions`).Scan(&stored); err != nil {
		t.Fatalf("select: %v", err)
	}
	if stored == token {
		t.Error("the session token is stored verbatim; only its hash should be")
	}
	// Expired is as good as unknown.
	if u, _, err := s.store.SessionUser(token, now.Add(2*time.Hour)); err != nil || u != nil {
		t.Errorf("expired session resolved to %v, %v; want nil, nil", u, err)
	}
	// ...and the expired row is cleaned up on the way.
	if u, _, err := s.store.SessionUser(token, now); err != nil || u != nil {
		t.Errorf("session survived expiry: %v, %v", u, err)
	}
}

func TestSessionsEndWhenAccountChanges(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name   string
		change func(t *testing.T, s *Server)
	}{
		{"password change", func(t *testing.T, s *Server) {
			if err := s.store.SetUserPassword("hana", "brand new password"); err != nil {
				t.Fatalf("SetUserPassword: %v", err)
			}
		}},
		{"disabled", func(t *testing.T, s *Server) {
			if err := s.store.SetUserDisabled("hana", true); err != nil {
				t.Fatalf("SetUserDisabled: %v", err)
			}
		}},
		{"deleted", func(t *testing.T, s *Server) {
			if err := s.store.DeleteUser("hana"); err != nil {
				t.Fatalf("DeleteUser: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := webServer(t)
			mustUser(t, s, "petr", "correct horse", UserRoleAdmin) // so hana is not the last admin
			mustUser(t, s, "hana", "another password", UserRoleAdmin)
			token, err := s.store.CreateSession("hana", "", now, now.Add(time.Hour))
			if err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			tc.change(t, s)
			if u, _, err := s.store.SessionUser(token, now); err != nil || u != nil {
				t.Errorf("session still valid after %s: %v, %v", tc.name, u, err)
			}
		})
	}
}

func TestPurgeExpiredSessions(t *testing.T) {
	s := webServer(t)
	mustUser(t, s, "petr", "correct horse", UserRoleAdmin)
	now := time.Now()
	if _, err := s.store.CreateSession("petr", "", now, now.Add(-time.Minute)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	live, err := s.store.CreateSession("petr", "", now, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	n, err := s.store.PurgeExpiredSessions(now)
	if err != nil {
		t.Fatalf("PurgeExpiredSessions: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d sessions; want 1", n)
	}
	if u, _, err := s.store.SessionUser(live, now); err != nil || u == nil {
		t.Errorf("the live session was purged too: %v, %v", u, err)
	}
}

// --- login and guards -------------------------------------------------------

func TestLoginSetsUsableSessionCookie(t *testing.T) {
	s := webServer(t)
	mustUser(t, s, "petr", "correct horse", UserRoleAdmin)

	cookie := login(t, s, "petr", "correct horse")
	if !cookie.HttpOnly {
		t.Error("the session cookie must be HttpOnly")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie SameSite = %v; want Strict", cookie.SameSite)
	}
	// The cookie must actually authenticate.
	rec := webCall(t, s, http.MethodGet, "/api/v1/whoami", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("whoami with cookie: status %d", rec.Code)
	}
	var id Identity
	if err := json.Unmarshal(rec.Body.Bytes(), &id); err != nil {
		t.Fatalf("decode whoami: %v", err)
	}
	if id.Name != "petr" || id.Role != UserRoleAdmin || id.Auth != "password" {
		t.Errorf("whoami = %+v; want petr/admin/password", id)
	}
	// And the login must be recorded, which is what the UI shows for an account.
	u, err := s.store.User("petr")
	if err != nil || u == nil {
		t.Fatalf("User: %v, %v", u, err)
	}
	if u.LastLogin.IsZero() {
		t.Error("last_login was not recorded")
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	s := webServer(t)
	mustUser(t, s, "petr", "correct horse", UserRoleAdmin)

	rec := webCall(t, s, http.MethodPost, "/api/v1/login",
		map[string]string{"username": "petr", "password": "wrong password"}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Error("a failed login must not set a session cookie")
		}
	}
}

func TestWebAPIRequiresLogin(t *testing.T) {
	s := webServer(t)
	mustUser(t, s, "petr", "correct horse", UserRoleAdmin)

	for _, path := range []string{"/api/v1/whoami", "/api/v1/runs", "/api/v1/instances",
		"/api/v1/runners", "/api/v1/users", "/status"} {
		rec := webCall(t, s, http.MethodGet, path, nil, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a session: status %d; want 401", path, rec.Code)
		}
	}
	// A made-up cookie is no better than none.
	fake := &http.Cookie{Name: sessionCookie, Value: "not-a-real-token"}
	if rec := webCall(t, s, http.MethodGet, "/api/v1/runs", nil, fake); rec.Code != http.StatusUnauthorized {
		t.Errorf("forged cookie: status %d; want 401", rec.Code)
	}
}

// The UI itself must load without a session — it is the page that asks for the login.
func TestWebUIAssetsAreServedWithoutLogin(t *testing.T) {
	s := webServer(t)
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		rec := webCall(t, s, http.MethodGet, path, nil, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status %d; want 200", path, rec.Code)
		}
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	s := webServer(t)
	mustUser(t, s, "petr", "correct horse", UserRoleAdmin)
	cookie := login(t, s, "petr", "correct horse")

	if rec := webCall(t, s, http.MethodPost, "/api/v1/logout", nil, cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("logout: status %d", rec.Code)
	}
	if rec := webCall(t, s, http.MethodGet, "/api/v1/whoami", nil, cookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("the cookie still works after logout: status %d", rec.Code)
	}
}

// A viewer sees everything and changes nothing: that is the whole point of the role.
func TestViewerMayReadButNotWrite(t *testing.T) {
	s := webServer(t)
	mustUser(t, s, "petr", "correct horse", UserRoleAdmin)
	mustUser(t, s, "hana", "another password", UserRoleViewer)
	cookie := login(t, s, "hana", "another password")

	for _, path := range []string{"/api/v1/runs", "/api/v1/instances", "/api/v1/runners", "/status"} {
		if rec := webCall(t, s, http.MethodGet, path, nil, cookie); rec.Code != http.StatusOK {
			t.Errorf("viewer GET %s: status %d; want 200", path, rec.Code)
		}
	}
	writes := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/v1/instances/hello/run", nil},
		{http.MethodDelete, "/api/v1/instances/hello", nil},
		{http.MethodPost, "/api/v1/runners/web-01/approve", nil},
		{http.MethodPost, "/api/v1/runners/revoke-all", nil},
		{http.MethodPost, "/api/v1/secrets/rekey", nil},
		{http.MethodPost, "/api/v1/users", map[string]string{"username": "novy", "password": "some password"}},
		{http.MethodDelete, "/api/v1/users/petr", nil},
	}
	for _, w := range writes {
		rec := webCall(t, s, w.method, w.path, w.body, cookie)
		if rec.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s: status %d; want 403", w.method, w.path, rec.Code)
		}
	}
	// Account management is not even readable for a viewer.
	if rec := webCall(t, s, http.MethodGet, "/api/v1/users", nil, cookie); rec.Code != http.StatusForbidden {
		t.Errorf("viewer GET /api/v1/users: status %d; want 403", rec.Code)
	}
	// Nothing was created along the way.
	if users, err := s.store.Users(); err != nil || len(users) != 2 {
		t.Errorf("users = %d, %v; want the original 2", len(users), err)
	}
}

// A viewer must still be able to change their own password.
func TestViewerCanChangeOwnPassword(t *testing.T) {
	s := webServer(t)
	mustUser(t, s, "petr", "correct horse", UserRoleAdmin)
	mustUser(t, s, "hana", "another password", UserRoleViewer)
	cookie := login(t, s, "hana", "another password")

	rec := webCall(t, s, http.MethodPost, "/api/v1/password",
		map[string]string{"current": "wrong password", "new": "third password"}, cookie)
	if rec.Code != http.StatusForbidden {
		t.Errorf("wrong current password: status %d; want 403", rec.Code)
	}
	rec = webCall(t, s, http.MethodPost, "/api/v1/password",
		map[string]string{"current": "another password", "new": "third password"}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("change own password: status %d, body %s", rec.Code, rec.Body.String())
	}
	// The old password is gone, the new one works, and the old session does not.
	if _, err := s.store.Authenticate("hana", "another password"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("the old password still works: %v", err)
	}
	if _, err := s.store.Authenticate("hana", "third password"); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
	if rec := webCall(t, s, http.MethodGet, "/api/v1/whoami", nil, cookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("session survived a password change: status %d", rec.Code)
	}
}

// Another site must not be able to act through an operator's logged-in browser.
func TestCrossOriginWriteIsRefused(t *testing.T) {
	s := webServer(t)
	mustUser(t, s, "petr", "correct horse", UserRoleAdmin)
	cookie := login(t, s, "petr", "correct horse")

	r := httptest.NewRequest(http.MethodPost, "/api/v1/runners/revoke-all", nil)
	r.Header.Set("Origin", "http://evil.example")
	r.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.WebHandler().ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin POST: status %d; want 403", rec.Code)
	}

	// Same-origin (what the UI sends) is allowed through to the handler.
	if rec := webCall(t, s, http.MethodPost, "/api/v1/runners/revoke-all", nil, cookie); rec.Code == http.StatusForbidden {
		t.Errorf("same-origin POST was refused: %s", rec.Body.String())
	}
	// Sec-Fetch-Site is honoured even when Origin is absent.
	r = httptest.NewRequest(http.MethodPost, "/api/v1/runners/revoke-all", nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.AddCookie(cookie)
	rec = httptest.NewRecorder()
	s.WebHandler().ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST: status %d; want 403", rec.Code)
	}
}

func TestLoginThrottlesRepeatedFailures(t *testing.T) {
	s := webServer(t)
	mustUser(t, s, "petr", "correct horse", UserRoleAdmin)

	for i := 0; i < loginFailureGrace+1; i++ {
		webCall(t, s, http.MethodPost, "/api/v1/login",
			map[string]string{"username": "petr", "password": "wrong password"}, nil)
	}
	// Even the correct password now has to wait: throttling protects the deliberately
	// expensive password check from being called in a loop.
	rec := webCall(t, s, http.MethodPost, "/api/v1/login",
		map[string]string{"username": "petr", "password": "correct horse"}, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d; want 429", rec.Code)
	}
	// Another account is unaffected — one locked login must not lock the system.
	mustUser(t, s, "hana", "another password", UserRoleAdmin)
	if rec := webCall(t, s, http.MethodPost, "/api/v1/login",
		map[string]string{"username": "hana", "password": "another password"}, nil); rec.Code != http.StatusOK {
		t.Errorf("second account: status %d; want 200", rec.Code)
	}
}

// --- account management over the API ----------------------------------------

func TestAdminManagesUsersOverAPI(t *testing.T) {
	s := webServer(t)
	mustUser(t, s, "petr", "correct horse", UserRoleAdmin)
	cookie := login(t, s, "petr", "correct horse")

	// Created without a password: the server generates one and returns it exactly once.
	rec := webCall(t, s, http.MethodPost, "/api/v1/users",
		map[string]any{"username": "hana", "role": UserRoleViewer}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("create user: status %d, body %s", rec.Code, rec.Body.String())
	}
	var created userResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Password == "" {
		t.Fatal("no generated password returned")
	}
	if created.Role != UserRoleViewer {
		t.Errorf("role = %q; want viewer", created.Role)
	}
	if _, err := s.store.Authenticate("hana", created.Password); err != nil {
		t.Errorf("the generated password does not work: %v", err)
	}

	// Listing an account must never carry a password or its hash.
	rec = webCall(t, s, http.MethodGet, "/api/v1/users", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list users: status %d", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "pbkdf2") || strings.Contains(body, created.Password) {
		t.Errorf("the user list leaks credentials: %s", body)
	}

	// Promote, then reset the password, then delete.
	rec = webCall(t, s, http.MethodPut, "/api/v1/users/hana",
		map[string]any{"role": UserRoleAdmin}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("update role: status %d, body %s", rec.Code, rec.Body.String())
	}
	if u, _ := s.store.User("hana"); u == nil || !u.IsAdmin() {
		t.Errorf("hana = %+v; want admin", u)
	}
	rec = webCall(t, s, http.MethodPut, "/api/v1/users/hana",
		map[string]any{"generate_password": true}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset password: status %d", rec.Code)
	}
	var reset userResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &reset); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if reset.Password == "" || reset.Password == created.Password {
		t.Error("a reset must return a new generated password")
	}
	if rec := webCall(t, s, http.MethodDelete, "/api/v1/users/hana", nil, cookie); rec.Code != http.StatusNoContent {
		t.Errorf("delete user: status %d", rec.Code)
	}
}

func TestUserAPIRefusesLockout(t *testing.T) {
	s := webServer(t)
	mustUser(t, s, "petr", "correct horse", UserRoleAdmin)
	cookie := login(t, s, "petr", "correct horse")

	// Deleting the account you are logged in as: refused before the last-admin check even
	// comes up, because it would log you out mid-action.
	if rec := webCall(t, s, http.MethodDelete, "/api/v1/users/petr", nil, cookie); rec.Code != http.StatusConflict {
		t.Errorf("deleting own account: status %d; want 409", rec.Code)
	}
	// Demoting the only admin leaves nobody who can administer the system.
	rec := webCall(t, s, http.MethodPut, "/api/v1/users/petr",
		map[string]any{"role": UserRoleViewer}, cookie)
	if rec.Code != http.StatusConflict {
		t.Errorf("demoting the last admin: status %d; want 409", rec.Code)
	}
	if u, _ := s.store.User("petr"); u == nil || !u.IsAdmin() {
		t.Errorf("petr = %+v; want unchanged admin", u)
	}
}

func TestUserAPIErrorsAreSpecific(t *testing.T) {
	s := webServer(t)
	mustUser(t, s, "petr", "correct horse", UserRoleAdmin)
	cookie := login(t, s, "petr", "correct horse")

	cases := []struct {
		name   string
		method string
		path   string
		body   any
		want   int
	}{
		{"duplicate", http.MethodPost, "/api/v1/users",
			map[string]any{"username": "petr", "password": "correct horse"}, http.StatusConflict},
		{"bad username", http.MethodPost, "/api/v1/users",
			map[string]any{"username": "Petr Novák", "password": "correct horse"}, http.StatusBadRequest},
		{"short password", http.MethodPost, "/api/v1/users",
			map[string]any{"username": "hana", "password": "krátké"}, http.StatusBadRequest},
		{"unknown role", http.MethodPost, "/api/v1/users",
			map[string]any{"username": "hana", "password": "correct horse", "role": "root"}, http.StatusBadRequest},
		{"unknown user", http.MethodPut, "/api/v1/users/nobody",
			map[string]any{"role": UserRoleViewer}, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := webCall(t, s, tc.method, tc.path, tc.body, cookie)
			if rec.Code != tc.want {
				t.Errorf("status = %d; want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// The mTLS listener is for runners and for certificate-holding operators; it must not
// grow a password login, and the browser UI does not live there any more.
func TestRunnerListenerHasNoPasswordEndpoints(t *testing.T) {
	s := webServer(t)
	h := s.Handler()
	for _, path := range []string{"/api/v1/login", "/api/v1/logout", "/api/v1/users", "/app.js"} {
		r := httptest.NewRequest(http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s on the mTLS listener: status %d; want 404/405", path, rec.Code)
		}
	}
}
