package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Instance secrets (database passwords and the like) are stored encrypted so that a
// copy of arcatum.db — a backup of it, a stolen disk, a careless scp — does not hand
// over every credential in the network.
//
// AES-256-GCM with a random nonce per value. Each value is encrypted separately, so
// secret *names* stay visible (the UI can show which are set) while values do not.
//
// Every ciphertext is bound to its instance and secret name through GCM's additional
// authenticated data. Someone who can write to the database therefore cannot move a
// ciphertext from one instance or parameter to another: the tag no longer verifies.

// sealedPrefix marks an encrypted value in the database and carries a version so the
// format can change later. Values without it are legacy plaintext.
const sealedPrefix = "enc:v1:"

// masterKeyLen is the AES-256 key length.
const masterKeyLen = 32

// ErrSealed is returned when an encrypted value is found but no key is configured.
var ErrSealed = errors.New("value is encrypted but no secrets master key is configured")

// AESSecretBox encrypts and decrypts instance secrets at rest.
type AESSecretBox struct{ aead cipher.AEAD }

// GenerateMasterKey returns the contents for a new master key file.
func GenerateMasterKey() ([]byte, error) {
	key := make([]byte, masterKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}
	// Base64 with a trailing newline keeps the file printable and easy to handle.
	return []byte(base64.StdEncoding.EncodeToString(key) + "\n"), nil
}

// LoadSecretBox reads a master key file and returns a box for it.
func LoadSecretBox(path string) (*AESSecretBox, error) {
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
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &AESSecretBox{aead: aead}, nil
}

// Seal encrypts plaintext, binding it to context (see SecretContext). The nonce is
// prepended to the ciphertext.
func (b *AESSecretBox) Seal(plaintext, context []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	return b.aead.Seal(nonce, nonce, plaintext, context), nil
}

// Open decrypts a ciphertext produced by Seal with the same context.
func (b *AESSecretBox) Open(ciphertext, context []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(ciphertext) < n {
		return nil, errors.New("ciphertext too short")
	}
	plaintext, err := b.aead.Open(nil, ciphertext[:n], ciphertext[n:], context)
	if err != nil {
		// Wrong key, tampered data, or a ciphertext moved to another instance/name.
		return nil, fmt.Errorf("decrypt secret: %w", err)
	}
	return plaintext, nil
}

// SecretContext builds the additional authenticated data binding a secret to the
// instance and parameter name it belongs to.
func SecretContext(instanceID, secretName string) []byte {
	return []byte("arcatum-secret/v1\x00" + instanceID + "\x00" + secretName)
}

// IsSealed reports whether a stored value is encrypted.
func IsSealed(value string) bool { return strings.HasPrefix(value, sealedPrefix) }

// SealToString encrypts a secret value into its database representation. A nil box
// means no key is configured: the value is stored as-is (development mode).
func SealToString(box SecretBox, value, instanceID, secretName string) (string, error) {
	if box == nil {
		return value, nil
	}
	sealed, err := box.Seal([]byte(value), SecretContext(instanceID, secretName))
	if err != nil {
		return "", err
	}
	return sealedPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// OpenFromString decrypts a stored value. Plaintext values are returned unchanged so
// databases written before encryption was enabled keep working; an encrypted value
// with no key configured is an error rather than a silent empty password.
func OpenFromString(box SecretBox, stored, instanceID, secretName string) (string, error) {
	if !IsSealed(stored) {
		return stored, nil
	}
	if box == nil {
		return "", fmt.Errorf("%s/%s: %w", instanceID, secretName, ErrSealed)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, sealedPrefix))
	if err != nil {
		return "", fmt.Errorf("%s/%s: not valid base64: %w", instanceID, secretName, err)
	}
	plaintext, err := box.Open(raw, SecretContext(instanceID, secretName))
	if err != nil {
		return "", fmt.Errorf("%s/%s: %w", instanceID, secretName, err)
	}
	return string(plaintext), nil
}
