package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"arcatum/pkg/jobspec"
	"arcatum/pkg/proto"
)

// ScriptEntry is a loaded script definition plus the path to its executable.
type ScriptEntry struct {
	Manifest       *jobspec.Manifest
	EntrypointPath string
}

// Catalog holds the script definitions found under a scripts directory, keyed by
// manifest name.
type Catalog struct {
	byName map[string]*ScriptEntry
}

// LoadCatalog walks dir for *.toml manifests and pairs each with its entrypoint.
func LoadCatalog(dir string) (*Catalog, error) {
	c := &Catalog{byName: map[string]*ScriptEntry{}}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".toml" {
			return nil
		}
		m, err := jobspec.Load(path)
		if err != nil {
			return err
		}
		entry := &ScriptEntry{Manifest: m}
		// restic jobs have no script file to ship (see jobspec.Manifest.Validate).
		if m.Entrypoint != "" {
			entry.EntrypointPath = filepath.Join(filepath.Dir(path), m.Entrypoint)
			if _, err := os.Stat(entry.EntrypointPath); err != nil {
				return fmt.Errorf("script %q: entrypoint not found: %w", m.Name, err)
			}
		}
		if prev, dup := c.byName[m.Name]; dup {
			return fmt.Errorf("duplicate script name %q (%s and %s)", m.Name, prev.EntrypointPath, entry.EntrypointPath)
		}
		c.byName[m.Name] = entry
		return nil
	})
	if os.IsNotExist(err) {
		return c, nil
	}
	return c, err
}

// Get returns the entry for a script name.
func (c *Catalog) Get(name string) (*ScriptEntry, bool) {
	e, ok := c.byName[name]
	return e, ok
}

// Names lists all known script names.
func (c *Catalog) Names() []string {
	names := make([]string, 0, len(c.byName))
	for n := range c.byName {
		names = append(names, n)
	}
	return names
}

// effectiveCapture decides whether a run's stdout is the backup payload or a log.
//
// The manifest is authoritative. Whether a script prints a dump or progress lines is a
// property of the script, not of the host it runs against, and instances predate the
// manifest declaration: rows written before it exists carry capture = "stream" even for
// scripts that only ever printed text, and honouring those would divert their output
// into a payload file where nobody would look for it. An instance may therefore only
// turn streaming *off*, for a script that can also write its data somewhere itself.
func effectiveCapture(m *jobspec.Manifest, in *Instance) string {
	if m.Capture != proto.CaptureStream {
		return proto.CaptureLog
	}
	if in.Capture == proto.CaptureLocal || in.Capture == proto.CaptureLog {
		return proto.CaptureLog
	}
	return proto.CaptureStream
}

// readArtifact reads the entrypoint file and returns its bytes and sha256 hex. A
// restic entry has no artifact, in which case both are empty.
func (e *ScriptEntry) readArtifact() (content []byte, sha string, err error) {
	if e.EntrypointPath == "" {
		return nil, "", nil
	}
	content, err = os.ReadFile(e.EntrypointPath)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(content)
	return content, hex.EncodeToString(sum[:]), nil
}

// effectiveManifestCapture is what a script's stdout is before any instance has a say.
// Exposed to the web UI so a form can offer dump retention only where dumps exist.
func effectiveManifestCapture(m *jobspec.Manifest) string {
	if m.Capture == proto.CaptureStream {
		return proto.CaptureStream
	}
	return proto.CaptureLog
}
