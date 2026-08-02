// Package config loads Arcatum's host-level configuration files: server.toml
// (Config, this file) and runner.toml (RunnerConfig, runner.go). These are
// distinct from instances (which live in the DB) and from script manifests.
//
// Note which side each address belongs to: the server's listen address is in
// server.toml; the address the runner dials is in runner.toml (written by
// install.sh) — the server does not know its own reachable address.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the full server configuration.
type Config struct {
	Server    Server    `toml:"server"`
	Web       Web       `toml:"web"`
	Storage   Storage   `toml:"storage"`
	TLS       TLS       `toml:"tls"`
	Signing   Signing   `toml:"signing"`
	Secrets   Secrets   `toml:"secrets"`
	Bootstrap Bootstrap `toml:"bootstrap"`
}

// Web configures the plain-HTTP listener serving the web UI. It is a separate port from
// [server] on purpose: runners authenticate with a certificate, while people log in with
// a username and a password, and mTLS would otherwise force a client certificate into
// every browser that is ever used to look at the backups.
//
// Leave listen empty to switch the web UI off entirely; the operator API on the mTLS port
// keeps working with an admin certificate.
type Web struct {
	Listen string `toml:"listen"` // e.g. "0.0.0.0:8080"
	// SessionTTL is how long a login lasts without activity, e.g. "12h". It slides
	// forward while the UI is in use. Empty means the built-in default.
	SessionTTL string `toml:"session_ttl"`
	// SecureCookie marks the session cookie Secure, so a browser only sends it over
	// HTTPS. Set it when a reverse proxy terminates TLS in front of this listener;
	// leaving it on for a plain-HTTP listener would stop logins from working at all.
	SecureCookie bool `toml:"secure_cookie"`
}

// Enabled reports whether the web UI listener should run.
func (w Web) Enabled() bool { return w.Listen != "" }

// TTL resolves session_ttl. A zero duration means "use the server's default".
func (w Web) TTL() (time.Duration, error) {
	if w.SessionTTL == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(w.SessionTTL)
	if err != nil {
		return 0, fmt.Errorf("config: [web] session_ttl %q: %w", w.SessionTTL, err)
	}
	if d <= 0 {
		return 0, errors.New("config: [web] session_ttl must be positive")
	}
	return d, nil
}

// Bootstrap configures the plain-HTTP listener used to install and enrol runners. A
// host that has no certificate yet cannot reach the mTLS listener at all, so this is
// what "curl … | sh" talks to. Leave listen empty to disable it.
type Bootstrap struct {
	Listen  string `toml:"listen"`   // e.g. "0.0.0.0:80"
	DistDir string `toml:"dist_dir"` // runner binaries: arcatum-runner-<os>-<arch>
	APIURL  string `toml:"api_url"`  // mTLS address written into the generated runner.toml
	CAKey   string `toml:"ca_key"`   // CA private key, needed to sign approved requests
	// CACert is the certificate matching CAKey — the authority new certificates are
	// issued under. It is separate from [tls] ca_cert, which is a *bundle* of every
	// authority still trusted: during a CA rotation the two differ, and signing must
	// use the new one while verification still accepts both.
	CACert string `toml:"ca_cert"`
}

// SigningCA returns the certificate to issue new certificates under, defaulting to the
// trust bundle when no rotation is in progress.
func (b Bootstrap) SigningCA(trustBundle string) string {
	if b.CACert != "" {
		return b.CACert
	}
	return trustBundle
}

// Enabled reports whether the bootstrap listener should run.
func (b Bootstrap) Enabled() bool { return b.Listen != "" }

// Signing points at the key used to sign job dispatches (server side).
type Signing struct {
	Key string `toml:"key"` // Ed25519 private key, e.g. pki/dispatch-signing.key
	// PreviousKeys are the *private* keys of predecessors, kept during a rotation. Their
	// public parts are published so runners still accept dispatches signed with them, and
	// the server co-signs the published trust material with each — which is what lets a
	// runner still on an old key accept the new set. Private, not public, for that reason.
	PreviousKeys []string `toml:"previous_keys"` // e.g. ["pki/dispatch-signing.key"]
}

// Secrets points at the master key encrypting instance secrets in the database.
// Without it secrets are stored in plaintext (development only).
type Secrets struct {
	MasterKey string `toml:"master_key"` // e.g. pki/secrets-master.key
	// PreviousKeys are master keys that older values are still sealed with. Rotation
	// means adding a new master_key and listing the old one here until everything has
	// been re-encrypted.
	PreviousKeys []string `toml:"previous_keys"`
}

// Server holds process-level settings.
type Server struct {
	Listen   string `toml:"listen"`    // API/web listen address
	Scripts  string `toml:"scripts"`   // dir with script definitions
	DataDir  string `toml:"data_dir"`  // DB and runtime state
	Timezone string `toml:"timezone"`  // default TZ for schedules without one
	LogLevel string `toml:"log_level"` // debug | info | warn | error
}

// Storage holds where backup data lands.
type Storage struct {
	BackupDir string `toml:"backup_dir"` // e.g. /central_backup/arcatum
}

// TLS holds the mTLS material for one side of the connection.
type TLS struct {
	CACert string `toml:"ca_cert"`
	Cert   string `toml:"cert"`
	Key    string `toml:"key"`
}

