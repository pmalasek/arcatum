package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// writeConfig writes a server.toml and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// fakeSystemDir points the /etc fallback at a temporary directory, so the search path
// under test does not depend on what the machine running the tests has installed.
func fakeSystemDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	previous := systemDir
	systemDir = dir
	t.Cleanup(func() { systemDir = previous })
	return dir
}

// The default search path is the whole point of the arrangement: nothing else is looked
// at, so a binary started anywhere without a server.toml of its own ends up on the
// production configuration — and with it on the production PKI.
func TestSearchPathIsWorkingDirectoryThenEtc(t *testing.T) {
	want := []string{"server.toml", "/etc/arcatum/server.toml"}
	if got := SearchPaths(); !slices.Equal(got, want) {
		t.Errorf("SearchPaths() = %v; want %v", got, want)
	}
}

// A server.toml in the working directory is what a checkout is run against, so it has to
// win over the installed one.
func TestResolvePrefersWorkingDirectory(t *testing.T) {
	system := fakeSystemDir(t)
	if err := os.WriteFile(filepath.Join(system, Filename), []byte("[server]\n"), 0o600); err != nil {
		t.Fatalf("write system config: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, Filename), []byte("[server]\n"), 0o600); err != nil {
		t.Fatalf("write local config: %v", err)
	}
	t.Chdir(dir)

	got, err := Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != Filename {
		t.Errorf("Resolve() = %q; want the working directory's %q", got, Filename)
	}
}

// With no server.toml in the working directory the start falls through to the installed
// configuration instead of failing.
func TestResolveFallsBackToSystemDir(t *testing.T) {
	system := fakeSystemDir(t)
	installed := filepath.Join(system, Filename)
	if err := os.WriteFile(installed, []byte("[server]\n"), 0o600); err != nil {
		t.Fatalf("write system config: %v", err)
	}
	t.Chdir(t.TempDir())

	got, err := Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != installed {
		t.Errorf("Resolve() = %q; want %q", got, installed)
	}
}

// config/server.toml exists in the repository. Were it part of the search path, running
// the binary from a checkout would quietly pick up development PKI instead of the
// production configuration.
func TestResolveIgnoresConfigSubdirectory(t *testing.T) {
	fakeSystemDir(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config", Filename), []byte("[server]\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Chdir(dir)

	if got, err := Resolve(""); err == nil {
		t.Errorf("Resolve() = %q; want an error, config/%s must not be searched", got, Filename)
	}
}

// A configuration that is not there used to start the server on built-in defaults: plain
// HTTP, secrets in the clear, an empty database in whatever directory it landed in. A
// typo in -config has to stop the start instead.
func TestResolveRejectsMissingFiles(t *testing.T) {
	fakeSystemDir(t)
	dir := t.TempDir()
	t.Chdir(dir)

	if _, err := Resolve(filepath.Join(dir, "typo.toml")); err == nil {
		t.Error("a -config path that does not exist was accepted")
	}
	if _, err := Resolve(""); err == nil {
		t.Error("a start with no configuration anywhere was accepted")
	}
	if _, err := Load(filepath.Join(dir, "absent.toml")); err == nil {
		t.Error("Load of a missing file returned the defaults instead of an error")
	}
}

// A configuration file that says nothing about [web] must still get the web UI, or an
// existing deployment would lose it on upgrade.
func TestWebListenerIsOnByDefault(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[server]
listen = "0.0.0.0:8443"

[storage]
backup_dir = "/central_backup/arcatum"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Web.Enabled() {
		t.Error("the web UI must be enabled by default")
	}
	if cfg.Web.Listen == cfg.Server.Listen {
		t.Errorf("the default web listen %q collides with the API listen", cfg.Web.Listen)
	}
	// No session_ttl means "use the server's default", not zero.
	ttl, err := cfg.Web.TTL()
	if err != nil || ttl != 0 {
		t.Errorf("TTL() = %v, %v; want 0, nil", ttl, err)
	}
}

func TestWebListenerCanBeDisabled(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[server]
listen = "0.0.0.0:8443"

[storage]
backup_dir = "/central_backup/arcatum"

[web]
listen = ""
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Web.Enabled() {
		t.Error("an empty [web] listen must disable the web UI")
	}
}

func TestWebSessionTTL(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[server]
listen = "0.0.0.0:8443"

[storage]
backup_dir = "/central_backup/arcatum"

[web]
listen      = "0.0.0.0:8080"
session_ttl = "30m"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ttl, err := cfg.Web.TTL()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if ttl != 30*time.Minute {
		t.Errorf("TTL = %v; want 30m", ttl)
	}
}

// A mistyped duration must be reported at startup, not silently turn into "no expiry".
func TestBadSessionTTLIsRejected(t *testing.T) {
	for _, value := range []string{"12 hours", "-1h", "0"} {
		_, err := Load(writeConfig(t, `
[server]
listen = "0.0.0.0:8443"

[storage]
backup_dir = "/central_backup/arcatum"

[web]
session_ttl = "`+value+`"
`))
		if err == nil {
			t.Errorf("session_ttl = %q was accepted", value)
		}
	}
}

// Two listeners on one address would fail with a bare "address already in use" from
// whichever lost the race, and it would not be obvious which pair collided.
func TestCollidingListenAddressesAreRejected(t *testing.T) {
	cases := []struct {
		name, body, wantIn string
	}{
		{"web and api", `
[server]
listen = "0.0.0.0:8443"
[storage]
backup_dir = "/b"
[web]
listen = "0.0.0.0:8443"
`, "[web] listen and [server] listen"},
		{"web and bootstrap", `
[server]
listen = "0.0.0.0:8443"
[storage]
backup_dir = "/b"
[web]
listen = "0.0.0.0:80"
[bootstrap]
listen = "0.0.0.0:80"
`, "[web] listen and [bootstrap] listen"},
		{"api and bootstrap", `
[server]
listen = "0.0.0.0:80"
[storage]
backup_dir = "/b"
[bootstrap]
listen = "0.0.0.0:80"
`, "[server] listen and [bootstrap] listen"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatal("colliding addresses were accepted")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("err = %v; want it to name %s", err, tc.wantIn)
			}
		})
	}
}

