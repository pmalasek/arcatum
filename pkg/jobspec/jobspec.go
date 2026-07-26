// Package jobspec parses script-definition manifests (scripts/<name>/<name>.toml).
//
// A manifest is the *template*: it declares the script's type, entrypoint and the
// parameters it accepts. Concrete values (an instance's host, credentials, schedule)
// live in the database, not here — see docs/architecture.md §5.
package jobspec

import (
	"fmt"

	"arcatum/pkg/proto"

	"github.com/BurntSushi/toml"
)

// Manifest is the parsed script definition.
type Manifest struct {
	Name       string           `toml:"name"`
	Type       proto.ScriptType `toml:"type"`
	Entrypoint string           `toml:"entrypoint"`
	Platforms  []string         `toml:"platforms"` // e.g. ["linux/amd64"] — only for type=binary
	Timeout    string           `toml:"timeout"`   // default; an instance may override
	Params     []Param          `toml:"param"`
}

// Param declares one input the script accepts. The server uses these to render a
// form in the web UI and to validate an instance before dispatch.
type Param struct {
	Name     string `toml:"name"`
	Type     string `toml:"type"` // string | int | bool
	Required bool   `toml:"required"`
	Secret   bool   `toml:"secret"`
	Default  string `toml:"default"`
}

// Load reads and validates a manifest file.
func Load(path string) (*Manifest, error) {
	var m Manifest
	if _, err := toml.DecodeFile(path, &m); err != nil {
		return nil, fmt.Errorf("decode manifest %s: %w", path, err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks the manifest is internally consistent.
func (m *Manifest) Validate() error {
	if m.Name == "" {
		return fmt.Errorf("manifest: name is required")
	}
	switch m.Type {
	case proto.TypeBash, proto.TypePython, proto.TypeBinary, proto.TypeRestic:
	default:
		return fmt.Errorf("manifest %q: invalid type %q", m.Name, m.Type)
	}
	// A restic job needs no script: the runner drives restic itself from the
	// instance's parameters (paths, excludes, retention).
	if m.Entrypoint == "" && m.Type != proto.TypeRestic {
		return fmt.Errorf("manifest %q: entrypoint is required", m.Name)
	}
	seen := map[string]bool{}
	for _, p := range m.Params {
		if p.Name == "" {
			return fmt.Errorf("manifest %q: param with empty name", m.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("manifest %q: duplicate param %q", m.Name, p.Name)
		}
		seen[p.Name] = true
	}
	return nil
}

// ParamByName returns the named parameter declaration, or nil.
func (m *Manifest) ParamByName(name string) *Param {
	for i := range m.Params {
		if m.Params[i].Name == name {
			return &m.Params[i]
		}
	}
	return nil
}
