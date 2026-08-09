package server

import (
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"arcatum/pkg/crypto"
)

// uploadData posts a payload for a run the way a runner does.
func uploadData(srv *Server, runID, body string, cert *x509.Certificate) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+runID+"/data", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/octet-stream")
	if cert != nil {
		r.TLS = tlsStateWith(cert)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	return rec
}

// startedRun creates a run for the fixture instance and marks it running.
func startedRun(t *testing.T, srv *Server) *Run {
	t.Helper()
	run, err := srv.store.CreateRun(&Instance{ID: "files-web01", Script: "files-backup", RunnerID: "web-01"}, "", 3600)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := srv.store.MarkRunStarted(run.ID, time.Now()); err != nil {
		t.Fatalf("MarkRunStarted: %v", err)
	}
	return run
}

// The whole point of the split: a streamed dump is stored as the backup payload and
// never turns up in the log the web UI tails.
func TestUploadedPayloadIsNotPartOfTheLog(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	run := startedRun(t, srv)
	dump := strings.Repeat("INSERT INTO t VALUES (1);\n", 500)

	if rec := uploadData(srv, run.ID, dump, nil); rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	// Not in the log, and the tail the browser polls sees nothing.
	if data, _, err := srv.store.ReadOutputFrom(run.ID, "stdout", 0, 1<<20); err != nil || len(data) != 0 {
		t.Errorf("stdout log = %q (err %v), want empty", data, err)
	}
	if res := tailFor(t, srv, run.ID, 0, "stdout"); res.Data != "" {
		t.Errorf("tail returned %q, want nothing — the payload is not a log", res.Data)
	}

	// Counted as data, not as log bytes.
	got, err := srv.store.Run(run.ID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.DataBytes != int64(len(dump)) {
		t.Errorf("data_bytes = %d, want %d", got.DataBytes, len(dump))
	}
	if got.Bytes != 0 {
		t.Errorf("bytes = %d, want 0 — nothing was logged", got.Bytes)
	}

	// Still provisional: only a successful finish promotes it.
	if _, err := os.Stat(srv.store.DataPath(run.ID)); !os.IsNotExist(err) {
		t.Errorf("payload was published before the run finished (err %v)", err)
	}
	if err := srv.store.FinishRun(run.ID, time.Now(), 0, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	stored, err := os.ReadFile(srv.store.DataPath(run.ID))
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if string(stored) != dump {
		t.Errorf("stored payload differs from what was uploaded (%d vs %d bytes)", len(stored), len(dump))
	}
}

// A dump from a run that failed is not a backup. It must not survive as one.
func TestFailedRunDiscardsItsPayload(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	run := startedRun(t, srv)
	if rec := uploadData(srv, run.ID, "half a dump", nil); rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, want 200", rec.Code)
	}
	if err := srv.store.FinishRun(run.ID, time.Now(), 1, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if _, err := os.Stat(srv.store.DataPath(run.ID)); !os.IsNotExist(err) {
		t.Errorf("failed run published a payload (err %v)", err)
	}
	if _, err := os.Stat(srv.store.dataPartPath(run.ID)); !os.IsNotExist(err) {
		t.Errorf("partial payload left behind (err %v)", err)
	}
}

// One host must not be able to write into another host's backup.
func TestUploadPayloadRejectsForeignRunner(t *testing.T) {
	srv, _ := resticTestServer(t, true)
	run := startedRun(t, srv) // dispatched to web-01

	rec := uploadData(srv, run.ID, "not mine", certWithCNAndRole("db-02", crypto.RoleRunner))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign upload = %d, want 403", rec.Code)
	}
	if _, err := os.Stat(srv.store.dataPartPath(run.ID)); !os.IsNotExist(err) {
		t.Errorf("a refused upload still wrote something (err %v)", err)
	}
}

// Once a run is finished nothing will promote a new payload, so accepting one would
// only leave a partial file nobody ever collects.
func TestUploadPayloadRejectedAfterTheRunFinished(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	run := startedRun(t, srv)
	if err := srv.store.FinishRun(run.ID, time.Now(), 0, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if rec := uploadData(srv, run.ID, "too late", nil); rec.Code != http.StatusConflict {
		t.Errorf("late upload = %d, want 409", rec.Code)
	}
}

func TestUploadPayloadUnknownRun(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	if rec := uploadData(srv, "run-999", "data", nil); rec.Code != http.StatusNotFound {
		t.Errorf("upload for unknown run = %d, want 404", rec.Code)
	}
}

func TestDownloadRunData(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	run := startedRun(t, srv)
	dump := "-- MySQL dump\nCREATE TABLE t (id INT);\n"
	if rec := uploadData(srv, run.ID, dump, nil); rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, want 200", rec.Code)
	}

	// Before the run finishes there is nothing to download: the payload is unconfirmed.
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID+"/data", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("download before finish = %d, want 404", rec.Code)
	}

	if err := srv.store.FinishRun(run.ID, time.Now(), 0, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID+"/data", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("download = %d, want 200", rec.Code)
	}
	if rec.Body.String() != dump {
		t.Errorf("downloaded %q, want %q", rec.Body.String(), dump)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, run.ID) {
		t.Errorf("Content-Disposition = %q, want it to name the run", cd)
	}
}
