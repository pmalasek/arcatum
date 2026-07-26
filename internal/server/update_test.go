package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"arcatum/pkg/crypto"
)

// publishBuild writes a runner binary and a VERSION file, which is all publishing means.
func publishBuild(t *testing.T, dir, version string, platforms map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for platform, content := range platforms {
		path := filepath.Join(dir, "arcatum-runner-"+platform)
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if version != "" {
		if err := os.WriteFile(filepath.Join(dir, versionFile), []byte(version+"\n"), 0o644); err != nil {
			t.Fatalf("write VERSION: %v", err)
		}
	}
}

func fetchUpdateManifest(t *testing.T, srv *Server) UpdateManifest {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/update", nil)
	if srv.requireClientCert {
		r.TLS = requestWithCert(adminCert()).TLS
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("update manifest = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var m UpdateManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

// Publishing a binary must require the dispatch-signing key, so the manifest is signed
// with it — control of the server alone must not be enough to ship an executable.
func TestUpdateManifestIsSignedAndHashesBinaries(t *testing.T) {
	srv, _, set := rotationServer(t, false)
	dist := t.TempDir()
	srv.dist = &distCache{dir: dist}
	publishBuild(t, dist, "2026.07.26", map[string]string{
		"linux-amd64": "ELF-AMD64",
		"linux-arm64": "ELF-ARM64",
	})

	m := fetchUpdateManifest(t, srv)
	if m.Version != "2026.07.26" {
		t.Errorf("version = %q, want 2026.07.26", m.Version)
	}
	if len(m.Binaries) != 2 {
		t.Fatalf("binaries = %v, want two platforms", m.Binaries)
	}
	sum := sha256.Sum256([]byte("ELF-AMD64"))
	if m.Binaries["linux-amd64"] != hex.EncodeToString(sum[:]) {
		t.Errorf("hash for linux-amd64 does not match the file")
	}
	if len(m.Signatures) != 1 {
		t.Fatalf("signatures = %v, want one per signing key", m.Signatures)
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signatures[0])
	if err != nil {
		t.Fatalf("signature not base64: %v", err)
	}
	if err := set.Verify(updateManifestBytesToSign(m.Version, m.Binaries), sig); err != nil {
		t.Errorf("manifest does not verify against the trusted key: %v", err)
	}
}

// Tampering with the manifest must break the signature: no build may be added, removed or
// swapped for another.
func TestUpdateManifestTamperingIsDetected(t *testing.T) {
	srv, _, set := rotationServer(t, false)
	dist := t.TempDir()
	srv.dist = &distCache{dir: dist}
	publishBuild(t, dist, "1.0", map[string]string{"linux-amd64": "GOOD"})

	m := fetchUpdateManifest(t, srv)
	sig, _ := base64.StdEncoding.DecodeString(m.Signatures[0])

	tests := []struct {
		name     string
		version  string
		binaries map[string]string
	}{
		{"swapped hash", m.Version, map[string]string{"linux-amd64": strings.Repeat("ff", 32)}},
		{"added platform", m.Version, map[string]string{
			"linux-amd64": m.Binaries["linux-amd64"], "linux-arm64": strings.Repeat("aa", 32)}},
		{"removed platform", m.Version, map[string]string{}},
		{"changed version", "9.9", m.Binaries},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := set.Verify(updateManifestBytesToSign(tc.version, tc.binaries), sig); err == nil {
				t.Error("tampering must invalidate the signature")
			}
		})
	}
}

// Without a VERSION file nothing is advertised: serving binaries with no version would let
// a runner install an unknown build.
func TestUpdateManifestNeedsVersionFile(t *testing.T) {
	srv, _, _ := rotationServer(t, false)
	dist := t.TempDir()
	srv.dist = &distCache{dir: dist}
	publishBuild(t, dist, "", map[string]string{"linux-amd64": "ELF"})

	m := fetchUpdateManifest(t, srv)
	if m.Version != "" || len(m.Binaries) != 0 {
		t.Errorf("manifest = %+v, want nothing advertised without a VERSION file", m)
	}
}

func TestUpdateManifestEmptyWithoutDistDir(t *testing.T) {
	srv, _, _ := rotationServer(t, false)
	srv.dist = &distCache{}
	m := fetchUpdateManifest(t, srv)
	if m.Version != "" || len(m.Binaries) != 0 {
		t.Errorf("manifest = %+v, want empty", m)
	}
}

// A binary must be downloadable over the authenticated connection, and only names of the
// published shape may be requested.
func TestUpdateDownload(t *testing.T) {
	srv, _, _ := rotationServer(t, false)
	dist := t.TempDir()
	srv.dist = &distCache{dir: dist}
	publishBuild(t, dist, "1.0", map[string]string{"linux-amd64": "ELF-BINARY"})
	// A file that exists but is not a published build name.
	if err := os.WriteFile(filepath.Join(dist, "secret.key"), []byte("PRIVATE"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/update/arcatum-runner-linux-amd64", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ELF-BINARY" {
		t.Errorf("download = %d/%q", rec.Code, rec.Body.String())
	}

	for _, name := range []string{"secret.key", "VERSION", "arcatum-runner-linux-arm64", "../../etc/passwd"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/update/"+name, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("GET /api/v1/update/%s = 200, want a refusal", name)
		}
	}
}

// An update is trust material, so a revoked runner must not be able to fetch one — and an
// unauthenticated caller must not reach it at all.
func TestUpdateEndpointsAuthorization(t *testing.T) {
	srv, _, _ := rotationServer(t, true)
	dist := t.TempDir()
	srv.dist = &distCache{dir: dist}
	publishBuild(t, dist, "1.0", map[string]string{"linux-amd64": "ELF"})
	enrolledRunner(t, srv, "web-01")

	get := func(path string, cert bool, runner bool) int {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if cert {
			if runner {
				r.TLS = requestWithCert(certWithCNAndRole("web-01", crypto.RoleRunner)).TLS
			} else {
				r.TLS = requestWithCert(adminCert()).TLS
			}
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, r)
		return rec.Code
	}

	if code := get("/api/v1/update", true, true); code != http.StatusOK {
		t.Errorf("approved runner = %d, want 200", code)
	}
	if code := get("/api/v1/update", true, false); code != http.StatusOK {
		t.Errorf("admin = %d, want 200", code)
	}
	if code := get("/api/v1/update", false, false); code != http.StatusUnauthorized {
		t.Errorf("no certificate = %d, want 401", code)
	}

	// After revocation the runner is cut off from updates too.
	if rec := adminPost(t, srv, "/api/v1/runners/web-01/revoke", adminCert()); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d", rec.Code)
	}
	if code := get("/api/v1/update", true, true); code != http.StatusForbidden {
		t.Errorf("revoked runner = %d, want 403", code)
	}
	if code := get("/api/v1/update/arcatum-runner-linux-amd64", true, true); code != http.StatusForbidden {
		t.Errorf("revoked runner download = %d, want 403", code)
	}
}

// Hashing every binary on every check-in would be wasteful; the cache must still notice a
// republished directory.
func TestDistCacheRefreshesOnChange(t *testing.T) {
	dist := t.TempDir()
	d := &distCache{dir: dist}
	publishBuild(t, dist, "1.0", map[string]string{"linux-amd64": "FIRST"})

	v, bins, err := d.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	first := bins["linux-amd64"]
	if v != "1.0" || first == "" {
		t.Fatalf("load = %q/%v", v, bins)
	}
	// Same directory state: the cached value comes back.
	v2, bins2, _ := d.load()
	if v2 != v || bins2["linux-amd64"] != first {
		t.Error("cache should return the same result for an unchanged directory")
	}

	// Republishing changes the directory, so the cache must reload.
	publishBuild(t, dist, "2.0", map[string]string{"linux-amd64": "SECOND"})
	if err := os.Chtimes(dist, timeNowPlus(), timeNowPlus()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	v3, bins3, err := d.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if v3 != "2.0" || bins3["linux-amd64"] == first {
		t.Errorf("cache did not pick up the republished build: %q/%v", v3, bins3)
	}
}

// The runner reports its build so an operator can see which hosts have picked up a
// published update.
func TestCheckinRecordsRunnerVersion(t *testing.T) {
	srv, _, _ := rotationServer(t, true)
	enrolledRunner(t, srv, "web-01")

	body := `{"runner_id":"web-01","hostname":"web-01","os":"linux","arch":"amd64","version":"2026.07.26"}`
	r := httptest.NewRequest(http.MethodPost, "/api/v1/checkin", strings.NewReader(body))
	r.TLS = requestWithCert(certWithCNAndRole("web-01", crypto.RoleRunner)).TLS
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("checkin = %d (%s)", rec.Code, rec.Body.String())
	}
	runners, err := srv.store.Runners()
	if err != nil || len(runners) == 0 {
		t.Fatalf("Runners: %v", err)
	}
	if runners[0].Version != "2026.07.26" {
		t.Errorf("version = %q, want 2026.07.26", runners[0].Version)
	}

	// A check-in without a version must not erase a known one.
	body = `{"runner_id":"web-01","hostname":"web-01","os":"linux","arch":"amd64"}`
	r = httptest.NewRequest(http.MethodPost, "/api/v1/checkin", strings.NewReader(body))
	r.TLS = requestWithCert(certWithCNAndRole("web-01", crypto.RoleRunner)).TLS
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	runners, _ = srv.store.Runners()
	if runners[0].Version != "2026.07.26" {
		t.Errorf("version = %q, want it kept", runners[0].Version)
	}
}

// timeNowPlus returns a timestamp in the future, to force a cache reload in tests.
func timeNowPlus() time.Time { return time.Now().Add(time.Second) }
