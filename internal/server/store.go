package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"arcatum/pkg/crypto"
	"arcatum/pkg/proto"

	_ "modernc.org/sqlite" // pure-Go driver: no CGO, keeps the binary static
)

// Store is the server's persistent state: instances, runs and runner registrations
// in SQLite. Run *output* is not in the DB — it is streamed to files under
// backupDir/runs/<run_id>/, since backup payloads belong in storage, not a table.
//
// Instance secrets are encrypted with box before they are written, so the database
// file alone does not reveal credentials. A nil box stores them in plaintext
// (development only).
type Store struct {
	db        *sql.DB
	backupDir string
	// box holds the secrets master key and its predecessors, so a rotation can read
	// values written under an older key. Nil means secrets are stored in plaintext.
	box *crypto.Keyring

	// Run logs are appended to constantly while a job runs, so the handles stay open and
	// the byte counters are buffered rather than written per chunk. See AppendOutput.
	logMu      sync.Mutex
	logFiles   map[string]*logFile // "<runID>/<stream>" -> open append handle
	logPending map[string]int64    // runID -> received bytes not yet in the database
	logFlushed time.Time           // when the counters last reached the database
}

// Open opens (creating if needed) the SQLite database at dbPath and applies the
// schema. box may be nil, in which case secrets are stored unencrypted.
func Open(dbPath, backupDir string, box *crypto.Keyring) (*Store, error) {
	if dir := filepath.Dir(dbPath); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("db dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// Writes are low-volume and serialized; one connection avoids SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{
		db:         db,
		backupDir:  backupDir,
		box:        box,
		logFiles:   map[string]*logFile{},
		logPending: map[string]int64{},
		logFlushed: time.Now(),
	}, nil
}

// migrate adds any columns introduced after the initial schema. Existing databases are
// upgraded in place; a fresh one simply already has everything.
func migrate(db *sql.DB) error {
	for _, c := range addColumns {
		has, err := hasColumn(db, c.table, c.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.column, c.definition)
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("%s.%s: %w", c.table, c.column, err)
		}
	}
	return nil
}

func hasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name       string
			ctype      string
			notNull    int
			defaultVal any
			pk         int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &defaultVal, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Close releases the database.
func (s *Store) Close() error {
	// Buffered byte counters would otherwise be lost on shutdown, and open log handles
	// would go with the process.
	s.logMu.Lock()
	for key, lf := range s.logFiles {
		lf.f.Close()
		delete(s.logFiles, key)
	}
	s.flushLogBytesLocked()
	s.logMu.Unlock()
	return s.db.Close()
}

// --- instances ---------------------------------------------------------------

