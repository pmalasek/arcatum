package server

import (
	"io"
	"log"
	"os"
	"testing"
	"time"
)

func reaperServer(t *testing.T) *Server {
	t.Helper()
	st, _ := openTestStore(t)
	return &Server{store: st, log: log.New(io.Discard, "", 0)}
}

// runStartedAt creates a run that started at the given time with the given timeout.
func runStartedAt(t *testing.T, st *Store, startedAt time.Time, timeoutSec int) *Run {
	t.Helper()
	run, err := st.CreateRun(&Instance{ID: "i", Script: "hello", RunnerID: "h"}, timeoutSec)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.MarkRunStarted(run.ID, startedAt); err != nil {
		t.Fatalf("MarkRunStarted: %v", err)
	}
	return run
}

// The case this exists for: a runner dies mid-backup and the run sits there as if it
// were still working. Nothing but the timeout can tell the server otherwise.
func TestReaperFinishesRunsThatOutlivedTheirTimeout(t *testing.T) {
	srv := reaperServer(t)
	now := time.Now()
	lost := runStartedAt(t, srv.store, now.Add(-8*time.Hour), 3600) // 1 h timeout, started 8 h ago

	srv.reapStuckRuns(now)

	got, err := srv.store.Run(lost.ID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != StatusError {
		t.Errorf("status = %s, want error", got.Status)
	}
	if got.Err == "" {
		t.Error("no explanation recorded — the operator has to be able to tell this from a script failure")
	}
	if got.EndedAt.IsZero() {
		t.Error("ended_at not stamped, so the run still has no duration")
	}
}

// A backup that is merely slow must be left alone — reaping it would report a failure
// for a job that is still working.
func TestReaperLeavesRunsInsideTheirTimeout(t *testing.T) {
	srv := reaperServer(t)
	now := time.Now()
	running := runStartedAt(t, srv.store, now.Add(-30*time.Minute), 3600)

	srv.reapStuckRuns(now)

	got, _ := srv.store.Run(running.ID)
	if got.Status != StatusRunning {
		t.Errorf("status = %s, want running — 30 minutes into a one-hour timeout", got.Status)
	}
}

// The grace period covers the runner's own shutdown: killing the process, draining the
// pipes and reporting are not instant for a large backup.
func TestReaperWaitsOutTheGracePeriod(t *testing.T) {
	srv := reaperServer(t)
	now := time.Now()
	// Just past the timeout, but inside the grace.
	run := runStartedAt(t, srv.store, now.Add(-time.Hour-time.Minute), 3600)

	srv.reapStuckRuns(now)

	got, _ := srv.store.Run(run.ID)
	if got.Status != StatusRunning {
		t.Errorf("status = %s, want running — still within the grace period", got.Status)
	}

	srv.reapStuckRuns(now.Add(reapGrace))
	got, _ = srv.store.Run(run.ID)
	if got.Status != StatusError {
		t.Errorf("status = %s after the grace expired, want error", got.Status)
	}
}

// A run dispatched to a runner that never picked it up has no start time; the clock has
// to run from when it was dispatched or it would never be reaped at all.
func TestReaperFinishesRunsThatNeverStarted(t *testing.T) {
	srv := reaperServer(t)
	run, err := srv.store.CreateRun(&Instance{ID: "i", Script: "hello", RunnerID: "h"}, 60)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	srv.reapStuckRuns(time.Now().Add(2 * time.Hour))

	got, _ := srv.store.Run(run.ID)
	if got.Status != StatusError {
		t.Errorf("status = %s, want error", got.Status)
	}
}

// Rows written before timeout_sec existed carry 0. Treating that as "no timeout" would
// reap every one of them on the first pass.
func TestReaperUsesTheDefaultTimeoutForOldRuns(t *testing.T) {
	srv := reaperServer(t)
	now := time.Now()
	old := runStartedAt(t, srv.store, now.Add(-30*time.Minute), 0)

	srv.reapStuckRuns(now)
	if got, _ := srv.store.Run(old.ID); got.Status != StatusRunning {
		t.Errorf("status = %s, want running — 30 minutes is inside the default hour", got.Status)
	}

	srv.reapStuckRuns(now.Add(2 * time.Hour))
	if got, _ := srv.store.Run(old.ID); got.Status != StatusError {
		t.Errorf("status = %s, want error once the default timeout passed", got.Status)
	}
}

// A run an operator stopped, whose runner then went silent, is still a cancellation.
func TestReaperKeepsCancelledRunsCancelled(t *testing.T) {
	srv := reaperServer(t)
	now := time.Now()
	run := runStartedAt(t, srv.store, now.Add(-8*time.Hour), 3600)
	if err := srv.store.RequestRunCancel(run.ID); err != nil {
		t.Fatalf("RequestRunCancel: %v", err)
	}

	srv.reapStuckRuns(now)

	got, _ := srv.store.Run(run.ID)
	if got.Status != StatusCancelled {
		t.Errorf("status = %s, want cancelled", got.Status)
	}
}

// The reaper decides a run is over, so the payload it left behind must go the same way
// any other unfinished backup's does.
func TestReaperDiscardsAnUnfinishedPayload(t *testing.T) {
	srv := reaperServer(t)
	now := time.Now()
	run := runStartedAt(t, srv.store, now.Add(-8*time.Hour), 3600)
	f, err := srv.store.CreateData(run.ID)
	if err != nil {
		t.Fatalf("CreateData: %v", err)
	}
	f.WriteString("half a dump")
	f.Close()

	srv.reapStuckRuns(now)

	if _, err := srv.store.Run(run.ID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(srv.store.dataPartPath(run.ID)); err == nil {
		t.Error("a partial payload survived the reaper")
	}
	if _, err := os.Stat(srv.store.DataPath(run.ID)); err == nil {
		t.Error("the reaper published a partial payload as a backup")
	}
}
