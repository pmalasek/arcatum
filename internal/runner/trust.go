package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"arcatum/pkg/crypto"
)

// Keeping trust material up to date is what lets the server rotate its signing key or
// its certificate authority without an operator visiting every host.
//
// The material is fetched on each check-in and is only adopted if it is **signed by a
// key this runner already trusts**. That is the crucial part: the connection being
// authenticated is not enough. If it were, taking over the server would be enough to
// introduce a new signing key and get arbitrary code executed — the exact thing
// dispatch signing prevents. Tying the change to the signing key means the authority to
// rotate rests with the key being rotated, which lives only on the server and is
// separate from its TLS key.

// trustFetch is the server's response; mirrors server.TrustBundle.
type trustFetch struct {
	SigningKeys []string `json:"signing_keys"`
	// One signature per key the server holds; any that verifies against a key this
	// runner already trusts authorises the change. Without this a rotation could never
	// start, because a runner on the old key would reject material signed only with the
	// new one.
	SigningKeysSignatures []string `json:"signing_keys_signatures"`
	CABundle              string   `json:"ca_bundle"`
	CABundleSignatures    []string `json:"ca_bundle_signatures"`
}

// verifyAny accepts the data if any of the offered signatures was made by a key this
// runner currently trusts.
func (a *Agent) verifyAny(data []byte, signatures []string) bool {
	for _, encoded := range signatures {
		sig, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			continue
		}
		if err := a.verifier.Verify(data, sig); err == nil {
			return true
		}
	}
	return false
}

// TrustPaths are where the runner keeps the material it has adopted.
type TrustPaths struct {
	// SigningKeys is the cached set of accepted dispatch-signing keys. It takes
	// precedence over the single key install.sh delivered.
	SigningKeys string
	// CACert is the trust bundle used to verify the server.
	CACert string
}

// RefreshTrust fetches the published trust material and adopts whatever is both valid
// and new. It reports whether anything changed, in which case the caller restarts so the
// new material takes effect.
//
// Failures are never fatal: the runner keeps working with what it already has.
func (a *Agent) RefreshTrust(ctx context.Context, paths TrustPaths) (changed bool) {
	if a.verifier == nil {
		return false // development mode: nothing to verify a change against
	}
	fetched, err := a.fetchTrust(ctx)
	if err != nil {
		a.log.Printf("trust: %v", err)
		return false
	}

	if a.adoptSigningKeys(fetched, paths.SigningKeys) {
		changed = true
	}
	if a.adoptCABundle(fetched, paths.CACert) {
		changed = true
	}
	return changed
}

func (a *Agent) fetchTrust(ctx context.Context) (*trustFetch, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.client.BaseURL()+"/api/v1/trust", nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.HTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch trust material: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch trust material: %s", resp.Status)
	}
	var out trustFetch
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode trust material: %w", err)
	}
	return &out, nil
}

// adoptSigningKeys verifies and stores a new set of accepted signing keys.
func (a *Agent) adoptSigningKeys(fetched *trustFetch, path string) bool {
	if path == "" || len(fetched.SigningKeys) == 0 {
		return false
	}
	pems := make([][]byte, 0, len(fetched.SigningKeys))
	for _, k := range fetched.SigningKeys {
		pems = append(pems, []byte(k))
	}
	// Verified against the keys this runner accepts *today*, so only the holder of a
	// currently-trusted key can change the set.
	if !a.verifyAny(crypto.SigningSetBytesToSign(pems), fetched.SigningKeysSignatures) {
		a.log.Printf("trust: REFUSED new signing set — not signed by any key we trust")
		return false
	}
	set, err := crypto.NewSigningSet(pems)
	if err != nil {
		a.log.Printf("trust: new signing set is unusable: %v", err)
		return false
	}
	current, err := os.ReadFile(path)
	if err == nil && bytes.Equal(bytes.TrimSpace(current), bytes.TrimSpace(set.Bytes())) {
		return false // unchanged
	}
	if err := writeFileMode(path, set.Bytes(), 0o644); err != nil {
		a.log.Printf("trust: cannot store signing set: %v", err)
		return false
	}
	a.log.Printf("trust: adopted %d dispatch-signing key(s)", set.Len())
	return true
}

// adoptCABundle verifies and stores a new certificate-authority bundle, which is how a
// runner follows a CA rotation.
func (a *Agent) adoptCABundle(fetched *trustFetch, path string) bool {
	if path == "" || fetched.CABundle == "" {
		return false
	}
	bundle := []byte(fetched.CABundle)
	if !a.verifyAny(crypto.CABundleBytesToSign(bundle), fetched.CABundleSignatures) {
		a.log.Printf("trust: REFUSED new CA bundle — not signed by any key we trust")
		return false
	}
	// It must be usable as a trust store before it replaces the working one, or a bad
	// bundle would cut this runner off from the server entirely.
	if _, err := crypto.ParseCAPool(bundle); err != nil {
		a.log.Printf("trust: new CA bundle is unusable: %v", err)
		return false
	}
	current, err := os.ReadFile(path)
	if err == nil && bytes.Equal(bytes.TrimSpace(current), bytes.TrimSpace(bundle)) {
		return false // unchanged
	}
	if err := writeFileMode(path, bundle, 0o644); err != nil {
		a.log.Printf("trust: cannot store CA bundle: %v", err)
		return false
	}
	a.log.Printf("trust: adopted a new CA bundle (%s)", path)
	return true
}

// LoadTrustedSigningKeys returns the signing keys a runner should accept: the cached set
// if there is one, otherwise the single key install.sh delivered.
func LoadTrustedSigningKeys(cachedSetPath, bootstrapKeyPath string) (crypto.Verifier, error) {
	if cachedSetPath != "" {
		if data, err := os.ReadFile(cachedSetPath); err == nil {
			set, err := crypto.ParseSigningSetBytes(data)
			if err == nil {
				return set, nil
			}
			// A corrupt cache must not lock the runner out; fall back to the bootstrap key.
		}
	}
	return crypto.LoadSigningSet(bootstrapKeyPath)
}
