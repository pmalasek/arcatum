package server

import (
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

// A script stuck in a loop must not be able to fill the disk with output nobody will
// read, and a log that was cut has to say so rather than just stop.
func TestAppendOutputCapsLogSize(t *testing.T) {
	st, _ := openTestStore(t)
	chunk := make([]byte, 512*1024)
	for i := range chunk {
		chunk[i] = 'x'
	}

	var received int
	for i := 0; i < 12; i++ { // 6 MiB against a 4 MiB cap
		n, err := st.AppendOutput("run-1", "stdout", chunk)
		if err != nil {
			t.Fatalf("AppendOutput: %v", err)
		}
		received += n
	}
	if received != 12*len(chunk) {
		t.Errorf("reported %d bytes received, want %d — the counter reports what the run "+
			"produced, not what survived the cap", received, 12*len(chunk))
	}

	info, err := os.Stat(st.StreamPath("run-1", "stdout"))
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	want := int64(maxRunLogBytes + len(logTruncatedMarker))
	if info.Size() != want {
		t.Errorf("log size = %d, want %d (cap plus the marker)", info.Size(), want)
	}
	data, err := os.ReadFile(st.StreamPath("run-1", "stdout"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.HasSuffix(string(data), logTruncatedMarker) {
		t.Error("truncated log does not end with the marker")
	}
}

// Reopening a run's log after its handle was closed must not restart the cap, or a
// long-running job would slip past it one idle period at a time.
func TestLogCapSurvivesReopening(t *testing.T) {
	st, _ := openTestStore(t)
	chunk := make([]byte, maxRunLogBytes)
	if _, err := st.AppendOutput("run-1", "stdout", chunk); err != nil {
		t.Fatalf("AppendOutput: %v", err)
	}
	st.closeRunLogs("run-1")

	if _, err := st.AppendOutput("run-1", "stdout", []byte("more")); err != nil {
		t.Fatalf("AppendOutput after close: %v", err)
	}
	data, err := os.ReadFile(st.StreamPath("run-1", "stdout"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if strings.HasSuffix(string(data), "more") {
		t.Error("writing continued past the cap after the handle was reopened")
	}
}

// retentionServer wires a server with the given retention policy over a fresh store.
func retentionServer(t *testing.T, opts RetentionOptions) *Server {
	t.Helper()
	st, _ := openTestStore(t)
	return &Server{store: st, log: log.New(io.Discard, "", 0), retention: opts}
}

// finishedRun creates a run that ended at the given time with the given status.
func finishedRun(t *testing.T, st *Store, endedAt time.Time, exitCode int, execErr string) *Run {
	t.Helper()
	run, err := st.CreateRun(&Instance{ID: "i", Script: "hello", RunnerID: "h"}, "", 3600)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := st.AppendOutput(run.ID, "stdout", []byte("output\n")); err != nil {
		t.Fatalf("AppendOutput: %v", err)
	}
	if err := st.FinishRun(run.ID, endedAt, exitCode, execErr); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	return run
}

// A failed run's log is the one somebody reads, so it outlives a successful one.
func TestSweepLogsKeepsFailuresLonger(t *testing.T) {
	srv := retentionServer(t, RetentionOptions{LogSuccess: 14 * 24 * time.Hour, LogFailed: 90 * 24 * time.Hour})
	now := time.Now()

	oldSuccess := finishedRun(t, srv.store, now.Add(-30*24*time.Hour), 0, "")
	oldFailure := finishedRun(t, srv.store, now.Add(-30*24*time.Hour), 1, "")
	recentSuccess := finishedRun(t, srv.store, now.Add(-time.Hour), 0, "")

	srv.sweepLogs(now)

	if _, err := os.Stat(srv.store.StreamPath(oldSuccess.ID, "stdout")); !os.IsNotExist(err) {
		t.Errorf("log of a 30-day-old successful run was kept (err %v)", err)
	}
	if _, err := os.Stat(srv.store.StreamPath(oldFailure.ID, "stdout")); err != nil {
		t.Errorf("log of a 30-day-old failure was removed, but failures are kept for 90 days: %v", err)
	}
	if _, err := os.Stat(srv.store.StreamPath(recentSuccess.ID, "stdout")); err != nil {
		t.Errorf("log of an hour-old run was removed: %v", err)
	}
	// The run itself stays; only its log goes.
	if got, err := srv.store.Run(oldSuccess.ID); err != nil || got == nil {
		t.Errorf("pruning removed the run row (%v)", err)
	}
}

// Sweeping must never touch the backup itself.
func TestSweepLogsKeepsThePayload(t *testing.T) {
	srv := retentionServer(t, RetentionOptions{LogSuccess: time.Hour})
	now := time.Now()
	run := startedRunFor(t, srv.store)
	f, err := srv.store.CreateData(run.ID)
	if err != nil {
		t.Fatalf("CreateData: %v", err)
	}
	f.WriteString("the backup")
	f.Close()
	if err := srv.store.FinishRun(run.ID, now.Add(-48*time.Hour), 0, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	srv.sweepLogs(now)

	if _, err := os.Stat(srv.store.StreamPath(run.ID, "stdout")); !os.IsNotExist(err) {
		t.Errorf("expired log survived the sweep (err %v)", err)
	}
	data, err := os.ReadFile(srv.store.DataPath(run.ID))
	if err != nil {
		t.Fatalf("the sweep removed the backup payload: %v", err)
	}
	if string(data) != "the backup" {
		t.Errorf("payload = %q, want it untouched", data)
	}
}

// startedRunFor creates a running run directly on a store.
func startedRunFor(t *testing.T, st *Store) *Run {
	t.Helper()
	run, err := st.CreateRun(&Instance{ID: "i", Script: "hello", RunnerID: "h"}, "", 3600)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := st.AppendOutput(run.ID, "stdout", []byte("output\n")); err != nil {
		t.Fatalf("AppendOutput: %v", err)
	}
	return run
}

// A swept run must not come back on the next pass, or the sweep would re-stat the whole
// history once an hour forever.
func TestSweepLogsMarksRunsPruned(t *testing.T) {
	srv := retentionServer(t, RetentionOptions{LogSuccess: time.Hour})
	now := time.Now()
	finishedRun(t, srv.store, now.Add(-48*time.Hour), 0, "")

	srv.sweepLogs(now)
	left, err := srv.store.PrunableLogRuns(now.Add(-time.Hour), time.Time{}, 100)
	if err != nil {
		t.Fatalf("PrunableLogRuns: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("%d run(s) still queued for pruning after a sweep, want 0", len(left))
	}
}

// Retention off must mean nothing is ever deleted.
func TestSweepLogsDisabledKeepsEverything(t *testing.T) {
	srv := retentionServer(t, RetentionOptions{})
	now := time.Now()
	run := finishedRun(t, srv.store, now.Add(-10*365*24*time.Hour), 0, "")

	srv.sweepLogs(now)

	if _, err := os.Stat(srv.store.StreamPath(run.ID, "stdout")); err != nil {
		t.Errorf("retention is off but a log was removed: %v", err)
	}
}
