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
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the full server configuration.
type Config struct {
	Server  Server  `toml:"server"`
	Storage Storage `toml:"storage"`
	TLS     TLS     `toml:"tls"`
	Signing Signing `toml:"signing"`
	Secrets Secrets `toml:"secrets"`
}

// Signing points at the key used to sign job dispatches (server side).
type Signing struct {
	Key string `toml:"key"` // Ed25519 private key, e.g. pki/dispatch-signing.key
}

// Secrets points at the master key encrypting instance secrets in the database.
// Without it secrets are stored in plaintext (development only).
type Secrets struct {
	MasterKey string `toml:"master_key"` // e.g. pki/secrets-master.key
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

// Default returns the built-in configuration used when server.toml is absent or
// omits fields.
func Default() *Config {
	return &Config{
		Server: Server{
			Listen:   "0.0.0.0:8443",
			Scripts:  "scripts",
			DataDir:  "/central_backup/arcatum/data",
			Timezone: "UTC",
			LogLevel: "info",
		},
		Storage: Storage{
			BackupDir: "/central_backup/arcatum",
		},
	}
}

// Load reads config from path, applying defaults for any missing fields. A missing
// file is not an error: the defaults are returned.
func Load(path string) (*Config, error) {
	cfg := Default()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return cfg, nil
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
