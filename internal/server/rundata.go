package server

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// The backup a script writes to stdout (capture = "stream") does not travel through the
// run-update stream. That stream is ndjson: every chunk would be base64-encoded, decoded
// again, appended to a file the UI offers to tail, and counted with its own SQLite
// transaction — for a database dump that is tens of thousands of transactions and a log
// nobody can read. It arrives here instead, as one raw request copied straight to disk.
//
// Upload is a runner endpoint on the mTLS listener; download is an operator endpoint,
// registered with the rest of the read-only API.

// payloadBuffer is the copy buffer for an upload. Large on purpose: the point of this
// path is to move gigabytes with as little per-chunk work as possible.
const payloadBuffer = 1 << 20

// payloadProgressEvery is how often an in-flight upload refreshes the run's byte
// counter, which is what the web UI shows while a backup is still running.
const payloadProgressEvery = 2 * time.Second

// handleUploadRunData receives a run's backup payload. The body is the script's stdout,
// so it has no declared length and is read until the runner closes it.
func (s *Server) handleUploadRunData(w http.ResponseWriter, r *http.Request) {
	runnerID, err := s.activeRunnerIdentity(r, "")
	if err != nil && s.requireClientCert {
		s.log.Printf("data upload denied: %v", err)
		s.denyRunner(w, err)
		return
	}
	runID := r.PathValue("id")
	run, err := s.store.Run(runID)
	if err != nil || run == nil {
		http.Error(w, "unknown run", http.StatusNotFound)
		return
	}
	// A runner may only upload for a run dispatched to it, so one host cannot overwrite
	// another's backup.
	if s.requireClientCert && run.RunnerID != runnerID {
		s.log.Printf("data upload denied: runner %q does not own run %s", runnerID, runID)
		http.Error(w, "not permitted", http.StatusForbidden)
		return
	}
	// After the run is finished nothing will ever promote a new payload (FinishRun has
	// already decided), so accepting one would only leave a partial file behind.
	if isTerminal(run.Status) {
		http.Error(w, "run has already finished", http.StatusConflict)
		return
	}

	f, err := s.store.CreateData(runID)
	if err != nil {
		s.log.Printf("run=%s data upload: create: %v", runID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	total, copyErr := s.copyPayload(runID, f, r.Body)
	if copyErr == nil {
		// This is a backup, not a log: pay for one fsync so a server that loses power
		// right after the run is marked successful still has the data it claims to have.
		copyErr = f.Sync()
	}
	if err := f.Close(); err != nil && copyErr == nil {
		copyErr = err
	}
	if copyErr != nil {
		// The upload cannot be resumed, so the partial file is dead weight. Remove it now
		// rather than wait for the run to be marked failed.
		s.log.Printf("run=%s data upload failed after %d bytes: %v", runID, total, copyErr)
		if err := s.store.discardData(runID); err != nil {
			s.log.Printf("run=%s discard partial data: %v", runID, err)
		}
		if err := s.store.SetRunDataBytes(runID, 0); err != nil {
			s.log.Printf("run=%s data bytes: %v", runID, err)
		}
		http.Error(w, "storing the payload failed", http.StatusInternalServerError)
		return
	}
	if err := s.store.SetRunDataBytes(runID, total); err != nil {
		s.log.Printf("run=%s data bytes: %v", runID, err)
	}
	s.log.Printf("run=%s received %d bytes of backup data", runID, total)
	writeJSON(w, map[string]int64{"bytes": total})
}

// copyPayload streams the body to disk, refreshing the run's byte counter at most once
// every payloadProgressEvery. Counting per chunk instead would put one SQLite
// transaction behind every buffer — the reason the old path crawled.
func (s *Server) copyPayload(runID string, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, payloadBuffer)
	var total int64
	last := time.Now()
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, err := dst.Write(buf[:n]); err != nil {
				return total, err
			}
			total += int64(n)
			if time.Since(last) >= payloadProgressEvery {
				last = time.Now()
				if err := s.store.SetRunDataBytes(runID, total); err != nil {
					s.log.Printf("run=%s data bytes: %v", runID, err)
				}
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

// handleDownloadRunData serves a completed run's backup payload. Only a successful run
// has one: FinishRun discards anything a failed run managed to upload.
func (s *Server) handleDownloadRunData(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	run, err := s.store.Run(runID)
	if err != nil || run == nil {
		http.Error(w, "unknown run", http.StatusNotFound)
		return
	}
	f, err := os.Open(s.store.DataPath(runID))
	if os.IsNotExist(err) {
		http.Error(w, "this run has no backup data", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Printf("run=%s data download: %v", runID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", run.InstanceID+"-"+runID+".dump"))
	// ServeContent rather than io.Copy: a multi-gigabyte download that drops halfway can
	// then be resumed with a range request instead of started again.
	http.ServeContent(w, r, "", info.ModTime(), f)
}
