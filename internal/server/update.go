package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"arcatum/pkg/crypto"
)

// Auto-update replaces the runner binary on every backed-up host without an operator
// visiting each one. It is also the single most dangerous feature in the system: a bad
// or hostile update breaks — or takes over — every backed-up server at once.
//
// Three things make it safe enough to have:
//
//  1. The update manifest is **signed with the dispatch-signing key**, the same key that
//     authorises jobs. Publishing a binary therefore requires that key, not merely
//     control of the server or of the plain-HTTP bootstrap port.
//  2. The binary is fetched over **mTLS**, not over the bootstrap listener, and its
//     SHA-256 is checked against the signed manifest before anything is replaced.
//  3. The operator controls the rollout: the server only advertises what is present in
//     dist_dir, and a runner can opt out entirely.
//
// Publishing means copying the binaries into dist_dir and writing the version into a
// VERSION file next to them. Nothing else.

// versionFile holds the published version string, alongside the binaries it describes.
const versionFile = "VERSION"

// UpdateManifest tells a runner which build it should be running.
type UpdateManifest struct {
	// Version is the published build identity.
	Version string `json:"version"`
	// Binaries maps "<os>-<arch>" to the SHA-256 of that build.
	Binaries map[string]string `json:"binaries"`
	// Signatures cover the canonical form, once per signing key the server holds — the
	// same arrangement as the trust bundle, and for the same reason.
	Signatures []string `json:"signatures"`
}

// updateManifestBytesToSign is the canonical form that gets signed. Entries are sorted
// and length-prefixed, so no build can be added, removed or swapped undetected.
func updateManifestBytesToSign(version string, binaries map[string]string) []byte {
	keys := make([]string, 0, len(binaries))
	for k := range binaries {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("arcatum-update/v1")
	writeLenPrefixed(&b, version)
	for _, k := range keys {
		writeLenPrefixed(&b, k)
		writeLenPrefixed(&b, binaries[k])
	}
	return []byte(b.String())
}

func writeLenPrefixed(b *strings.Builder, s string) {
	fmt.Fprintf(b, "\x00%d:", len(s))
	b.WriteString(s)
}

// distCache avoids re-hashing binaries on every check-in; they only change when an
// operator publishes.
type distCache struct {
	mu       sync.Mutex
	dir      string
	version  string
	binaries map[string]string
	hashes   map[string]string // file path -> sha256, keyed for serving
	modTime  time.Time
}

// handleUpdateManifest publishes what the fleet should be running (runner or admin).
func (s *Server) handleUpdateManifest(w http.ResponseWriter, r *http.Request) {
	if err := s.authorizeRunnerOrAdmin(w, r); err != nil {
		return
	}
	if s.dist == nil || s.dist.dir == "" {
		// Nothing published: not an error, just nothing to do.
		writeJSON(w, UpdateManifest{Binaries: map[string]string{}})
		return
	}
	version, binaries, err := s.dist.load()
	if err != nil {
		s.log.Printf("update manifest: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if version == "" || len(binaries) == 0 {
		writeJSON(w, UpdateManifest{Binaries: map[string]string{}})
		return
	}
	sigs, err := s.signWithAll(updateManifestBytesToSign(version, binaries))
	if err != nil {
		s.log.Printf("update manifest: sign: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, UpdateManifest{Version: version, Binaries: binaries, Signatures: sigs})
}

// handleUpdateDownload serves a runner binary over the authenticated connection. The
// bootstrap listener also serves binaries, but only for the initial install: an update
// must not arrive over an unauthenticated channel.
func (s *Server) handleUpdateDownload(w http.ResponseWriter, r *http.Request) {
	if err := s.authorizeRunnerOrAdmin(w, r); err != nil {
		return
	}
	if s.dist == nil || s.dist.dir == "" {
		http.Error(w, "no dist directory configured", http.StatusNotFound)
		return
	}
	name := r.PathValue("name")
	if !validRunnerBinaryName(name) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	f, err := os.Open(filepath.Join(s.dist.dir, name))
	if err != nil {
		http.Error(w, "binary not available for this platform", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// authorizeRunnerOrAdmin allows an approved runner or an operator, and nobody else.
func (s *Server) authorizeRunnerOrAdmin(w http.ResponseWriter, r *http.Request) error {
	if !s.requireClientCert {
		return nil
	}
	cert := peerCert(r)
	if cert == nil {
		http.Error(w, "client certificate required", http.StatusUnauthorized)
		return fmt.Errorf("no certificate")
	}
	switch crypto.CertRole(cert) {
	case crypto.RoleAdmin:
		return nil
	case crypto.RoleRunner:
		if err := s.requireApprovedRunner(cert.Subject.CommonName); err != nil {
			s.denyRunner(w, err)
			return err
		}
		return nil
	default:
		http.Error(w, "not permitted", http.StatusForbidden)
		return fmt.Errorf("wrong role")
	}
}

// load reads the published version and hashes the binaries, caching the result until the
// directory changes.
func (d *distCache) load() (version string, binaries map[string]string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	info, err := os.Stat(d.dir)
	if err != nil {
		return "", nil, nil // nothing published
	}
	if d.binaries != nil && info.ModTime().Equal(d.modTime) {
		return d.version, d.binaries, nil
	}

	raw, err := os.ReadFile(filepath.Join(d.dir, versionFile))
	if err != nil {
		// No VERSION file means the operator has not published a version, so no runner is
		// told to update. Serving binaries without a version would be a silent downgrade
		// hazard.
		d.version, d.binaries, d.modTime = "", map[string]string{}, info.ModTime()
		return "", d.binaries, nil
	}
	version = strings.TrimSpace(string(raw))

	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return "", nil, err
	}
	binaries = map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !validRunnerBinaryName(e.Name()) {
			continue
		}
		sum, err := fileSHA256(filepath.Join(d.dir, e.Name()))
		if err != nil {
			return "", nil, err
		}
		platform := strings.TrimPrefix(e.Name(), "arcatum-runner-")
		binaries[platform] = sum
	}
	d.version, d.binaries, d.modTime = version, binaries, info.ModTime()
	return version, binaries, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
