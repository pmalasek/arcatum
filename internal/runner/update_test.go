package runner

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"arcatum/pkg/crypto"
	"arcatum/pkg/version"
)

// The canonical form must match the server's byte for byte, or no update would ever
// verify. It must also be order-independent yet sensitive to membership.
func TestUpdateManifestBytesToSign(t *testing.T) {
	a := map[string]string{"linux-amd64": "aa", "linux-arm64": "bb"}
	b := map[string]string{"linux-arm64": "bb", "linux-amd64": "aa"}
	if string(updateManifestBytesToSign("1.0", a)) != string(updateManifestBytesToSign("1.0", b)) {
		t.Error("the signed form must not depend on map order")
	}
	if string(updateManifestBytesToSign("1.0", a)) == string(updateManifestBytesToSign("1.1", a)) {
		t.Error("a different version must produce a different signed form")
	}
	dropped := map[string]string{"linux-amd64": "aa"}
	if string(updateManifestBytesToSign("1.0", a)) == string(updateManifestBytesToSign("1.0", dropped)) {
		t.Error("removing a build must change the signed form")
	}
	swapped := map[string]string{"linux-amd64": "bb", "linux-arm64": "aa"}
	if string(updateManifestBytesToSign("1.0", a)) == string(updateManifestBytesToSign("1.0", swapped)) {
		t.Error("swapping hashes between platforms must change the signed form")
	}
	// Length prefixes stop one field's content bleeding into the next.
	x := map[string]string{"linux-amd6": "4aa"}
	if string(updateManifestBytesToSign("1.0", dropped)) == string(updateManifestBytesToSign("1.0", x)) {
		t.Error("field boundaries must be unambiguous")
	}
}

// Replacing the running binary must be atomic and must keep the outgoing one for
// diagnosis, so a host is never left without a working executable.
func TestReplaceExecutable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "arcatum-runner")
	if err := os.WriteFile(path, []byte("OLD"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := replaceExecutable(path, []byte("NEW")); err != nil {
		t.Fatalf("replaceExecutable: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "NEW" {
		t.Fatalf("binary = %q, %v; want NEW", data, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("mode = %v, want the new binary executable", info.Mode().Perm())
	}
	// The previous build is kept: without it a failed start cannot be diagnosed.
	old, err := os.ReadFile(path + ".old")
	if err != nil || string(old) != "OLD" {
		t.Errorf("previous binary = %q, %v; want OLD kept", old, err)
	}
	// No temporary files left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if len(e.Name()) > 0 && e.Name()[0] == '.' {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

// One attempt per version: a build that does not take effect must not put the host into a
// restart loop.
func TestUpdateAttemptIsRecordedOnce(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{workBase: dir}

	if a.alreadyAttempted("2.0") {
		t.Error("nothing attempted yet")
	}
	if err := a.recordAttempt("2.0"); err != nil {
		t.Fatalf("recordAttempt: %v", err)
	}
	if !a.alreadyAttempted("2.0") {
		t.Error("the attempt was not remembered")
	}
	// A different version is still fair game.
	if a.alreadyAttempted("2.1") {
		t.Error("a different version must not count as attempted")
	}
	if err := a.recordAttempt("2.1"); err != nil {
		t.Fatalf("recordAttempt: %v", err)
	}
	if a.alreadyAttempted("2.0") {
		t.Error("only the most recent attempt is tracked")
	}
}

// An unstamped development build must never replace itself with a published one — a
// developer's working binary is not something the fleet should overwrite.
func TestUpdateSkippedForDevBuild(t *testing.T) {
	if !version.IsDev() {
		t.Skip("test binary is version-stamped")
	}
	// autoUpdate on and a verifier present, so only the dev check can stop it.
	set, err := crypto.NewSigningSet([][]byte{mustPub(t)})
	if err != nil {
		t.Fatalf("NewSigningSet: %v", err)
	}
	a := &Agent{
		log:        log.New(io.Discard, "", 0),
		autoUpdate: true,
		verifier:   set,
		// A client that would fail if it were ever used, proving no request is made.
		client: NewClient("http://127.0.0.1:1", nil),
	}
	if a.UpdateIfAvailable(context.Background()) {
		t.Error("a development build must not update itself")
	}
}

// Opting out must stop updates even for a stamped build.
func TestUpdateSkippedWhenDisabled(t *testing.T) {
	a := &Agent{log: log.New(io.Discard, "", 0), autoUpdate: false}
	if a.UpdateIfAvailable(context.Background()) {
		t.Error("auto_update = false must stop updates")
	}
}

// Without a verifier there is nothing to check a manifest against, so no update happens.
func TestUpdateSkippedWithoutVerifier(t *testing.T) {
	a := &Agent{log: log.New(io.Discard, "", 0), autoUpdate: true}
	if a.UpdateIfAvailable(context.Background()) {
		t.Error("no verifier must mean no update")
	}
}

func mustPub(t *testing.T) []byte {
	t.Helper()
	_, pub, err := crypto.GenerateSigningKey()
	if err != nil {
		t.Fatalf("GenerateSigningKey: %v", err)
	}
	return pub
}
