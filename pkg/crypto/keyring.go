package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// A Keyring holds the secrets master key plus any predecessors, which is what makes key
// rotation possible: the newest key encrypts, and older ones stay available so values
// written before the rotation can still be read.
//
// Stored values carry the id of the key that sealed them:
//
//	enc:v2:<keyid>:<base64(nonce||ciphertext)>
//
// The id makes two things cheap. Decryption picks the right key directly instead of
// trying each one, and re-encryption can tell at a glance which values are already on
// the current key — so rotating a large database is resumable and can be repeated
// without redoing work.
//
// Values in the older enc:v1: format carry no id; every key is tried for those.

const sealedPrefixV2 = "enc:v2:"

// keyEntry is one master key, identified by a short digest of the key material.
type keyEntry struct {
	id   string
	aead cipher.AEAD
}

// Keyring is an ordered set of master keys; the first is the one used for encryption.
type Keyring struct {
	keys []keyEntry
}

// keyID derives the short identifier stored alongside a ciphertext. It is a digest of
// the key, so it identifies the key without revealing it.
func keyID(key []byte) string {
	sum := sha256.Sum256(append([]byte("arcatum-master-key/v1\x00"), key...))
	return hex.EncodeToString(sum[:4])
}

// LoadKeyring reads the primary master key and any previous keys still needed to read
// older values. Duplicate keys are collapsed, so listing the primary again is harmless.
func LoadKeyring(primaryPath string, previousPaths []string) (*Keyring, error) {
	if primaryPath == "" {
		return nil, errors.New("keyring: no primary master key")
	}
	kr := &Keyring{}
	for i, p := range append([]string{primaryPath}, previousPaths...) {
		key, err := readMasterKey(p)
		if err != nil {
			return nil, err
		}
		id := keyID(key)
		if kr.has(id) {
			continue
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		kr.keys = append(kr.keys, keyEntry{id: id, aead: aead})
		_ = i
	}
	return kr, nil
}

func (k *Keyring) has(id string) bool {
	for _, e := range k.keys {
		if e.id == id {
			return true
		}
	}
	return false
}

// readMasterKey reads and validates a base64 master key file.
func readMasterKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read master key: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("%s: master key must be base64: %w", path, err)
	}
	if len(key) != masterKeyLen {
		return nil, fmt.Errorf("%s: master key must be %d bytes, got %d", path, masterKeyLen, len(key))
	}
	return key, nil
}

// Has reports whether a key with this id is loaded, which is what tells a caller
// holding a ciphertext whether this server can read it. An empty id means a v1 value,
// which carries no id and is readable as long as there is any key at all.
func (k *Keyring) Has(id string) bool {
	if id == "" {
		return len(k.keys) > 0
	}
	return k.has(id)
}

// SealedKeyID reports which master key a stored value was sealed with. sealed is false
// for a legacy plaintext value. A v1 ciphertext is sealed but carries no key id, which
// is reported as an empty id — every loaded key is a candidate for it.
//
// This is what lets a configuration archive be checked before it is imported: the
// ciphertexts travel without the keys, so a server that cannot decrypt them should say
// so up front rather than after the secrets are already in its database.
func SealedKeyID(stored string) (id string, sealed bool) {
	switch {
	case strings.HasPrefix(stored, sealedPrefixV2):
		id, _, _ = strings.Cut(strings.TrimPrefix(stored, sealedPrefixV2), ":")
		return id, true
	case IsSealed(stored):
		return "", true
	default:
		return "", false
	}
}

// PrimaryID is the id of the key new values are sealed with.
func (k *Keyring) PrimaryID() string {
	if len(k.keys) == 0 {
		return ""
	}
	return k.keys[0].id
}

// Len reports how many keys are available for decryption.
func (k *Keyring) Len() int { return len(k.keys) }

// SealString encrypts a secret value into its stored representation, tagged with the
// primary key's id.
func (k *Keyring) SealString(value, instanceID, secretName string) (string, error) {
	if len(k.keys) == 0 {
		return "", errors.New("keyring: no keys")
	}
	e := k.keys[0]
	nonce := make([]byte, e.aead.NonceSize())
	if err := randRead(nonce); err != nil {
		return "", err
	}
	sealed := e.aead.Seal(nonce, nonce, []byte(value), SecretContext(instanceID, secretName))
	return sealedPrefixV2 + e.id + ":" + base64.StdEncoding.EncodeToString(sealed), nil
}

// OpenString decrypts a stored value. Plaintext passes through unchanged so databases
// written before encryption was enabled keep working.
func (k *Keyring) OpenString(stored, instanceID, secretName string) (string, error) {
	ctx := SecretContext(instanceID, secretName)

	switch {
	case strings.HasPrefix(stored, sealedPrefixV2):
		id, b64, found := strings.Cut(strings.TrimPrefix(stored, sealedPrefixV2), ":")
		if !found {
			return "", fmt.Errorf("%s/%s: malformed sealed value", instanceID, secretName)
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return "", fmt.Errorf("%s/%s: not valid base64: %w", instanceID, secretName, err)
		}
		for _, e := range k.keys {
			if e.id != id {
				continue
			}
			out, err := openWith(e.aead, raw, ctx)
			if err != nil {
				return "", fmt.Errorf("%s/%s: %w", instanceID, secretName, err)
			}
			return string(out), nil
		}
		return "", fmt.Errorf("%s/%s: sealed with key %s, which is not configured — "+
			"add it to [secrets] previous_keys", instanceID, secretName, id)

	case IsSealed(stored): // enc:v1:, no key id — try every key
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, sealedPrefix))
		if err != nil {
			return "", fmt.Errorf("%s/%s: not valid base64: %w", instanceID, secretName, err)
		}
		for _, e := range k.keys {
			if out, err := openWith(e.aead, raw, ctx); err == nil {
				return string(out), nil
			}
		}
		return "", fmt.Errorf("%s/%s: no configured master key can decrypt this value",
			instanceID, secretName)

	default:
		return stored, nil // legacy plaintext
	}
}

// IsAnySealed reports whether a stored value is encrypted in any supported format. It
// exists so a server with no master key can refuse loudly instead of handing back a
// ciphertext as if it were a password.
func IsAnySealed(value string) bool {
	return strings.HasPrefix(value, sealedPrefixV2) || IsSealed(value)
}

// IsOnPrimary reports whether a stored value is already sealed with the primary key,
// which is how re-encryption skips work it has already done.
func (k *Keyring) IsOnPrimary(stored string) bool {
	if !strings.HasPrefix(stored, sealedPrefixV2) {
		return false // plaintext or v1: needs re-encryption
	}
	id, _, _ := strings.Cut(strings.TrimPrefix(stored, sealedPrefixV2), ":")
	return id == k.PrimaryID()
}

func openWith(aead cipher.AEAD, raw, ctx []byte) ([]byte, error) {
	n := aead.NonceSize()
	if len(raw) < n {
		return nil, errors.New("ciphertext too short")
	}
	out, err := aead.Open(nil, raw[:n], raw[n:], ctx)
	if err != nil {
		return nil, fmt.Errorf("decrypt secret: %w", err)
	}
	return out, nil
}
