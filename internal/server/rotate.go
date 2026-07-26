package server

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"time"

	"arcatum/pkg/crypto"
)

// Rotation of the three long-lived secrets — the secrets master key, the dispatch
// signing key and the CA — has one hard part in common: new trust material has to reach
// every runner *before* the old one stops being used, or hosts drop out one by one.
//
// The approach is the same for all three: keep an overlap window in which both the old
// and the new material are valid, let runners pick the new one up by themselves over the
// authenticated connection, and let an operator close the window once the server can
// confirm every runner has moved. The operator decides *when*; the system does the work
// and reports whether it is safe to finish.
//
// What is deliberately *not* automated is the final cutover. Dropping a trust anchor is
// the one operation that can lock an operator out of their own fleet, and an unattended
// job that gets it wrong at three in the morning leaves runners trusting neither the old
// nor the new authority. Routine certificate renewal is automatic precisely because its
// failure mode is safe: the old certificate keeps working.

// TrustBundle is what a runner fetches to learn what to trust.
//
// Each part carries *several* signatures — one from every signing key the server holds.
// That is what lets a rotation start at all: a runner still on the old key would reject
// a set signed only with the new one, and the server cannot know which key any given
// runner has reached yet. With one signature per key, each runner verifies with whichever
// it currently trusts, and the authority to change trust still rests on possession of a
// signing key rather than on control of the server.
type TrustBundle struct {
	// SigningKeys are the PEM public keys whose dispatch signatures to accept.
	SigningKeys []string `json:"signing_keys"`
	// SigningKeysSignatures covers the canonical form of SigningKeys, once per key held.
	SigningKeysSignatures []string `json:"signing_keys_signatures"`
	// CABundle is the PEM bundle of certificate authorities to trust.
	CABundle string `json:"ca_bundle,omitempty"`
	// CABundleSignatures covers CABundle, once per key held.
	CABundleSignatures []string `json:"ca_bundle_signatures,omitempty"`
}

// RotationOptions carries what the server needs to publish trust material.
type RotationOptions struct {
	// SigningSet is every public key runners should accept, newest first.
	SigningSet *crypto.SigningSet
	// Signers are the matching private keys, newest first. Every one of them signs the
	// published material so runners on any generation of key can verify it.
	Signers []crypto.Signer
	// CABundlePEM is the trust bundle: every authority still accepted.
	CABundlePEM []byte
	// SigningCAName is the common name of the authority new certificates are issued
	// under. Runners still on a different issuer have not completed a CA rotation.
	SigningCAName string
}

