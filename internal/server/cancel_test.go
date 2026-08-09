package server

import (
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"arcatum/pkg/crypto"
)

// cancelRun posts an operator's request to stop a run.
func cancelRun(srv *Server, runID string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+runID+"/cancel", nil))
	return rec
}

// cancelState asks the way a runner does, mid-job.
func cancelState(t *testing.T, srv *Server, runID string, cert *x509.Certificate) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID+"/cancel", nil)
	if cert != nil {
		r.TLS = tlsStateWith(cert)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	return rec
}

// A running job stops only once the runner collects the request, so the immediate
// answer is "cancelling" — and the flag has to be visible to the runner that asks.
func TestCancelRunningRunIsCollectedByTheRunner(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	run := startedRun(t, srv)

	// Nothing asked for yet.
	rec := cancelState(t, srv, run.ID, nil)
	if got := decodeCancel(t, rec); got.Cancel {
		t.Fatal("cancel reported before anybody asked for it")
	}

	if rec := cancelRun(srv, run.ID); rec.Code != http.StatusOK {
		t.Fatalf("cancel = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	// Still running: the server cannot stop anything by itself.
	got, err := srv.store.Run(run.ID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != StatusRunning || !got.CancelRequested {
		t.Errorf("run = %s/cancel_requested=%v, want running/true", got.Status, got.CancelRequested)
	}
	if res := decodeCancel(t, cancelState(t, srv, run.ID, nil)); !res.Cancel {
		t.Error("the runner is not being told to stop")
	}

	// The runner kills the process and reports what that looks like: a killed command.
	if err := srv.store.FinishRun(run.ID, time.Now(), -1, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	got, _ = srv.store.Run(run.ID)
	if got.Status != StatusCancelled {
		t.Errorf("status = %s, want cancelled — a stopped run is not a fault to investigate", got.Status)
	}
}

// A run that has not started has nothing to wait for; the operator sees the outcome now.
func TestCancelPendingRunFinishesImmediately(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	run, err := srv.store.CreateRun(&Instance{ID: "files-web01", Script: "files-backup", RunnerID: "web-01"}, "", 3600)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if rec := cancelRun(srv, run.ID); rec.Code != http.StatusOK {
		t.Fatalf("cancel = %d, want 200", rec.Code)
	}
	got, _ := srv.store.Run(run.ID)
	if got.Status != StatusCancelled {
		t.Errorf("status = %s, want cancelled", got.Status)
	}
	if got.EndedAt.IsZero() {
		t.Error("ended_at not stamped")
	}
}

// A job that finished cleanly in the moment between the request and the runner noticing
// produced a real backup. Relabelling that as cancelled would throw it away for nothing.
func TestCancelDoesNotRelabelASuccessfulRun(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	run := startedRun(t, srv)
	if rec := cancelRun(srv, run.ID); rec.Code != http.StatusOK {
		t.Fatalf("cancel = %d, want 200", rec.Code)
	}
	if err := srv.store.FinishRun(run.ID, time.Now(), 0, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	got, _ := srv.store.Run(run.ID)
	if got.Status != StatusSuccess {
		t.Errorf("status = %s, want success — the backup did complete", got.Status)
	}
}

func TestCancelFinishedRunIsRejected(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	run := startedRun(t, srv)
	if err := srv.store.FinishRun(run.ID, time.Now(), 0, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if rec := cancelRun(srv, run.ID); rec.Code != http.StatusConflict {
		t.Errorf("cancel of a finished run = %d, want 409", rec.Code)
	}
}

func TestCancelUnknownRun(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	if rec := cancelRun(srv, "run-999"); rec.Code != http.StatusNotFound {
		t.Errorf("cancel of an unknown run = %d, want 404", rec.Code)
	}
}

// One host must not learn about — or act on — another host's runs.
func TestCancelStateRejectsForeignRunner(t *testing.T) {
	srv, _ := resticTestServer(t, true)
	run := startedRun(t, srv) // dispatched to web-01

	rec := cancelState(t, srv, run.ID, certWithCNAndRole("db-02", crypto.RoleRunner))
	if rec.Code != http.StatusForbidden {
		t.Errorf("foreign cancel check = %d, want 403", rec.Code)
	}
	rec = cancelState(t, srv, run.ID, certWithCNAndRole("web-01", crypto.RoleRunner))
	if rec.Code != http.StatusOK {
		t.Errorf("owning runner's cancel check = %d, want 200", rec.Code)
	}
}

func decodeCancel(t *testing.T, rec *httptest.ResponseRecorder) cancelResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel state = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var res cancelResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return res
}
