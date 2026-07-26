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
	box       crypto.SecretBox
}

// Open opens (creating if needed) the SQLite database at dbPath and applies the
// schema. box may be nil, in which case secrets are stored unencrypted.
func Open(dbPath, backupDir string, box crypto.SecretBox) (*Store, error) {
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
	return &Store{db: db, backupDir: backupDir, box: box}, nil
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
func (s *Store) Close() error { return s.db.Close() }

// --- instances ---------------------------------------------------------------

// ImportInstances upserts instances from a JSON array file and returns how many were
// imported. A missing file is not an error: instances may be managed via the API instead.
func (s *Store) ImportInstances(path string) (int, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
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
	for _, in := range list {
		if in.ID == "" {
			return 0, fmt.Errorf("instances %s: an instance is missing id", path)
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
			INSERT INTO instances (id, script, runner_id, params, secrets, capture, timeout, schedule)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
			  script=excluded.script, runner_id=excluded.runner_id, params=excluded.params,
			  secrets=excluded.secrets, capture=excluded.capture, timeout=excluded.timeout,
			  schedule=excluded.schedule`,
			in.ID, in.Script, in.RunnerID, string(params), string(secrets), in.Capture, in.Timeout, string(sched))
		if err != nil {
			return 0, fmt.Errorf("upsert instance %q: %w", in.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(list), nil
}

const instanceCols = `id, script, runner_id, params, secrets, capture, timeout, schedule`

// sealSecrets encrypts each secret value for storage, binding it to this instance and
// secret name so a ciphertext cannot be relocated within the database.
func (s *Store) sealSecrets(instanceID string, secrets map[string]string) (map[string]string, error) {
	if secrets == nil {
		return nil, nil
	}
	out := make(map[string]string, len(secrets))
	for name, value := range secrets {
		sealed, err := crypto.SealToString(s.box, value, instanceID, name)
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
		plain, err := crypto.OpenFromString(s.box, value, instanceID, name)
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
	if err := sc.Scan(&in.ID, &in.Script, &in.RunnerID, &params, &secrets, &in.Capture, &in.Timeout, &sched); err != nil {
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
func (s *Store) RecordCheckin(req proto.CheckinRequest, certNotAfter, now time.Time) error {
	ms := toMillis(now)
	_, err := s.db.Exec(`
		INSERT INTO runners (id, hostname, os, arch, first_seen, last_seen, cert_not_after)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  hostname=excluded.hostname, os=excluded.os, arch=excluded.arch,
		  last_seen=excluded.last_seen,
		  -- Keep a known expiry if this check-in did not carry one.
		  cert_not_after=CASE WHEN excluded.cert_not_after > 0
		                      THEN excluded.cert_not_after ELSE runners.cert_not_after END`,
		req.RunnerID, req.Hostname, req.OS, req.Arch, ms, ms, toMillis(certNotAfter))
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
}

const runnerCols = `id, hostname, os, arch, status, cert_fingerprint, enroll_ip,
                    first_seen, last_seen, enrolled_at, approved_at, cert_not_after`

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
			&r.CertFingerprint, &r.EnrollIP, &first, &last, &enrolled, &approved, &notAfter); err != nil {
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

// CreateRun records a new pending run for an instance.
func (s *Store) CreateRun(inst *Instance) (*Run, error) {
	now := time.Now()
	res, err := s.db.Exec(`
		INSERT INTO runs (instance_id, runner_id, script, status, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		inst.ID, inst.RunnerID, inst.Script, string(StatusPending), toMillis(now))
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

// AddRunBytes accumulates received output/data bytes for a run.
func (s *Store) AddRunBytes(runID string, delta int64) error {
	n, err := parseRunID(runID)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE runs SET bytes = bytes + ? WHERE id = ?`, delta, n)
	return err
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
	_, err = s.db.Exec(`UPDATE runs SET status = ?, exit_code = ?, ended_at = ?, err = ? WHERE id = ?`,
		string(status), exitCode, toMillis(at), execErr, n)
	return err
}

const runCols = `id, instance_id, runner_id, script, status, exit_code, bytes, started_at, ended_at, err`

func scanRun(sc interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var id int64
	var started, ended int64
	var status string
	if err := sc.Scan(&id, &r.InstanceID, &r.RunnerID, &r.Script, &status,
		&r.ExitCode, &r.Bytes, &started, &ended, &r.Err); err != nil {
		return nil, err
	}
	r.ID = formatRunID(id)
	r.Status = RunStatus(status)
	r.StartedAt, r.EndedAt = fromMillis(started), fromMillis(ended)
	return &r, nil
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

// --- run output (files, not DB) ---------------------------------------------

// AppendOutput appends streamed bytes to backupDir/runs/<runID>/<stream>.log and
// returns the number of bytes written.
func (s *Store) AppendOutput(runID, stream string, data []byte) (int, error) {
	if stream != "stdout" && stream != "stderr" {
		stream = "stdout"
	}
	dir := filepath.Join(s.backupDir, "runs", runID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(filepath.Join(dir, stream+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.Write(data)
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