// handleTrustBundle serves the signed trust material. Runners call it on every check-in,
// which is what makes rotation propagate without an operator touching each host.
func (s *Server) handleTrustBundle(w http.ResponseWriter, r *http.Request) {
	// Runners and operators may both read it; a rejected runner may not.
	if s.requireClientCert {
		cert := peerCert(r)
		if cert == nil {
			http.Error(w, "client certificate required", http.StatusUnauthorized)
			return
		}
		if crypto.CertRole(cert) == crypto.RoleRunner {
			if err := s.requireApprovedRunner(cert.Subject.CommonName); err != nil {
				s.denyRunner(w, err)
				return
			}
		}
	}
	if s.rotation.SigningSet == nil || len(s.rotation.Signers) == 0 {
		http.Error(w, "server has no signing key configured", http.StatusServiceUnavailable)
		return
	}

	pems := s.rotation.SigningSet.PEMs()
	keys := make([]string, 0, len(pems))
	for _, p := range pems {
		keys = append(keys, string(p))
	}
	setSigs, err := s.signWithAll(crypto.SigningSetBytesToSign(pems))
	if err != nil {
		s.log.Printf("trust bundle: sign signing set: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := TrustBundle{SigningKeys: keys, SigningKeysSignatures: setSigs}

	if len(s.rotation.CABundlePEM) > 0 {
		caSigs, err := s.signWithAll(crypto.CABundleBytesToSign(s.rotation.CABundlePEM))
		if err != nil {
			s.log.Printf("trust bundle: sign CA bundle: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		out.CABundle = string(s.rotation.CABundlePEM)
		out.CABundleSignatures = caSigs
	}
	writeJSON(w, out)
}

// signWithAll produces one signature per signing key the server holds, so a runner can
// verify with whichever generation of key it has reached.
func (s *Server) signWithAll(data []byte) ([]string, error) {
	sigs := make([]string, 0, len(s.rotation.Signers))
	for _, signer := range s.rotation.Signers {
		sig, err := signer.Sign(data)
		if err != nil {
			return nil, err
		}
		sigs = append(sigs, base64.StdEncoding.EncodeToString(sig))
	}
	return sigs, nil
}

// RotationStatus tells an operator whether a rotation can be finished.
type RotationStatus struct {
	// SecretsKeyID is the master key new secrets are sealed with.
	SecretsKeyID string `json:"secrets_key_id,omitempty"`
	// SecretsKeys is how many master keys are loaded; more than one means a rotation is
	// in progress.
	SecretsKeys int `json:"secrets_keys"`
	// SecretsPending is how many stored values are not yet on the current key.
	SecretsPending int `json:"secrets_pending"`

	// SigningKeys is how many dispatch-signing keys runners are told to accept.
	SigningKeys int `json:"signing_keys"`

	// SigningCA is the authority new certificates are issued under.
	SigningCA string `json:"signing_ca,omitempty"`
	// TrustedCAs lists every authority in the trust bundle.
	TrustedCAs []CAInfo `json:"trusted_cas,omitempty"`
	// RunnersOnOldCA are approved runners whose certificate was issued by something
	// other than the current authority — the old CA cannot be dropped until this is empty.
	RunnersOnOldCA []string `json:"runners_on_old_ca,omitempty"`
	// RunnersUnknownCA are runners that have not checked in since issuer tracking was
	// added, so their authority is not known yet.
	RunnersUnknownCA []string `json:"runners_unknown_ca,omitempty"`
	// SafeToDropOldCA is true when every approved runner is on the current authority.
	SafeToDropOldCA bool `json:"safe_to_drop_old_ca"`
	// ServerCertIssuer is the authority that issued this server's own certificate.
	ServerCertIssuer string `json:"server_cert_issuer,omitempty"`
	// Warning flags an ordering mistake that would cut runners off. Reissuing the
	// server's certificate under the new authority before runners have adopted the trust
	// bundle leaves them unable to connect — and therefore unable to ever fetch it.
	Warning string `json:"warning,omitempty"`
}

// CAInfo describes one authority in the trust bundle.
type CAInfo struct {
	CommonName string    `json:"common_name"`
	NotAfter   time.Time `json:"not_after"`
	DaysLeft   int       `json:"days_left"`
	IsSigner   bool      `json:"is_signer"`
}

// handleRotationStatus reports the state of all three rotations (admin only).
func (s *Server) handleRotationStatus(w http.ResponseWriter, r *http.Request) {
	st := RotationStatus{SigningCA: s.rotation.SigningCAName}
	if s.store.box != nil {
		st.SecretsKeyID = s.store.box.PrimaryID()
		st.SecretsKeys = s.store.box.Len()
		st.SecretsPending = s.store.countSecretsNotOnPrimary()
	}
	if s.rotation.SigningSet != nil {
		st.SigningKeys = s.rotation.SigningSet.Len()
	}
	for _, cert := range parseCABundle(s.rotation.CABundlePEM) {
		st.TrustedCAs = append(st.TrustedCAs, CAInfo{
			CommonName: cert.Subject.CommonName,
			NotAfter:   cert.NotAfter,
			DaysLeft:   daysUntil(cert.NotAfter),
			IsSigner:   cert.Subject.CommonName == s.rotation.SigningCAName,
		})
	}

	runners, err := s.store.Runners()
	if err != nil {
		s.log.Printf("rotation status: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, rn := range runners {
		if rn.Status != EnrollApproved {
			continue
		}
		switch {
		case rn.CertIssuer == "":
			st.RunnersUnknownCA = append(st.RunnersUnknownCA, rn.ID)
		case s.rotation.SigningCAName != "" && rn.CertIssuer != s.rotation.SigningCAName:
			st.RunnersOnOldCA = append(st.RunnersOnOldCA, rn.ID)
		}
	}
	// Unknown counts as not-yet-migrated: dropping an authority on a guess is exactly
	// the mistake this endpoint exists to prevent.
	st.SafeToDropOldCA = len(st.RunnersOnOldCA) == 0 && len(st.RunnersUnknownCA) == 0 &&
		s.rotation.SigningCAName != ""

	st.ServerCertIssuer = s.serverCertIssuer
	// The server's certificate must stay under the authority every runner already trusts
	// until they have all adopted the bundle. Otherwise they cannot complete the
	// handshake, and a runner that cannot connect can never fetch the bundle that would
	// fix it — the rotation deadlocks.
	if len(st.TrustedCAs) > 1 && st.ServerCertIssuer != "" &&
		st.ServerCertIssuer == s.rotation.SigningCAName &&
		(len(st.RunnersOnOldCA) > 0 || len(st.RunnersUnknownCA) > 0) {
		st.Warning = fmt.Sprintf("the server certificate is already issued by %q, but some runners "+
			"may still trust only the previous authority; they cannot connect and so cannot adopt "+
			"the new bundle. Reissue the server certificate under the previous authority until "+
			"every runner reports the new one.", st.ServerCertIssuer)
	}
	writeJSON(w, st)
}

// handleRekeySecrets re-encrypts every stored secret with the current master key
// (admin only). Safe to repeat: values already on the current key are skipped.
func (s *Server) handleRekeySecrets(w http.ResponseWriter, r *http.Request) {
	res, err := s.store.RekeySecrets()
	if err != nil {
		s.log.Printf("rekey: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Printf("rekey: %d secret(s) re-encrypted with key %s, %d already current, %d unreadable",
		res.Secrets, res.KeyID, res.Skipped, res.Remaining)
	writeJSON(w, res)
}

// countSecretsNotOnPrimary counts stored values still sealed with an older key, which is
// what tells an operator whether a master-key rotation is finished.
func (s *Store) countSecretsNotOnPrimary() int {
	if s.box == nil {
		return 0
	}
	rows, err := s.db.Query(`SELECT secrets FROM instances`)
	if err != nil {
		return 0
	}
	defer rows.Close()
	pending := 0
	for rows.Next() {
		var secretsJSON string
		if err := rows.Scan(&secretsJSON); err != nil {
			continue
		}
		var stored map[string]string
		if err := json.Unmarshal([]byte(secretsJSON), &stored); err != nil {
			continue
		}
		for _, v := range stored {
			if !s.box.IsOnPrimary(v) {
				pending++
			}
		}
	}
	return pending
}

// parseCABundle reads every certificate from a PEM bundle, ignoring anything unparsable.
func parseCABundle(bundlePEM []byte) []*x509.Certificate {
	var out []*x509.Certificate
	rest := bundlePEM
	for {
		block, remainder := pem.Decode(rest)
		if block == nil {
			return out
		}
		rest = remainder
		if block.Type != "CERTIFICATE" {
			continue
		}
		if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
			out = append(out, cert)
		}
	}
}

// ReadTrustBundle loads the CA trust bundle from disk. A missing or unreadable file
// simply means nothing to publish; TLS setup would already have failed if it mattered.
func ReadTrustBundle(path string) []byte {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return data
}