// ImportInstances seeds instances from a JSON array file and returns how many were
// created. A missing or empty file is not an error.
//
// Existing instances are left alone unless overwrite is set. Instances are editable
// through the API now, and re-importing on every start would silently undo those edits —
// the seed file describes the initial state, not the authoritative one.
func (s *Store) ImportInstances(path string, overwrite bool) (int, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	// An empty file is a plausible operator state (instances managed elsewhere, or the
	// seed emptied deliberately) and must not stop the server from starting.
	if len(strings.TrimSpace(string(data))) == 0 {
		return 0, nil
	}
	var list []*Instance
	if err := json.Unmarshal(data, &list); err != nil {
		return 0, fmt.Errorf("parse instances %s: %w", path, err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	imported := 0
	for _, in := range list {
		if in.ID == "" {
			return 0, fmt.Errorf("instances %s: an instance is missing id", path)
		}
		if !overwrite {
			var one int
			err := tx.QueryRow(`SELECT 1 FROM instances WHERE id = ?`, in.ID).Scan(&one)
			if err == nil {
				continue // already managed; do not clobber it
			} else if err != sql.ErrNoRows {
				return 0, err
			}
		}
		params, err := json.Marshal(in.Params)
		if err != nil {
			return 0, err
		}
		sealed, err := s.sealSecrets(in.ID, in.Secrets)
		if err != nil {
			return 0, err
		}
		secrets, err := json.Marshal(sealed)
		if err != nil {
			return 0, err
		}
		sched, err := json.Marshal(in.Schedule)
		if err != nil {
			return 0, err
		}
		_, err = tx.Exec(`
			INSERT INTO instances (id, script, runner_id, params, secrets, capture, timeout, schedule,
			  keep_last, keep_days)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
			  script=excluded.script, runner_id=excluded.runner_id, params=excluded.params,
			  secrets=excluded.secrets, capture=excluded.capture, timeout=excluded.timeout,
			  schedule=excluded.schedule, keep_last=excluded.keep_last, keep_days=excluded.keep_days`,
			in.ID, in.Script, in.RunnerID, string(params), string(secrets), in.Capture, in.Timeout, string(sched),
			in.KeepLast, in.KeepDays)
		if err != nil {
			return 0, fmt.Errorf("upsert instance %q: %w", in.ID, err)
		}
		imported++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return imported, nil
}

const instanceCols = `id, script, runner_id, params, secrets, capture, timeout, schedule, keep_last, keep_days`

// sealValue encrypts one secret value with the primary master key. Without a keyring the
// value is stored as-is (development mode).
func (s *Store) sealValue(value, instanceID, name string) (string, error) {
	if s.box == nil {
		return value, nil
	}
	return s.box.SealString(value, instanceID, name)
}

// openValue decrypts one stored secret value.
func (s *Store) openValue(stored, instanceID, name string) (string, error) {
	if s.box == nil {
		if crypto.IsAnySealed(stored) {
			return "", fmt.Errorf("%s/%s: %w", instanceID, name, crypto.ErrSealed)
		}
		return stored, nil
	}
	return s.box.OpenString(stored, instanceID, name)
}

// RekeyResult reports what a re-encryption pass did.
type RekeyResult struct {
	Instances int    `json:"instances"`      // instances examined
	Secrets   int    `json:"secrets"`        // values re-encrypted
	Skipped   int    `json:"skipped"`        // already on the current key
	KeyID     string `json:"key_id"`         // the key everything is now sealed with
	Remaining int    `json:"remaining"`      // values still not on the current key
	Note      string `json:"note,omitempty"` // e.g. why nothing happened
}

// RekeySecrets re-encrypts every stored secret with the primary master key, which is what
// completes a master-key rotation.
//
// It is safe to run repeatedly: values already on the current key are skipped, so an
// interrupted pass simply resumes. Each instance is committed on its own, so a failure
// part-way leaves earlier instances done rather than rolling everything back — the mixed
// state is readable either way, because the keyring still holds the old key.
func (s *Store) RekeySecrets() (*RekeyResult, error) {
	res := &RekeyResult{}
	if s.box == nil {
		res.Note = "no master key configured, secrets are stored in plaintext"
		return res, nil
	}
	res.KeyID = s.box.PrimaryID()

	rows, err := s.db.Query(`SELECT id, secrets FROM instances ORDER BY id`)
	if err != nil {
		return nil, err
	}
	type pending struct {
		id      string
		secrets map[string]string
	}
	var todo []pending
	for rows.Next() {
		var id, secretsJSON string
		if err := rows.Scan(&id, &secretsJSON); err != nil {
			rows.Close()
			return nil, err
		}
		var stored map[string]string
		if err := json.Unmarshal([]byte(secretsJSON), &stored); err != nil {
			rows.Close()
			return nil, fmt.Errorf("instance %q secrets: %w", id, err)
		}
		todo = append(todo, pending{id: id, secrets: stored})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, p := range todo {
		res.Instances++
		if len(p.secrets) == 0 {
			continue
		}
		changed := false
		next := make(map[string]string, len(p.secrets))
		for name, value := range p.secrets {
			if s.box.IsOnPrimary(value) {
				res.Skipped++
				next[name] = value
				continue
			}
			plain, err := s.box.OpenString(value, p.id, name)
			if err != nil {
				// Report rather than abort: one unreadable value should not block
				// rotating everything else.
				res.Remaining++
				next[name] = value
				continue
			}
			sealed, err := s.box.SealString(plain, p.id, name)
			if err != nil {
				return nil, fmt.Errorf("instance %q: seal %q: %w", p.id, name, err)
			}
			next[name] = sealed
			changed = true
			res.Secrets++
		}
		if !changed {
			continue
		}
		encoded, err := json.Marshal(next)
		if err != nil {
			return nil, err
		}
		if _, err := s.db.Exec(`UPDATE instances SET secrets = ? WHERE id = ?`, string(encoded), p.id); err != nil {
			return nil, fmt.Errorf("instance %q: %w", p.id, err)
		}
	}
	return res, nil
}

// sealSecrets encrypts each secret value for storage, binding it to this instance and
// secret name so a ciphertext cannot be relocated within the database.
func (s *Store) sealSecrets(instanceID string, secrets map[string]string) (map[string]string, error) {
	if secrets == nil {
		return nil, nil
	}
	out := make(map[string]string, len(secrets))
	for name, value := range secrets {
		sealed, err := s.sealValue(value, instanceID, name)
		if err != nil {
			return nil, fmt.Errorf("instance %q: seal secret %q: %w", instanceID, name, err)
		}
		out[name] = sealed
	}
	return out, nil
}

// openSecrets decrypts stored secret values.
func (s *Store) openSecrets(instanceID string, stored map[string]string) (map[string]string, error) {
	if stored == nil {
		return nil, nil
	}
	out := make(map[string]string, len(stored))
	for name, value := range stored {
		plain, err := s.openValue(value, instanceID, name)
		if err != nil {
			return nil, err
		}
		out[name] = plain
	}
	return out, nil
}

func (s *Store) scanInstance(sc interface{ Scan(...any) error }) (*Instance, error) {
	var in Instance
	var params, secrets, sched string
	if err := sc.Scan(&in.ID, &in.Script, &in.RunnerID, &params, &secrets, &in.Capture, &in.Timeout, &sched,
		&in.KeepLast, &in.KeepDays); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(params), &in.Params); err != nil {
		return nil, fmt.Errorf("instance %q params: %w", in.ID, err)
	}
	var stored map[string]string
	if err := json.Unmarshal([]byte(secrets), &stored); err != nil {
		return nil, fmt.Errorf("instance %q secrets: %w", in.ID, err)
	}
	opened, err := s.openSecrets(in.ID, stored)
	if err != nil {
		return nil, err
	}
	in.Secrets = opened
	if err := json.Unmarshal([]byte(sched), &in.Schedule); err != nil {
		return nil, fmt.Errorf("instance %q schedule: %w", in.ID, err)
	}
	return &in, nil
}

// Instances returns all instances ordered by id.
func (s *Store) Instances() ([]*Instance, error) {
	rows, err := s.db.Query(`SELECT ` + instanceCols + ` FROM instances ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Instance
	for rows.Next() {
		in, err := s.scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// InstancesForRunner returns instances targeting one runner.
func (s *Store) InstancesForRunner(runnerID string) ([]*Instance, error) {
	rows, err := s.db.Query(`SELECT `+instanceCols+` FROM instances WHERE runner_id = ? ORDER BY id`, runnerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Instance
	for rows.Next() {
		in, err := s.scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// Instance returns one instance by id.
func (s *Store) Instance(id string) (*Instance, error) {
	row := s.db.QueryRow(`SELECT `+instanceCols+` FROM instances WHERE id = ?`, id)
	in, err := s.scanInstance(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return in, err
}

// --- runners ----------------------------------------------------------------

// RecordCheckin registers or refreshes a runner's last-seen state. certNotAfter is the
// expiry of the certificate the runner presented; pass the zero time when there is none
// (development mode). Taking it from the live connection means expiry is tracked for
// hand-issued certificates as well as enrolled ones.
func (s *Store) RecordCheckin(req proto.CheckinRequest, certNotAfter, now time.Time, certIssuer string) error {
	ms := toMillis(now)
	_, err := s.db.Exec(`
		INSERT INTO runners (id, hostname, os, arch, first_seen, last_seen, cert_not_after,
		                     cert_issuer, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  hostname=excluded.hostname, os=excluded.os, arch=excluded.arch,
		  last_seen=excluded.last_seen,
		  -- Keep what is already known if this check-in did not carry a certificate.
		  cert_not_after=CASE WHEN excluded.cert_not_after > 0
		                      THEN excluded.cert_not_after ELSE runners.cert_not_after END,
		  cert_issuer=CASE WHEN excluded.cert_issuer != ''
		                   THEN excluded.cert_issuer ELSE runners.cert_issuer END,
		  version=CASE WHEN excluded.version != '' THEN excluded.version ELSE runners.version END`,
		req.RunnerID, req.Hostname, req.OS, req.Arch, ms, ms, toMillis(certNotAfter), certIssuer,
		req.Version)
	return err
}

// Runner is a registered runner as stored by the server.
type Runner struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	// Status is the enrollment state: pending, approved or rejected.
	Status string `json:"status"`
	// CertFingerprint of the issued certificate, or of the pending request — it is what
	// an operator compares against the host to be sure the request is genuine.
	CertFingerprint string    `json:"cert_fingerprint,omitempty"`
	EnrollIP        string    `json:"enroll_ip,omitempty"`
	FirstSeen       time.Time `json:"first_seen"`
	LastSeen        time.Time `json:"last_seen"`
	EnrolledAt      time.Time `json:"enrolled_at,omitempty"`
	ApprovedAt      time.Time `json:"approved_at,omitempty"`
	// CertNotAfter is when the runner's certificate expires. Zero means unknown, which
	// is normal until the runner has checked in once.
	CertNotAfter time.Time `json:"cert_not_after,omitempty"`
	// CertIssuer is the authority that issued the current certificate, which is how a CA
	// rotation is tracked to completion.
	CertIssuer string `json:"cert_issuer,omitempty"`
	// Version is the runner build last reported, for tracking an update rollout.
	Version string `json:"version,omitempty"`
}

const runnerCols = `id, hostname, os, arch, status, cert_fingerprint, enroll_ip,
                    first_seen, last_seen, enrolled_at, approved_at, cert_not_after, cert_issuer,
                    version`

// Runners lists known runners: those waiting for approval first, then the rest by how
// recently they checked in — so a pending request is never buried at the bottom.
func (s *Store) Runners() ([]*Runner, error) {
	rows, err := s.db.Query(`SELECT ` + runnerCols + ` FROM runners
		ORDER BY (status = 'pending') DESC, last_seen DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Runner
	for rows.Next() {
		var r Runner
		var first, last, enrolled, approved, notAfter int64
		if err := rows.Scan(&r.ID, &r.Hostname, &r.OS, &r.Arch, &r.Status,
			&r.CertFingerprint, &r.EnrollIP, &first, &last, &enrolled, &approved, &notAfter,
			&r.CertIssuer, &r.Version); err != nil {
			return nil, err
		}
		r.FirstSeen, r.LastSeen = fromMillis(first), fromMillis(last)
		r.EnrolledAt, r.ApprovedAt = fromMillis(enrolled), fromMillis(approved)
		r.CertNotAfter = fromMillis(notAfter)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// --- runs -------------------------------------------------------------------

// CreateRun records a new pending run for an instance. timeoutSec is the timeout the
// run is being dispatched with; the server keeps it so it can later tell a run that is
// merely slow from one whose runner is never going to report back (reaper.go).
func (s *Store) CreateRun(inst *Instance, timeoutSec int) (*Run, error) {
	now := time.Now()
	res, err := s.db.Exec(`
		INSERT INTO runs (instance_id, runner_id, script, status, created_at, timeout_sec)
		VALUES (?, ?, ?, ?, ?, ?)`,
		inst.ID, inst.RunnerID, inst.Script, string(StatusPending), toMillis(now), timeoutSec)
	if err != nil {
		return nil, err
	}
	n, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Run{
		ID:         formatRunID(n),
		InstanceID: inst.ID,
		RunnerID:   inst.RunnerID,
		Script:     inst.Script,
		Status:     StatusPending,
		TimeoutSec: timeoutSec,
	}, nil
}

// MarkRunStarted moves a run to running and stamps its start time.
func (s *Store) MarkRunStarted(runID string, at time.Time) error {
	n, err := parseRunID(runID)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE runs SET status = ?, started_at = ? WHERE id = ?`,
		string(StatusRunning), toMillis(at), n)
	return err
}

// SetRunDataBytes records how much backup payload a run has uploaded. Unlike the log
// counter this is set outright rather than accumulated, because the payload arrives as
// one request with one running total.
func (s *Store) SetRunDataBytes(runID string, total int64) error {
	n, err := parseRunID(runID)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE runs SET data_bytes = ? WHERE id = ?`, total, n)
	return err
}

// RequestRunCancel records that an operator wants a run stopped. It does not stop
// anything by itself: in a pull model the server cannot reach into a runner, so this is
// a flag the runner polls for while it works (cancel.go).
func (s *Store) RequestRunCancel(runID string) error {
	n, err := parseRunID(runID)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE runs SET cancel_requested = 1 WHERE id = ?`, n)
	return err
}

// CancelRequested reports whether a run has been asked to stop.
func (s *Store) CancelRequested(runID string) (bool, error) {
	n, err := parseRunID(runID)
	if err != nil {
		return false, err
	}
	return s.cancelRequested(n)
}

func (s *Store) cancelRequested(id int64) (bool, error) {
	var flag int
	err := s.db.QueryRow(`SELECT cancel_requested FROM runs WHERE id = ?`, id).Scan(&flag)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return flag != 0, err
}

// FinishRun records a run's terminal state. A non-empty execErr means the runner
// failed around the execution; otherwise the exit code decides success vs failure.
func (s *Store) FinishRun(runID string, at time.Time, exitCode int, execErr string) error {
	n, err := parseRunID(runID)
	if err != nil {
		return err
	}
	status := StatusSuccess
	switch {
	case execErr != "":
		status = StatusError
	case exitCode != 0:
		status = StatusFailed
	}
	// A run an operator stopped is reported as cancelled rather than failed: the killed
	// process looks exactly like a crash from here, and "failed" would send somebody
	// looking for a fault that was a deliberate act.
	//
	// Only a non-success outcome is relabelled. A job that finished cleanly in the moment
	// between the request and the runner noticing it produced a real backup, and calling
	// that cancelled would throw it away for nothing.
	if status != StatusSuccess {
		if requested, err := s.cancelRequested(n); err == nil && requested {
			status = StatusCancelled
		}
	}
	// No more output is coming, so the handles are released and the buffered byte count
	// is written before the run is reported as finished — the UI reads it right after.
	s.closeRunLogs(runID)
	if _, err := s.db.Exec(`UPDATE runs SET status = ?, exit_code = ?, ended_at = ?, err = ? WHERE id = ?`,
		string(status), exitCode, toMillis(at), execErr, n); err != nil {
		return err
	}
	// The payload is promoted here and nowhere else, so "there is a data.bin" and "the
	// run succeeded" cannot disagree. A dump cut short by a failing script or a dropped
	// connection is not a backup and must never be left looking like one.
	if status == StatusSuccess {
		return s.commitData(runID)
	}
	return s.discardData(runID)
}

const runCols = `id, instance_id, runner_id, script, status, exit_code, bytes, data_bytes, ` +
	`created_at, started_at, ended_at, err, timeout_sec, cancel_requested, data_pruned`

func scanRun(sc interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var id int64
	var created, started, ended int64
	var status string
	var cancelRequested, dataPruned int
	if err := sc.Scan(&id, &r.InstanceID, &r.RunnerID, &r.Script, &status,
		&r.ExitCode, &r.Bytes, &r.DataBytes, &created, &started, &ended, &r.Err,
		&r.TimeoutSec, &cancelRequested, &dataPruned); err != nil {
		return nil, err
	}
	r.CancelRequested, r.DataPruned = cancelRequested != 0, dataPruned != 0
	r.ID = formatRunID(id)
	r.Status = RunStatus(status)
	r.CreatedAt = fromMillis(created)
	r.StartedAt, r.EndedAt = fromMillis(started), fromMillis(ended)
	return &r, nil
}

// UnfinishedRuns returns every run still pending or running, oldest first. The list is
// what the reaper walks, so it is small in normal operation: one entry per job actually
// in flight.
func (s *Store) UnfinishedRuns() ([]*Run, error) {
	rows, err := s.db.Query(`SELECT `+runCols+` FROM runs WHERE status IN (?, ?) ORDER BY id`,
		string(StatusPending), string(StatusRunning))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Run returns one run by id, or nil if it does not exist.
func (s *Store) Run(runID string) (*Run, error) {
	n, err := parseRunID(runID)
	if err != nil {
		return nil, err
	}
	row := s.db.QueryRow(`SELECT `+runCols+` FROM runs WHERE id = ?`, n)
	r, err := scanRun(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// ListRuns returns runs newest-first, at most limit (0 means no limit).
func (s *Store) ListRuns(limit int) ([]*Run, error) {
	q := `SELECT ` + runCols + ` FROM runs ORDER BY id DESC`
	args := []any{}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- run payload (the backup itself) ----------------------------------------

// A script declared with capture = "stream" writes its backup to stdout. That stream is
// not a log and is not treated as one: it is uploaded to the server as a single raw
// request and stored next to the log as data.bin.
//
// The upload lands in data.part first. FinishRun renames it once — and only if — the run
// succeeded, which is what stops a truncated dump from ever presenting itself as a
// finished backup.
const (
	dataFileName = "data.bin"
	dataPartName = "data.part"
)

// DataPath returns where a run's completed payload is stored.
func (s *Store) DataPath(runID string) string {
	return filepath.Join(s.backupDir, "runs", runID, dataFileName)
}

func (s *Store) dataPartPath(runID string) string {
	return filepath.Join(s.backupDir, "runs", runID, dataPartName)
}

// CreateData opens a run's partial payload file for writing, discarding whatever an
// earlier attempt left behind.
func (s *Store) CreateData(runID string) (*os.File, error) {
	dir := filepath.Join(s.backupDir, "runs", runID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return os.OpenFile(s.dataPartPath(runID), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
}

// commitData promotes a fully uploaded payload to its final name.
func (s *Store) commitData(runID string) error {
	err := os.Rename(s.dataPartPath(runID), s.DataPath(runID))
	if os.IsNotExist(err) {
		return nil // nothing was streamed, which is the normal case for a log-only job
	}
	return err
}

// discardData removes a partial payload.
func (s *Store) discardData(runID string) error {
	err := os.Remove(s.dataPartPath(runID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// --- stored dumps (payload retention, see dumps.go) --------------------------

// InstanceDumps returns an instance's stored backup dumps, newest first. Only successful
// runs appear: a payload is promoted on success and discarded otherwise, so anything else
// has no file to offer. limit 0 means all of them.
func (s *Store) InstanceDumps(instanceID string, limit int) ([]DumpInfo, error) {
	q := `SELECT id, ended_at, data_bytes FROM runs
	       WHERE instance_id = ? AND status = ? AND data_bytes > 0 AND data_pruned = 0
	       ORDER BY id DESC`
	args := []any{instanceID, string(StatusSuccess)}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DumpInfo{}
	for rows.Next() {
		var id, ended, bytes int64
		if err := rows.Scan(&id, &ended, &bytes); err != nil {
			return nil, err
		}
		out = append(out, DumpInfo{RunID: formatRunID(id), EndedAt: fromMillis(ended), Bytes: bytes})
	}
	return out, rows.Err()
}

// DumpStats summarises what an instance currently has stored.
type DumpStats struct {
	Count int       `json:"count"`
	Bytes int64     `json:"bytes"`
	Last  time.Time `json:"last"`
}

// InstanceDumpStats reports how many dumps an instance holds and how much they weigh.
func (s *Store) InstanceDumpStats(instanceID string) (DumpStats, error) {
	var st DumpStats
	var bytes, last *int64
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*), SUM(data_bytes), MAX(ended_at) FROM runs
		 WHERE instance_id = ? AND status = ? AND data_bytes > 0 AND data_pruned = 0`,
		instanceID, string(StatusSuccess)).Scan(&count, &bytes, &last)
	if err != nil {
		return st, err
	}
	st.Count = count
	if bytes != nil {
		st.Bytes = *bytes
	}
	if last != nil {
		st.Last = fromMillis(*last)
	}
	return st, nil
}

// PrunableDumps returns the runs whose dumps have fallen outside an instance's retention,
// oldest last. keepLast and keepDays are a union: a dump survives if it is among the most
// recent keepLast, *or* if it is younger than keepDays. Zero disables that half; with
// both zero nothing is ever prunable.
func (s *Store) PrunableDumps(instanceID string, keepLast, keepDays int, now time.Time) ([]string, error) {
	if keepLast <= 0 && keepDays <= 0 {
		return nil, nil
	}
	dumps, err := s.InstanceDumps(instanceID, 0)
	if err != nil {
		return nil, err
	}
	var cutoff time.Time
	if keepDays > 0 {
		cutoff = now.AddDate(0, 0, -keepDays)
	}
	var out []string
	for i, d := range dumps {
		if keepLast > 0 && i < keepLast {
			continue // among the most recent
		}
		if keepDays > 0 && d.EndedAt.After(cutoff) {
			continue // still inside the time window
		}
		out = append(out, d.RunID)
	}
	return out, nil
}

// DeleteRunPayload removes a run's stored dump and returns how large it was. The run row
// and its byte count stay: the history should still say what the run produced, so a
// missing dump reads as "rotated away" rather than "never happened".
func (s *Store) DeleteRunPayload(runID string) (int64, error) {
	n, err := parseRunID(runID)
	if err != nil {
		return 0, err
	}
	var bytes int64
	if err := s.db.QueryRow(`SELECT data_bytes FROM runs WHERE id = ?`, n).Scan(&bytes); err != nil {
		return 0, err
	}
	if err := os.Remove(s.DataPath(runID)); err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	// The partial file cannot exist for a successful run, but a crash mid-upload could
	// have left one behind under a different run; removing it here costs nothing.
	if err := s.discardData(runID); err != nil {
		return 0, err
	}
	if _, err := s.db.Exec(`UPDATE runs SET data_pruned = 1 WHERE id = ?`, n); err != nil {
		return 0, err
	}
	return bytes, nil
}

// --- run output (files, not DB) ---------------------------------------------

// Log writing is on the hot path of every run, so it is deliberately not the obvious
// open-write-close per chunk. That cost three syscalls and one SQLite transaction for
// every 32 KiB of output, which a long restic run produces tens of thousands of.
// Instead the file stays open for as long as the run is producing output, and the byte
// counter is buffered and written a few times a minute.
const (
	// maxRunLogBytes caps one stream of one run. A log is for reading; past a few
	// megabytes nobody does, and a script stuck in a loop should not be able to fill the
	// disk. The backup payload does not come through here (see rundata.go).
	maxRunLogBytes = 4 << 20
	// logTruncatedMarker is appended once when the cap is reached, so a cut log says so
	// instead of just stopping.
	logTruncatedMarker = "\n--- log truncated at 4 MiB; further output was discarded ---\n"
	// logFlushEvery is how often buffered byte counters reach the database, and how
	// often idle log files are closed.
	logFlushEvery = 2 * time.Second
	// logIdleTimeout closes the handle of a run that has gone quiet. A runner that dies
	// mid-run never reports "finished", and its file would otherwise stay open forever.
	logIdleTimeout = 5 * time.Minute
)

// logFile is one open run stream, with what is needed to enforce the size cap.
type logFile struct {
	f         *os.File
	written   int64 // bytes in the file, counted against maxRunLogBytes
	truncated bool
	lastUsed  time.Time
}

// write appends to the log until the cap is reached, then writes the marker once and
// silently drops the rest.
func (lf *logFile) write(data []byte) error {
	if lf.truncated {
		return nil
	}
	room := maxRunLogBytes - lf.written
	if int64(len(data)) <= room {
		n, err := lf.f.Write(data)
		lf.written += int64(n)
		return err
	}
	if room > 0 {
		n, err := lf.f.Write(data[:room])
		lf.written += int64(n)
		if err != nil {
			return err
		}
	}
	lf.truncated = true
	_, err := lf.f.WriteString(logTruncatedMarker)
	return err
}

// AppendOutput appends streamed bytes to backupDir/runs/<runID>/<stream>.log and returns
// how many bytes were received. Bytes dropped by the size cap are still counted: the
// counter reports what the run produced, which is the interesting number when a log has
// been truncated.
func (s *Store) AppendOutput(runID, stream string, data []byte) (int, error) {
	if stream != "stdout" && stream != "stderr" {
		stream = "stdout"
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()

	lf, err := s.openLogLocked(runID, stream)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	lf.lastUsed = now
	if err := lf.write(data); err != nil {
		return 0, err
	}
	s.logPending[runID] += int64(len(data))
	if now.Sub(s.logFlushed) >= logFlushEvery {
		s.flushLogBytesLocked()
		s.closeIdleLogsLocked(now)
	}
	return len(data), nil
}

// openLogLocked returns the open handle for a run stream, opening it on first use.
func (s *Store) openLogLocked(runID, stream string) (*logFile, error) {
	key := runID + "/" + stream
	if lf, ok := s.logFiles[key]; ok {
		return lf, nil
	}
	dir := filepath.Join(s.backupDir, "runs", runID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, stream+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, err
	}
	lf := &logFile{f: f}
	// Size comes from the file, not from what this process has written: the handle may
	// have been closed for being idle, and the cap applies to the log as a whole.
	if info, err := f.Stat(); err == nil {
		lf.written = info.Size()
		lf.truncated = lf.written >= maxRunLogBytes+int64(len(logTruncatedMarker))
	}
	s.logFiles[key] = lf
	return lf, nil
}

// flushLogBytesLocked writes the buffered byte counters to the database. A counter that
// cannot be written stays buffered and is retried on the next flush.
func (s *Store) flushLogBytesLocked() {
	s.logFlushed = time.Now()
	for runID, delta := range s.logPending {
		n, err := parseRunID(runID)
		if err != nil {
			delete(s.logPending, runID) // not a run id; retrying cannot help
			continue
		}
		if delta == 0 {
			delete(s.logPending, runID)
			continue
		}
		if _, err := s.db.Exec(`UPDATE runs SET bytes = bytes + ? WHERE id = ?`, delta, n); err == nil {
			delete(s.logPending, runID)
		}
	}
}

// closeIdleLogsLocked closes handles for runs that have stopped producing output.
func (s *Store) closeIdleLogsLocked(now time.Time) {
	for key, lf := range s.logFiles {
		if now.Sub(lf.lastUsed) >= logIdleTimeout {
			lf.f.Close()
			delete(s.logFiles, key)
		}
	}
}

// closeRunLogs flushes and closes a finished run's log files.
func (s *Store) closeRunLogs(runID string) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	for _, stream := range []string{"stdout", "stderr"} {
		key := runID + "/" + stream
		if lf, ok := s.logFiles[key]; ok {
			lf.f.Close()
			delete(s.logFiles, key)
		}
	}
	s.flushLogBytesLocked()
}

// OutputPath returns where a run's stdout is stored (for the UI/inspection).
func (s *Store) OutputPath(runID string) string {
	return s.StreamPath(runID, "stdout")
}

// ReadOutputFrom reads up to max bytes of a run's output starting at offset, and
// returns the new offset. This backs the web UI's live tail: the browser keeps asking
// from where it left off while the run is still producing output.
func (s *Store) ReadOutputFrom(runID, stream string, offset int64, max int) (data []byte, newOffset int64, err error) {
	f, err := os.Open(s.StreamPath(runID, stream))
	if os.IsNotExist(err) {
		return nil, offset, nil // nothing captured yet
	}
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}
	// A shrunken file means the run was reset; start over rather than read garbage.
	if offset > info.Size() {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	buf := make([]byte, max)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, offset, err
	}
	return buf[:n], offset + int64(n), nil
}

// PrunableLogRuns returns finished runs whose logs are past their retention window. A
// zero time for either class means it is kept forever. limit caps one pass.
func (s *Store) PrunableLogRuns(successBefore, failedBefore time.Time, limit int) ([]string, error) {
	var conds []string
	var args []any
	if !successBefore.IsZero() {
		conds = append(conds, `(status = 'success' AND ended_at < ?)`)
		args = append(args, toMillis(successBefore))
	}
	if !failedBefore.IsZero() {
		conds = append(conds, `(status IN ('failed', 'error') AND ended_at < ?)`)
		args = append(args, toMillis(failedBefore))
	}
	if len(conds) == 0 {
		return nil, nil
	}
	// logs_pruned keeps an already-swept run out of every later pass; without it the
	// sweep would re-stat the whole history once an hour, forever.
	q := `SELECT id FROM runs WHERE logs_pruned = 0 AND ended_at > 0 AND (` +
		strings.Join(conds, " OR ") + `) ORDER BY id LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, formatRunID(n))
	}
	return out, rows.Err()
}

// PruneRunLogs deletes a run's log files and marks it swept. The run row and its backup
// payload are left alone: the metadata is small and the payload is the backup.
func (s *Store) PruneRunLogs(runID string) error {
	n, err := parseRunID(runID)
	if err != nil {
		return err
	}
	for _, stream := range []string{"stdout", "stderr"} {
		if err := os.Remove(s.StreamPath(runID, stream)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	_, err = s.db.Exec(`UPDATE runs SET logs_pruned = 1 WHERE id = ?`, n)
	return err
}

// StreamPath returns the file backing one of a run's streams ("stdout"/"stderr").
func (s *Store) StreamPath(runID, stream string) string {
	if stream != "stderr" {
		stream = "stdout"
	}
	return filepath.Join(s.backupDir, "runs", runID, stream+".log")
}

// --- helpers ----------------------------------------------------------------

// Run IDs are surfaced as "run-<rowid>" so they stay stable and human-readable.
func formatRunID(n int64) string { return "run-" + strconv.FormatInt(n, 10) }

func parseRunID(id string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimPrefix(id, "run-"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid run id %q", id)
	}
	return n, nil
}

func toMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
