package crypto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"sort"
)

// Rotating the dispatch-signing key needs an overlap: the server has to start signing
// with the new key only once every runner already trusts it. So runners hold a *set* of
// trusted signing keys rather than one, and the server publishes the current set.
//
// The catch is that a set the server hands out is only as trustworthy as the server. If
// a runner adopted whatever it was sent, anyone who took over the server could add their
// own key and get code executed — exactly what dispatch signing exists to prevent.
//
// So the published set is itself **signed with the current signing key**. Authority to
// introduce a new key therefore rests with whoever holds the key being replaced, which
// lives only on the server and is separate from its TLS key. A runner adopts a new set
// only if the signature verifies against a key it already trusts.

// SigningSet is a set of public keys whose signatures a runner accepts.
type SigningSet struct {
	keys []ed25519.PublicKey
	pems [][]byte
}

// NewSigningSet builds a set from PEM-encoded public keys.
func NewSigningSet(pems [][]byte) (*SigningSet, error) {
	s := &SigningSet{}
	seen := map[string]bool{}
	for _, p := range pems {
		key, err := parsePublicKeyPEM(p)
		if err != nil {
			return nil, err
		}
		fingerprint := string(key)
		if seen[fingerprint] {
			continue
		}
		seen[fingerprint] = true
		s.keys = append(s.keys, key)
		s.pems = append(s.pems, normalisePEM(p))
	}
	if len(s.keys) == 0 {
		return nil, errors.New("signing set: no keys")
	}
	return s, nil
}

// LoadSigningSet reads the public keys from files.
func LoadSigningSet(paths ...string) (*SigningSet, error) {
	var pems [][]byte
	for _, p := range paths {
		if p == "" {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read signing public key: %w", err)
		}
		pems = append(pems, data)
	}
	return NewSigningSet(pems)
}

// Verify accepts a signature made by any key in the set. During a rotation both the old
// and the new key are present, so dispatches signed with either are accepted.
func (s *SigningSet) Verify(data, sig []byte) error {
	for _, key := range s.keys {
		if ed25519.Verify(key, data, sig) {
			return nil
		}
	}
	return ErrBadSignature
}

// PEMs returns the keys in the order they were added, for publishing or storing.
func (s *SigningSet) PEMs() [][]byte { return s.pems }

// Len reports how many keys are trusted.
func (s *SigningSet) Len() int { return len(s.keys) }

// Bytes serialises the set into one PEM blob, which is how a runner caches it.
func (s *SigningSet) Bytes() []byte {
	var buf bytes.Buffer
	for _, p := range s.pems {
		buf.Write(p)
	}
	return buf.Bytes()
}

// ParseSigningSetBytes reads a set back from the concatenated form Bytes produces.
func ParseSigningSetBytes(data []byte) (*SigningSet, error) {
	var pems [][]byte
	rest := data
	for {
		block, remainder := pem.Decode(rest)
		if block == nil {
			break
		}
		pems = append(pems, pem.EncodeToMemory(block))
		rest = remainder
	}
	if len(pems) == 0 {
		return nil, errors.New("signing set: no PEM blocks")
	}
	return NewSigningSet(pems)
}

// SigningSetBytesToSign is the canonical form signed when publishing a set.
//
// Keys are sorted and length-prefixed, so the signature covers exactly which keys are in
// the set regardless of the order they were listed in — and no key can be spliced in or
// out without breaking it.
func SigningSetBytesToSign(pems [][]byte) []byte {
	sorted := make([]string, 0, len(pems))
	for _, p := range pems {
		sorted = append(sorted, string(normalisePEM(p)))
	}
	sort.Strings(sorted)

	var buf bytes.Buffer
	buf.WriteString("arcatum-signing-set/v1")
	for _, p := range sorted {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(p)))
		buf.Write(n[:])
		buf.WriteString(p)
	}
	return buf.Bytes()
}

// CABundleBytesToSign is the canonical form signed when publishing the CA bundle. The
// bundle tells a runner which authorities to trust, so it is signed for the same reason
// the signing set is: the server alone must not be able to redirect that trust.
func CABundleBytesToSign(bundlePEM []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString("arcatum-ca-bundle/v1")
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(bundlePEM)))
	buf.Write(n[:])
	buf.Write(bundlePEM)
	return buf.Bytes()
}

func parsePublicKeyPEM(data []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("signing key: no PEM block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse signing public key: %w", err)
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("signing key: not an Ed25519 public key")
	}
	return key, nil
}

// normalisePEM re-encodes a PEM block so equivalent keys compare equal regardless of
// stray whitespace in the file they came from.
func normalisePEM(data []byte) []byte {
	block, _ := pem.Decode(data)
	if block == nil {
		return data
	}
	return pem.EncodeToMemory(&pem.Block{Type: block.Type, Bytes: block.Bytes})
}
