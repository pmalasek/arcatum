// Package jobspec parses script-definition manifests (scripts/<name>/<name>.toml).
//
// A manifest is the *template*: it declares the script's type, entrypoint and the
// parameters it accepts. Concrete values (an instance's host, credentials, schedule)
// live in the database, not here — see docs/architecture.md §5.
package jobspec

import (
	"fmt"
	"strconv"
	"strings"

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
	// Capture declares what the script writes to stdout: "log" (the default) or
	// "stream", meaning stdout is the backup payload itself. It belongs here rather
	// than on the instance because it is a property of the script — mysqldump writes a
	// dump wherever it is pointed. An instance may only turn streaming off.
	Capture string  `toml:"capture"`
	Params  []Param `toml:"param"`
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
	switch m.Capture {
	case "", proto.CaptureLog, proto.CaptureStream:
	default:
		return fmt.Errorf("manifest %q: capture must be %q or %q, got %q",
			m.Name, proto.CaptureLog, proto.CaptureStream, m.Capture)
	}
	// restic pushes its data into the repository over its own connection; its stdout is
	// progress output. Declaring it as a payload stream would silently hide that log.
	if m.Capture == proto.CaptureStream && m.Type == proto.TypeRestic {
		return fmt.Errorf("manifest %q: a restic job cannot use capture = %q", m.Name, proto.CaptureStream)
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

// ValidateParams checks an instance's values against what the script declares. This is
// what the parameter declarations in a manifest are for: catching a missing password or a
// mistyped parameter name when the instance is saved, rather than when the backup runs at
// two in the morning.
//
// Unknown names are rejected on purpose. A typo like "datbase" would otherwise sit there
// silently, with the script failing for a reason nobody connects to the typo.
func (m *Manifest) ValidateParams(params, secrets map[string]string) error {
	declared := map[string]*Param{}
	for i := range m.Params {
		declared[m.Params[i].Name] = &m.Params[i]
	}

	for name := range params {
		p, ok := declared[name]
		if !ok {
			return fmt.Errorf("script %q has no parameter %q", m.Name, name)
		}
		if p.Secret {
			return fmt.Errorf("parameter %q is a secret and must be given as a secret, not a parameter", name)
		}
	}
	for name := range secrets {
		p, ok := declared[name]
		if !ok {
			return fmt.Errorf("script %q has no parameter %q", m.Name, name)
		}
		if !p.Secret {
			return fmt.Errorf("parameter %q is not declared as a secret", name)
		}
	}

	for _, p := range m.Params {
		value, given := params[p.Name]
		if p.Secret {
			value, given = secrets[p.Name]
		}
		if !given || strings.TrimSpace(value) == "" {
			// A default covers a missing optional value; anything required must be set.
			if p.Required && strings.TrimSpace(p.Default) == "" {
				return fmt.Errorf("parameter %q is required", p.Name)
			}
			continue
		}
		if err := checkParamType(p, value); err != nil {
			return err
		}
	}
	return nil
}

// checkParamType rejects a value that cannot be what the manifest says it is.
func checkParamType(p Param, value string) error {
	switch p.Type {
	case "", "string":
		return nil
	case "int":
		if _, err := strconv.Atoi(strings.TrimSpace(value)); err != nil {
			return fmt.Errorf("parameter %q must be a whole number, got %q", p.Name, value)
		}
	case "bool":
		if _, err := strconv.ParseBool(strings.TrimSpace(value)); err != nil {
			return fmt.Errorf("parameter %q must be true or false, got %q", p.Name, value)
		}
	default:
		// An unknown declared type is the manifest author's problem, not the operator's;
		// accept the value rather than block a legitimate instance.
		return nil
	}
	return nil
}
