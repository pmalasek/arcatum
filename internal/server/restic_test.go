package server

import (
	"crypto/x509"
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

// A valid restic object name is a hex id; these are the shapes the backend must accept
// and reject.
func TestResticObjectPath(t *testing.T) {
	const repo = "/backup/restic/inst"
	name64 := strings.Repeat("ab", 32)

	tests := []struct {
		name    string
		rest    string
		want    string
		wantErr bool
	}{
		{"config", "config", repo + "/config", false},
		{"snapshot", "snapshots/" + name64, repo + "/snapshots/" + name64, false},
		{"index", "index/" + name64, repo + "/index/" + name64, false},
		{"lock", "locks/" + name64, repo + "/locks/" + name64, false},
		// Pack files are sharded by the first two hex characters.
		{"data is sharded", "data/" + name64, repo + "/data/ab/" + name64, false},

		{"unknown type", "bogus/" + name64, "", true},
		{"missing name", "data/", "", true},
		{"name not hex", "data/" + strings.Repeat("z", 64), "", true},
		{"name too short", "data/abc", "", true},
		{"traversal in name", "data/../../etc/passwd", "", true},
		{"traversal as type", "../../etc/passwd", "", true},
		{"absolute escape", "data/" + name64 + "/../../../etc/passwd", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resticObjectPath(repo, tc.rest)
			if tc.wantErr {
				if err == nil {
					t.Errorf("resticObjectPath(%q) = %q, want error", tc.rest, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resticObjectPath(%q): %v", tc.rest, err)
			}
			if got != tc.want {
				t.Errorf("resticObjectPath(%q) = %q, want %q", tc.rest, got, tc.want)
			}
			// Whatever the input, the result must stay inside the repository.
			if !strings.HasPrefix(filepath.Clean(got), repo) {
				t.Errorf("path %q escaped the repository", got)
			}
		})
	}
}

func TestSplitResticPath(t *testing.T) {
	tests := []struct {
		path         string
		wantInstance string
		wantRest     string
		wantErr      bool
	}{
		{"/restic/files-web01/config", "files-web01", "config", false},
		{"/restic/files-web01/data/", "files-web01", "data/", false},
		{"/restic/files-web01/", "files-web01", "", false},
		{"/restic/files-web01", "files-web01", "", false},
		{"/restic/../etc/passwd", "", "", true},
		{"/restic/bad id/config", "", "", true},
		{"/restic//config", "", "", true},
		{"/other/path", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			inst, rest, err := splitResticPath(tc.path)
			if tc.wantErr {
				if err == nil {
					t.Errorf("splitResticPath(%q) = %q/%q, want error", tc.path, inst, rest)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitResticPath(%q): %v", tc.path, err)
			}
			if inst != tc.wantInstance || rest != tc.wantRest {
				t.Errorf("splitResticPath(%q) = %q/%q, want %q/%q", tc.path, inst, rest, tc.wantInstance, tc.wantRest)
			}
		})
	}
}

// resticTestServer wires a Server over a real store with one instance targeted at
// runner "web-01".
func resticTestServer(t *testing.T, requireClientCert bool) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	backupDir := filepath.Join(dir, "backup")
	st, err := Open(filepath.Join(dir, "test.db"), backupDir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	path := writeInstances(t, dir, []*seedInstance{{
		Instance: Instance{ID: "files-web01", Script: "files-backup", RunnerID: "web-01"},
	}})
	if _, err := st.ImportInstances(path, true); err != nil {
		t.Fatalf("ImportInstances: %v", err)
	}
	// Mirror what New() wires up, so handlers that read the scheduler or catalog work.
	srv := &Server{
		store:             st,
		log:               log.New(io.Discard, "", 0),
		requireClientCert: requireClientCert,
		sched:             NewScheduler(time.UTC),
		catalog:           &Catalog{byName: map[string]*ScriptEntry{}},
	}
	schedules, err := st.Schedules()
	if err != nil {
		t.Fatalf("Schedules: %v", err)
	}
	for _, sc := range schedules {
		if err := srv.sched.TrackSchedule(sc, time.Now()); err != nil {
			t.Fatalf("TrackSchedule: %v", err)
		}
	}
	return srv, backupDir
}

func mustInstances(t *testing.T, st *Store) []*Instance {
	t.Helper()
	list, err := st.Instances()
	if err != nil {
		t.Fatalf("Instances: %v", err)
	}
	return list
}

// resticRequest issues a request against the restic backend with an optional cert.
func resticRequest(srv *Server, method, path string, body io.Reader, cert *x509.Certificate) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, body)
	if cert != nil {
		r.TLS = requestWithCert(cert).TLS
	}
	rec := httptest.NewRecorder()
	srv.handleRestic(rec, r)
	return rec
}

