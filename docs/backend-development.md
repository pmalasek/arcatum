# Guide: backend development and debugging

A practical guide to working on the Go code of the server and the runner: how to set up a
local environment, where the data flows, where to add a new thing, how to test it and how to
debug it when it does not work.

Background and *why* things are the way they are belong in [architecture.md](architecture.md).
This document is about *how*.

- [1. Environment and basic commands](#1-environment-and-basic-commands)
- [2. The local development loop](#2-the-local-development-loop)
- [3. Where the data flows](#3-where-the-data-flows)
- [4. Where to add what](#4-where-to-add-what)
- [5. Tests](#5-tests)
- [6. Debugging](#6-debugging)
- [7. Development with mTLS enabled](#7-development-with-mtls-enabled)
- [8. Web UI](#8-web-ui)
- [9. Traps that cost time](#9-traps-that-cost-time)
- [10. Before you submit a change](#10-before-you-submit-a-change)

---

## 1. Environment and basic commands

Go is not on `PATH` in this environment:

```sh
export PATH=/usr/local/go/bin:$PATH
```

```sh
go build ./...          # compile everything
go vet ./...            # static checks
go test ./...           # tests (currently clean)
go test -race ./...     # data race detector — mainly for the executor and the scheduler
gofmt -l .              # what is not formatted
```

**The `just` task runner** (optional, `cargo install just` or `apt install just`) has these
commands as recipes in the root `justfile`. `just` with no argument lists them all:

```sh
just build     just vet     just test     just test-race     just fmt
just check                  # gofmt + vet + test + build, i.e. the whole gate from §10
just build-all              # binaries into ./bin
just release                # the same with the version baked in through -ldflags (V=…)
just clean                  # deletes bin/ and local/dist
```

The recipes are a thin wrapper — nothing you could not type by hand. They call `go` from
`PATH`; if it is not there, pass the path in a variable instead of exporting it:

```sh
GO=/usr/local/go/bin/go just test     # gofmt is derived as <GO>fmt, override with GOFMT=
```

Unlike `gofmt -l .`, `just check` **fails** when something is not formatted — `gofmt` itself
returns zero and only prints a list, which is easy to miss both in CI and in a quick run.

The module is called `arcatum`, so imports are `arcatum/internal/server`, `arcatum/pkg/proto`,
… Dependencies are deliberately minimal: `BurntSushi/toml` and `modernc.org/sqlite`. **The
server runs without CGO** — SQLite is pure Go and the result is a static binary. Adding a
dependency that requires CGO would destroy that property.

Convention: **comments and identifiers in English, documentation in Czech and English.**
Every document exists in two versions — `*_cz.md` (Czech) and the plain filename (English) —
and both must be kept in sync in the same change. Comments explain *why*, not what a line
does — stick to the style of the surrounding code.

---

## 2. The local development loop

Development mode is without TLS and without signatures: plain HTTP, the server does not
authenticate callers, the runner runs what it receives. Both components warn about it at
startup. For working on logic it is the fastest route.

**One-time setup** (nothing touches `/central_backup`):

```sh
mkdir -p local/{data,backup}
cat > local/server.toml <<'EOF'
[server]
listen   = "127.0.0.1:8443"
scripts  = "scripts"
data_dir = "./local/data"
timezone = "Europe/Prague"

[web]
listen = "127.0.0.1:8080"

[storage]
backup_dir = "./local/backup"
EOF

cp data/instances.example.json local/instances.json
# runner_id in instances.json must be the hostname of this machine:
hostname
```

`just dev-init` does the same — it creates the directories, copies both example files and
rewrites both `listen` values, `data_dir` and `backup_dir` in the config to local paths, and
the placeholder `REPLACE-WITH-RUNNER-HOSTNAME` in the seed to `hostname -s`. It does **not
overwrite** existing files, so it can be run at any time.

The web UI is then at `http://127.0.0.1:8080/`. On the first start the server prints the
password generated for the `admin` account to the log; at any later point `just passwd admin`
resets it.

`local/` is in `.gitignore`.

**The loop:**

```sh
# terminal 1 — the server
go run ./cmd/server -config local/server.toml -instances local/instances.json

# terminal 2 — force a job and let the runner finish one cycle
curl -X POST http://127.0.0.1:8443/api/v1/instances/hello-demo/run
go run ./cmd/runner -server http://127.0.0.1:8443 -once

# the result
curl http://127.0.0.1:8443/api/v1/runs
curl http://127.0.0.1:8443/api/v1/runs/run-1/output
```

> **The run ID is `run-1`, not `1`.** The output endpoints build the on-disk path from the ID
> (`backup_dir/runs/run-1/stdout.log`), so with a bare number they return an empty body and
> HTTP 200 — which looks like "the script printed nothing". The `just run-output` and
> `just run-tail` recipes fill the number in for you.

Through `just` the same loop is shorter:

```sh
just server                    # terminal 1
just trigger                   # terminal 2 — the default instance is hello-demo
just runner-once
just runs
just run-output 1
```

The recipes point at `local/server.toml`, `local/instances.json` and
`http://127.0.0.1:8443`. They can be overridden with environment variables, so you do not
have to touch the `justfile` even for the mTLS variant ([§7](#7-development-with-mtls-enabled)):

| Variable | Affects | Default |
|---|---|---|
| `GO` | what compiles and runs | `go` from `PATH` |
| `SERVER_CONFIG` | `just server` | `local/server.toml` |
| `INSTANCES` | the seed for `just server` | `local/instances.json` |
| `SERVER_URL` | `just trigger`, `runner-once`, `runs`, `run-output` | `http://127.0.0.1:8443` |
| `LISTEN` | the address written into the config `just dev-init` creates | `127.0.0.1:8443` |
| `V` | the version in `just release` / `dist-runner` | today's date |

```sh
SERVER_CONFIG=local/server-mtls.toml INSTANCES=/dev/null just server
just runner-once http://127.0.0.1:8443 local/runner.toml   # a runner with its own config
```

> **The development server listens on loopback only.** From another machine (typically a
> browser on your laptop against a server in a VM) the connection ends in "Connection error"
> and there is **no trace of the request** in the server log — the kernel refuses it, not
> Arcatum. The fix is `listen = "0.0.0.0:8443"` in the config and a restart;
> `LISTEN=0.0.0.0:8443 just dev-init` creates it that way straight away. Such a server is
> plain HTTP **without authenticating the caller**, though, so the admin API is open to the
> whole network — for anything longer than an experiment, turn
> [mTLS](#7-development-with-mtls-enabled) on.

> `just runs` and friends are bare `curl` without a client certificate — against a server with
> mTLS enabled they end in a handshake error. There, use `curl` with the `-A` set from
> [§7](#7-development-with-mtls-enabled).

Useful flags:

| Flag | Component | What for |
|---|---|---|
| `-config` | server, runner | path to the configuration; optional for the server — without it `./server.toml` is used, then `/etc/arcatum/server.toml` |
| `-instances` | server | the seed file; `/dev/null` when you do not want to seed |
| `-import-force` | server | overwrites existing instances from the seed too |
| `-server` | runner | overrides `runner.server` from the config — handy for a quick test |
| `-once` | runner | **one full cycle** and exit, with the log in the terminal |

`-once` is not a shortcut — it does exactly what one round of the loop does, including picking
up rotated trust material. That is why `-once` behaves like production, not like a test
simplification.

The server **does not restart itself**. After changing Go code, `web/` assets or
`scripts/*.toml`, stop it and start it again.

---

## 3. Where the data flows

One job run, from the runner's tick to the written output. This is the map to search along
when looking for where something broke:

```
runner: Agent.Tick                      internal/runner/loop.go
  └─ POST /api/v1/checkin ──────────►  server: handleCheckin        internal/server/http.go:151
                                         ├─ activeRunnerIdentity     internal/server/auth.go
                                         │    identity comes from the certificate CN, not the request
                                         ├─ store.RecordCheckin      internal/server/store.go
                                         ├─ store.InstancesForRunner
                                         ├─ sched.DueFor             internal/server/scheduler.go
                                         │    → the ids of the due schedules + a manual flag;
                                         │      several due at once still produce ONE run,
                                         │      attributed to the earliest, all of them advanced
                                         └─ buildDispatch(in, scheduleID)  internal/server/http.go
                                              ├─ catalog.Get + readArtifact  (artifact SHA-256)
                                              ├─ store.CreateRun     → status "pending", schedule_id
                                              └─ signer.Sign(d.SigningBytes())
                                         (then sched.MarkDispatched per schedule,
                                          sched.ClearManual for a "run now")
  ◄──── CheckinResponse{Due: […]} ─────┘
  ├─ verifyDispatch                     internal/runner/loop.go:56   the signature BEFORE running
  ├─ Execute                            internal/runner/executor.go
  │    ├─ SHA-256 check of the artifact contents
  │    ├─ ARCATUM_<PARAM> into env, secrets into a file (ARCATUM_SECRETS_FILE)
  │    └─ bash | python3 | binary, cwd = a temporary workdir (deleted after the run)
  └─ POST /api/v1/runs/updates ──────►  handleUpdates                internal/server/http.go:252
       an ndjson stream of RunUpdate      ├─ ownsRun — a runner may only report its own runs
       (started, output, finished)        └─ applyUpdate
                                              ├─ store.StartRun / FinishRun
                                              └─ store.AppendOutput  → backup_dir/runs/<id>/*.log
```

The signature is over the **canonical serialization** in
[pkg/proto/signing.go](../pkg/proto/signing.go): length-prefixed fields, sorted map keys, and
the artifact contents covered by their SHA‑256. The server signs and the runner verifies with
**the same function** — which is why they live in `pkg/proto` and not on one side.

The security layers that come into play:

| Layer | Server | Runner |
|---|---|---|
| mTLS, role from `OU` | `internal/server/auth.go` | `pkg/crypto/tls.go` |
| job signing (Ed25519) | `pkg/crypto/sign.go`, `signingset.go` | `internal/runner/trust.go` |
| secret encryption | `pkg/crypto/secretbox.go`, `keyring.go` | — (receives plaintext in the job) |

---

## 4. Where to add what

### A new endpoint

1. The handler into a suitable file in `internal/server/` (`instances.go`, `restore.go`,
   `update.go`, …).
2. Registration in [internal/server/http.go](../internal/server/http.go). The server has **two
   routers**, because it has two kinds of caller:
   - `Server.Handler()` — the `[server] listen` port (mTLS): runners and calls with an admin
     certificate.
   - `Server.WebHandler()` — the `[web] listen` port (plain HTTP): the web UI and password
     login.

   Operator endpoints belong in `registerOperatorRoutes`, which is registered **on both** —
   it gets a guard according to the listener (`adminOnly` for mTLS, `webRead`/`webAdmin` for
   the web UI), so a new endpoint works from curl and from the web UI and nowhere gets
   forgotten. Register runner endpoints in `Handler()` only. Routes use Go 1.22 patterns
   (`"GET /api/v1/runs/{id}"`).
3. **Decide whether the endpoint only reads or also changes something.** In
   `registerOperatorRoutes` wrap it in `read(...)` (reads — the `viewer` role is let through
   too) or `write(...)` (changes — admin only). Runner endpoints pull the identity through
   `s.activeRunnerIdentity(r, "")` and must check that the runner only touches its own things
   (pattern: `ownsRun`). The logged-in person is returned by `userFrom(r)` (nil on the mTLS
   listener); `actor(r)` gives a name for the log in both cases.
4. The response through `writeJSON`. Secret values **never** go into a response.
5. A row in the API table in the [README](../README.md#http-api) — it is the only list of
   endpoints.

### A new configuration field

`pkg/config/config.go` (server) or `runner.go` (runner) → the struct + a `toml` tag →
`Default()` → `Validate()` → a commented-out example into `config/*.example.toml`.

`Validate()` is there so that **a half-configuration is an error, not a mode**. The pattern:
`[tls]` requires all three paths, `[tls]` enforces `[signing]` and `[secrets]` as well. When
a new field can silently turn something off, add a check too.

### A new database column

Into `addColumns` in [internal/server/schema.go](../internal/server/schema.go) — it is only
added where missing, so an existing database is upgraded in place. **Do not change history**
in `schemaSQL`, and give the column a default that keeps older data working (see the
enrollment columns: default `approved`, so manually issued certificates keep working).

### A new index, and a new table

An index over a column that `addColumns` added belongs in **`postMigrateSQL`**, never in
`schemaSQL`: the schema is applied *before* the migration, so such an index would fail on every
database that predates the column. A whole new table goes into `schemaSQL` as usual — it is
`CREATE TABLE IF NOT EXISTS`, which is safe everywhere.

### Moving data, not just schema

If a change has to *move* data — the schedule split is the first one that did — put it in its
own file (see [internal/server/migrate_schedules.go](../internal/server/migrate_schedules.go))
and run it from `Open` after `postMigrateSQL`.

Two rules that migration paid for:

- **Guard per row, not per database.** A global "done" flag is right exactly once. A marker
  column on the row being migrated is also right for a row restored later from an older
  archive — and, the case that matters, it never resurrects something the operator has since
  deleted. Every writer sets the marker as it inserts, so the scan never revisits a fresh row.
- **Do not fail the open over one bad row.** Log it, mark it handled and carry on. A server
  that refuses to start over one malformed value takes every other backup down with it.

### A new protocol message or field

`pkg/proto/proto.go`. If the field takes part in the signature, add it to `SigningBytes()` in
`signing.go` as well — and mind the deployment order: **the server first, then the runners.**
A change to the canonical form means an old runner will not verify a new signed message, so an
incompatible change reaches production only together with a runner update (and runners only
update themselves after the server has been restarted with the new manifest).

### A new script type

`proto.ScriptType` → allow it in `jobspec.Manifest.Validate()` → the `switch d.Type` in
`prepare()` in [internal/runner/executor.go](../internal/runner/executor.go). A type without
an entrypoint (like `restic`) needs an exception in `Validate()` and in `Catalog`.

---

## 5. Tests

42 test files; `go test ./...` currently passes. The tests are in the same package as the code
(`package server`), so they see unexported things too.

Useful patterns already in the repository — use them, do not invent new ones:

```go
// a store over a temp DB, cleans up after itself       internal/server/store_test.go
st, dir := openTestStore(t)

// a server with a script catalogue "on paper" — no files on disk
catalog := &Catalog{byName: map[string]*ScriptEntry{
    "mysql-backup": {Manifest: &jobspec.Manifest{ /* … */ }},
}}
```

HTTP endpoints are tested through `httptest` against `srv.Handler()` (see
[instances_test.go](../internal/server/instances_test.go)); crypto and schedules with table
tests in `pkg/`.

```sh
go test ./internal/server -run TestImportInstances -v    # a single test
go test ./internal/server -count=1                        # without the cache
go test -race ./internal/runner                           # the executor and the loop
```

What always deserves a test: **authorization** (can runner A report runner B's run?), the
**canonical serialization** of the signature, **migrations** (an old row after a column is
added), and **rejections** (a wrong signature, an unapproved runner, an unknown parameter).
These are easy to miss in a manual test, because "it works".

---

## 6. Debugging

**The server log** is the main source. The server reports every dispatch
(`dispatch: instance=… run=… schedule=… -> runner=…`, where a manual run logs `schedule=manual`),
rejections (`checkin denied: …`) and instance
errors. A broken instance does not stop the checkin — it is only skipped with a log entry, so
when a job "did not run and nobody said anything", look here.

**State in the DB.** The database is ordinary SQLite (WAL):

```sh
sqlite3 local/data/arcatum.db 'SELECT id,instance_id,status,exit_code,bytes FROM runs ORDER BY id DESC LIMIT 10;'
sqlite3 local/data/arcatum.db 'SELECT id,script,runner_id FROM instances;'
# when a task runs — instances.schedule is the legacy column and is empty for anything
# created after the split, so reading it will mislead you
sqlite3 local/data/arcatum.db 'SELECT id,instance_id,name,frequency,time,enabled FROM schedules;'
sqlite3 local/data/arcatum.db 'SELECT id,instance_id,schedule_id,status FROM runs ORDER BY id DESC LIMIT 10;'
sqlite3 local/data/arcatum.db 'SELECT id,status,last_seen,cert_not_after FROM runners;'
```

You will see secret values as `enc:v1:…` — that is correct; only the names are readable.

**The output of a run** is on disk, independently of the API:
`backup_dir/runs/<run_id>/{stdout,stderr}.log`. The incremental read that the live tail in the
web UI uses:

```sh
curl "http://127.0.0.1:8443/api/v1/runs/run-1/tail?offset=0&stream=stdout"
just run-tail 1                     # the same, the ID is filled in for you
```

**The runner's side.** In local development, `go run ./cmd/runner … -once` and read the
terminal. On a host:

```sh
journalctl -u arcatum-runner -f
```

Typical errors and what they mean:

| What you see | Where it happens | Cause |
|---|---|---|
| `checkin denied: certificate … has role …` | server, `auth.go` | the certificate is not a `runner` one (e.g. an admin cert) |
| 403 at checkin | server | the certificate `CN` ≠ `runner_id` — the runner is impersonating another host |
| the runner polls repeatedly and does nothing | server | the runner is `pending` — approve it in the UI |
| `artifact hash mismatch` | runner, `executor.go` | the artifact contents do not match the signed SHA‑256 |
| the runner rejects a job over the signature | runner, `trust.go` | the signing key or the canonical serialization has drifted |
| `unknown script "x"` in the server log | server, `buildDispatch` | the instance points at a script the catalogue does not know |
| the server does not start at all | `LoadCatalog` | a broken manifest or a missing entrypoint under `scripts/` |

**Checking outside a run.** `GET /status` also prints the list of scripts the catalogue
loaded — the fastest check that the server sees what you think it does.

---

## 7. Development with mTLS enabled

For things like authorization, enrollment, renewal or key rotation, plain HTTP is unusable. A
local PKI:

```sh
deploy/gen-certs.sh -d local/pki -H 127.0.0.1 -a dev
just dev-certs                       # the same; other values: just dev-certs 127.0.0.1,localhost petr
```

Add `[tls]`, `[signing]` and `[secrets]` with paths into `local/pki` to `local/server.toml`
(the template is in [config/server.example.toml](../config/server.example.toml)), and
analogously `local/runner.toml` for the runner with the `runner-<hostname>` pair and
`dispatch-signing.pub`.

```sh
# a runner certificate for this machine
go run ./cmd/arcatum-ca runner -dir local/pki -id "$(hostname -s)"
just dev-runner-cert                        # the same; another host: just dev-runner-cert web-02
# any other CA command: just ca admin -dir local/pki -name colleague

# calling the API
A=(--cacert local/pki/ca.pem --cert local/pki/admin-dev.pem --key local/pki/admin-dev.key)
curl "${A[@]}" https://127.0.0.1:8443/api/v1/whoami
```

Testing enrollment: add `[bootstrap]` with `listen = "127.0.0.1:8080"`, `ca_key` and
`api_url`, then delete the runner's `cert`/`key` and start the runner — it generates a key,
sends a CSR and waits for approval (`POST /api/v1/runners/{id}/approve`).

> If you are testing **CA rotation**, stick to the order from
> [README → Key rotation](../README.md#key-rotation). Issuing the server certificate under the
> new CA too early is the one step that can lock runners out of their own system — and in a
> test it looks exactly like "TLS is broken".

---

## 8. Web UI

`web/index.html`, `app.js`, `style.css` — no build step, no framework, no dependencies. The
assets are in the binary through `embed.FS` ([web/web.go](../web/web.go)), so they cannot
drift apart from the server version.

The consequence for development: **after a change in `web/` the server must be restarted**
(with `go run` it is enough to kill it and start again, `//go:embed` reads the files at
compile time). Add a new file in `web/` to the `//go:embed` directive and to the list of
served assets in `WebHandler()` as well.

The views are section elements toggled by a `hidden` class: `dashboard` (the landing page),
`instances`, `instance-form`, `schedules`, `schedule-form`, `history` (one task's runs),
`restore`, `runners`, `rotation`, `users`, `admin`, `account` and `detail` (a run with its live
tail). Which of them refresh on the five-second timer is decided by the `loaders` map — the
forms, the account page and the run detail are deliberately absent from it, or a refresh would
overwrite a half-filled form under the operator's hands.

**Every `<td>` a render produces carries a `data-label`.** Below 620 px the tables stop being
tables: each row becomes a card and the cell's `data-label` is drawn beside its value, because
scrolling a table sideways to find out whether last night succeeded is what makes a UI useless
on a phone. A new table without `data-label` looks fine on a desktop and turns into an unlabelled
column of values on a phone, which is exactly the sort of thing nobody notices until they are
on a train.

The web UI runs on its own port (`[web] listen`) and logs in with a username and password —
the assets themselves are available without a login (it is the page that *asks* for the
login), all data goes through the API behind a session cookie. On a 401 from the API, `app.js`
shows the login screen, so an expired session does not end in empty tables. Accounts, sessions
and the middleware are in [internal/server/users.go](../internal/server/users.go) and
`users_store.go`; the password is stored as a PBKDF2 verifier from
[pkg/crypto/password.go](../pkg/crypto/password.go).

> The tests in `internal/server` lower the PBKDF2 iteration count (`TestMain` in
> `users_test.go`). Without it, creating each account would cost almost half a second.

The live tail is polling — `GET /api/v1/runs/{id}/tail?offset=N` returns only the increment.
No websockets: it survives a dropped connection and needs nothing extra on the server.

The UI is **English-only**; the documentation is the bilingual part of the project.

---

## 9. Traps that cost time

- **The scheduler is in memory.** `next_run` is recomputed per schedule from the current time
  after a restart, so a run that was due during the restart is skipped. It is not a bug —
  persisting next-run times is deliberately out of scope, but it is confusing when testing
  schedules. A disabled schedule is tracked but never due, so enabling it is a flag rather than
  a re-parse that could fail at the worst moment.
- **`instances.schedule` is not the source of truth.** It is the pre-split column, read once by
  the migration and empty for anything created since. Query the `schedules` table.
- **The script catalogue is loaded at startup only.** A change to `scripts/*.toml` without a
  restart has no effect; a broken manifest, on the other hand, takes the startup down straight
  away.
- **The instance seed does not overwrite existing ones.** Editing `instances.json` without
  `-import-force` "does not happen" — and that is how it should be, otherwise a restart would
  erase changes made from the web UI. The same goes for the `schedules` in it: adding one for an
  instance that already exists does nothing at all, deliberately, so that a schedule an operator
  deleted is not re-created on every restart. With `-import-force` the instance's schedules are
  **replaced**, not appended to.
- **Identity is determined by the certificate, not the request.** `req.RunnerID` from the body
  is discarded and overwritten with the value from `CN`. Code that trusts the body circumvents
  authorization.
- **Secrets never go into the log or an API response.** Nor into env (env is readable from
  `/proc/<pid>/environ`) — hence the temporary file in `executor.go`.
- **`ErrRestartRequired` is not an error.** It means the certificate or the trust material has
  changed; the process should exit and the service manager will start it with the new state.
- **`log_level` in the config is currently unused** — the server logs at a single level.
  Anyone waiting for `debug` to give more is waiting in vain.

---

## 10. Before you submit a change

```sh
gofmt -l . && go vet ./... && go test ./... && go build ./...
# with just: gofmt -l . && just vet && just test && just build
```

- [ ] a test for the new behaviour — mandatory for authorization, signatures and migrations
- [ ] `Validate()` extended when a new field can silently turn something off
- [ ] the new endpoint has a guard — `read`/`write` in `registerOperatorRoutes`, or
      `adminOnly`, or its own ownership check for runners — and is in the API table in the README
- [ ] no secrets in the log or in responses
- [ ] an old runner copes with protocol changes, or the deployment order is documented
- [ ] documentation **in both languages**: [README](../README.md) /
      [README_cz](../README_cz.md) for procedures, [architecture.md](architecture.md) /
      [architecture_cz.md](architecture_cz.md) for decisions and *why*

Related: [architecture](architecture.md) · [production deployment](production.md) ·
[script development](script-development.md)
