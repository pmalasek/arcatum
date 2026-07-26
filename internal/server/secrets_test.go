package server

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"arcatum/pkg/crypto"
)

const testSecret = "sup3r-s3cret-passw0rd"

// openEncryptedStore returns a store whose secrets are encrypted, plus the paths of
// the database and master key.
func openEncryptedStore(t *testing.T) (st *Store, dir, dbPath, keyPath string) {
	t.Helper()
	dir = t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	keyPath = filepath.Join(dir, "secrets-master.key")

	key, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	box, err := crypto.LoadSecretBox(keyPath)
	if err != nil {
		t.Fatalf("LoadSecretBox: %v", err)
	}
	st, err = Open(dbPath, filepath.Join(dir, "backup"), box)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, dir, dbPath, keyPath
}

func importOneSecret(t *testing.T, st *Store, dir string) {
	t.Helper()
	path := writeInstances(t, dir, []*Instance{{
		ID:       "mysql-web01",
		Script:   "mysql-backup",
		RunnerID: "web-01",
		Params:   map[string]string{"host": "127.0.0.1"},
		Secrets:  map[string]string{"password": testSecret},
		Schedule: ScheduleJSON{Frequency: "daily", Time: "02:30"},
	}})
	if _, err := st.ImportInstances(path); err != nil {
		t.Fatalf("ImportInstances: %v", err)
	}
}

// The whole point of encryption at rest: a copy of arcatum.db must not reveal
// credentials, while the running server still reads them back.
func TestSecretIsNotInDatabaseFileButReadsBack(t *testing.T) {
	st, dir, dbPath, _ := openEncryptedStore(t)
	importOneSecret(t, st, dir)

	got, err := st.Instance("mysql-web01")
	if err != nil || got == nil {
		t.Fatalf("Instance: %v", err)
	}
	if got.Secrets["password"] != testSecret {
		t.Errorf("secret round-trip failed: got %q", got.Secrets["password"])
	}

	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Scan every file SQLite may have written (db, -wal, -shm).
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	checked := 0
	for _, e := range entries {
		// instances.json legitimately holds the plaintext: it is the import source.
		if e.IsDir() || e.Name() == "instances.json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		checked++
		if bytes.Contains(data, []byte(testSecret)) {
			t.Errorf("plaintext secret found in %s", e.Name())
		}
	}
	if checked == 0 {
		t.Fatal("no files scanned — the test would pass vacuously")
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database file missing: %v", err)
	}
}

func TestSecretSurvivesReopenWithSameKey(t *testing.T) {
	st, dir, dbPath, keyPath := openEncryptedStore(t)
	importOneSecret(t, st, dir)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	box, err := crypto.LoadSecretBox(keyPath)
	if err != nil {
		t.Fatalf("LoadSecretBox: %v", err)
	}
	reopened, err := Open(dbPath, filepath.Join(dir, "backup"), box)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	got, err := reopened.Instance("mysql-web01")
	if err != nil || got == nil {
		t.Fatalf("Instance after reopen: %v", err)
	}
	if got.Secrets["password"] != testSecret {
		t.Errorf("secret = %q, want %q", got.Secrets["password"], testSecret)
	}
}

// Losing or swapping the master key must fail loudly rather than yield garbage.
func TestSecretUnreadableWithWrongKey(t *testing.T) {
	st, dir, dbPath, _ := openEncryptedStore(t)
	importOneSecret(t, st, dir)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	otherKey, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	otherPath := filepath.Join(dir, "other.key")
	if err := os.WriteFile(otherPath, otherKey, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	box, err := crypto.LoadSecretBox(otherPath)
	if err != nil {
		t.Fatalf("LoadSecretBox: %v", err)
	}
	reopened, err := Open(dbPath, filepath.Join(dir, "backup"), box)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	if _, err := reopened.Instance("mysql-web01"); err == nil {
		t.Error("reading a secret with the wrong master key must fail")
	}
}

func TestSecretUnreadableWithoutKey(t *testing.T) {
	st, dir, dbPath, _ := openEncryptedStore(t)
	importOneSecret(t, st, dir)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	plain, err := Open(dbPath, filepath.Join(dir, "backup"), nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer plain.Close()

	if _, err := plain.Instance("mysql-web01"); err == nil {
		t.Error("an encrypted secret must not be readable without the master key")
	}
}

// A database written without encryption must keep working after a key is configured,
// so enabling encryption on an existing install does not break it.
func TestLegacyPlaintextSecretStillReadable(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	backupDir := filepath.Join(dir, "backup")

	plain, err := Open(dbPath, backupDir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	importOneSecret(t, plain, dir)
	if err := plain.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	key, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	keyPath := filepath.Join(dir, "secrets-master.key")
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	box, err := crypto.LoadSecretBox(keyPath)
	if err != nil {
		t.Fatalf("LoadSecretBox: %v", err)
	}
	encrypted, err := Open(dbPath, backupDir, box)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer encrypted.Close()

	got, err := encrypted.Instance("mysql-web01")
	if err != nil || got == nil {
		t.Fatalf("legacy plaintext secret became unreadable: %v", err)
	}
	if got.Secrets["password"] != testSecret {
		t.Errorf("secret = %q, want %q", got.Secrets["password"], testSecret)
	}

	// Re-importing now encrypts it.
	importOneSecret(t, encrypted, dir)
	if err := encrypted.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read db: %v", err)
	}
	if bytes.Contains(data, []byte(testSecret)) {
		t.Error("after re-import with a key configured, the plaintext should be gone from the database")
	}
}

// Redaction must still work on decrypted values.
func TestEncryptedInstanceStillRedacts(t *testing.T) {
	st, dir, _, _ := openEncryptedStore(t)
	importOneSecret(t, st, dir)

	got, err := st.Instance("mysql-web01")
	if err != nil || got == nil {
		t.Fatalf("Instance: %v", err)
	}
	if red := got.Redacted(); red.Secrets["password"] != "***" {
		t.Errorf("Redacted secret = %q, want ***", red.Secrets["password"])
	}
}
