package server

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"arcatum/pkg/crypto"
	"arcatum/pkg/proto"
)

// masterKeyFile writes a fresh secrets master key.
func masterKeyFile(t *testing.T, dir, name string) string {
	t.Helper()
	key, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, key, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return path
}

// storeWithKeyring opens a store on dbPath using the given master keys.
func storeWithKeyring(t *testing.T, dbPath, backupDir, primary string, previous []string) *Store {
	t.Helper()
	kr, err := crypto.LoadKeyring(primary, previous)
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	st, err := Open(dbPath, backupDir, kr)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return st
}

// A master-key rotation: add a new key, re-encrypt, and the old key becomes unnecessary.
func TestRekeySecretsRotatesToNewKey(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	backupDir := filepath.Join(dir, "backup")
	oldKey := masterKeyFile(t, dir, "old.key")
	newKey := masterKeyFile(t, dir, "new.key")

	// Written under the old key.
	st := storeWithKeyring(t, dbPath, backupDir, oldKey, nil)
	importOneSecret(t, st, dir)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Rotation window: new key primary, old one still available.
	st = storeWithKeyring(t, dbPath, backupDir, newKey, []string{oldKey})
	if pending := st.countSecretsNotOnPrimary(); pending != 1 {
		t.Fatalf("pending = %d, want 1 before re-encryption", pending)
	}
	res, err := st.RekeySecrets()
	if err != nil {
		t.Fatalf("RekeySecrets: %v", err)
	}
	if res.Secrets != 1 || res.Remaining != 0 {
		t.Fatalf("rekey = %+v, want 1 re-encrypted and nothing left", res)
	}
	if st.countSecretsNotOnPrimary() != 0 {
		t.Error("values remain on an older key after re-encryption")
	}
	// Still readable, obviously.
	got, err := st.Instance("mysql-web01")
	if err != nil || got == nil || got.Secrets["password"] != testSecret {
		t.Fatalf("secret unreadable after rekey: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The whole point: the old key can now be dropped from the configuration.
	newOnly := storeWithKeyring(t, dbPath, backupDir, newKey, nil)
	defer newOnly.Close()
	got, err = newOnly.Instance("mysql-web01")
	if err != nil || got == nil || got.Secrets["password"] != testSecret {
		t.Fatalf("after rotation the secret must be readable with the new key alone: %v", err)
	}
}

// Re-running must be cheap and safe, so an interrupted pass can simply be repeated.
func TestRekeySecretsIsRepeatable(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	backupDir := filepath.Join(dir, "backup")
	key := masterKeyFile(t, dir, "master.key")

	st := storeWithKeyring(t, dbPath, backupDir, key, nil)
	defer st.Close()
	importOneSecret(t, st, dir)

	first, err := st.RekeySecrets()
	if err != nil {
		t.Fatalf("RekeySecrets: %v", err)
	}
	// Already on the current key from the import, so nothing to do.
	if first.Secrets != 0 || first.Skipped != 1 {
		t.Errorf("first pass = %+v, want everything skipped", first)
	}
	second, err := st.RekeySecrets()
	if err != nil {
		t.Fatalf("RekeySecrets again: %v", err)
	}
	if second.Secrets != 0 || second.Skipped != 1 {
		t.Errorf("second pass = %+v, want everything skipped again", second)
	}
}

// A value whose key is gone must be reported, not silently dropped or crash the pass.
func TestRekeySecretsReportsUnreadableValues(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	backupDir := filepath.Join(dir, "backup")
	oldKey := masterKeyFile(t, dir, "old.key")
	newKey := masterKeyFile(t, dir, "new.key")

	st := storeWithKeyring(t, dbPath, backupDir, oldKey, nil)
	importOneSecret(t, st, dir)
	st.Close()

	// The old key was forgotten, so its values cannot be re-encrypted.
	st = storeWithKeyring(t, dbPath, backupDir, newKey, nil)
	defer st.Close()
	res, err := st.RekeySecrets()
	if err != nil {
		t.Fatalf("RekeySecrets: %v", err)
	}
	if res.Remaining != 1 || res.Secrets != 0 {
		t.Errorf("rekey = %+v, want the unreadable value reported as remaining", res)
	}
}

func TestRekeyWithoutMasterKeyIsANoop(t *testing.T) {
	st, dir := openTestStore(t) // no keyring
	importOneSecret(t, st, dir)
	res, err := st.RekeySecrets()
	if err != nil {
		t.Fatalf("RekeySecrets: %v", err)
	}
	if res.Secrets != 0 || res.Note == "" {
		t.Errorf("rekey = %+v, want a no-op explaining why", res)
	}
}

// --- trust bundle -----------------------------------------------------------

// rotationServer builds a server with signing keys and a CA bundle configured.
func rotationServer(t *testing.T, requireClientCert bool) (*Server, *crypto.Ed25519Signer, *crypto.SigningSet) {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "test.db"), filepath.Join(dir, "backup"), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	privPEM, pubPEM, err := crypto.GenerateSigningKey()
	if err != nil {
		t.Fatalf("GenerateSigningKey: %v", err)
	}
	keyPath := filepath.Join(dir, "signing.key")
	if err := os.WriteFile(keyPath, privPEM, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	signer, err := crypto.LoadSigner(keyPath)
	if err != nil {
		t.Fatalf("LoadSigner: %v", err)
	}
	set, err := crypto.NewSigningSet([][]byte{pubPEM})
	if err != nil {
		t.Fatalf("NewSigningSet: %v", err)
	}
	ca, err := crypto.CreateCA("Arcatum CA", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateCA: %v", err)
	}
	caPath := filepath.Join(dir, "ca.pem")
	if err := ca.Save(caPath, filepath.Join(dir, "ca.key")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	bundle, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("read ca: %v", err)
	}

	srv := &Server{
		store:             st,
		log:               log.New(io.Discard, "", 0),
		requireClientCert: requireClientCert,
		signer:            signer,
		ca:                ca,
		sched:             NewScheduler(time.UTC),
		catalog:           &Catalog{byName: map[string]*ScriptEntry{}},
		rotation: RotationOptions{
			SigningSet:    set,
			Signers:       []crypto.Signer{signer},
			CABundlePEM:   bundle,
			SigningCAName: "Arcatum CA",
		},
	}
	return srv, signer, set
}

// The published trust material must be signed by a currently-trusted key, so only the
// holder of that key can redirect what runners trust.
func TestTrustBundleIsSigned(t *testing.T) {
	srv, _, set := rotationServer(t, false)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/trust", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("trust = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var tb TrustBundle
	if err := json.Unmarshal(rec.Body.Bytes(), &tb); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(tb.SigningKeys) != 1 || len(tb.SigningKeysSignatures) != 1 {
		t.Fatalf("trust bundle = %+v, want one signed key", tb)
	}

	// Verify exactly as a runner does.
	pems := make([][]byte, 0, len(tb.SigningKeys))
	for _, k := range tb.SigningKeys {
		pems = append(pems, []byte(k))
	}
	sig, err := base64.StdEncoding.DecodeString(tb.SigningKeysSignatures[0])
	if err != nil {
		t.Fatalf("signature not base64: %v", err)
	}
	if err := set.Verify(crypto.SigningSetBytesToSign(pems), sig); err != nil {
		t.Errorf("published signing set does not verify against the trusted key: %v", err)
	}

	if tb.CABundle == "" || len(tb.CABundleSignatures) != 1 {
		t.Fatal("the CA bundle must be published and signed")
	}
	caSig, err := base64.StdEncoding.DecodeString(tb.CABundleSignatures[0])
	if err != nil {
		t.Fatalf("CA signature not base64: %v", err)
	}
	if err := set.Verify(crypto.CABundleBytesToSign([]byte(tb.CABundle)), caSig); err != nil {
		t.Errorf("published CA bundle does not verify: %v", err)
	}
}

// Tampering with the published material must break the signature — that is what protects
// a runner from being told to trust something else.
func TestTrustBundleTamperingIsDetected(t *testing.T) {
	srv, _, set := rotationServer(t, false)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/trust", nil))
	var tb TrustBundle
	if err := json.Unmarshal(rec.Body.Bytes(), &tb); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sig, _ := base64.StdEncoding.DecodeString(tb.SigningKeysSignatures[0])

	// An attacker appends their own key to the set.
	_, hostilePub, err := crypto.GenerateSigningKey()
	if err != nil {
		t.Fatalf("GenerateSigningKey: %v", err)
	}
	tampered := [][]byte{[]byte(tb.SigningKeys[0]), hostilePub}
	if err := set.Verify(crypto.SigningSetBytesToSign(tampered), sig); err == nil {
		t.Error("adding a key to the published set must invalidate the signature")
	}

	// And tampers with the CA bundle.
	caSig, _ := base64.StdEncoding.DecodeString(tb.CABundleSignatures[0])
	if err := set.Verify(crypto.CABundleBytesToSign([]byte(tb.CABundle+"extra")), caSig); err == nil {
		t.Error("changing the CA bundle must invalidate the signature")
	}
}

// A revoked runner must not keep pulling trust material.
func TestTrustBundleRefusedForRevokedRunner(t *testing.T) {
	srv, _, _ := rotationServer(t, true)
	enrolledRunner(t, srv, "web-01")
	if rec := adminPost(t, srv, "/api/v1/runners/web-01/revoke", adminCert()); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d", rec.Code)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/trust", nil)
	r.TLS = requestWithCert(certWithCNAndRole("web-01", crypto.RoleRunner)).TLS
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("trust for a revoked runner = %d, want 403", rec.Code)
	}
}

