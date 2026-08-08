package server

import (
	"net/http"
	"time"
)

// Stopping a run is not something the server can simply do. Communication is pull-only —
// the runner dials out, the server never dials in — so there is no connection to
// interrupt and no process the server can see. A cancellation is therefore a flag the
// operator sets and the runner collects while it works:
//
//	operator → POST /api/v1/runs/{id}/cancel      sets cancel_requested
//	runner   → GET  /api/v1/runs/{id}/cancel      polled during execution
//
// When the runner sees it, it cancels the run's context; exec.CommandContext kills the
// process, and the run ends like any other failure — except that FinishRun reports it as
// "cancelled", because a killed process is indistinguishable from a crash from here and
// "failed" would send somebody hunting for a fault that was a deliberate act.
//
// A run whose runner has died never collects the flag. That case belongs to the reaper
// (reaper.go), not here.

// handleCancelRun records an operator's request to stop a run.
func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	run, err := s.store.Run(runID)
	if err != nil || run == nil {
		http.Error(w, "unknown run", http.StatusNotFound)
		return
	}
	if isTerminal(run.Status) {
		writeError(w, http.StatusConflict, "run has already finished")
		return
	}
	if err := s.store.RequestRunCancel(runID); err != nil {
		s.log.Printf("cancel run=%s: %v", runID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.log.Printf("run=%s: cancellation requested", runID)

	// A pending run has not started: the runner reports "started" as its very first
	// update, so nothing is executing yet and there is nothing to wait for. Finishing it
	// here means the operator sees the result immediately instead of waiting out a
	// timeout. Should the runner turn out to have been starting after all, it collects
	// the flag on its first poll and stops, and reports the same verdict again.
	if run.Status == StatusPending {
		if err := s.store.FinishRun(runID, time.Now(), -1, "cancelled before it started"); err != nil {
			s.log.Printf("cancel run=%s: finish: %v", runID, err)
		}
		writeJSON(w, map[string]string{"status": string(StatusCancelled), "run": runID})
		return
	}
	writeJSON(w, map[string]string{"status": "cancelling", "run": runID})
}

// cancelResponse tells a runner whether to stop the run it is executing.
type cancelResponse struct {
	Cancel bool `json:"cancel"`
}

// handleRunCancelState is polled by the runner while a job runs. It is deliberately
// cheap: one indexed lookup, no body, called every few seconds per active run.
func (s *Server) handleRunCancelState(w http.ResponseWriter, r *http.Request) {
	runnerID, err := s.activeRunnerIdentity(r, "")
	if err != nil && s.requireClientCert {
		s.denyRunner(w, err)
		return
	}
	runID := r.PathValue("id")
	run, err := s.store.Run(runID)
	if err != nil || run == nil {
		http.Error(w, "unknown run", http.StatusNotFound)
		return
	}
	if s.requireClientCert && run.RunnerID != runnerID {
		http.Error(w, "not permitted", http.StatusForbidden)
		return
	}
	writeJSON(w, cancelResponse{Cancel: run.CancelRequested})
}
