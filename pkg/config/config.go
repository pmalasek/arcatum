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

// TLS holds mTLS material (wired up later).
type TLS struct {
	CACert string `toml:"ca_cert"`
	Cert   string `toml:"cert"`
	Key    string `toml:"key"`
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