// Retention is thought about in days, which is exactly what time.ParseDuration rejects.
func TestLogRetentionParsing(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{"days", "14d", 14 * 24 * time.Hour, false},
		{"go duration", "36h", 36 * time.Hour, false},
		{"zero keeps forever", "0", 0, false},
		{"empty keeps forever", "", 0, false},
		{"negative", "-5d", 0, true},
		{"nonsense", "soon", 0, true},
		{"nonsense days", "twod", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := Storage{LogRetentionSuccess: tc.value}.LogRetention()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%q was accepted, want an error", tc.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q: %v", tc.value, err)
			}
			if got != tc.want {
				t.Errorf("%q = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// The shipped defaults have to parse, or every server would refuse to start.
func TestDefaultLogRetentionIsValid(t *testing.T) {
	success, failed, err := Default().Storage.LogRetention()
	if err != nil {
		t.Fatalf("default retention: %v", err)
	}
	if success <= 0 || failed <= success {
		t.Errorf("defaults = success %v / failed %v, want both set and failures kept longer", success, failed)
	}
}

// Replication that is switched on but has nowhere to write is worse than one that is
// off: the UI reports it as configured while the backlog grows behind a target that
// never existed. The same reasoning as the half-filled [tls] section.
func TestReplicaRejectsHalfConfiguration(t *testing.T) {
	cases := []struct {
		name, body, wantIn string
	}{
		{"no host", `
[replica]
enabled = true
path    = "/data"
ssh_key = "/pki/replica.key"
`, "host is required"},
		{"no path", `
[replica]
enabled = true
host    = "172.26.0.2"
ssh_key = "/pki/replica.key"
`, "path is required"},
		{"no key", `
[replica]
enabled = true
host    = "172.26.0.2"
path    = "/data"
`, "ssh_key is required"},
		{"relative path", `
[replica]
enabled = true
host    = "172.26.0.2"
path    = "data"
ssh_key = "/pki/replica.key"
`, "must be absolute"},
		// Mirroring without a ceiling is the configuration in which a backup_dir that has
		// gone missing empties the off-site copy. It has to be a mistake, not a mode.
		{"mirror without max_delete", `
[replica]
enabled = true
host    = "172.26.0.2"
path    = "/data"
ssh_key = "/pki/replica.key"
mirror  = true
`, "max_delete"},
		{"bad timeout", `
[replica]
enabled = true
host    = "172.26.0.2"
path    = "/data"
ssh_key = "/pki/replica.key"
timeout = "two hours"
`, "timeout"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, `
[server]
listen = "0.0.0.0:8443"

[storage]
backup_dir = "/central_backup/arcatum"
`+c.body))
			if err == nil {
				t.Fatalf("configuration was accepted: %s", c.body)
			}
			if !strings.Contains(err.Error(), c.wantIn) {
				t.Fatalf("error %q does not mention %q", err, c.wantIn)
			}
		})
	}
}

func TestReplicaDefaultsAndAccessors(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[server]
listen = "0.0.0.0:8443"

[storage]
backup_dir = "/central_backup/arcatum"

[replica]
enabled    = true
host       = "172.26.0.2"
user       = "arcatum"
path       = "/data"
ssh_key    = "/pki/replica.key"
mirror     = true
max_delete = 100
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Replica.Addr(); got != "arcatum@172.26.0.2:/data" {
		t.Fatalf("Addr = %q", got)
	}
	if got := cfg.Replica.SSHPort(); got != 22 {
		t.Fatalf("SSHPort = %d, want the default 22", got)
	}
	timeout, sweep, probe, err := cfg.Replica.Durations()
	if err != nil {
		t.Fatalf("Durations: %v", err)
	}
	if timeout != 2*time.Hour || sweep != time.Hour || probe != 5*time.Minute {
		t.Fatalf("defaults = %v/%v/%v", timeout, sweep, probe)
	}
}

// A server with no [replica] section must load exactly as it did before.
func TestReplicaIsOffByDefault(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[server]
listen = "0.0.0.0:8443"

[storage]
backup_dir = "/central_backup/arcatum"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Replica.Enabled {
		t.Fatal("replication must be off unless asked for")
	}
	if err := cfg.Replica.Validate(); err != nil {
		t.Fatalf("a disabled replica must validate: %v", err)
	}
}
