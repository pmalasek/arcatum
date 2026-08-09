package server

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"
)

// Persistence for the off-site replica: the work queue and the link's health. Kept in
// the same database as everything else so that "what is still not off-site" survives a
// restart — a backlog that only lived in memory would silently reset to empty every
// time the server came back, which is exactly when it matters most.

// ReplicaKind is what a queue row points at.
type ReplicaKind string

const (
	// ReplicaKindRun is one finished run's directory: the dump and its logs.
	ReplicaKindRun ReplicaKind = "run"
	// ReplicaKindRepo is an instance's restic repository. It has no single run to hang
	// off — every backup adds to it — so it is replicated as a whole.
	ReplicaKindRepo ReplicaKind = "repo"
	// ReplicaKindTree is a whole subtree of backup_dir, reconciled as one. It is what
	// carries a deletion here through to the replica: retention removing a dump raises
	// no event the queue could hear, so the trees are compared wholesale on a schedule.
	ReplicaKindTree ReplicaKind = "tree"
	// ReplicaKindMeta is the server itself: the database snapshot, server.toml and,
	// when configured, the keys. Without those the repositories are undecryptable.
	ReplicaKindMeta ReplicaKind = "meta"
)

// ReplicaStatus is a queue row's state.
type ReplicaStatus string

const (
	ReplicaPending ReplicaStatus = "pending"
	ReplicaSyncing ReplicaStatus = "syncing"
	ReplicaDone    ReplicaStatus = "done"
	ReplicaFailed  ReplicaStatus = "failed"
)

// metaKey is the single key used for ReplicaKindMeta rows.
const metaKey = "server"

// ReplicaItem is one row of the queue.
type ReplicaItem struct {
	Kind      ReplicaKind   `json:"kind"`
	Key       string        `json:"key"`
	Status    ReplicaStatus `json:"status"`
	Attempts  int           `json:"attempts"`
	QueuedAt  time.Time     `json:"queued_at"`
	StartedAt time.Time     `json:"started_at,omitempty"`
	DoneAt    time.Time     `json:"done_at,omitempty"`
	NextTryAt time.Time     `json:"next_try_at,omitempty"`
	Bytes     int64         `json:"bytes"`
	Err       string        `json:"err,omitempty"`
}

// ReplicaLink is the health of the connection to the replica.
type ReplicaLink struct {
	LastOKAt  time.Time `json:"last_ok_at,omitempty"`
	LastErr   string    `json:"last_err,omitempty"`
	LastErrAt time.Time `json:"last_err_at,omitempty"`
	// DownSince is when the link first stopped working, so the UI can say how long the
	// outage has lasted rather than only that the last attempt failed. Zero means the
	// link is healthy.
	DownSince time.Time `json:"down_since,omitempty"`
}

// Healthy reports whether the replica is currently reachable.
func (l ReplicaLink) Healthy() bool { return l.DownSince.IsZero() }

const replicaCols = `kind, key, status, attempts, queued_at, started_at, done_at, ` +
	`next_try_at, bytes, err`

func scanReplicaItem(sc interface{ Scan(...any) error }) (*ReplicaItem, error) {
	var it ReplicaItem
	var kind, status string
	var queued, started, done, next int64
	if err := sc.Scan(&kind, &it.Key, &status, &it.Attempts, &queued, &started, &done,
		&next, &it.Bytes, &it.Err); err != nil {
		return nil, err
	}
	it.Kind, it.Status = ReplicaKind(kind), ReplicaStatus(status)
	it.QueuedAt, it.StartedAt = fromMillis(queued), fromMillis(started)
	it.DoneAt, it.NextTryAt = fromMillis(done), fromMillis(next)
	return &it, nil
}

