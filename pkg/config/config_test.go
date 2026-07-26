package config

import (
	"os"
	"path/filepath"
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
