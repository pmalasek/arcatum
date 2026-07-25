// Package crypto is the intended home for Arcatum's security primitives:
//   - mTLS identity for runners (enrollment: CSR -> admin approval -> client cert)
//   - signing of JobDispatch on the server, verification on the runner
//   - encryption of instance secrets at rest, decrypted only at dispatch time
//
// This is a placeholder that pins down the interfaces; implementations land later.
package crypto

// Signer signs outgoing job dispatches (server side).
type Signer interface {
	Sign(data []byte) (sig []byte, err error)
}

// Verifier verifies server signatures on received dispatches (runner side).
type Verifier interface {
	Verify(data, sig []byte) error
}

// SecretBox encrypts and decrypts instance secrets at rest on the server.
type SecretBox interface {
	Seal(plaintext []byte) (ciphertext []byte, err error)
	Open(ciphertext []byte) (plaintext []byte, err error)
}
