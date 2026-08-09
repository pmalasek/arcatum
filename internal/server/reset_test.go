package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// decodeJSON reads a handler's answer into v.
func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
	}
}

// resetCall drives the reset endpoints through the router.
func resetCall(t *testing.T, srv *Server, method, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(method, "/api/v1/reset"+query, nil))
	return rec
}

// seedBackupData gives a server a finished run with a log and a dump on disk, plus a
// restic repository — the three shapes a reset has to clear.
func seedBackupData(t *testing.T, srv *Server) {
	t.Helper()
	inst, err := srv.store.Instance("mysql-web01")
	if err != nil || inst == nil {
		t.Fatalf("Instance: %v", err)
	}
	run, err := srv.store.CreateRun(inst, "", 60)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := srv.store.AppendOutput(run.ID, "stdout", []byte("dumping…\n")); err != nil {
		t.Fatalf("AppendOutput: %v", err)
	}
	f, err := srv.store.CreateData(run.ID)
	if err != nil {
		t.Fatalf("CreateData: %v", err)
	}
	payload := "payload"
	f.WriteString(payload)
	f.Close()
	// The upload handler is what accounts a payload (rundata.go); writing the file here
	// without the counter would leave the run looking like it produced nothing.
	if err := srv.store.SetRunDataBytes(run.ID, int64(len(payload))); err != nil {
		t.Fatalf("SetRunDataBytes: %v", err)
	}
	if err := srv.store.FinishRun(run.ID, time.Now(), 0, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	repo := filepath.Join(srv.store.backupDir, "restic", "mysql-web01", "data")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "pack"), []byte("snapshot"), 0o600); err != nil {
		t.Fatalf("write pack: %v", err)
	}
}

// The whole point: everything collected goes, everything configured stays.
func TestResetClearsDataAndKeepsConfiguration(t *testing.T) {
	srv := configServer(t, masterKeyFile(t, t.TempDir(), "master.key"))
	seedConfig(t, srv)
	seedBackupData(t, srv)

	rec := resetCall(t, srv, http.MethodPost, "?confirm="+ResetConfirm)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	runs, err := srv.store.ListRuns(0)
	if err != nil || len(runs) != 0 {
		t.Errorf("ListRuns = %d runs, %v; want none", len(runs), err)
	}
	for _, dir := range []string{"runs", "restic", "restic-cache"} {
		if _, err := os.Stat(filepath.Join(srv.store.backupDir, dir)); !os.IsNotExist(err) {
			t.Errorf("%s survived the reset (%v)", dir, err)
		}
	}

	// Configuration is untouched, including the runner: a host that is enrolled keeps
	// backing up, it just starts a fresh history.
	if inst, err := srv.store.Instance("mysql-web01"); err != nil || inst == nil {
		t.Errorf("the instance did not survive the reset: %v, %v", inst, err)
	}
	if _, err := srv.store.Authenticate("petr", "adminpassword"); err != nil {
		t.Errorf("the account did not survive the reset: %v", err)
	}
	runners, err := srv.store.Runners()
	if err != nil || len(runners) != 1 || runners[0].Status != EnrollApproved {
		t.Errorf("Runners = %v, %v; want one approved runner", runners, err)
	}

	// The ids are directory names and every directory was just removed, so the next run
	// starts from the top rather than beside gaps.
	inst, _ := srv.store.Instance("mysql-web01")
	next, err := srv.store.CreateRun(inst, "", 60)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if next.ID != "run-1" {
		t.Errorf("first run after a reset is %q, want run-1", next.ID)
	}
}

// Deleting the directory a runner is streaming into would leave the job writing into
// nothing, so a reset waits for the operator to deal with it.
func TestResetRefusesWhileAJobIsRunning(t *testing.T) {
	srv := configServer(t, masterKeyFile(t, t.TempDir(), "master.key"))
	seedConfig(t, srv)
	inst, err := srv.store.Instance("mysql-web01")
	if err != nil || inst == nil {
		t.Fatalf("Instance: %v", err)
	}
	run, err := srv.store.CreateRun(inst, "", 60)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	rec := resetCall(t, srv, http.MethodPost, "?confirm="+ResetConfirm)
	if rec.Code != http.StatusConflict {
		t.Fatalf("reset = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), run.ID) {
		t.Errorf("the refusal does not name the run in flight: %s", rec.Body.String())
	}
	if runs, err := srv.store.ListRuns(0); err != nil || len(runs) != 1 {
		t.Errorf("the refused reset still deleted runs: %d, %v", len(runs), err)
	}
}

// The one action in Arcatum that destroys backups must not be reachable by a POST that
// merely arrives at the right path.
func TestResetNeedsConfirmation(t *testing.T) {
	srv := configServer(t, masterKeyFile(t, t.TempDir(), "master.key"))
	seedConfig(t, srv)
	seedBackupData(t, srv)

	for _, query := range []string{"", "?confirm=yes"} {
		rec := resetCall(t, srv, http.MethodPost, query)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("reset%q = %d, want 400 (%s)", query, rec.Code, rec.Body.String())
		}
	}
	if runs, err := srv.store.ListRuns(0); err != nil || len(runs) != 1 {
		t.Errorf("an unconfirmed reset deleted runs: %d, %v", len(runs), err)
	}
}

// The preview is what the web shows before the button is pressed; it must count what is
// there without removing any of it.
func TestResetPreviewCountsWithoutDeleting(t *testing.T) {
	srv := configServer(t, masterKeyFile(t, t.TempDir(), "master.key"))
	seedConfig(t, srv)
	seedBackupData(t, srv)

	rec := resetCall(t, srv, http.MethodGet, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d (%s)", rec.Code, rec.Body.String())
	}
	var res ResetResult
	decodeJSON(t, rec, &res)
	if res.Runs != 1 {
		t.Errorf("Runs = %d, want 1", res.Runs)
	}
	if res.Repositories != 1 {
		t.Errorf("Repositories = %d, want 1", res.Repositories)
	}
	if res.DataBytes == 0 {
		t.Errorf("DataBytes = 0, want the dump to be counted")
	}
	if res.Kept.Instances != 1 || res.Kept.Users != 2 || res.Kept.Runners != 1 {
		t.Errorf("Kept = %+v, want 1 instance, 2 users, 1 runner", res.Kept)
	}
	if runs, err := srv.store.ListRuns(0); err != nil || len(runs) != 1 {
		t.Errorf("the preview deleted something: %d, %v", len(runs), err)
	}
}

// Resetting a server that has never run anything is a no-op, not an error: it is the
// state a fresh install is in, and the button should not fail there.
func TestResetOnAnEmptyServer(t *testing.T) {
	srv := configServer(t, masterKeyFile(t, t.TempDir(), "master.key"))
	seedConfig(t, srv)

	rec := resetCall(t, srv, http.MethodPost, "?confirm="+ResetConfirm)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var res ResetResult
	decodeJSON(t, rec, &res)
	if res.Runs != 0 || res.Repositories != 0 {
		t.Errorf("reset of an empty server reported %+v", res)
	}
}
