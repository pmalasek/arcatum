package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Emptying the server of everything it has collected, while leaving everything an
// operator set up.
//
// This is the other half of the configuration archive (config_archive.go): that one
// carries the configuration and leaves the data alone, this one deletes the data and
// leaves the configuration alone. Together they cover the two things an operator
// actually wants — "move my setup somewhere else" and "start again with the same setup".
//
// What goes: the run history, the logs, the stored dumps and the restic repositories.
// What stays: the keys (they are files on disk and are never touched from the web), the
// accounts, the instances, and the runners — a host that is enrolled and approved keeps
// working, so the next scheduled backup simply starts a fresh history.
//
// Config archives written before an import are kept as well. They are the way back from
// a mistake, and a reset is not the moment to throw the way back away.

// ResetConfirm is the value the confirm parameter must carry. Nothing about this
// operation should be reachable by accident.
const ResetConfirm = "delete-all-backups"

// ResetResult reports what a reset removed.
type ResetResult struct {
	Runs int `json:"runs"`
	// LogBytes and DataBytes are what the database knew those runs had produced. They are
	// what was accounted, not a fresh measurement of the disk: an honest total from the
	// records rather than a walk over every repository.
	LogBytes  int64 `json:"log_bytes"`
	DataBytes int64 `json:"data_bytes"`
	// Repositories is how many restic repositories were removed.
	Repositories int `json:"repositories"`
	// Kept summarises what deliberately survived, so the answer says as much about what
	// is still there as about what is gone.
	Kept ConfigCounts `json:"kept"`
}

// ErrRunsInFlight refuses a reset while a job is still running. Deleting the directory a
// runner is streaming into would leave the run writing to a file nobody will ever read,
// and the operator with a half-truth about what was removed.
var ErrRunsInFlight = errors.New("a job is still running")

// ResetStats returns what a reset would remove, without removing anything.
func (s *Store) ResetStats() (*ResetResult, error) {
	res := &ResetResult{}
	var logBytes, dataBytes *int64
	if err := s.db.QueryRow(`SELECT COUNT(*), SUM(bytes), SUM(data_bytes) FROM runs`).
		Scan(&res.Runs, &logBytes, &dataBytes); err != nil {
		return nil, err
	}
	if logBytes != nil {
		res.LogBytes = *logBytes
	}
	if dataBytes != nil {
		res.DataBytes = *dataBytes
	}
	entries, err := os.ReadDir(filepath.Join(s.backupDir, "restic"))
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				res.Repositories++
			}
		}
	}
	var counts ConfigCounts
	if err := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM instances), (SELECT COUNT(*) FROM users),
		(SELECT COUNT(*) FROM runners)`).
		Scan(&counts.Instances, &counts.Users, &counts.Runners); err != nil {
		return nil, err
	}
	res.Kept = counts
	return res, nil
}

// ResetBackupData deletes every run and everything on disk that belongs to one.
//
// The order is deliberate: the database goes first, so a failure while removing files
// leaves orphaned directories — which cost disk space — rather than run records pointing
// at files that are already gone, which would make the UI offer downloads that fail.
//
// The run id counter is reset with the rows. The ids are directory names under
// backup_dir, and since every one of those directories is being removed here, starting
// again at run-1 collides with nothing and gives a genuinely fresh history.
func (s *Store) ResetBackupData() (*ResetResult, error) {
	unfinished, err := s.UnfinishedRuns()
	if err != nil {
		return nil, err
	}
	if len(unfinished) > 0 {
		ids := make([]string, 0, len(unfinished))
		for _, r := range unfinished {
			ids = append(ids, r.ID)
		}
		return nil, fmt.Errorf("%w: %s — cancel it or wait for it to finish first",
			ErrRunsInFlight, strings.Join(ids, ", "))
	}

	res, err := s.ResetStats()
	if err != nil {
		return nil, err
	}

	// Any log handles still open belong to runs that have gone quiet; closing them here
	// means nothing is holding a file in a directory about to be removed.
	s.logMu.Lock()
	for key, lf := range s.logFiles {
		lf.f.Close()
		delete(s.logFiles, key)
	}
	clear(s.logPending)
	s.logMu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM runs`); err != nil {
		return nil, err
	}
	// sqlite_sequence only has a row once an AUTOINCREMENT table has been written to, so
	// a delete that matches nothing is the normal case on a server with no runs yet.
	if _, err := tx.Exec(`DELETE FROM sqlite_sequence WHERE name = 'runs'`); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	// runs/ holds logs and dumps, restic/ the repositories, restic-cache/ what restic
	// keeps to avoid re-reading them. config-backups/ is not in the list on purpose.
	for _, dir := range []string{"runs", "restic", "restic-cache"} {
		if err := os.RemoveAll(filepath.Join(s.backupDir, dir)); err != nil {
			return res, fmt.Errorf("remove %s: %w", dir, err)
		}
	}
	return res, nil
}

// --- HTTP ---------------------------------------------------------------------

// handleResetPreview reports what a reset would delete.
func (s *Server) handleResetPreview(w http.ResponseWriter, r *http.Request) {
	res, err := s.store.ResetStats()
	if err != nil {
		s.log.Printf("reset preview: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, res)
}

// handleReset deletes every backup and every log. It needs confirm=delete-all-backups:
// this is the one action in Arcatum that destroys backups, and it must not be something
// a mistyped URL can do.
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("confirm") != ResetConfirm {
		writeError(w, http.StatusBadRequest,
			"this deletes every backup and every log; repeat with confirm="+ResetConfirm)
		return
	}
	started := time.Now()
	res, err := s.store.ResetBackupData()
	if err != nil {
		if errors.Is(err, ErrRunsInFlight) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		s.log.Printf("reset by %s failed: %v", actor(r), err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Printf("reset by %s in %s: %d run(s), %d repositor(ies) deleted; kept %d instance(s), "+
		"%d account(s), %d runner(s)", actor(r), time.Since(started).Round(time.Millisecond),
		res.Runs, res.Repositories, res.Kept.Instances, res.Kept.Users, res.Kept.Runners)
	writeJSON(w, res)
}
