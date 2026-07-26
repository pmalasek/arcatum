package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// RunnerConfig is the arcatum-runner configuration (runner.toml), installed on
// each backed-up host. Its key field is Server — where to check in. install.sh
// fills it in at install time from the URL the installer was fetched from.
type RunnerConfig struct {
	Runner  RunnerSection `toml:"runner"`
	TLS     TLS           `toml:"tls"`
	Signing RunnerSigning `toml:"signing"`
}

// RunnerSigning points at the server's public key used to verify job dispatches.
type RunnerSigning struct {
	PublicKey string `toml:"public_key"` // e.g. /var/lib/arcatum-runner/pki/dispatch-signing.pub
}

// RunnerSection holds the runner's process-level settings.
type RunnerSection struct {
	Server string `toml:"server"` // base URL of arcatum-server, e.g. https://172.24.0.60:8443
	// EnrollServer is the plain-HTTP bootstrap address used only to obtain a
	// certificate on first start; afterwards everything goes over mTLS to Server.
	EnrollServer string `toml:"enroll_server"`
	PollInterval string `toml:"poll_interval"` // how often to check in, e.g. "30s"
	DataDir      string `toml:"data_dir"`      // runner state (identity, temp dispatch files)
}

// DefaultRunner returns the built-in runner configuration. Server is empty on
// purpose: it must come from runner.toml (written by install.sh) or -server.
func DefaultRunner() *RunnerConfig {
	return &RunnerConfig{
		Runner: RunnerSection{
			Server:       "",
			PollInterval: "30s",
			DataDir:      "/var/lib/arcatum-runner",
		},
	}
}

// LoadRunner reads runner config from path, applying defaults for missing fields.
// A missing file is not an error: the defaults are returned (Server still empty).
func LoadRunner(path string) (*RunnerConfig, error) {
	cfg := DefaultRunner()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("decode runner config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks the runner config is usable.
func (c *RunnerConfig) Validate() error {
	if c.Runner.Server == "" {
		return errors.New("runner config: runner.server is required (set in runner.toml or via -server)")
	}
	if err := c.TLS.Validate(); err != nil {
		return err
	}
	if c.TLS.Enabled() && c.Signing.PublicKey == "" {
		return errors.New("runner config: [signing] public_key is required when mTLS is enabled (used to verify job signatures)")
	}
	if _, err := c.Interval(); err != nil {
		return err
	}
	return nil
}

// Interval resolves the poll interval, defaulting to 30s.
func (c *RunnerConfig) Interval() (time.Duration, error) {
	if c.Runner.PollInterval == "" {
		return 30 * time.Second, nil
	}
	d, err := time.ParseDuration(c.Runner.PollInterval)
	if err != nil {
		return 0, fmt.Errorf("runner config: invalid poll_interval %q: %w", c.Runner.PollInterval, err)
	}
	return d, nil
}
