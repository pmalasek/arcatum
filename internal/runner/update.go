package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"arcatum/pkg/version"
)

// Auto-update keeps the runner binary current without an operator visiting every host.
// It is the riskiest thing the runner does — replacing its own executable — so it is
// deliberately paranoid:
//
//   - The manifest naming the expected build must be **signed by a dispatch-signing key
//     this runner already trusts**. Control of the server is not enough; publishing a
//     binary requires that key.
//   - The download goes over the authenticated mTLS connection, never the plain-HTTP
//     bootstrap port, and its SHA-256 must match the signed manifest before anything is
//     written.
//   - An unstamped development build never updates itself.
//   - A version that fails to take effect is tried once and then left alone, so a broken
//     build cannot put the host into a restart loop.

// attemptFile records the version this runner last tried to install, so a failed update
// is not retried forever.
const attemptFile = "update-attempted"

// updateManifest mirrors server.UpdateManifest.
type updateManifest struct {
	Version    string            `json:"version"`
	Binaries   map[string]string `json:"binaries"`
	Signatures []string          `json:"signatures"`
}

// updateManifestBytesToSign must match the server's canonical form exactly.
func updateManifestBytesToSign(v string, binaries map[string]string) []byte {
	keys := make([]string, 0, len(binaries))
	for k := range binaries {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("arcatum-update/v1")
	writeLenPrefixed(&b, v)
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

// UpdateIfAvailable installs a newer published build if there is one. It reports whether
// the binary was replaced, in which case the caller exits so the service manager restarts
// into the new build.
//
// Every failure path is non-fatal: the current binary keeps working and the attempt is
// retried later.
func (a *Agent) UpdateIfAvailable(ctx context.Context) (updated bool) {
	if !a.autoUpdate || a.verifier == nil {
		return false
	}
	if version.IsDev() {
		return false // never replace a development build
	}
	manifest, err := a.fetchUpdateManifest(ctx)
	if err != nil {
		a.log.Printf("update: %v", err)
		return false
	}
	if manifest.Version == "" || manifest.Version == version.Version {
		return false
	}
	// Signed by a key we trust, or we do not touch the binary. This is what stops a
	// compromised server from shipping its own executable.
	if !a.verifyAny(updateManifestBytesToSign(manifest.Version, manifest.Binaries), manifest.Signatures) {
		a.log.Printf("update: REFUSED %q — manifest not signed by any key we trust", manifest.Version)
		return false
	}

	platform := runtime.GOOS + "-" + runtime.GOARCH
	wantHash, ok := manifest.Binaries[platform]
	if !ok {
		a.log.Printf("update: %q has no build for %s", manifest.Version, platform)
		return false
	}
	// One attempt per version: if the new build does not actually report the new version
	// after a restart, something is wrong and retrying would loop forever.
	if a.alreadyAttempted(manifest.Version) {
		a.log.Printf("update: %q was already attempted and this build still reports %q — "+
			"not retrying; investigate on the server", manifest.Version, version.Version)
		return false
	}

	self, err := os.Executable()
	if err != nil {
		a.log.Printf("update: cannot locate own binary: %v", err)
		return false
	}
	a.log.Printf("update: %s → %s, downloading %s", version.Version, manifest.Version, platform)
	data, err := a.downloadBinary(ctx, "arcatum-runner-"+platform)
	if err != nil {
		a.log.Printf("update: %v", err)
		return false
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != wantHash {
		a.log.Printf("update: REFUSED — downloaded binary hashes to %s, manifest says %s", got, wantHash)
		return false
	}
	if err := a.recordAttempt(manifest.Version); err != nil {
		a.log.Printf("update: cannot record the attempt, skipping to avoid a restart loop: %v", err)
		return false
	}
	if err := replaceExecutable(self, data); err != nil {
		a.log.Printf("update: %v", err)
		return false
	}
	a.log.Printf("update: installed %s, restarting", manifest.Version)
	return true
}

func (a *Agent) fetchUpdateManifest(ctx context.Context) (*updateManifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.client.BaseURL()+"/api/v1/update", nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.HTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch update manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch update manifest: %s", resp.Status)
	}
	var out updateManifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode update manifest: %w", err)
	}
	return &out, nil
}

// downloadBinary fetches a build over the authenticated connection.
func (a *Agent) downloadBinary(ctx context.Context, name string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.client.BaseURL()+"/api/v1/update/"+name, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.HTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", name, resp.Status)
	}
	// Bounded: a runner binary is tens of megabytes, not gigabytes.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	return data, nil
}

// replaceExecutable swaps the running binary for new bytes.
//
// The new file is written alongside and renamed into place, which is atomic on the same
// filesystem — a crash mid-update leaves either the old binary or the new one, never a
// half-written file. The previous binary is kept as .old for diagnosis.
func replaceExecutable(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".arcatum-runner-new-*")
	if err != nil {
		return fmt.Errorf("create temporary binary: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write new binary: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync new binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return fmt.Errorf("chmod new binary: %w", err)
	}
	// Keep the outgoing binary: if the new one refuses to start, this is what an operator
	// needs.
	if err := os.Rename(path, path+".old"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("set aside the current binary: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		// Put the old one back rather than leaving the host with no binary at all.
		if rerr := os.Rename(path+".old", path); rerr != nil {
			return fmt.Errorf("install new binary: %w (and restoring the old one failed: %v)", err, rerr)
		}
		return fmt.Errorf("install new binary: %w", err)
	}
	return nil
}

// alreadyAttempted reports whether this exact version has been installed before without
// taking effect.
func (a *Agent) alreadyAttempted(v string) bool {
	data, err := os.ReadFile(a.attemptPath())
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == v
}

func (a *Agent) recordAttempt(v string) error {
	return writeFileMode(a.attemptPath(), []byte(v+"\n"), 0o644)
}

func (a *Agent) attemptPath() string {
	return filepath.Join(a.workBase, attemptFile)
}