func TestResticRepoLifecycle(t *testing.T) {
	srv, backupDir := resticTestServer(t, false)
	name := strings.Repeat("ab", 32)

	// Create the repository.
	if rec := resticRequest(srv, http.MethodPost, "/restic/files-web01/?create=true", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("create = %d, want 200", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(backupDir, "restic", "files-web01", "data")); err != nil {
		t.Fatalf("repository layout not created: %v", err)
	}

	// A missing object is a 404, which is how restic detects an empty repository.
	if rec := resticRequest(srv, http.MethodGet, "/restic/files-web01/config", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET missing config = %d, want 404", rec.Code)
	}

	// Store, read back, and confirm it landed in the sharded location.
	if rec := resticRequest(srv, http.MethodPost, "/restic/files-web01/data/"+name, strings.NewReader("packdata"), nil); rec.Code != http.StatusOK {
		t.Fatalf("POST data = %d, want 200", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(backupDir, "restic", "files-web01", "data", "ab", name)); err != nil {
		t.Errorf("pack file not stored in the sharded path: %v", err)
	}
	rec := resticRequest(srv, http.MethodGet, "/restic/files-web01/data/"+name, nil, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "packdata" {
		t.Errorf("GET data = %d/%q, want 200/packdata", rec.Code, rec.Body.String())
	}

	// HEAD must report the size without a body.
	rec = resticRequest(srv, http.MethodHead, "/restic/files-web01/data/"+name, nil, nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Length") != "8" {
		t.Errorf("HEAD = %d, Content-Length %q; want 200/8", rec.Code, rec.Header().Get("Content-Length"))
	}

	// restic treats stored objects as immutable.
	if rec := resticRequest(srv, http.MethodPost, "/restic/files-web01/data/"+name, strings.NewReader("other"), nil); rec.Code != http.StatusForbidden {
		t.Errorf("overwrite = %d, want 403", rec.Code)
	}

	// Delete, then confirm it is gone.
	if rec := resticRequest(srv, http.MethodDelete, "/restic/files-web01/data/"+name, nil, nil); rec.Code != http.StatusOK {
		t.Errorf("DELETE = %d, want 200", rec.Code)
	}
	if rec := resticRequest(srv, http.MethodGet, "/restic/files-web01/data/"+name, nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", rec.Code)
	}
}

func TestResticListing(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	a := strings.Repeat("aa", 32)
	b := strings.Repeat("bb", 32)
	resticRequest(srv, http.MethodPost, "/restic/files-web01/?create=true", nil, nil)
	resticRequest(srv, http.MethodPost, "/restic/files-web01/data/"+a, strings.NewReader("12345"), nil)
	resticRequest(srv, http.MethodPost, "/restic/files-web01/data/"+b, strings.NewReader("123"), nil)

	// API v1: plain array of names.
	rec := resticRequest(srv, http.MethodGet, "/restic/files-web01/data/", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list v1 = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, a) || !strings.Contains(body, b) {
		t.Errorf("v1 listing %s does not contain both packs", body)
	}
	if strings.Contains(body, "size") {
		t.Errorf("v1 listing should not include sizes: %s", body)
	}

	// API v2: objects with sizes, and the matching content type.
	r := httptest.NewRequest(http.MethodGet, "/restic/files-web01/data/", nil)
	r.Header.Set("Accept", resticAPIv2)
	rec = httptest.NewRecorder()
	srv.handleRestic(rec, r)
	if ct := rec.Header().Get("Content-Type"); ct != resticAPIv2 {
		t.Errorf("v2 Content-Type = %q, want %q", ct, resticAPIv2)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"size":5`) {
		t.Errorf("v2 listing missing sizes: %s", body)
	}

	// Listing a type that has no directory yet must be an empty list, not an error.
	rec = resticRequest(srv, http.MethodGet, "/restic/files-web01/snapshots/", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "[]") {
		t.Errorf("empty listing = %d/%q, want 200/[]", rec.Code, rec.Body.String())
	}
}

// In-progress uploads must not appear in listings, or restic would try to fetch them.
func TestResticListingSkipsTempFiles(t *testing.T) {
	srv, backupDir := resticTestServer(t, false)
	resticRequest(srv, http.MethodPost, "/restic/files-web01/?create=true", nil, nil)
	shard := filepath.Join(backupDir, "restic", "files-web01", "data", "ab")
	if err := os.MkdirAll(shard, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(shard, ".upload-123"), []byte("partial"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	rec := resticRequest(srv, http.MethodGet, "/restic/files-web01/data/", nil, nil)
	if strings.Contains(rec.Body.String(), "upload") {
		t.Errorf("listing exposed a temporary upload file: %s", rec.Body.String())
	}
}

func TestResticUnknownInstanceIsRejected(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	rec := resticRequest(srv, http.MethodPost, "/restic/does-not-exist/?create=true", nil, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("unknown instance = %d, want 403", rec.Code)
	}
}

// A runner must reach only the repositories of instances targeted at it — otherwise one
// backed-up host could read or corrupt another's backups.
func TestResticRepoAuthorization(t *testing.T) {
	srv, _ := resticTestServer(t, true)
	name := strings.Repeat("ab", 32)

	tests := []struct {
		name     string
		cert     *x509.Certificate
		wantCode int
	}{
		{"owning runner allowed", certWithCNAndRole("web-01", crypto.RoleRunner), http.StatusOK},
		{"admin allowed", certWithCNAndRole("petr", crypto.RoleAdmin), http.StatusOK},
		{"other runner forbidden", certWithCNAndRole("db-01", crypto.RoleRunner), http.StatusForbidden},
		{"roleless certificate forbidden", certWithCNAndRole("web-01", ""), http.StatusForbidden},
		{"no certificate forbidden", nil, http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := resticRequest(srv, http.MethodPost, "/restic/files-web01/?create=true", nil, tc.cert)
			if rec.Code != tc.wantCode {
				t.Errorf("create = %d, want %d", rec.Code, tc.wantCode)
			}
			// Reads must be gated the same way as writes.
			rec = resticRequest(srv, http.MethodGet, "/restic/files-web01/data/"+name, nil, tc.cert)
			if tc.wantCode == http.StatusForbidden && rec.Code != http.StatusForbidden {
				t.Errorf("GET = %d, want 403", rec.Code)
			}
			if tc.wantCode == http.StatusOK && rec.Code == http.StatusForbidden {
				t.Errorf("GET = 403, want the request to be authorized")
			}
		})
	}
}

func TestResticRejectsBadMethods(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	if rec := resticRequest(srv, http.MethodPut, "/restic/files-web01/data/"+strings.Repeat("ab", 32), nil, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT = %d, want 405", rec.Code)
	}
	if rec := resticRequest(srv, http.MethodPost, "/restic/files-web01/data/", nil, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST to a listing = %d, want 405", rec.Code)
	}
	if rec := resticRequest(srv, http.MethodGet, "/restic/files-web01/", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET repo root = %d, want 404", rec.Code)
	}
}

func TestResticRepoInfo(t *testing.T) {
	srv, _ := resticTestServer(t, false)

	// A repository that does not exist yet reports exists=false rather than failing.
	info, err := srv.resticRepoInfo("files-web01")
	if err != nil {
		t.Fatalf("resticRepoInfo: %v", err)
	}
	if info.Exists || info.Bytes != 0 {
		t.Errorf("info = %+v, want exists=false and no bytes", info)
	}

	resticRequest(srv, http.MethodPost, "/restic/files-web01/?create=true", nil, nil)
	resticRequest(srv, http.MethodPost, "/restic/files-web01/data/"+strings.Repeat("ab", 32), strings.NewReader("packdata"), nil)
	resticRequest(srv, http.MethodPost, "/restic/files-web01/snapshots/"+strings.Repeat("cd", 32), strings.NewReader("{}"), nil)

	info, err = srv.resticRepoInfo("files-web01")
	if err != nil {
		t.Fatalf("resticRepoInfo: %v", err)
	}
	if !info.Exists {
		t.Error("info.Exists = false, want true")
	}
	if info.Packs != 1 || info.Snapshots != 1 {
		t.Errorf("packs/snapshots = %d/%d, want 1/1", info.Packs, info.Snapshots)
	}
	if info.Bytes != int64(len("packdata")+len("{}")) {
		t.Errorf("bytes = %d, want %d", info.Bytes, len("packdata")+len("{}"))
	}
}

// Handler() panics on conflicting route patterns, which unit tests calling handlers
// directly never notice. Building the mux here catches that class of mistake.
func TestHandlerRoutesDoNotConflict(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	h := srv.Handler() // panics if two patterns are ambiguous

	tests := []struct {
		method     string
		path       string
		wantStatus int
	}{
		{http.MethodGet, "/", http.StatusOK},
		{http.MethodGet, "/api/v1/runs", http.StatusOK},
		{http.MethodGet, "/api/v1/runners", http.StatusOK},
		{http.MethodGet, "/api/v1/instances/files-web01/repo", http.StatusOK},
		// The restic backend must win over the root handler.
		{http.MethodPost, "/restic/files-web01/?create=true", http.StatusOK},
		{http.MethodGet, "/restic/files-web01/data/", http.StatusOK},
		{http.MethodGet, "/nonexistent", http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != tc.wantStatus {
				t.Errorf("%s %s = %d, want %d (body %q)", tc.method, tc.path, rec.Code, tc.wantStatus,
					strings.TrimSpace(rec.Body.String()))
			}
		})
	}
}