// EnqueueReplica marks an item as needing to go off-site. It is idempotent by design:
// the same item queued twice is one row asking to be sent again, not two. An item that
// is already syncing is left alone — the pass in flight is copying the current contents
// anyway, and resetting the row under it would lose the attempt count that drives
// the backoff.
func (s *Store) EnqueueReplica(kind ReplicaKind, key string, now time.Time) error {
	if key == "" {
		return errors.New("replica: empty queue key")
	}
	// queued_at is only refreshed for an item that had finished. For one still waiting,
	// the original time is what makes "the backlog is two hours old" true — re-queueing
	// the same unsent item must not reset that clock.
	_, err := s.db.Exec(`INSERT INTO replica_queue
		  (kind, key, status, attempts, queued_at, next_try_at, err)
		  VALUES (?, ?, 'pending', 0, ?, 0, '')
		ON CONFLICT(kind, key) DO UPDATE SET
		  status      = CASE WHEN replica_queue.status = 'syncing' THEN 'syncing' ELSE 'pending' END,
		  queued_at   = CASE WHEN replica_queue.status = 'done' THEN excluded.queued_at ELSE replica_queue.queued_at END,
		  next_try_at = CASE WHEN replica_queue.status = 'syncing' THEN replica_queue.next_try_at ELSE 0 END,
		  attempts    = CASE WHEN replica_queue.status = 'done' THEN 0 ELSE replica_queue.attempts END,
		  err         = CASE WHEN replica_queue.status = 'done' THEN '' ELSE replica_queue.err END`,
		string(kind), key, toMillis(now))
	return err
}

// ResetReplicaSyncing returns items left mid-transfer to the queue. Only the worker sets
// `syncing`, and there is only ever one of it, so a row still in that state at startup
// belongs to a process that no longer exists.
func (s *Store) ResetReplicaSyncing() error {
	_, err := s.db.Exec(`UPDATE replica_queue SET status = ?, next_try_at = 0 WHERE status = ?`,
		string(ReplicaPending), string(ReplicaSyncing))
	return err
}