func TestTrustBundleRequiresCertificate(t *testing.T) {
	srv, _, _ := rotationServer(t, true)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/trust", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("trust without a certificate = %d, want 401", rec.Code)
	}
}

// --- rotation status --------------------------------------------------------

func rotationStatus(t *testing.T, srv *Server) RotationStatus {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/rotation", nil)
	if srv.requireClientCert {
		r.TLS = requestWithCert(adminCert()).TLS
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotation = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var st RotationStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return st
}

// A CA rotation may only be finished once every runner is on the new authority. This is
// the check that stops an operator dropping the old one too early.
func TestRotationStatusTracksCAMigration(t *testing.T) {
	srv, _, _ := rotationServer(t, true)

	// A runner still on the old authority.
	enrolledRunner(t, srv, "web-01")
	if err := srv.store.RecordCheckin(
		checkinReq("web-01"), time.Now().Add(time.Hour), time.Now(), "Arcatum CA OLD"); err != nil {
		t.Fatalf("RecordCheckin: %v", err)
	}
	st := rotationStatus(t, srv)
	if len(st.RunnersOnOldCA) != 1 || st.RunnersOnOldCA[0] != "web-01" {
		t.Errorf("runners_on_old_ca = %v, want [web-01]", st.RunnersOnOldCA)
	}
	if st.SafeToDropOldCA {
		t.Error("dropping the old CA must not be reported as safe while a runner still uses it")
	}

	// After renewing onto the current authority.
	if err := srv.store.RecordCheckin(
		checkinReq("web-01"), time.Now().Add(time.Hour), time.Now(), "Arcatum CA"); err != nil {
		t.Fatalf("RecordCheckin: %v", err)
	}
	st = rotationStatus(t, srv)
	if len(st.RunnersOnOldCA) != 0 {
		t.Errorf("runners_on_old_ca = %v, want empty", st.RunnersOnOldCA)
	}
	if !st.SafeToDropOldCA {
		t.Error("with every runner migrated it should be safe to drop the old CA")
	}
}

// A runner that has not checked in since issuer tracking existed has an unknown
// authority, and guessing would be exactly the wrong thing to do.
func TestRotationStatusUnknownIssuerBlocksCutover(t *testing.T) {
	srv, _, _ := rotationServer(t, true)
	enrolledRunner(t, srv, "web-01") // approved, never checked in

	st := rotationStatus(t, srv)
	if len(st.RunnersUnknownCA) != 1 {
		t.Errorf("runners_unknown_ca = %v, want [web-01]", st.RunnersUnknownCA)
	}
	if st.SafeToDropOldCA {
		t.Error("an unknown issuer must block the cutover rather than be assumed migrated")
	}
}

func TestRotationStatusReportsSecretsAndSigning(t *testing.T) {
	dir := t.TempDir()
	oldKey := masterKeyFile(t, dir, "old.key")
	newKey := masterKeyFile(t, dir, "new.key")

	srv, _, _ := rotationServer(t, false)
	// Swap in a store whose keyring is mid-rotation and holds a value on the old key.
	st := storeWithKeyring(t, filepath.Join(dir, "test.db"), filepath.Join(dir, "backup"), oldKey, nil)
	importOneSecret(t, st, dir)
	st.Close()
	srv.store = storeWithKeyring(t, filepath.Join(dir, "test.db"), filepath.Join(dir, "backup"),
		newKey, []string{oldKey})
	t.Cleanup(func() { srv.store.Close() })

	status := rotationStatus(t, srv)
	if status.SecretsKeys != 2 {
		t.Errorf("secrets_keys = %d, want 2 during a rotation", status.SecretsKeys)
	}
	if status.SecretsPending != 1 {
		t.Errorf("secrets_pending = %d, want 1", status.SecretsPending)
	}
	if status.SecretsKeyID == "" {
		t.Error("the current key id should be reported")
	}
	if status.SigningKeys != 1 {
		t.Errorf("signing_keys = %d, want 1", status.SigningKeys)
	}
	if len(status.TrustedCAs) != 1 || !status.TrustedCAs[0].IsSigner {
		t.Errorf("trusted_cas = %+v, want the signing authority marked", status.TrustedCAs)
	}
}

func TestRotationEndpointsRequireAdmin(t *testing.T) {
	srv, _, _ := rotationServer(t, true)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/rotation"},
		{http.MethodPost, "/api/v1/secrets/rekey"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.TLS = requestWithCert(certWithCNAndRole("web-01", crypto.RoleRunner)).TLS
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, r)
			if rec.Code != http.StatusForbidden {
				t.Errorf("as runner = %d, want 403", rec.Code)
			}
		})
	}
}

