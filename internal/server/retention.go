package server

import (
	"context"
	"time"
)

// Run logs pile up: every run leaves a stdout and a stderr file under
// backup_dir/runs/<run_id>/, and nothing used to remove them. The size cap in
// AppendOutput bounds one run; this bounds their number over time.
//
// Only logs are swept. The backup payload beside them (data.bin) is the backup itself,
// and deleting one on a schedule is not a default anybody should inherit without having
// asked for it — that policy belongs to the repository, not to a log janitor.
//
// A failed run's log outlives a successful one, because it is the one somebody reads.

// logSweepEvery is how often the sweep runs. Retention is measured in days, so there is
// nothing to gain from looking more often.
const logSweepEvery = time.Hour

// logSweepBatch caps one pass. A first run against years of history then spreads its
// work over several passes instead of blocking on tens of thousands of files.
const logSweepBatch = 500

// RetentionOptions says how long finished runs' logs are kept. A zero duration keeps
// that class forever.
type RetentionOptions struct {
	LogSuccess time.Duration
	LogFailed  time.Duration
}

// enabled reports whether anything would ever be pruned.
func (r RetentionOptions) enabled() bool { return r.LogSuccess > 0 || r.LogFailed > 0 }

// StartLogRetention sweeps expired run logs in the background until ctx is cancelled.
// It returns immediately, and does nothing at all when retention is off.
func (s *Server) StartLogRetention(ctx context.Context) {
	if !s.retention.enabled() {
		s.log.Printf("  run logs are kept indefinitely ([storage] log_retention_* is 0)")
		return
	}
	go func() {
		t := time.NewTicker(logSweepEvery)
		defer t.Stop()
		s.sweepLogs(time.Now())
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				s.sweepLogs(now)
			}
		}
	}()
}

// sweepLogs deletes the log files of runs that have outlived their retention.
func (s *Server) sweepLogs(now time.Time) {
	var successBefore, failedBefore time.Time
	if s.retention.LogSuccess > 0 {
		successBefore = now.Add(-s.retention.LogSuccess)
	}
	if s.retention.LogFailed > 0 {
		failedBefore = now.Add(-s.retention.LogFailed)
	}
	runIDs, err := s.store.PrunableLogRuns(successBefore, failedBefore, logSweepBatch)
	if err != nil {
		s.log.Printf("log retention: %v", err)
		return
	}
	pruned := 0
	for _, runID := range runIDs {
		if err := s.store.PruneRunLogs(runID); err != nil {
			s.log.Printf("log retention: run=%s: %v", runID, err)
			continue
		}
		pruned++
	}
	if pruned > 0 {
		s.log.Printf("log retention: removed the logs of %d run(s)", pruned)
	}
}