// Enabled reports whether mTLS is configured. All three paths are needed; a partial
// configuration is a mistake rather than a mode, and Validate rejects it.
func (t TLS) Enabled() bool {
	return t.CACert != "" && t.Cert != "" && t.Key != ""
}

// Validate rejects a half-filled TLS section, which would otherwise silently fall
// back to unencrypted, unauthenticated HTTP.
func (t TLS) Validate() error {
	set := 0
	for _, p := range []string{t.CACert, t.Cert, t.Key} {
		if p != "" {
			set++
		}
	}
	if set != 0 && set != 3 {
		return errors.New("config: [tls] needs ca_cert, cert and key together (or none at all)")
	}
	return nil
}

// Filename is what a server configuration file is called when it is looked up
// instead of being named explicitly.
const Filename = "server.toml"

// SystemDir is where a production install keeps its configuration.
const SystemDir = "/etc/arcatum"

// systemDir is SystemDir, indirected so tests can point the fallback at a temporary
// directory instead of whatever the machine running them happens to have in /etc.
var systemDir = SystemDir

// SearchPaths lists, in order, where Resolve looks when no path is given: the working
// directory first, so a checkout can be run against a file of its own, then the system
// location. Anything started from a directory without a server.toml — the service, a
// binary run by hand for debugging — therefore lands on the same production
// configuration, and with it on the same PKI.
//
// Only a bare server.toml counts, not config/server.toml: the repository has one of
// those, and picking it up would mean a debug run silently switching to development
// certificates, which is what this search path exists to prevent.
func SearchPaths() []string {
	return []string{Filename, filepath.Join(systemDir, Filename)}
}

// Resolve picks the configuration file to load. An explicit path (-config) wins and
// must exist; otherwise the first existing entry of SearchPaths is used.
//
// Finding nothing is an error rather than a fallback to Default: a server without a
// configuration would come up on plain HTTP with unencrypted secrets, so a mistyped
// path or a wrong working directory has to stop the start, not quietly downgrade it.
func Resolve(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("config %s: %w", explicit, err)
		}
		return explicit, nil
	}
	paths := SearchPaths()
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("config: no configuration found (looked for %s) — "+
		"install one in %s or pass -config", strings.Join(paths, ", "), systemDir)
}

// Default returns the built-in configuration for fields a server.toml omits.
func Default() *Config {
	return &Config{
		Server: Server{
			Listen:   "0.0.0.0:8443",
			Scripts:  "scripts",
			DataDir:  "/central_backup/arcatum/data",
			Timezone: "UTC",
			LogLevel: "info",
		},
		Web: Web{
			Listen: "0.0.0.0:8080",
		},
		Storage: Storage{
			BackupDir: "/central_backup/arcatum",
		},
	}
}

// Load reads config from path, applying defaults for any missing fields. The file has
// to be there — see Resolve for why a missing one is not treated as "no configuration".
func Load(path string) (*Config, error) {
	cfg := Default()
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	// DecodeFile overwrites only the keys present in the file, so defaults survive.
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("decode config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks the configuration is usable.
func (c *Config) Validate() error {
	if c.Server.Listen == "" {
		return errors.New("config: server.listen is required")
	}
	if c.Storage.BackupDir == "" {
		return errors.New("config: storage.backup_dir is required")
	}
	if err := c.TLS.Validate(); err != nil {
		return err
	}
	if c.TLS.Enabled() && c.Signing.Key == "" {
		return errors.New("config: [signing] key is required when mTLS is enabled (runners verify job signatures)")
	}
	if c.TLS.Enabled() && c.Secrets.MasterKey == "" {
		return errors.New("config: [secrets] master_key is required when mTLS is enabled (otherwise credentials sit in the database in plaintext)")
	}
	if _, err := c.Web.TTL(); err != nil {
		return err
	}
	// Two listeners on one address would mean whichever starts first wins and the other
	// dies with a confusing "address already in use" — say which pair collided instead.
	for _, pair := range []struct{ what, a, b string }{
		{"[web] listen and [server] listen", c.Web.Listen, c.Server.Listen},
		{"[web] listen and [bootstrap] listen", c.Web.Listen, c.Bootstrap.Listen},
		{"[server] listen and [bootstrap] listen", c.Server.Listen, c.Bootstrap.Listen},
	} {
		if pair.a != "" && pair.a == pair.b {
			return fmt.Errorf("config: %s must differ (both %q)", pair.what, pair.a)
		}
	}
	if c.Bootstrap.Enabled() {
		if c.Bootstrap.APIURL == "" {
			return errors.New("config: [bootstrap] api_url is required (runners are told to check in there)")
		}
		if c.Bootstrap.CAKey == "" {
			return errors.New("config: [bootstrap] ca_key is required to sign approved enrollment requests")
		}
		if !c.TLS.Enabled() {
			return errors.New("config: [bootstrap] needs [tls] as well — enrolling runners into a server that does not use mTLS would hand out certificates nothing checks")
		}
	}
	if _, err := c.Location(); err != nil {
		return err
	}
	return nil
}

// Location resolves the configured default timezone.
func (c *Config) Location() (*time.Location, error) {
	tz := c.Server.Timezone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("config: invalid timezone %q: %w", tz, err)
	}
	return loc, nil
}