// parseCABundle must survive a bundle with several authorities and ignore junk.
func TestParseCABundle(t *testing.T) {
	first, err := crypto.CreateCA("CA One", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateCA: %v", err)
	}
	second, err := crypto.CreateCA("CA Two", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateCA: %v", err)
	}
	dir := t.TempDir()
	p1 := filepath.Join(dir, "ca1.pem")
	p2 := filepath.Join(dir, "ca2.pem")
	if err := first.Save(p1, filepath.Join(dir, "ca1.key")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := second.Save(p2, filepath.Join(dir, "ca2.key")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	d1, _ := os.ReadFile(p1)
	d2, _ := os.ReadFile(p2)
	bundle := append(append(d1, []byte("some noise\n")...), d2...)

	certs := parseCABundle(bundle)
	if len(certs) != 2 {
		t.Fatalf("got %d certificates, want 2", len(certs))
	}
	names := map[string]bool{}
	for _, c := range certs {
		names[c.Subject.CommonName] = true
	}
	if !names["CA One"] || !names["CA Two"] {
		t.Errorf("parsed %v, want both authorities", names)
	}
	if got := parseCABundle([]byte("nothing")); len(got) != 0 {
		t.Errorf("parseCABundle(junk) = %d certs, want none", len(got))
	}
	var _ *x509.Certificate = certs[0]
}

// checkinReq builds a minimal check-in request for a runner.
func checkinReq(runnerID string) proto.CheckinRequest {
	return proto.CheckinRequest{RunnerID: runnerID, Hostname: runnerID, OS: "linux", Arch: "amd64"}
}