// DueReplicaItems returns items ready to be transferred, oldest first. Rows in backoff
// are skipped until their time comes.
func (s *Store) DueReplicaItems(now time.Time, limit int) ([]*ReplicaItem, error) {
	q := `SELECT ` + replicaCols + ` FROM replica_queue
	      WHERE status IN (?, ?) AND next_try_at <= ?
	      ORDER BY next_try_at, queued_at`
	args := []any{string(ReplicaPending), string(ReplicaFailed), toMillis(now)}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ReplicaItem
	for rows.Next() {
		it, err := scanReplicaItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ReplicaItems returns the whole queue, newest activity first. It backs the admin view.
func (s *Store) ReplicaItems(limit int) ([]*ReplicaItem, error) {
	q := `SELECT ` + replicaCols + ` FROM replica_queue ORDER BY queued_at DESC`
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
	var out []*ReplicaItem
	for rows.Next() {
		it, err := scanReplicaItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ReplicaItemFor returns one queue row, or nil if the item has never been queued.
func (s *Store) ReplicaItemFor(kind ReplicaKind, key string) (*ReplicaItem, error) {
	row := s.db.QueryRow(`SELECT `+replicaCols+` FROM replica_queue WHERE kind = ? AND key = ?`,
		string(kind), key)
	it, err := scanReplicaItem(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return it, err
}

// MarkReplicaSyncing claims an item for transfer.
func (s *Store) MarkReplicaSyncing(kind ReplicaKind, key string, now time.Time) error {
	_, err := s.db.Exec(`UPDATE replica_queue SET status = ?, started_at = ?
		WHERE kind = ? AND key = ?`,
		string(ReplicaSyncing), toMillis(now), string(kind), key)
	return err
}

// MarkReplicaDone records a completed transfer and clears the retry state.
func (s *Store) MarkReplicaDone(kind ReplicaKind, key string, now time.Time, bytes int64) error {
	_, err := s.db.Exec(`UPDATE replica_queue
		SET status = ?, done_at = ?, bytes = ?, attempts = 0, next_try_at = 0, err = ''
		WHERE kind = ? AND key = ?`,
		string(ReplicaDone), toMillis(now), bytes, string(kind), key)
	return err
}

// replicaBackoffMax caps the retry interval. Beyond half an hour the delay stops being
// politeness towards a broken link and starts being a backlog nobody is clearing.
const replicaBackoffMax = 30 * time.Minute

// replicaBackoffBase is the first retry delay; each further attempt doubles it.
const replicaBackoffBase = 30 * time.Second

// replicaBackoff returns how long to wait before attempt number n (1-based).
func replicaBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := replicaBackoffBase
	for i := 1; i < attempts && d < replicaBackoffMax; i++ {
		d *= 2
	}
	if d > replicaBackoffMax {
		d = replicaBackoffMax
	}
	return d
}

// MarkReplicaFailed records a failed transfer and schedules the retry. The item is never
// removed: a link that comes back has to find the work still waiting for it, which is
// the whole point of keeping a queue rather than reacting to events.
func (s *Store) MarkReplicaFailed(kind ReplicaKind, key string, now time.Time, cause string) error {
	if cause == "" {
		cause = "unknown error"
	}
	// Read-modify-write rather than attempts+1 inline, so the same backoff arithmetic is
	// visible in one place and testable on its own.
	var attempts int
	err := s.db.QueryRow(`SELECT attempts FROM replica_queue WHERE kind = ? AND key = ?`,
		string(kind), key).Scan(&attempts)
	if err == sql.ErrNoRows {
		return nil // the item was removed while it was being transferred; nothing to record
	}
	if err != nil {
		return err
	}
	attempts++
	next := now.Add(replicaBackoff(attempts))
	_, err = s.db.Exec(`UPDATE replica_queue
		SET status = ?, attempts = ?, next_try_at = ?, err = ?
		WHERE kind = ? AND key = ?`,
		string(ReplicaFailed), attempts, toMillis(next), cause, string(kind), key)
	return err
}

// DeferReplicaItem pushes an item back without counting it as a failure. It is what a
// repository whose instance is mid-backup gets: not an error, just not now.
func (s *Store) DeferReplicaItem(kind ReplicaKind, key string, until time.Time) error {
	_, err := s.db.Exec(`UPDATE replica_queue SET status = ?, next_try_at = ?
		WHERE kind = ? AND key = ?`,
		string(ReplicaPending), toMillis(until), string(kind), key)
	return err
}

// RetryReplicaFailed puts every failed item back in line immediately. It backs the
// "retry now" button, for when an operator has fixed the link and does not want to wait
// out the backoff.
func (s *Store) RetryReplicaFailed() (int, error) {
	res, err := s.db.Exec(`UPDATE replica_queue SET status = ?, next_try_at = 0, attempts = 0
		WHERE status = ?`, string(ReplicaPending), string(ReplicaFailed))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// DeleteReplicaItem drops a row. Used when what it points at no longer exists here —
// a run whose directory retention has removed has nothing left to replicate, and the
// mirroring pass is what takes it off the replica.
func (s *Store) DeleteReplicaItem(kind ReplicaKind, key string) error {
	_, err := s.db.Exec(`DELETE FROM replica_queue WHERE kind = ? AND key = ?`, string(kind), key)
	return err
}

// ReplicaCounts summarises the queue for the status endpoint.
type ReplicaCounts struct {
	Pending int `json:"pending"`
	Syncing int `json:"syncing"`
	Done    int `json:"done"`
	Failed  int `json:"failed"`
	// OldestPendingAt is when the longest-waiting unfinished item was queued. A backlog
	// is only alarming once it is old, and a count alone cannot say that.
	OldestPendingAt time.Time `json:"oldest_pending_at,omitempty"`
}

// ReplicaQueueCounts returns how much work is outstanding.
func (s *Store) ReplicaQueueCounts() (ReplicaCounts, error) {
	var c ReplicaCounts
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM replica_queue GROUP BY status`)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return c, err
		}
		switch ReplicaStatus(status) {
		case ReplicaPending:
			c.Pending = n
		case ReplicaSyncing:
			c.Syncing = n
		case ReplicaDone:
			c.Done = n
		case ReplicaFailed:
			c.Failed = n
		}
	}
	if err := rows.Err(); err != nil {
		return c, err
	}
	var oldest sql.NullInt64
	if err := s.db.QueryRow(`SELECT MIN(queued_at) FROM replica_queue WHERE status != ?`,
		string(ReplicaDone)).Scan(&oldest); err != nil {
		return c, err
	}
	if oldest.Valid {
		c.OldestPendingAt = fromMillis(oldest.Int64)
	}
	return c, nil
}

// ReplicaLinkState returns the health of the connection.
func (s *Store) ReplicaLinkState() (ReplicaLink, error) {
	var l ReplicaLink
	var okAt, errAt, down int64
	err := s.db.QueryRow(`SELECT last_ok_at, last_err, last_err_at, down_since
		FROM replica_state WHERE id = 1`).Scan(&okAt, &l.LastErr, &errAt, &down)
	if err == sql.ErrNoRows {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	l.LastOKAt, l.LastErrAt, l.DownSince = fromMillis(okAt), fromMillis(errAt), fromMillis(down)
	return l, nil
}

// RecordReplicaOK notes that the replica answered.
func (s *Store) RecordReplicaOK(now time.Time) error {
	_, err := s.db.Exec(`INSERT INTO replica_state (id, last_ok_at, last_err, last_err_at, down_since)
		VALUES (1, ?, '', 0, 0)
		ON CONFLICT(id) DO UPDATE SET last_ok_at = excluded.last_ok_at, down_since = 0`,
		toMillis(now))
	return err
}

// RecordReplicaError notes that the replica did not answer. down_since is set only on
// the first failure and left alone afterwards, so the UI can report how long the outage
// has lasted instead of restarting the clock on every retry.
func (s *Store) RecordReplicaError(now time.Time, cause string) error {
	if cause == "" {
		cause = "unknown error"
	}
	_, err := s.db.Exec(`INSERT INTO replica_state (id, last_ok_at, last_err, last_err_at, down_since)
		VALUES (1, 0, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  last_err    = excluded.last_err,
		  last_err_at = excluded.last_err_at,
		  down_since  = CASE WHEN replica_state.down_since = 0 THEN excluded.down_since
		                     ELSE replica_state.down_since END`,
		cause, toMillis(now), toMillis(now))
	return err
}

// replicaStatusFor is the sub-select mapping a run to its replication row. A run's
// off-site state is the state of whatever carries its data: the run directory for a
// dump, the repository for a restic backup. Both are tried, dump first, because a run
// row does not know which kind of script produced it and the queue does.
const replicaStatusFor = `(SELECT status FROM replica_queue
	WHERE (kind = 'run'  AND key = 'run-' || runs.id)
	   OR (kind = 'repo' AND key = runs.instance_id)
	ORDER BY CASE kind WHEN 'run' THEN 0 ELSE 1 END LIMIT 1)`

// HasActiveRun reports whether an instance has a run in flight. A repository is not
// replicated while its instance is being backed up: restic writes packs, then the index,
// then the snapshot, and copying that mid-flight is how a replica ends up with a
// snapshot referring to packs that are not there yet.
func (s *Store) HasActiveRun(instanceID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM runs WHERE instance_id = ? AND status IN (?, ?)`,
		instanceID, string(StatusPending), string(StatusRunning)).Scan(&n)
	return n > 0, err
}

// ReplicableRuns lists runs whose stored payload should be off-site: finished
// successfully and not yet rotated away. It is what the catch-up sweep walks, so a run
// that was produced while the link was down is picked up even though the event that
// would have queued it is long gone.
func (s *Store) ReplicableRuns(limit int) ([]string, error) {
	q := `SELECT id FROM runs WHERE status = ? AND data_pruned = 0 ORDER BY id DESC`
	args := []any{string(StatusSuccess)}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, formatRunID(id))
	}
	return out, rows.Err()
}

// SnapshotDatabase writes a consistent copy of the database to path. A live SQLite file
// in WAL mode cannot be copied byte for byte while it is open — the copy would be a mix
// of the file and a write-ahead log it does not include — so the replica gets a snapshot
// taken by the database itself rather than whatever rsync happened to read.
func (s *Store) SnapshotDatabase(path string) error {
	// VACUUM INTO refuses to overwrite, and the previous snapshot is of no further use
	// once a new one is being taken.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if _, err := s.db.Exec(`VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("database snapshot: %w", err)
	}
	return nil
}
