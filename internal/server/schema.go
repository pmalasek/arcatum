package server

// schemaSQL is applied on every open; all statements are idempotent. When the schema
// needs to change incompatibly, add migration steps rather than editing history.
const schemaSQL = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;

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
`
