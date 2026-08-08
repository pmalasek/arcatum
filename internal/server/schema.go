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
-- parameters and a schedule. Secrets are plaintext at this stage; encryption at
-- rest arrives with pkg/crypto.SecretBox.
CREATE TABLE IF NOT EXISTS instances (
  id        TEXT PRIMARY KEY,
  script    TEXT NOT NULL,
  runner_id TEXT NOT NULL,
  params    TEXT NOT NULL DEFAULT '{}',  -- JSON object
  secrets   TEXT NOT NULL DEFAULT '{}',  -- JSON object
  capture   TEXT NOT NULL DEFAULT '',
  timeout   TEXT NOT NULL DEFAULT '',
  schedule  TEXT NOT NULL DEFAULT '{}'   -- JSON object
);

CREATE INDEX IF NOT EXISTS idx_instances_runner ON instances(runner_id);

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
}
