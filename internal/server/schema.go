package server

// schemaSQL is applied on every open; all statements are idempotent. When the schema
// needs to change incompatibly, add migration steps rather than editing history.
const schemaSQL = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
-- With WAL, NORMAL syncs at checkpoints rather than on every commit. The default (FULL)
-- means one fsync per transaction, which a run's progress updates pay for repeatedly.
-- A crash can then lose the last few commits, but not corrupt the database — and what
-- would be lost is run metadata, seconds old, never a backup.
PRAGMA synchronous = NORMAL;

CREATE TABLE IF NOT EXISTS runners (
  id         TEXT PRIMARY KEY,
  hostname   TEXT NOT NULL DEFAULT '',
  os         TEXT NOT NULL DEFAULT '',
  arch       TEXT NOT NULL DEFAULT '',
  first_seen INTEGER NOT NULL DEFAULT 0,  -- unix millis
  last_seen  INTEGER NOT NULL DEFAULT 0
);

-- An instance binds a script definition to one runner (N:1) with concrete
-- parameters. It says what to back up and with what, never when — that is the
-- schedules table below. Secrets are plaintext at this stage; encryption at rest
-- arrives with pkg/crypto.SecretBox.
--
-- The schedule column is the pre-split form, kept because dropping a column would
-- mean rebuilding the table and losing the record of what an operator had before the
-- split. It is written once by whoever created the row, read once by
-- migrateInlineSchedules, and never read again.
CREATE TABLE IF NOT EXISTS instances (
  id        TEXT PRIMARY KEY,
  script    TEXT NOT NULL,
  runner_id TEXT NOT NULL,
  params    TEXT NOT NULL DEFAULT '{}',  -- JSON object
  secrets   TEXT NOT NULL DEFAULT '{}',  -- JSON object
  capture   TEXT NOT NULL DEFAULT '',
  timeout   TEXT NOT NULL DEFAULT '',
  schedule  TEXT NOT NULL DEFAULT '{}'   -- JSON object, legacy (see above)
);

CREATE INDEX IF NOT EXISTS idx_instances_runner ON instances(runner_id);

