package server

import (
	"context"
	"net/http"
	"time"
)

// A backup dump is not a filesystem. It is one opaque artifact that gets restored whole:
// nobody browses inside it and nobody restores a single row out of it. So it is rotated
// rather than deduplicated into a repository — keep the last N, and anything younger
// than D days — which is how database backups have always been operated, and which keeps
// the thing you reach for in an emergency a plain file rather than a repository, a
// password and a restic binary.
//
// Retention is the instance's decision (keep_last / keep_days). Zero for both keeps
// everything: deleting a backup must never be a default somebody inherits without having
// asked for it. The two combine as a union — "at least this many, and at least this
// old" — so a count that is too small for a quiet week cannot silently shorten the
// window, and vice versa.

// dumpSweepEvery is how often dumps are rotated. Retention is measured in copies and
// days, so at worst one extra dump sits on disk for an hour.
const dumpSweepEvery = time.Hour

// StartDumpRetention rotates instances' backup dumps until ctx is cancelled.
func (s *Server) StartDumpRetention(ctx context.Context) {
	go func() {
		t := time.NewTicker(dumpSweepEvery)
		defer t.Stop()
		s.sweepDumps(time.Now())
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				s.sweepDumps(now)
			}
		}
	}()
}

// sweepDumps applies every instance's retention to the dumps it has produced.
func (s *Server) sweepDumps(now time.Time) {
	instances, err := s.store.Instances()
	if err != nil {
		s.log.Printf("dump retention: %v", err)
		return
	}
	for _, in := range instances {
		if in.KeepLast <= 0 && in.KeepDays <= 0 {
			continue // keeps everything, by the operator's choice
		}
		runIDs, err := s.store.PrunableDumps(in.ID, in.KeepLast, in.KeepDays, now)
		if err != nil {
			s.log.Printf("dump retention: instance=%s: %v", in.ID, err)
			continue
		}
		var freed int64
		pruned := 0
		for _, runID := range runIDs {
			bytes, err := s.store.DeleteRunPayload(runID)
			if err != nil {
				s.log.Printf("dump retention: run=%s: %v", runID, err)
				continue
			}
			freed += bytes
			pruned++
		}
		if pruned > 0 {
			s.log.Printf("dump retention: instance=%s removed %d dump(s), %d bytes (keeping last %d / %d days)",
				in.ID, pruned, freed, in.KeepLast, in.KeepDays)
		}
	}
}

// pruneDumpsForRun applies the owning instance's retention as soon as a run has stored a
// new dump. Leaving it to the hourly sweep would keep a whole extra copy on disk for an
// hour — at the exact moment the disk just grew by one backup.
func (s *Server) pruneDumpsForRun(runID string) {
	run, err := s.store.Run(runID)
	if err != nil || run == nil || run.DataBytes == 0 || run.Status != StatusSuccess {
		return
	}
	in, err := s.store.Instance(run.InstanceID)
	if err != nil || in == nil || (in.KeepLast <= 0 && in.KeepDays <= 0) {
		return
	}
	runIDs, err := s.store.PrunableDumps(in.ID, in.KeepLast, in.KeepDays, time.Now())
	if err != nil {
		s.log.Printf("dump retention: instance=%s: %v", in.ID, err)
		return
	}
	for _, id := range runIDs {
		bytes, err := s.store.DeleteRunPayload(id)
		if err != nil {
			s.log.Printf("dump retention: run=%s: %v", id, err)
			continue
		}
		s.log.Printf("dump retention: instance=%s removed %s (%d bytes)", in.ID, id, bytes)
	}
}

// DumpInfo is one stored backup dump, as the restore view lists them.
type DumpInfo struct {
	RunID   string    `json:"run_id"`
	EndedAt time.Time `json:"ended_at"`
	Bytes   int64     `json:"bytes"`
}

// handleListDumps returns an instance's stored dumps, newest first. This is what the
// restore view browses for a streaming instance, where there is no repository to walk:
// each dump is a single file, and restoring means downloading it.
func (s *Server) handleListDumps(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := validateInstanceID(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	dumps, err := s.store.InstanceDumps(id, 0)
	if err != nil {
		s.log.Printf("dumps %s: %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, dumps)
}
