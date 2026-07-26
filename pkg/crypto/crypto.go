// Package crypto holds Arcatum's security primitives:
//
//   - PKI (pki.go): the Arcatum CA, the server certificate, and one client
//     certificate per runner, so both sides authenticate each other.
//   - TLS (tls.go): mTLS configs — the server requires a client certificate signed
//     by the CA, the runner verifies the server against the same CA.
//   - Signing (sign.go): Ed25519 signatures over a JobDispatch. mTLS proves who the
//     peer is on the wire; the signature proves the *job* originates from Arcatum
//     and was not altered.
//   - Secrets at rest (secretbox.go): AES-256-GCM encryption of instance secrets in
//     the database, so a copy of arcatum.db does not leak credentials.
package crypto

// Signer signs outgoing job dispatches (server side).
type Signer interface {
	Sign(data []byte) (sig []byte, err error)
}

// Verifier verifies server signatures on received dispatches (runner side).
type Verifier interface {
	Verify(data, sig []byte) error
}

// SecretBox encrypts and decrypts instance secrets at rest on the server. The context
// argument is additional authenticated data binding a ciphertext to the instance and
// secret name it belongs to — see SecretContext.
type SecretBox interface {
	Seal(plaintext, context []byte) (ciphertext []byte, err error)
	Open(ciphertext, context []byte) (plaintext []byte, err error)
}

// Compile-time checks that the concrete types satisfy the interfaces.
var (
	_ Signer    = (*Ed25519Signer)(nil)
	_ Verifier  = (*Ed25519Verifier)(nil)
	_ SecretBox = (*AESSecretBox)(nil)
)
