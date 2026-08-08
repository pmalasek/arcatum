package server

import (
	"context"
	"time"
)

// A run only ever leaves "running" because the runner says so. When the runner stops
// existing mid-run — killed, restarted by systemd, host rebooted — nobody ever says so,
// and the run sits there as if it were still working. That is how a backup ends up
// apparently running since the morning: not a slow dump, but a row nothing will finish.
//
// The timeout is what makes this decidable. The runner enforces it with
// exec.CommandContext, so a live run cannot outlive it; a run that has is therefore one
// whose runner is gone. CreateRun stores the dispatched value for exactly this.
//
// The reaper only writes down what already happened. It kills nothing (there is nothing
// left to kill) and it never touches a run that is merely slow.

// reapEvery is how often stuck runs are looked for. Cheap enough to run often, and a run
// that has already overshot its timeout should not have to wait an hour to be admitted
// as lost.
const reapEvery = time.Minute

// reapGrace is added to a run's timeout before it is declared lost. It covers the
// runner's own shutdown: after the timeout fires it still has to kill the process, drain
// the pipes and report — none of which is instant for a large backup.
const reapGrace = 5 * time.Minute

// defaultRunTimeout applies to runs recorded before timeout_sec existed, and matches the
// default in timeoutSeconds. Without it every one of those old rows would be reaped on
// the first pass.
const defaultRunTimeout = time.Hour

// StartRunReaper marks runs whose runner never reported back, until ctx is cancelled.
func (s *Server) StartRunReaper(ctx context.Context) {
	go func() {
		t := time.NewTicker(reapEvery)
		defer t.Stop()
		// Once at startup: a run interrupted by the *server* restarting is the commonest
		// orphan there is, and waiting a full tick to notice helps nobody.
		s.reapStuckRuns(time.Now())
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				s.reapStuckRuns(now)
			}
		}
	}()
}

// reapStuckRuns finishes every run that has outlived its timeout without reporting.
func (s *Server) reapStuckRuns(now time.Time) {
	runs, err := s.store.UnfinishedRuns()
	if err != nil {
		s.log.Printf("run reaper: %v", err)
		return
	}
	for _, run := range runs {
		deadline, ok := runDeadline(run)
		if !ok || now.Before(deadline) {
			continue
		}
		// A run cancelled by an operator whose runner then went silent is still a
		// cancellation; FinishRun relabels it from the flag, so the message here only has
		// to explain the silence.
		msg := "runner did not report completion within the timeout"
		if run.Status == StatusPending {
			msg = "runner never started this run"
		}
		if err := s.store.FinishRun(run.ID, now, -1, msg); err != nil {
			s.log.Printf("run reaper: run=%s: %v", run.ID, err)
			continue
		}
		s.log.Printf("run=%s: %s (dispatched to %q) — marked as ended", run.ID, msg, run.RunnerID)
	}
}

// runDeadline is the moment after which an unfinished run must be considered lost. The
// clock starts when the run started, or — for one never started — when it was dispatched.
func runDeadline(run *Run) (time.Time, bool) {
	timeout := time.Duration(run.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = defaultRunTimeout
	}
	from := run.StartedAt
	if from.IsZero() {
		from = run.CreatedAt
	}
	if from.IsZero() {
		// Neither timestamp: nothing to measure from, so leave it alone rather than
		// guess. A row like this predates the scheduler recording anything useful.
		return time.Time{}, false
	}
	return from.Add(timeout + reapGrace), true
}
