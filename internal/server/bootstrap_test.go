package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bootstrapGet issues a request against the bootstrap (plain HTTP) handler.
func bootstrapGet(t *testing.T, srv *Server, cfg BootstrapConfig, path, host string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if host != "" {
		r.Host = host
	}
	rec := httptest.NewRecorder()
	srv.BootstrapHandler(cfg).ServeHTTP(rec, r)
	return rec
}

// install.sh is generated per request so the runner is configured to talk back to
// whichever address it was just downloaded from — nothing to configure twice.
func TestInstallScriptUsesRequestAddress(t *testing.T) {
	srv, _ := enrollTestServer(t, false)
	cfg := BootstrapConfig{APIURL: "https://172.24.0.60:8443"}

	rec := bootstrapGet(t, srv, cfg, "/arcatum_runner/install.sh", "172.24.0.60")
	if rec.Code != http.StatusOK {
		t.Fatalf("install.sh = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, `BOOTSTRAP_URL="http://172.24.0.60"`) {
		t.Errorf("install.sh does not carry the request address:\n%s", firstLines(body, 20))
	}
	if !strings.Contains(body, `API_URL="https://172.24.0.60:8443"`) {
		t.Errorf("install.sh does not carry the API address")
	}
	// A different address must produce a differently configured script.
	rec2 := bootstrapGet(t, srv, cfg, "/arcatum_runner/install.sh", "backup.xtuning.local:8080")
	if !strings.Contains(rec2.Body.String(), `BOOTSTRAP_URL="http://backup.xtuning.local:8080"`) {
		t.Errorf("install.sh ignored the request host")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "shellscript") {
		t.Errorf("Content-Type = %q, want a shell script type", ct)
	}
}

// The generated script must contain the pieces that make a one-command install work.
func TestInstallScriptContents(t *testing.T) {
	srv, _ := enrollTestServer(t, false)
	body := bootstrapGet(t, srv, BootstrapConfig{APIURL: "https://arcatum:8443"},
		"/arcatum_runner/install.sh", "arcatum").Body.String()

	for _, want := range []string{
		"set -euo pipefail",        // fail loudly rather than half-install
		"arcatum-runner-$OS-$ARCH", // platform-specific binary
		"ca.pem",                   // trust material
		"dispatch-signing.pub",     // job signature verification
		"runner.toml",              // configuration
		"systemd/system",           // service unit
		"enroll_server",            // how it asks for a certificate
		"keeping existing",         // re-running must not reset the configuration
	} {
		if !strings.Contains(body, want) {
			t.Errorf("install.sh is missing %q", want)
		}
	}
}

func TestBootstrapServesTrustMaterial(t *testing.T) {
	srv, _ := enrollTestServer(t, false)
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, []byte("CA-PEM-CONTENT"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := BootstrapConfig{CACert: caPath, SigningPubPEM: []byte("SIGNING-PUB-CONTENT")}

	rec := bootstrapGet(t, srv, cfg, "/arcatum_runner/ca.pem", "arcatum")
	if rec.Code != http.StatusOK || rec.Body.String() != "CA-PEM-CONTENT" {
		t.Errorf("ca.pem = %d/%q", rec.Code, rec.Body.String())
	}
	rec = bootstrapGet(t, srv, cfg, "/arcatum_runner/dispatch-signing.pub", "arcatum")
	if rec.Code != http.StatusOK || rec.Body.String() != "SIGNING-PUB-CONTENT" {
		t.Errorf("dispatch-signing.pub = %d/%q", rec.Code, rec.Body.String())
	}

	// Nothing configured must be a clean 404, not a panic or an empty 200.
	rec = bootstrapGet(t, srv, BootstrapConfig{}, "/arcatum_runner/ca.pem", "arcatum")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unconfigured ca.pem = %d, want 404", rec.Code)
	}
}

func TestBootstrapServesRunnerBinary(t *testing.T) {
	srv, _ := enrollTestServer(t, false)
	dist := t.TempDir()
	if err := os.WriteFile(filepath.Join(dist, "arcatum-runner-linux-amd64"), []byte("ELF-BINARY"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := BootstrapConfig{DistDir: dist}

	rec := bootstrapGet(t, srv, cfg, "/arcatum_runner/arcatum-runner-linux-amd64", "arcatum")
	if rec.Code != http.StatusOK || rec.Body.String() != "ELF-BINARY" {
		t.Errorf("binary = %d/%q", rec.Code, rec.Body.String())
	}
	// A platform we have no build for must say so rather than serve nothing silently.
	rec = bootstrapGet(t, srv, cfg, "/arcatum_runner/arcatum-runner-linux-arm64", "arcatum")
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing platform = %d, want 404", rec.Code)
	}
}

// The binary name comes from the URL, so it must not be able to reach outside DistDir.
func TestValidRunnerBinaryName(t *testing.T) {
	valid := []string{
		"arcatum-runner-linux-amd64",
		"arcatum-runner-linux-arm64",
	}
	invalid := []string{
		"arcatum-runner-../../etc/passwd",
		"../../etc/passwd",
		"arcatum-runner-linux-amd64/../../ca.key",
		"ca.key",
		"arcatum-runner-linux",         // missing arch
		"arcatum-runner-linux-amd64-x", // extra segment
		"arcatum-runner--amd64",        // empty os
		"arcatum-runner-Linux-AMD64",   // upper case is not a build we publish
		"arcatum-runner-linux-amd64.bak",
		"",
	}
	for _, name := range valid {
		if !validRunnerBinaryName(name) {
			t.Errorf("validRunnerBinaryName(%q) = false, want true", name)
		}
	}
	for _, name := range invalid {
		if validRunnerBinaryName(name) {
			t.Errorf("validRunnerBinaryName(%q) = true, want false", name)
		}
	}
}

// Attempts to fetch something outside the published files must not succeed.
func TestBootstrapRefusesUnexpectedPaths(t *testing.T) {
	srv, _ := enrollTestServer(t, false)
	dist := t.TempDir()
	// A file that exists in the directory but is not a published binary name.
	if err := os.WriteFile(filepath.Join(dist, "secret.key"), []byte("PRIVATE"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := BootstrapConfig{DistDir: dist}

	for _, path := range []string{
		"/arcatum_runner/secret.key",
		"/arcatum_runner/",
		"/etc/passwd",
		"/api/v1/runs",
	} {
		rec := bootstrapGet(t, srv, cfg, path, "arcatum")
		if rec.Code == http.StatusOK {
			t.Errorf("GET %s = 200, want a refusal (body %q)", path, rec.Body.String())
		}
	}
}

// The bootstrap listener must not expose the admin API — it has no authentication.
func TestBootstrapDoesNotExposeAdminAPI(t *testing.T) {
	srv, _ := enrollTestServer(t, false)
	for _, path := range []string{"/api/v1/instances", "/api/v1/runners", "/api/v1/runs"} {
		rec := bootstrapGet(t, srv, BootstrapConfig{}, path, "arcatum")
		if rec.Code == http.StatusOK {
			t.Errorf("bootstrap listener served %s — it must only carry install and enrollment", path)
		}
	}
}

func TestBootstrapIndexExplainsItself(t *testing.T) {
	srv, _ := enrollTestServer(t, false)
	rec := bootstrapGet(t, srv, BootstrapConfig{APIURL: "https://arcatum:8443"}, "/", "172.24.0.60")
	if rec.Code != http.StatusOK {
		t.Fatalf("index = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "install.sh") {
		t.Errorf("index should show the install command:\n%s", rec.Body.String())
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