-- A schedule says when one instance runs. It is a table of its own, N per instance,
-- because "what to back up" and "when to back it up" change for different reasons and
-- at different moments: one task can want a nightly dump *and* a full copy once a
-- month, and pausing the nightly run must not mean editing the backup itself.
CREATE TABLE IF NOT EXISTS schedules (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  instance_id TEXT    NOT NULL,
  name        TEXT    NOT NULL DEFAULT '',      -- operator's label, e.g. "nightly"
  frequency   TEXT    NOT NULL DEFAULT 'daily', -- daily | weekly | monthly
  time        TEXT    NOT NULL DEFAULT '',      -- HH:MM
  weekdays    TEXT    NOT NULL DEFAULT '',      -- comma list for weekly, "mon,thu"
  day         INTEGER NOT NULL DEFAULT 0,       -- day of month for monthly (1-28)
  timezone    TEXT    NOT NULL DEFAULT '',      -- empty falls back to the server default
  -- A paused schedule keeps its definition and stops coming due. Deleting one to skip a
  -- week and typing it in again is how a schedule comes back subtly different.
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  INTEGER NOT NULL DEFAULT 0,       -- unix millis
  updated_at  INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_schedules_instance ON schedules(instance_id);

CREATE TABLE IF NOT EXISTS runs (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  instance_id TEXT NOT NULL,
  runner_id   TEXT NOT NULL,
  script      TEXT NOT NULL,
  status      TEXT NOT NULL,
  exit_code   INTEGER NOT NULL DEFAULT 0,
  bytes       INTEGER NOT NULL DEFAULT 0,
  started_at  INTEGER NOT NULL DEFAULT 0,
  ended_at    INTEGER NOT NULL DEFAULT 0,
  err         TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_runs_instance ON runs(instance_id);
CREATE INDEX IF NOT EXISTS idx_runs_created  ON runs(created_at DESC);

-- Web operators. Runners authenticate with certificates; people log in with a
-- username and a password, which is what the web UI asks for. Only a PBKDF2
-- verifier is stored, never the password (pkg/crypto/password.go).
CREATE TABLE IF NOT EXISTS users (
  username   TEXT PRIMARY KEY,
  pass_hash  TEXT NOT NULL,
  role       TEXT NOT NULL DEFAULT 'admin',   -- admin | viewer
  disabled   INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL DEFAULT 0,      -- unix millis
  updated_at INTEGER NOT NULL DEFAULT 0,
  last_login INTEGER NOT NULL DEFAULT 0
);

-- Login sessions behind the web UI's cookie. Only a SHA-256 of the token is stored,
-- so the database cannot be used to impersonate a logged-in operator. Rows are
-- deleted on logout, when the account changes, and once expired.
CREATE TABLE IF NOT EXISTS sessions (
  token_hash TEXT PRIMARY KEY,
  username   TEXT NOT NULL,
  created_at INTEGER NOT NULL DEFAULT 0,
  expires_at INTEGER NOT NULL DEFAULT 0,
  last_seen  INTEGER NOT NULL DEFAULT 0,
  ip         TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(username);

-- What still has to reach the off-site replica, and what already has. The queue is the
-- single source of truth for replication: the run list joins it, the catch-up sweep
-- walks it, and an outage shows up as rows that stay pending rather than as anything
-- lost. Nothing is ever dropped from it on failure — an item that cannot be transferred
-- keeps its place and is retried, which is what makes a repaired link catch up by itself.
--
-- One row per item, keyed by kind and key, so re-queueing the same thing is an update
-- rather than a duplicate:
--   run  <run id>       a finished run's directory (dump and logs)
--   repo <instance id>  an instance's restic repository
--   meta server         the database snapshot, server.toml and the keys
CREATE TABLE IF NOT EXISTS replica_queue (
  kind        TEXT NOT NULL,
  key         TEXT NOT NULL,
  status      TEXT NOT NULL,                  -- pending | syncing | done | failed
  attempts    INTEGER NOT NULL DEFAULT 0,
  queued_at   INTEGER NOT NULL DEFAULT 0,     -- unix millis
  started_at  INTEGER NOT NULL DEFAULT 0,
  done_at     INTEGER NOT NULL DEFAULT 0,
  next_try_at INTEGER NOT NULL DEFAULT 0,     -- backoff: not before this
  bytes       INTEGER NOT NULL DEFAULT 0,     -- transferred by the last successful pass
  err         TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (kind, key)
);

CREATE INDEX IF NOT EXISTS idx_replica_due ON replica_queue(status, next_try_at);

-- Whether the replica is reachable at all, kept apart from the queue so the UI can say
-- "off-site has been down since 14:02" even at a moment when there is nothing to send.
-- One row, id 1.
CREATE TABLE IF NOT EXISTS replica_state (
  id          INTEGER PRIMARY KEY CHECK (id = 1),
  last_ok_at  INTEGER NOT NULL DEFAULT 0,
  last_err    TEXT    NOT NULL DEFAULT '',
  last_err_at INTEGER NOT NULL DEFAULT 0,
  down_since  INTEGER NOT NULL DEFAULT 0      -- 0 = healthy
);
`

// addColumns lists columns added after the initial schema. They are applied only when
// missing, so an existing database is upgraded in place instead of needing a rebuild.
//
// The enrollment columns default to 'approved'/empty on purpose: runners that already
// exist got their certificate by hand, and their check-ins must keep working.
var addColumns = []struct{ table, column, definition string }{
	// Enrollment state: a runner that asked for a certificate starts as 'pending' and
	// only becomes 'approved' once an operator says so.
	{"runners", "status", "TEXT NOT NULL DEFAULT 'approved'"},
	{"runners", "csr", "TEXT NOT NULL DEFAULT ''"},
	{"runners", "cert_pem", "TEXT NOT NULL DEFAULT ''"},
	{"runners", "cert_fingerprint", "TEXT NOT NULL DEFAULT ''"},
	{"runners", "enroll_ip", "TEXT NOT NULL DEFAULT ''"},
	{"runners", "enrolled_at", "INTEGER NOT NULL DEFAULT 0"},
	{"runners", "approved_at", "INTEGER NOT NULL DEFAULT 0"},
	// When the runner's certificate expires. Recorded from the certificate presented at
	// check-in, so it is known for hand-issued certificates too — without it, every
	// runner would silently stop working on the same day.
	{"runners", "cert_not_after", "INTEGER NOT NULL DEFAULT 0"},
	{"runners", "revoked_at", "INTEGER NOT NULL DEFAULT 0"},
	{"runners", "renewed_at", "INTEGER NOT NULL DEFAULT 0"},
	// Which authority issued the runner's current certificate. Recorded at check-in so a
	// CA rotation can be tracked: an operator needs to know every runner has moved to the
	// new authority before the old one is dropped from the trust bundle.
	{"runners", "cert_issuer", "TEXT NOT NULL DEFAULT ''"},
	// The build a runner reports at check-in, so an operator can see which hosts have
	// picked up a published update.
	{"runners", "version", "TEXT NOT NULL DEFAULT ''"},
	// Backup payload received for a run, kept apart from `bytes` (the log). Runs from
	// before the split have their dump counted in `bytes`, which is what it meant then.
	{"runs", "data_bytes", "INTEGER NOT NULL DEFAULT 0"},
	// Set once the retention sweeper has deleted a run's logs, so old runs are not
	// re-scanned on every pass (retention.go).
	{"runs", "logs_pruned", "INTEGER NOT NULL DEFAULT 0"},
	// The timeout this run was dispatched with. The runner enforces it, but the server
	// needs its own copy: a runner that dies mid-run reports nothing, and without this
	// there is no moment at which the server may conclude the run is never coming back
	// (reaper.go). 0 means the default was in force.
	{"runs", "timeout_sec", "INTEGER NOT NULL DEFAULT 0"},
	// Set when an operator asks for a run to stop. The runner polls for it, since in a
	// pull model the server cannot reach in and interrupt anything (cancel.go).
	{"runs", "cancel_requested", "INTEGER NOT NULL DEFAULT 0"},
	// Set once retention has deleted a run's backup payload. data_bytes is left alone, so
	// the history still says what the run produced (dumps.go).
	{"runs", "data_pruned", "INTEGER NOT NULL DEFAULT 0"},
	// How many dumps an instance keeps, and for how long. A dump is one opaque artifact
	// restored whole, so it is rotated by count and age rather than deduplicated into a
	// repository. 0 for both means keep everything: deleting a backup must never be a
	// default somebody inherits without having asked for it.
	{"instances", "keep_last", "INTEGER NOT NULL DEFAULT 0"},
	{"instances", "keep_days", "INTEGER NOT NULL DEFAULT 0"},
	// Which schedule caused this run. Empty means somebody pressed "run now": a manual
	// run belongs to no schedule, and the two reach the runner by different paths
	// (scheduler.go). History is only readable if it says which of an instance's
	// schedules a run came from.
	{"runs", "schedule_id", "TEXT NOT NULL DEFAULT ''"},
	// Set once this instance's legacy inline schedule has been lifted into the schedules
	// table. The marker is per row rather than per database on purpose: a global "done"
	// flag is right only the first time, whereas this one is also right for an instance
	// restored later from an older archive — and, the case that matters, it never
	// resurrects a schedule the operator has since deleted (migrate_schedules.go).
	{"instances", "schedule_migrated", "INTEGER NOT NULL DEFAULT 0"},
}

// postMigrateSQL is applied after addColumns, so unlike schemaSQL it may refer to
// columns those added. An index over runs(schedule_id) in schemaSQL would fail on every
// database that predates the column, because the schema is applied before the migration.
const postMigrateSQL = `
-- The run history of one task, newest first. idx_runs_instance above has no sort column
-- and leaves the ordering to a sort pass; this one is what the per-instance history and
-- the "last run of this schedule" lookups actually walk.
CREATE INDEX IF NOT EXISTS idx_runs_instance_id  ON runs(instance_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_runs_schedule     ON runs(schedule_id, id DESC);
-- The dashboard's "what failed in the last 24 hours".
CREATE INDEX IF NOT EXISTS idx_runs_status_ended ON runs(status, ended_at DESC);
-- The period view scans a date range of finished runs regardless of status (stats.go).
-- Every other index over runs leads with a different column, so that range would
-- otherwise be a full table scan.
CREATE INDEX IF NOT EXISTS idx_runs_ended       ON runs(ended_at);
`
