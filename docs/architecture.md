# Arcatum — architecture

A backup system for the Xtuning internal network. Monorepo, **Go** for both the runner and the
server.

Implementation status: phases A–J done — scaffold, protocol, SQLite, mTLS and job signing,
secret encryption, restic backups, web UI, installation and enrollment, certificate lifecycle,
restore from the web UI, key rotation, instance management from the web UI, runner
auto-update, web login with a username and password. An overview in §10, details in §11–21.

The **off-site replica** (§21) has been added: everything the server stores flows continuously
onto a second machine, so `backup_dir` has stopped being the only place the backups sit.

---

## 1. Overview

Arcatum consists of two components:

- **arcatum-server** — the central brain. Holds the scheduler (the timer), the job definitions
  (scripts + config), the database of runs and results, the storage for backed-up data, the
  web UI and the API.
- **arcatum-runner** — a lightweight service on every backed-up server. It checks in with the
  server itself (pull), downloads signed jobs, runs them and streams the output/data back.

Installing a runner:

```sh
curl -LsSf http://172.24.0.60/arcatum_runner/install.sh | sh
```

The install script downloads a static binary, creates a systemd service, and the runner
generates its own identity (a key + a CSR); it receives a certificate once an operator
approves it — see §11.

---

## 2. Key decisions

| # | Decision | Choice | Reason |
|---|-----------|-------|-------|
| 1 | Direction of communication | **Pull** (runner → server, outbound HTTPS) | No inbound port on the backed-up servers, firewall-friendly, a smaller attack surface |
| 2 | File backup | **Orchestrating restic** | Dedup, incremental backups, encryption at rest and integrity are solved; we solve scheduling and the UI |
| 3 | Restore | **Phase 2** (after backup MVP) | A usable first version sooner. Done: browsing and downloading from the web UI, runs on the server — §13 |
| 4 | Server language | **Go** (not Python) | Code shared with the runner in a monorepo, a single static binary, an embedded web UI, concurrency |
| 5 | Authorization | **mTLS + job signing** | Mutual authentication server↔runner; a runner only runs signed jobs. Not a per-script certificate |
| 6 | Configuration | **Two levels: script definition + instance** | A script = a template (git, no secrets); an instance = a deployment onto a specific target with parameters and a schedule (DB). See §5 |
| 7 | Script types | **bash / python / binary / restic** | Binaries are accounted for too (artifact selected by the runner's arch, an emphasis on signing) |

---

## 3. The communication model (pull)

```
 runner                              server
   │  1. POST /api/v1/checkin (mTLS) │   "I'm host web-01, what have you got for me?"
   │ ───────────────────────────────► │
   │  2. a list of due jobs (signed)  │
   │ ◄─────────────────────────────── │
   │  3. runs restic / bash / py      │
   │  4. streams stdout+stderr+status │
   │ ───────────────────────────────► │   stores into the DB
   │  5. streams data (restic → REST) │
   │ ───────────────────────────────► │   stores into storage
```

- The runner poll interval is configurable (default e.g. 30 s). For an immediate "run now"
  from the web UI, a short interval or long polling is enough.
- The scheduler (deciding when a job is "due") lives **on the server**; the runner only asks.
- Step 5 is implemented as a **restic REST backend on the server** (`/restic/<instance>/`,
  `internal/server/restic.go`). The runner runs restic with the repository
  `rest:https://<server>/restic/<instance>/`, so pack files go straight to the server; only
  restic's local cache stays on the backed-up host. Every instance has **its own repository**
  and a runner only reaches the repositories of instances targeted at it — so one backed-up
  server cannot read or damage another's backups.

---

## 4. Monorepo structure

```
arcatum/
├── cmd/
│   ├── server/          # main for arcatum-server
│   ├── runner/          # main for arcatum-runner
│   └── arcatum-ca/      # PKI management (CA, certificates, signing and master key)
├── pkg/                  # SHARED code (the main benefit of the monorepo)
│   ├── proto/            # protocol messages, versioning
│   ├── crypto/           # mTLS, job signing/verification, enrollment
│   ├── jobspec/          # parsing and validating config (TOML/YAML)
│   └── schedule/         # "next run" computation (cron-like)
├── internal/
│   ├── server/           # scheduler, API, DB layer, storage
│   └── runner/           # script executor, restic wrapper, streaming
├── web/                  # web UI embedded into the binary (embed.FS)
├── scripts/              # script DEFINITIONS (code + manifest) – versioned in git, no secrets
│   └── example/
│       ├── mysql_backup.sh       # or a binary (type=binary)
│       └── mysql_backup.toml      # manifest: parameter declarations
│   # CAREFUL: instances (concrete values + secrets) are NOT here, they live in the DB / web UI
├── config/               # server.example.toml, runner.example.toml
├── data/                 # instances.example.json
├── deploy/
│   └── gen-certs.sh      # generates the whole PKI with a single command
│                          # (install.sh is generated by the server at runtime — §11)
├── docs/
│   └── architecture.md
├── justfile              # shortcuts over the go/curl commands from the documentation (just is optional)
└── go.mod
```

---

## 5. Two-level configuration: script definition vs. instance

The **template + deployment** pattern. One script (e.g. a MySQL backup) runs against several
servers; each server is a separate **instance** with its own parameters and schedule.

### 5.1 The script (definition) — `scripts/<name>/<name>.toml`

Code (bash / python / **a binary**) + a *manifest* declaring the parameters. Versioned in git,
**containing no concrete values or passwords**.

```toml
# scripts/example/mysql_backup.toml
name        = "mysql-backup"
type        = "bash"              # bash | python | binary | restic
entrypoint  = "mysql_backup.sh"   # path relative to this config
platforms   = ["linux/amd64"]     # only for type=binary (artifact selected by the runner's arch)
timeout     = "1h"                # default, an instance may override it
capture     = "stream"            # what stdout is: "log" (default) or "stream" = the backup payload (§17)

# Parameter declarations → the server generates the web form from them and validates the instance
[[param]]
name = "host";     type = "string"; required = true
[[param]]
name = "port";     type = "int";    default = 3306
[[param]]
name = "database"; type = "string"; required = true
[[param]]
name = "user";     type = "string"; required = true
[[param]]
name = "password"; type = "string"; required = true; secret = true
```

### 5.2 The instance — in the DB, managed through the web UI

A concrete binding of a script to a target. This is where the parameter values live (secrets
encrypted at rest) and **the schedule** (which belongs here, not in the definition — every
MySQL server may back up at a different time).

```jsonc
// conceptually (in the DB, not a file):
{
  "instance": "mysql-web01",
  "script":   "mysql-backup",
  "target":   "web-01",            // which runner
  "params":   { "host": "127.0.0.1", "port": 3306, "database": "shop", "user": "backup" },
  "secrets":  { "password": "<enc:age...>" },   // encrypted with the master key
  "schedule": { "frequency": "weekly", "time": "02:30", "weekdays": ["mon","thu"],
                "timezone": "Europe/Prague" },
  "run":      { "timeout": "1h", "on_failure": "notify", "capture": "stream" }
}
```

**More databases → more instances.** That gives each DB an independent schedule, status, retry
and restore granularity. Do not use one script with a list of databases.

### 5.3 Passing parameters to the script

- **Non-secret** parameters → environment variables (`ARCATUM_HOST`, `ARCATUM_PORT`…).
- **Secrets** → a short-lived config file passed by path in an argument, deleted once the
  script finishes (not env — it is visible in `/proc/<pid>/environ`). This also fits the "as
  little local data as possible" goal.

### 5.4 `target`

An instance targets **exactly one runner** (no groups). From the runner's point of view it is
**N:1** — one runner can host several instances (e.g. a MySQL backup + a file backup), but
every instance has a single target. So in the DB it is a plain `instances.runner_id` FK, no M:N
table.

### 5.5 Server configuration — `config/server.toml`

The third, separate level of configuration (alongside script definitions and instances):
host-level server settings. Missing fields fall back to defaults (`pkg/config.Default`).

```toml
[server]
listen    = "0.0.0.0:8443"
scripts   = "scripts"
data_dir  = "/central_backup/arcatum/data"   # the DB and runtime state
timezone  = "Europe/Prague"                    # default TZ for schedules without their own
log_level = "info"

[storage]
backup_dir = "/central_backup/arcatum"         # where backed-up data is stored

[tls]
# ca_cert / cert / key — mTLS, wired up later
```

### 5.6 Runner configuration — `runner.toml`

The runner has **its own** config on the backed-up host. The key field is `server` — where to
check in. **install.sh fills it in** from the URL the installer was downloaded from
(`172.24.0.60`),
so the operator types nothing by hand.

> Which address lives where: the server's **listen** is in `server.toml`; **the address the
> runner calls** is held by `runner.toml`. The server does not know its own reachable address
> and does not need to.

```toml
[runner]
server        = "https://172.24.0.60:8443"   # where to check in (arcatum-server)
poll_interval = "30s"
data_dir      = "/var/lib/arcatum-runner"

[tls]
# ca_cert / cert / key — client mTLS, later
```

---

## 6. The database (server)

**SQLite** (a single file in `data_dir/arcatum.db`), the `modernc.org/sqlite` driver — pure
Go, **without CGO**, so the binary stays static. The schema is in
`internal/server/schema.go` and is applied idempotently at every start.

Implemented tables:

- `runners` — id, hostname, os, arch, first_seen, last_seen *(the cert fingerprint and the
  pending/approved state are added with enrollment)*
- `instances` — script, runner_id, params (JSON), secrets (JSON), capture, timeout,
  schedule (JSON)
- `runs` — id (rowid → `run-<n>`), instance_id, runner_id, script, status, exit_code,
  bytes, started_at, ended_at, err, created_at
- `users` — web UI accounts: username, PBKDF2 password verifier, role (`admin`/`viewer`),
  disabled, created_at, updated_at, last_login
- `sessions` — web logins: **the SHA‑256 of the token** from the cookie (not the token
  itself), username, created_at, expires_at, last_seen, ip

**Scripts are not in the DB** — the definitions stay as files under `scripts/` (versioned in
git); the server reads them into a catalogue at startup.

**Run output is not in the DB either** — it is streamed into
`backup_dir/runs/<run_id>/{stdout,stderr}.log`. The backup payload belongs in storage, not in
a table; only metadata and a byte count are in the DB.

**The log and the data are two different things.** A script declared in the manifest as
`capture = "stream"` (e.g. `mysql-backup`) writes the dump itself to stdout. That does not
travel through the output channel — it goes in its own request into `runs/<run_id>/data.bin`
and never shows up in the log. Were it to go through the log, it would be base64 in an ndjson
stream, in chunks, with its own SQLite transaction for each of them, and the runs web page
would offer a gigabyte-sized SQL dump for viewing. More detail in §17.

Logs are capped at **4 MiB per stream and run** (an overflow is marked with a marker) and have
**retention**: `[storage] log_retention_success` / `log_retention_failed`. Data is not deleted
by retention — deleting a backup is not a default behaviour anyone should inherit without
deciding on it themselves.

Times are unix millis (0 = not set). **Secrets in `instances.secrets` are encrypted** (see
§7). Moving to Postgres remains an open option.

---

## 7. Security

Implemented in `pkg/crypto` (PKI, mTLS, signatures, secret encryption) and
`internal/server/auth.go` (authorization). Three **independent** layers:

- **mTLS** — *who is on the wire*. Every runner has a client cert from the Arcatum CA, the
  server requires `RequireAndVerifyClientCert`. That satisfies "authorization in both
  directions". The keys are ECDSA P‑256 (not Ed25519), so that browsers can handle the same
  server cert for the web UI.
- **Job signing** — *where the work comes from*. The server signs a `JobDispatch` with an
  Ed25519 key, the runner verifies it **before running anything**; on a mismatch it does not
  execute the code and reports a failure. The signature deliberately uses **a different key
  from TLS** — if the server's TLS key leaked, an attacker still cannot slip code to a runner.
- **Secret encryption at rest** — *what sits in the database*. AES‑256‑GCM, every value
  separately (secret names stay readable for the UI, values do not). A copy of `arcatum.db` on
  its own reveals no credentials. The master key is again **a different file** from both the
  TLS and the signing key.

**What the signature covers:** the canonical serialization in `pkg/proto/signing.go` — all
fields with length prefixes (so a value cannot imitate a field boundary) and maps with sorted
keys (otherwise verification would fail nondeterministically). The artifact contents are signed
**through their SHA‑256**; the runner is required to verify the received bytes against the
hash, so the signature is bound to a specific piece of code.

**Roles in the certificate (`OU`):**

| Role | Who | Permissions |
|---|---|---|
| `runner` | a backed-up server | `checkin` and reporting **its own** runs |
| `admin` | an operator calling the API from a shell | the rest of the API (trigger, listings, reading outputs) |

**People log in with a password, machines with a certificate**
(`internal/server/users.go`). The web UI has its own plain-HTTP listener (`[web] listen`) and
accounts in the `users` table; the API port stays mTLS for runners and for calls with an admin
certificate. The reason is operational: a certificate has to be exported, imported in every
browser and replaced after a year, which protects nothing for a person who only looks at
backup results — *jobs* are protected by the signature anyway and *secrets* by encryption, and
the web UI touches neither of those keys. For a runner, by contrast, the certificate is what
makes the server let it in (or not) already at the TLS handshake, and a machine is installed
once.

A PBKDF2‑HMAC‑SHA256 verifier is stored (600,000 iterations, a salt per account) from the
standard library — no new dependency, for the same reason as `modernc.org/sqlite`. Only the
SHA‑256 of a session token is in the DB, so not even a copy of `sessions` allows impersonating
a logged-in user. The web roles are `admin` (changes things) and `viewer` (reads only); the
same operator endpoints therefore have the `adminOnly` guard on the mTLS listener and
`webRead`/`webAdmin` on the web one (`registerOperatorRoutes` in `http.go`). Changing a
password, disabling or deleting an account immediately invalidates its sessions; the web UI
refuses to remove the last working admin, because the way back would lead only through a shell
(`arcatum-server -passwd`).

**Identity is determined by the certificate, not the request** — a runner is identified by its
`CN`, which must match the instance's `runner_id`. An attempt to impersonate another host ends
in a 403 (it does not fail silently). A runner also cannot report the results of a run that was
not assigned to it.

- **Enrollment** — automatic: the runner generates its own key (which **never leaves the
  host**), sends only a CSR, the server records it as `pending`, an operator approves it in the
  web UI and the runner collects the signed certificate. Detail in §11. Manual issuance through
  `arcatum-ca runner` remains as an alternative.
- **Secrets** — passwords are never in plaintext in `scripts/`; they are passed in the job and
  on the runner through a temporary file (not env, see §5.3). In the DB they are **encrypted**
  (`enc:v1:` + base64) and the ciphertext is bound through AAD to the **instance and the
  parameter name** — whoever can write to the DB cannot move a password between instances.
  Without the master key a stored value is unreadable and a read fails with an error
  (`ErrSealed`), not a silently empty password. Values stored before encryption was turned on
  are still read and get encrypted at the next import. **Losing the master key = losing the
  secrets** → back it up off the machine it protects.
- **Without `[tls]`** the server runs plain HTTP and authenticates nobody; without `[secrets]`
  it stores passwords in plaintext. It is a mode for local development; both components warn
  about it at startup. The config refuses a half-filled `[tls]`, and with mTLS on it requires
  `[signing]` and `[secrets]` too, so it cannot silently fall through into an insecure mode.
- **Certificate lifecycle** — automatic renewal and revocation, see §12.
- **Key rotation** — all three long-lived keys, see §14.
- **Web login** — a username and password, `admin`/`viewer` roles, account management from the
  web UI; see above and §16. CRL/OCSP deliberately not (§14).

---

## 8. Script debugging (the requester's priority)

- ✓ **A manual trigger** from the web UI ("run now") and over the API.
- ✓ **A live tail** of stdout/stderr in the web UI during a run.
- ✓ Keeping runs and their outputs for comparison (`backup_dir/runs/<run_id>/`).
- **Dry-run** mode — outstanding.

**How the live tail is done:** no websockets, no SSE. The browser asks
`GET /api/v1/runs/{id}/tail?offset=N` and the server returns only the increment plus a new
offset (`handleRunTail` + `Store.ReadOutputFrom`). It is simpler, it survives a dropped
connection and it needs no state on the server.

One subtlety that decides whether the last lines get lost: the run status is read **before**
the output. When the job finishes in between, the response still says "running", so the client
asks one more time. In the opposite order it could get `done=true` and miss output written
after the read.

---

## 9. Open questions / backlog

- ~~**Retention and rotation** of backups (GFS)~~ — done, the `keep_*` parameters on restic
  instances (§10, phase F).
- ~~**Restore flow**~~ — done for browsing and downloading from the web UI (§13); restoring
  **back onto the backed-up host** is missing.
- **Notifications** on failure (e-mail/Slack).
- ~~**The server's storage backend** — today a local disk (`backup_dir`); NAS/S3 not yet.~~ —
  partly solved: `backup_dir` stays a local disk, but everything from it flows onto an
  **off-site replica** (§21). NAS/S3 as primary storage still not.
- **Behaviour during unavailability** (the server is down at backup time → catch up / skip).
- **Runner auto-update**.
- **Authentication to the web/API** + an audit log.
- **Concurrency / locking** and resumability of large transfers.

---

## 10. Implementation status

### Phase A — scaffold ✓
The monorepo skeleton, config (server + runner), the manifest parser, schedule computation,
proto messages.

### Phase B — the protocol end to end (plain HTTP) ✓
The runner checks in, receives a job, runs it and **streams the output back to the server**,
where it is stored in `backup_dir/runs/<run_id>/` — it does not stay on the backed-up host.
Verified end to end.

**The HTTP API (`internal/server`)** — a complete overview in the
[README](../README.md#http-api).

**The runner (`internal/runner`):** a checkin loop according to `poll_interval`, an executor
that materializes the artifact (verifying the SHA-256), non-secret params → `ARCATUM_*` env,
secrets → a temporary sourced file (deleted after the run), stdout/stderr streamed as
`RunUpdate`.

### Phase C — persistence in SQLite ✓
The in-memory store was replaced by SQLite (`internal/server/store.go`, the schema in
`schema.go`). Instances, runs and the runner registry survive a server restart; run numbering
continues. The endpoints `GET /api/v1/instances` (with `next_run`),
`GET /api/v1/runs/{id}/output` and `GET /api/v1/runners` were added.

**Verified end to end:** a run → a server restart → data and output preserved → the next run is
`run-2`. Tests in `internal/server/store_test.go` (instance upserts, the run lifecycle, status
mapping, persistence across a reopen, secret masking).

**A security detail:** `Instance.Redacted()` masks secret values for the API and logs; the real
values leave the server only inside a `JobDispatch` to their own runner.

### Phase D — mTLS and job signing ✓
The security model from §7 is implemented: `pkg/crypto` (PKI, TLS configuration, Ed25519
signatures), `pkg/proto/signing.go` (canonical serialization), `internal/server/auth.go`
(identity and role from the certificate), signature verification in the runner before
execution. The tooling: `cmd/arcatum-ca` and `deploy/gen-certs.sh`.

**Verified end to end with real mTLS:** a signed job runs; without a certificate the connection
does not go through; a runner cert against an admin endpoint → 403; a cert from a foreign CA →
rejected at the handshake; a valid cert claiming to be another host → 403 with a clear message
in the log. Unit tests: PKI (chaining, SAN, role separation, a foreign CA), signatures (damaged
data/signature, a foreign key, an empty signature), canonical serialization (determinism,
independence from map ordering, detection of a change in every field, resistance to shifting
field boundaries), authorization (roles, an identity mismatch), verification in the runner.

### Phase E — secret encryption at rest ✓
`pkg/crypto/secretbox.go` (AES‑256‑GCM, a master key, `SealToString`/`OpenFromString` with the
`enc:v1:` marker), wired into `internal/server/store.go` (encryption on write, decryption on
read). The master key is generated by `arcatum-ca master-key` / `init` and
`deploy/gen-certs.sh`. A new config section, `[secrets] master_key`.

**Verified end to end:** there is no plaintext password in `arcatum.db` (nor in `-wal`/`-shm`)
— only `enc:v1:VzeO2ee…` is there; the API returns `***`; the runner meanwhile receives the
real value. Unit tests: round-trip, different ciphertext for the same value, damaged
ciphertext, a foreign master key, **moving ciphertext to another instance/parameter**, broken
key files, legacy plaintext, a missing key (`ErrSealed`), redaction preserved.

### Phase F — file backups through restic ✓
Server: the restic REST backend (`internal/server/restic.go`) — objects are validated against
`^[0-9a-f]{16,64}$`, pack files are sharded by their first two characters, a write goes into a
temp file and is renamed (no half packs), an existing object is immutable. It supports API v1
and v2 listing, HEAD and Range (through `http.ServeContent`).
Runner: `internal/runner/restic.go` — parameter parsing, repository initialization on first
use, `backup` with the tags `arcatum`/`instance:<id>`, retention through `forget --prune`
only **after a successful** backup and restricted by tag. For restic, the client cert and key
are combined into one file (`--tls-client-cert`), the CA through `--cacert`.
A `restic`-type manifest has no entrypoint (the runner drives restic itself).

**Verified end to end with real restic over mTLS:** the repository initialized itself, 3 files
were backed up (`.tmp` skipped according to `excludes`), the snapshot was stored, retention was
applied. **The restored data is bit-for-bit identical to the original**, including a binary
file (matching md5). The second backup transferred only 2.3 KiB instead of 48 KiB (dedup
works). A foreign runner against the repository → 403; traversal → 400.
Unit tests: path validation and traversal, the object lifecycle, immutability, v1/v2 listing,
hiding in-progress uploads, per-repository authorization, assembling the `backup`/`forget`
commands, empty retention ≠ "keep nothing", assembling the TLS file for restic, and **a test
that the router assembles without collisions** (unit tests calling handlers directly missed
that bug and only an end-to-end run revealed it).

### Phase G — the web UI ✓
The UI is in `web/` and embedded into the binary through `embed.FS` (the `arcatum/web`
package), served from `/` — originally on the API port under an admin certificate, today on
its own port behind a username-and-password login (§16). The text overview moved to `/status`.
Vanilla JS with no build step — the goal is for the server to remain a single self-contained
file.
New endpoints: `GET /api/v1/runs/{id}` and `GET /api/v1/runs/{id}/tail?offset=`.

Contents: the Runs / Instances / Runners tabs, a run detail with a **live tail**, a
stdout/stderr switch, "follow" (autoscroll) and a **run now** button (§8).

**Verified end to end:** the UI and its assets are served (HTTP 200), without a certificate the
connection does not go through, every endpoint the UI calls answers 200. The live tail was
simulated against a genuinely running job: increments without duplicates or gaps, `done=true`
only with the last line. Unit tests: `ReadOutputFrom` (offset, an empty file, the cap, an
offset past the end, separating stdout and stderr), the tail across a whole run lifecycle,
serving the assets and their Content-Type, admin protection of the UI.

**Not rendered in a browser** — there is no headless browser in this environment, so the
appearance of the UI has not been verified, only that the files are served and that the API
under them works.

### Phase H — one-command installation and enrollment ✓
See §11.

**Knowingly missing for now (later phases):** restore through the API/web UI (today directly
with restic and an admin certificate), notifications, dry-run, instance management through the
API (today a seed from JSON), key revocation and rotation, runner auto-update.

---

## 11. Runner installation and enrollment

### Why a separate plain-HTTP listener

A new host has no client certificate, and the main listener has
`RequireAndVerifyClientCert` — the connection would not get through the TLS handshake. The
bootstrap files are therefore served by **a second listener** (`[bootstrap] listen`, typically
`:80`, `internal/server/bootstrap.go`), which serves **only**:

```
/arcatum_runner/install.sh              the generated installer
/arcatum_runner/arcatum-runner-<os>-<arch>
/arcatum_runner/ca.pem
/arcatum_runner/dispatch-signing.pub
POST /api/v1/enroll        submitting a CSR
GET  /api/v1/enroll/{id}   collecting the certificate
```

None of it is secret: the CA certificate and the **public** signing key are public by their
nature, a CSR carries only a public key, and an issued certificate is useless without the
private key. The administrative API is **not** on this port (covered by a test).

### install.sh is generated at runtime

The template `internal/server/install.sh.tmpl` (embedded) is rendered per request and takes
the server address from the **request's Host header** — so a runner is configured for the
address it just downloaded from, and nothing is entered twice. `api_url` (the mTLS port) comes
from the config. Repeated installation updates the binary but leaves an existing `runner.toml`
alone.

### The enrollment flow

```
 runner (a new host)                      server
   1. generates its own key (which stays)
   2. POST /api/v1/enroll  {CSR}          → status pending, stores the IP + fingerprint
   3. GET /api/v1/enroll/{id} … pending      (the operator sees the request in the web UI)
                                          ← the operator approves → SignCSR
   4. GET /api/v1/enroll/{id} → cert      writes the cert, switches to mTLS
```

**Approval is a security safeguard**, not a formality — the endpoint has to be available
without authentication, so anyone on the network can submit a request, but without approval
they get nothing. So that a forged request can be recognized, the **IP address** and the
**fingerprint** are stored and shown in the UI.

The rules decided on:
- **Resubmitting while `pending` is allowed** — reinstalling is a common thing.
- **For an approved runner a further request is refused (409)** — otherwise anyone on the
  network could overwrite the certificate of a running host.
- **The CN in the CSR must match the `runner_id`**, otherwise the operator would be approving
  a different identity from the one that gets issued.
- **A rejected runner is refused even with a valid certificate** (checked at checkin), so a
  rejection takes effect immediately and does not wait for revocation.
- Runners with a manually issued certificate have the `approved` status **by default** (a
  migration), so an upgrade does not break existing installations.

### The database

The `status`, `csr`, `cert_pem`, `cert_fingerprint`, `enroll_ip`, `enrolled_at`,
`approved_at`, `cert_not_after`, `revoked_at`, `renewed_at` columns in `runners`. They are
added by a **migration** (`addColumns` + `migrate()` in `store.go`), because
`CREATE TABLE IF NOT EXISTS` would not modify an existing DB.

### Verified end to end
The whole flow against a running server: downloading `install.sh` (syntax verified with
`bash -n`, containing the right addresses) → the runner generated a `0600` key and sent a CSR
→ the server tracked it as `pending` with the IP and fingerprint → approval through the admin
API → the runner collected its certificate (`CN=backup-cental, OU=runner`, signed by the
Arcatum CA) → switched to mTLS → **a real restic backup ran with a signed job**. Negatively:
an attacker's request for an already approved runner → 409, the admin API on the bootstrap
port → 404.

**Not verified:** the root parts of `install.sh` (writing into `/usr/local/bin`, `/etc`,
installing the systemd unit) were not run in this environment — what is verified is the
script's syntax and the configuration it generated, which the runner really used.

**Trying it locally:** see [README — Quick start](../README.md#quick-start-trying-it-locally)
and [Security](../README.md#security-mtls-and-job-signing).

---

## 12. Certificate lifecycle

### Enforcement is at the application level, not TLS

A revoked certificate is still cryptographically valid and passes the handshake until it
expires. Refusing it is therefore **an application decision** and must happen on **all** the
paths a runner can take: `requireApprovedRunner` in `auth.go` is called from checkin, from
receiving results and from restic repository authorization.

> This was a real gap after phase H: the status was checked only at checkin, so a rejected
> runner did not get work but **did get into the repository** — it could read the backups and
> overwrite them. Fixed; covered by tests.

The semantics of the states:

| State | Meaning | What the runner does |
|---|---|---|
| `""` (no record) | a manually issued certificate that has not checked in yet | it works — the certificate **is** the authorization, the row is created by the first checkin as `approved` |
| `approved` | fine | it works |
| `pending` | waiting for approval, **or it was revoked** | discards the certificate and asks for a new one |
| `rejected` | the operator refused it | it only logs, it does not ask again |

Distinguishing `pending` from `rejected` matters: the server sends a 403 with a
**machine-readable reason** (`enroll_required` / `rejected`, in the body and in the
`X-Arcatum-Reason` header). Without it a rejected runner would fill the queue with requests
forever.

### Automatic renewal

Certificates are issued in bulk, so they would expire in bulk too — all the runners would go
dark on the same day. A runner therefore asks for a new one 30 days before expiry through
`POST /api/v1/renew` **on the mTLS listener**: it proves its identity with the very
certificate it is replacing, so operator approval has nothing to add here. Renewal generates a
**new key**, not just a new certificate.

A runner may only renew **its own** identity (the CN in the CSR must match the caller's CN),
and a revoked runner cannot renew — it has to go through enrollment.

### Switching to the new certificate

After a renewal (or after discarding a revoked certificate) the runner **exits cleanly** and
lets the service restart it (`Restart=always` in the unit). That is simpler and more reliable
than juggling TLS state at runtime — hot-swapping a TLS configuration mid-run is a source of
subtle bugs, a restart is trivially correct.

### Expiry visibility

`cert_not_after` is filled in from the certificate on the **live connection** at checkin, so it
is known for manually issued certificates too. `GET /api/v1/whoami` returns the expiry of the
caller's certificate **and** the server's; from that the UI builds a warning 30 days ahead
(7 days = a sharper one). The admin certificate has a default validity of 1 year, so it expires
first — and without a warning that would show up as a browser that simply does not connect.

### Verified end to end
Revocation → the runner got a 403 with `enroll_required`, discarded its certificate and key and
exited → after a restart it asked on its own → approval → **a new fingerprint** and the backup
ran.
Automatic renewal: a manually handed-over certificate with 10 days of validity → the runner
requested a new one (825 days) itself at startup, **the key was replaced**, the server recorded
the renewal. `whoami` reported 364 days for the admin certificate and 824 for the server.

---

## 13. Data restore

### It runs on the server, not on the runner

A restore is run **on the server** against the repository that is already there, and the server
decrypts the password from the instance (it has the master key). The runner is not involved at
all.

That is a deliberate decision, not a simplification: **needing a restore often means the
backed-up machine is unavailable.** If a restore went through the runner, it would not work at
precisely the moment you need it most.

The price is a dependency: the server has to have `restic` installed. When it is missing, the
endpoints return a clear error (`restic is not installed on the server`).

### The endpoints (all admin only)

| Path | What it does |
|---|---|
| `GET …/snapshots` | `restic snapshots --json`, reordered newest first |
| `GET …/snapshots/{snap}/ls?path=` | the contents of **one level** of the tree |
| `GET …/snapshots/{snap}/download?path=&archive=tar` | `restic dump`, streamed to the browser |

`restic dump` writes to stdout, so nothing is staged on disk — a large archive starts arriving
immediately (`copyStream` flushes continuously).

### Browsing one level at a time

`restic ls` lists a snapshot **recursively** and has no depth option. Filtering down to direct
children is therefore done by `parseResticLS`. Two things it has to handle:

- **Inferring missing directories** — when restic prints `/data/sub/deep.txt` but not
  `/data/sub`, `sub` must appear in the listing, otherwise part of the tree would be
  unreachable.
- **An older format** — restic used to mark rows with `struct_type` instead of `message_type`.

The listing is capped (`maxListEntries`) and a `truncated` flag is sent to the UI, so
truncation does not look like an empty directory.

### Security

Paths and snapshot IDs are validated (`cleanSnapshotPath`, `resticSnapshotIDPattern`) — `..` is
normalized away and an ID has to be hex. Arguments go into `exec` without a shell, so injection
through `;` or a backtick has no way in. The repository password is passed **in a file**, not
an argument, so it is not visible in the process list.

### Errors: whose fault they are

`ErrNoRepository` (a non-existent instance, a missing password, a repository that does not
exist yet) → **404** with an explanation. A restic failure → **502**.

There is one subtlety in the download: the headers are sent **only after the first bytes have
been read**. A non-existent path in a snapshot makes restic fail immediately with no output,
and if the headers went out earlier, the result would be **HTTP 200 with an empty body** —
which in a browser looks like an empty file, not an error. (This was revealed end to end and is
fixed.)

### Verified end to end
Against a real repository: listing snapshots, browsing the tree in depth, downloading a text
file, a binary file (50 kB) and a whole directory as a tar — **all bit for bit identical to the
original** (matching md5 for every file). A restore from an **older snapshot** returned the
state without a file added later, so point-in-time works. Error states: a non-existent path →
404 with a message from restic, a non-existent instance → 404, an instance without a password →
404 with an explanation.

**Not verified:** the appearance of the Restore tab in a browser — there is no headless browser
in this environment. What is verified is that all the endpoints the UI calls work.

---

## 14. Rotating the long-lived keys

Three keys live a long time and all of them can be replaced without anyone visiting the hosts:
the secrets master key, the job signing key and the CA. The procedure is the same for all of
them — **a window in which both the old and the new one are valid**, the runners pick the new
one up themselves over an authenticated channel, and the operator closes the window once the
server confirms everyone has moved over.

### What is automatic and what is not

| | Automatic? |
|---|---|
| **distributing** the new material | yes — the runners download it themselves |
| **carrying it out** (re-encryption, certificate renewal) | yes |
| **closing the window** (removing the old key) | **no, the operator** |

The criterion: **an operation whose failure is safe may run unattended.** Certificate renewal
(§12) is therefore automatic — when it fails, the old certificate remains valid. Removing a
trust anchor, by contrast, can lock the operator out of their own system, and an unattended job
that gets it wrong at night leaves runners that trust neither the old nor the new CA. The
system therefore does the legwork and reports **whether it is safe to finish**
(`safe_to_drop_old_ca`).

### The secrets master key — `pkg/crypto/keyring.go`

The keyring holds the primary key and its predecessors. Stored values carry a **key ID**:

```
enc:v2:<keyid>:<base64(nonce||ciphertext)>
```

The ID does two things: decryption reaches straight for the right key, and re-encryption can
tell what is already done — so it is **repeatable and interruptible**. The older `enc:v1:`
format (without an ID) is read by trying all the keys. `RekeySecrets` commits per instance, so
a failure halfway leaves the earlier ones done; a mixed state is readable, because the keyring
has both keys.

No distribution — it is all on the server.

### The job signing key — `pkg/crypto/signingset.go`

A runner holds a **set** of accepted keys, not one. The set is published at
`GET /api/v1/trust` and a runner accepts it only when it is **signed with a key it already
trusts**. The authority to rotate therefore rests on holding the key being replaced — not on
control of the server. If an authenticated channel were enough, taking over the server would
allow adding your own signing key and running arbitrary code, which is exactly what job signing
prevents.

**The set is signed with every key the server holds.** An end-to-end run revealed this as a bug
in the first design: signing it with the new key only means a runner on the old key will never
accept it and rotation **gets stuck**. That is why `[signing] previous_keys` are **private**
keys.

The canonical form (`SigningSetBytesToSign`) is sorted and length-prefixed, so the signature
does not depend on ordering, but no key can be added or removed unnoticed.

### The certificate authority

The trust bundle is a single PEM with several authorities (`LoadCAPool` handles it through
`AppendCertsFromPEM`). During rotation `[tls] ca_cert` = the bundle and
`[bootstrap] ca_cert/ca_key` = the new CA: **verification accepts both, issuance happens under
the new one**. Runners take over the bundle (signed, just like the key set) and move to the new
CA on renewal. `cert_issuer` is filled in from the live connection at checkin, so the server
knows who has already switched.

**The ordering is treacherous here and an end-to-end run revealed it:** the server certificate
has to stay under the **old** CA until the runners have the bundle. If the new CA issued it
right away, a runner with the old `ca.pem` would not get through the handshake — and therefore
could not download the bundle that would fix it. The server therefore warns about it itself
(`warning` in the rotation status).

### Why not CRL/OCSP

We considered them and they are **deliberately absent**. Revocation is enforced by checking
state in the DB at every authorization point (§12), which for a closed system is better than a
CRL: it takes effect immediately and has no cache or propagation delay. A CRL would only
replace that with something slower.

One gap remains — the leak of the **server's TLS key** is not detected by the runners
themselves. The impact is limited, though: job signing uses a different key, so an attacker
cannot run code, and pack files are encrypted with the repository password. And that gap is
addressed by **CA rotation** above, with less machinery than a CRL infrastructure (which Go
would not even check in TLS by itself, so it would have to be written into the runner,
including dealing with the list going stale).

### Verified end to end
All three rotations were carried out against a running system, including the cutover:
**the master key** — a window with two keys, backups keep running, 2 values re-encrypted, a
repeat is a no-op, after removing the old key everything is readable with the new one.
**The signing key** — a runner on the old key accepted a set with two keys, a job signed with
the new one went through, after the cutover the set was narrowed to one and backups keep
running.
**The CA** — a bundle with two authorities, the runner accepted it, moved to the new CA on
renewal (`CN=Arcatum CA 2026`), the status reported `safe_to_drop_old_ca`, and after narrowing
the bundle a backup ran. **Two real ordering bugs** were revealed and fixed, both described
above.

---

## 15. Instance management and runner updates

### Instances: from JSON into the database

Instances are created and changed through the API (`internal/server/instances.go`), so the seed
file has stopped being the source of truth. Three things it has to satisfy:

**Validation against the manifest.** `Manifest.ValidateParams` checks required values and
types and **rejects unknown names** — a typo like `datbase` would otherwise sit silently in the
configuration and the script would fail for a reason nobody connects with it. This is what the
parameter declarations in §5.1 were designed for; the web form is built from them as well.

**A masked secret must not be stored.** The API returns passwords as `***`. If a form were sent
back exactly as it arrived, that masking would overwrite every password. A value that arrives
as `***`, empty, or is not in the payload at all therefore **keeps what is stored**.

**The seed no longer overwrites.** `ImportInstances` with `overwrite=false` creates only what
does not exist. Previously it upserted at every start, so a server restart would revert every
change made from the web UI. The old behaviour can be forced with `-import-force`.

The scheduler is updated at runtime (`Track` again, `Untrack` on deletion), so a schedule change
takes effect immediately. Deleting an instance **does not delete the restic repository**:
throwing away the configuration must not throw away the backups.

### Auto-update: the riskiest feature in the system

A bad or forged update breaks all backed-up servers at once, and the bootstrap port is plain
HTTP. Without protections, auto-update would be the ideal way to slip code in — that is,
exactly what job signing prevents. Therefore:

| Protection | Why |
|---|---|
| The manifest **signed with the job signing key** | publishing a build requires that key, not control of the server |
| Downloading **over mTLS**, not from the bootstrap | an update must not arrive over an unauthenticated channel |
| **SHA‑256** verification before writing | the manifest pins specific bytes |
| Write alongside + `rename` | atomic; a crash halfway leaves no half binary |
| The previous build kept as `.old` | when the new one will not start, there is something to compare against |
| A `dev` build is not updated | the fleet does not overwrite a developer's binary |
| One attempt per version (`update-attempted`) | a broken build does not cause a restart loop |

The canonical form of the manifest is sorted and length-prefixed (`updateManifestBytesToSign`,
identical on the server and in the runner), so a build cannot be added, removed or swapped
unnoticed. Without a `VERSION` file nothing is offered — the binaries in the directory do not
trigger an update on their own.

The runner reports its version at checkin (`runners.version`), so the rollout progress is
visible in the UI. It can be turned off per host (`[runner] auto_update = false`).

### Verified end to end
**Instances:** created through the API → the backup ran **without a server restart** →
validation rejected a missing required parameter (400 with the parameter name) → a masked
secret was preserved when editing → the seed file did not overwrite an instance managed from
the API.
**Auto-update:** runner 1.0.0 → 2.0.0 published → it downloaded on its own, verified the hash,
replaced itself and restarted; the `.old` stayed. Negatively: **a forged binary was rejected**
(the hash did not match, the binary was left untouched) and **a manifest signed with a foreign
key was rejected**.

Along the way this revealed an operational trap that is now in the README: after rotating the
signing key, `dispatch-signing.pub` on a host is stale and the authority is the downloaded set.
Losing that set blocks the runner (fail closed, correctly) — the fix is to download the current
key from the bootstrap.

---

## 16. Web login and operator accounts

The web UI originally sat on the API port and was protected by an **admin certificate** (§7,
phase G). Operationally that did not fit: a certificate has to be exported to PKCS#12, imported
in every browser of every operator, the CA added to the trusted ones, and the whole thing
repeated after a year. For runners a certificate makes sense — a machine is installed once and
the server does not let it past the TLS handshake without one. For a person looking at backup
results, a password makes sense.

**Two listeners, two kinds of caller:**

| Listener | Config | Who | Authentication | Router |
|---|---|---|---|---|
| API | `[server] listen` | runners, calls from a shell | an mTLS certificate (`OU`) | `Server.Handler()` |
| Web | `[web] listen` | people in a browser | a session cookie after logging in | `Server.WebHandler()` |
| Bootstrap | `[bootstrap] listen` | hosts without a certificate | nothing (§11) | `Server.BootstrapHandler()` |

Operator endpoints are on **both** — registered by `registerOperatorRoutes`, which gets a guard
according to the listener (`adminOnly`, or `webRead`/`webAdmin`). One list of routes therefore
applies to both worlds and a new endpoint cannot end up protected on only one of them. Runner
endpoints (`checkin`, `runs/updates`, `renew`, `trust`, `update`, `/restic/`) stay exclusively
on the mTLS listener; the UI and `login`/`users` exclusively on the web one.

### What is stored

- **Never the password** — only a PBKDF2‑HMAC‑SHA256 verifier (600,000 iterations per OWASP, a
  random salt per account, the format `pbkdf2-sha256$<iter>$<salt>$<hash>`). The iterations are
  inside the value, so they can be raised later without logging everyone out. The KDF is from
  the standard library (`crypto/pbkdf2`), so it adds no dependency — the same reasoning as with
  `modernc.org/sqlite`.
- **Never the session token** — the `sessions` table holds its SHA‑256. A copy of the DB
  therefore does not allow impersonating a logged-in operator, only (at most) logging them out.
- The `arcatum_session` cookie is `HttpOnly`, `SameSite=Strict` and has no `Expires` (closing
  the browser discards it); `[web] secure_cookie` switches it to HTTPS-only for deployment
  behind a reverse proxy.

### Roles and boundaries

`admin` changes things, `viewer` only reads. The split is in the routes (`read`/`write`), not
in the UI — the web UI does hide the buttons from a viewer, but the server is what decides
(403). Protections worth mentioning:

| Situation | Behaviour | Why |
|---|---|---|
| Changing a password, disabling, deleting an account | its sessions disappear immediately | otherwise removing access would take effect only after 12 h |
| The last working admin | cannot be deleted, disabled or demoted (409) | the way back would lead only through a shell |
| 5+ failed logins | a 1 min delay, doubling further up to 15 min, per account | the password check is deliberately expensive; it must not be callable in a loop |
| A non-existent username | verified against a *decoy* hash, then the same error | the response time does not reveal which accounts exist |
| A request from outside Arcatum (`Origin`) | 403 for anything that changes state | a foreign page must not act on an operator's cookie |
| Your own account and deletion | 409 | logging yourself out mid-action is confusing, not useful |
| The first start with no accounts | an `admin` is created, the password goes **once** into the log | a server nobody can log into is useless |
| The last admin's password lost | `arcatum-server -passwd <user>` | the only way back, deliberately outside the web UI |

A generated password (a new account with no password given, or a reset) is returned in the API
response **once** and is stored nowhere in readable form — the UI shows it to be copied down.

### Verified end to end

Password login → cookie → `whoami` reports `auth: password` and the role; the API without a
cookie 401, with a made-up cookie 401, after logging out 401. A viewer: reads 200, every write
403, `users` 403, may change their own password. A foreign `Origin` and
`Sec-Fetch-Site: cross-site` on a write 403, the same origin goes through. Throttling: after six
wrong attempts 429 even for the right password, a second account untouched. Accounts: creation
with a generated password (which works for logging in), the listing contains neither a hash nor
a password, promotion, reset, deletion; demoting the last admin 409. Runners over the mTLS
listener untouched (`checkin` keeps working), the UI is not served on the API port.

**Not rendered in a browser** — there is no headless browser in this environment, so the login
screen and the Users tab are verified only at the level of the API and asset serving.

---

## 17. Payload vs. log

A script can write two entirely different things to stdout: **the log** (what happened) or **the
data** (the backup itself). `mysql_backup.sh` writes the database dump to stdout — which is why
the manifest declares it as `capture = "stream"`.

As long as both were sent over the same channel, that had two consequences:

1. **The runs page offered a gigabyte-sized SQL dump for viewing** instead of a log. That made
   the log unusable and `runs/<run_id>/stdout.log` grew without limit.
2. **It was slow.** 1.2 GB of dump took ~20 minutes (~1 MB/s). The path was:
   stdout → 32 KiB chunks → a `RunUpdate` with base64 in ndjson → server: `MkdirAll` + `open` +
   `write` + `close` per chunk + `UPDATE runs SET bytes = bytes + ?` per chunk. For 1.2 GB that
   is ~39,000 SQLite transactions (WAL with `synchronous = FULL`, i.e. ~39,000 fsyncs) and the
   same number of file opens.

### The split

| | log (`capture = "log"`, the default) | data (`capture = "stream"`) |
|---|---|---|
| channel | `POST /api/v1/runs/updates` (ndjson `RunUpdate`) | `POST /api/v1/runs/{id}/data` (a raw body) |
| stored in | `runs/<run_id>/{stdout,stderr}.log` | `runs/<run_id>/data.bin` |
| in the web UI | a live tail in the run detail | a "download" link, `GET …/runs/{id}/data` |
| in the DB | `runs.bytes` | `runs.data_bytes` |
| cap | 4 MiB per stream | none |
| retention | `[storage] log_retention_*` | never deleted automatically |

The runner passes `cmd.StdoutPipe()` straight through as the request body — so the dump waits
nowhere, is not chunked, does not go through JSON and is not staged on the backed-up host.

### Who decides what stdout is

**The manifest, not the instance** (`effectiveCapture` in `internal/server/catalog.go`).
Whether a script prints a dump or its progress is a property of the script, not of the target
it runs on. On top of that, the instances are older than that declaration and carry
`capture = "stream"` even for scripts that have always only printed text — respecting that
would mean sending the output of `hello` into a data file where nobody looks for it. An
instance may only **turn streaming off** (`capture = "local"`), for a script that stores the
data itself.

### An unfinished dump is not a backup

The upload lands in `data.part` and is renamed to `data.bin` only in `FinishRun`, and only when
the run finished successfully. An unsuccessful run discards its partial file. The runner also
waits for the upload to finish before reporting `finished`, so the verdict never gets ahead of
the data; when the upload fails and the script itself finished fine, the run is still marked as
failed — otherwise something that never arrived would pose as a successful backup.

### Writing the log

The log stays at `runs/<run_id>/{stdout,stderr}.log`, but it is no longer opened and closed for
every chunk: the handle stays open for as long as the run produces output (5 minutes idle
closes it, and so does `FinishRun`). The byte counter is buffered and goes into the DB every 2 s
and at the end of the run. `PRAGMA synchronous = NORMAL` (safe with WAL) removes the fsync on
every commit.

### Compatibility on upgrade

**The server first, then the runners.** An old runner against a new server sends stdout the old
way over ndjson — it works, just without the speed-up. A new runner against an old server would
get a 404 on `/runs/{id}/data` and the run would fail. Auto-update maintains the order itself.

---

## 18. Stopping a run and orphaned runs

A run leaves the `running` state only by the runner reporting it. When a runner stops existing
mid-job — killed, systemd restarted, the host rebooted — nobody reports it and the row stays as
if work were still going on. That is what a backup that "has been running since the morning"
looks like: not a slow dump, but a row that has nobody to finish it.

### Stopping (`cancel.go`)

The server cannot stop a run itself. Communication is pull only — the runner calls out, the
server never in — so there is nothing to interrupt and no process is visible. A cancellation is
therefore a flag the operator sets and the runner picks up:

```
operator → POST /api/v1/runs/{id}/cancel     sets cancel_requested
runner   → GET  /api/v1/runs/{id}/cancel     asks for the duration of the run (every 5 s)
```

The runner otherwise does not call at all during a job (jobs are run synchronously in the
checkin loop), so a separate goroutine does the asking — `watchForCancel` in
`internal/runner/loop.go`. When it sees the flag, it cancels the run's context and the process
dies.

**The whole process group is killed.** The artifact is a shell script, but the backup is done
by its child — `mysqldump`, or a whole pipeline. Killing only the interpreter is not enough:
the children keep running, holding the write end of the pipes the runner reads from, so the
read never ends, `cmd.Wait` does not return and the run hangs "stopped" — while the dump keeps
hammering a database nobody is watching any more. Hence `Setpgid` and a signal to the whole
group (`setupProcessGroup`), with `WaitDelay` as a safety net against anyone who ignores the
signal.

The status is reported as `cancelled`, not `failed`: from here a killed process is
indistinguishable from a crash, and "failed" would send someone looking for a fault that was
the intention. Only an unsuccessful ending is relabelled — a job that finished between the
request and it being noticed produced a valid backup, and throwing it away would be for
nothing.

**An unfinished payload is discarded**, just as with any other unsuccessful run (§17).

### Orphaned runs (`reaper.go`)

What makes this decidable is the timeout. The runner enforces it through
`exec.CommandContext`, so a live run cannot outlive it; a run that did outlive it is therefore
a run without a runner. `CreateRun` therefore stores `timeout_sec` on the row — the server must
not forget it after sending the dispatch.

The sweeper runs every minute (and once at startup, because a run interrupted by a *server*
restart is the most common orphan) and marks a run past `started_at + timeout + 5 min` as
`error` with an explanation. For a run that never started, the count begins at the dispatch.
Rows without `timeout_sec` (written before the column existed) get a default of one hour.

The reaper only records what has already happened: it kills nothing (there is nothing to kill)
and does not touch a run that is merely slow.

### When an upload has nowhere to go

A failed payload upload ends the run immediately. Letting the script finish would mean holding
a dump on the database for bytes that are going to be discarded anyway.

---

## 19. Retention of database backups

File backups go into restic (§3, §13). Database dumps do not — and that is a deliberate
decision, not an omission.

Restic can browse a tree, restore a single file and dedup across a filesystem. A dump is **a
single opaque artifact that is restored as a whole**: nobody looks inside it and nobody pulls a
single row out of it. What would be left is dedup between days, and it would be paid for by
the fact that at the moment a database needs restoring, a repository, its password and the
restic binary stand between the operator and the data. Against that,
`gunzip < dump.sql.gz | mysql` is bulletproof.

So **rotation instead of deduplication**: the last N are kept plus everything younger than D
days.

### Configuration

On the instance (`keep_last`, `keep_days`), not in the script parameters. With restic,
`keep_daily` and friends are parameters because the runner consumes them when running
`restic forget`; dumps are deleted by **the server** and the script knows nothing about
retention.

Both values are a **union**, not an intersection — "at least this many copies **and** at least
this old". A small count therefore cannot silently shorten the time window, nor the other way
round.

**0 for both means keep everything.** Deleting a backup must not be a default behaviour someone
inherits without asking for it; the new instance form prefills `keep_last = 7`, but the stored
value of an existing instance is left alone.

### When deletion happens

Right after a successful run (`pruneDumpsForRun`), because that is exactly when the disk grew —
waiting for the hourly sweep would mean holding a whole extra copy at precisely the most
inconvenient moment. The hourly sweep (`StartDumpRetention`) is a safety net: it catches a
policy change and instances that did not happen to run.

The **file** is deleted, not the run's row: `data_bytes` stays and `data_pruned` is set. The
history therefore still says what the run produced, and downloading a rotated-away dump returns
`410 Gone` with an explanation — not a `404`, which would look as if the run had never produced
anything.

### In the web UI

The **Repository** column shows `<size> · N dumps` instead of `—` for a streamed instance
(`storedSummary`), so an instance that has every backup it ever asked for no longer looks like
an instance with no backups. The **Restore** tab lists dumps for download for it instead of
tree browsing — there is nothing to browse.

### What this does not solve

**Compression.** It does not depend on this decision and is the single biggest saving: SQL text
shrinks roughly 5–10×, which reduces both disk and transfer time. It can be done in the script
right away (`mysqldump … | gzip -1`); a flag in the manifest, so the web UI knows too and the
file gets a `.sql.gz` extension, is not there yet.

**A long history.** `keep_last = 7` with a daily schedule means the entire history is a week —
silent data corruption or ransomware may only be noticed after a month. That is what
`keep_days` is for.

---

## 20. Config backup and server reset

Arcatum could back up other people's data but not its own settings: instances, accounts and the
runner registry lived only in `arcatum.db` and reached another machine by copying files. This
is one download and one upload — plus the opposite operation, emptying the server of everything
it has collected. Both are handled by the **Administration** tab and both are `admin only`.

The code: `internal/server/config_archive.go` (format, validation, handlers),
`config_archive_store.go` (reading and replacing tables), `reset.go`.

### A logical export, not a copy of `arcatum.db`

Copying the database file would be simpler and is wrong in three ways at once: in WAL mode it
cannot be copied consistently at runtime, it drags along the run history and live sessions that
have no business travelling, and it nails the archive to one schema version. JSON per table is
readable and diffable — for a file whose only purpose is to be restored under pressure, that is
not cosmetic.

`instances`, `users` and `runners` are carried. Runs are history, not configuration, and the
files they point at are on disk anyway. Sessions are discarded, not transferred: a session
belongs to the browser that opened it, not to the configuration it was created against.

### No keys in the archive

The CA key, the signing key and `secrets-master.key` are not in the archive. One such file
would unlock every repository and could issue a certificate to any host — that should not be
something downloaded with a click and left sitting in Downloads.

The price: secrets travel as **ciphertext**, so an import only works where the same master key
is present. So that this is not discovered a month later during a failed backup, the manifest
carries `secret_key_ids` and the import verifies in advance that the server has those keys
(`crypto.SealedKeyID`, `Keyring.Has`). If not, the whole thing is refused.

The archive still contains the PBKDF2 verifiers of operator passwords, i.e. something crackable
offline. It is not a public file and the UI says so out loud. A passphrase on the archive could
be added but was not chosen: without the keys this is sensitive, not catastrophic, and an extra
password is one more thing that can be forgotten at precisely the moment the archive is needed.

### The import replaces, and asks first

The semantics are **replace-all**: the three tables are emptied and filled from the archive, so
after the import there is exactly what was exported, with no leftovers. All in one transaction —
a half-imported configuration is the state there is no way out of through the web UI.

`POST /api/v1/config/import` **without** `?confirm=replace-all` is a dry run: it verifies the
archive, computes a diff and returns it, but writes nothing. A POST that arrives by mistake
therefore describes what it would do. The web UI shows that diff and only then lets you
confirm.

The diff compares only what is configuration: for a runner, the identity and enrollment, not
`last_seen` and the reported version. Without that, every checkin would light up "changed" for
all runners and the real differences would be lost in the noise.

Before writing, the server saves the current configuration itself into
`backup_dir/config-backups/` as a full-fledged archive — the way back from a file imported by
mistake is to import the one that was produced by it. If saving fails, the import is
**refused**: "the disk ran out of space" is the last moment you want to find out about
afterwards.

Anything that would leave behind an irreversible state is refused: an archive without an
enabled admin, an instance with a script that is not here (the server would not come up after a
restart — see `New`), a schedule the scheduler does not understand, unreadable secrets, a
checksum that does not match.

After the write, a `Reset` is performed on the scheduler — without it, removed instances would
hold their place in the schedule and new ones would never get their turn. No restart is needed;
precisely because no keys are transferred, the import is a purely database operation.

### `server.toml` travels along but is not applied

It is in the archive for reference. Applying it would mean an archive can overwrite `listen` or
the paths to the keys — that is, lock the door from the outside. Whoever wants to change the
config does so by hand, with a restart at the ready.

### Reset: the opposite operation

It deletes `runs` and, under `backup_dir`, the `runs/`, `restic/` and `restic-cache/`
directories. The keys, accounts, instances **and runners** stay — enrollment is configuration,
not collected data, and throwing it away would mean visiting every backed-up host and approving
it again.

The order is deliberate: the database first, then the files. A failure in the middle therefore
leaves orphaned directories taking up space, rather than rows pointing at files that are no
longer there — and the web UI would offer downloads that end in an error. Along with the rows,
the `sqlite_sequence` counter is deleted too, because run ids are directory names and all of
them have just been removed.

`config-backups/` is not deleted. It is a way back and a reset is not the moment to throw it
away.

If a job is running, the reset is refused (`409`): deleting a directory a runner is streaming
into would leave the run writing into nothing. The confirmation is
`?confirm=delete-all-backups` — the one action in the system that deletes backups must not be
reachable by a typo in a URL.

### Verified end to end

Export → deleting an instance and an account → a dry run (which wrote nothing) → an import with
confirmation: the instance back **with its schedule** without a restart, the account back, the
old session `401`, an archive of the previous state in `config-backups/`. A viewer gets `403`
on export, import and reset. A reset with data on disk: `runs/`, `restic/` and the cache gone,
`config-backups/` and the whole configuration untouched.

---

## 21. Off-site replica

`backup_dir` used to be the only place the backups sit — `production.md` says so openly and §9
tracked it as an open point. One copy on one machine means a fire, ransomware or a mistaken
`rm` wipes out everything at once, including `arcatum.db` and the PKI, without which the restic
repositories will not open.

Replication sends everything the server stores to a second machine over `rsync`/`ssh`.
The code: `internal/server/replica.go` (worker, sweep, probe, API), `replica_rsync.go`
(transport), `replica_store.go` (the queue and the link state).

### A queue, not an event

The unit of work is a row in `replica_queue`, not a reaction to a finished backup. The
difference is entirely in what happens when the link is down: an event is evaluated once and is
gone, a row stays. An item that did not transfer keeps its place and is retried (30 s, then
doubling up to 30 min), so **a repaired link finds the work where it left it**. Nothing is
thrown away because a transfer failed.

For the same reason the queue is in the database and not in memory: a backlog that a server
restart would zero out would disappear at exactly the moment it matters most.

Things get into the queue by three routes:

| Route | When | What it queues |
|---|---|---|
| after a run | right after a successful `FinishRun` | `run:<id>`, plus `repo:<instance>` for a restic instance |
| the sweep | every hour and at startup | everything not yet transferred, plus `tree:*` and `meta:server` |
| manually | a button in the web UI | a full pass now |

`tree:*` (`runs`, `restic`, `config-backups`) are **reconciliation passes over whole trees**.
Without them mirroring would not work: retention deletes a dump locally and no event occurs —
only a comparison of the trees carries that outwards. They are also a safety net against the
fast route not knowing about something.

Queueing is an idempotent upsert. One subtlety: `queued_at` is refreshed only for an item that
was already done. For an item that is still waiting, overwriting the time would mean "the queue
is two hours behind" could never happen, because the clock would be reset every hour.

### One transfer at a time

The worker is a single goroutine; new work wakes it through a channel, otherwise it ticks every
30 s. Parallel transfers over one tunnel speed nothing up and would only take I/O away from the
backups that are arriving right now. The process goes through `nice`/`ionice`, in its own
process group and with a hard timeout that kills the children too — the same pattern as
cancelling runs (§18).

A row left in the `syncing` state after the process crashed is returned to the queue at
startup: only the worker can set that state and there is exactly one of it, so there is nobody
to finish it.

### What makes the replica a recovery point

Besides `runs/` and `restic/`, a **database snapshot**, `server.toml` and (optionally) the keys
flow over as well. Without them the replica is a pile of files nobody will open — the
repository password sits encrypted in `arcatum.db` and only the master key decrypts it.

`arcatum.db` is **not copied as a file**. In WAL mode a copy would be a mixture of the file and
a log that does not belong to it — exactly the mistake §20 describes for the configuration
archive. Instead there is a `VACUUM INTO` into `replica-staging/`, and only the result is
rsynced.

The keys are sent **from where they live**, not through staging: a private key copied into
`backup_dir` would end up on the same volume as the repositories it unlocks, which is precisely
what the production layout separates. The list is assembled from the loaded config, not from a
fixed directory — after a rotation (§14) a file is added and a hard-coded path would silently
miss it. `--delete` is not used on the keys: during a rotation the predecessor is exactly what a
restore may need.

The price is clear and deliberate: **the replica is just as sensitive as this server.** Whoever
reaches its directory opens every repository and can issue a certificate to any host. The server
warns about it at startup just as loudly as about a missing master key.

### Mirroring and the deletion ceiling

A mirror means a deletion here propagates outwards — that is, exactly the risk the off-site
copy exists for. `--max-delete` is not enough for that: `rsync` deletes **up to the limit** and
only then stops, so it limits the damage but does not prevent it. (A test against real `rsync`
revealed that, not reasoning.)

Every mirroring pass is therefore preceded by `--dry-run --itemize-changes`, which counts the
planned deletions; if the ceiling is exceeded, the pass **does not start at all**. To `rsync`, an
unmounted volume looks indistinguishable from "the operator deleted everything", and this is the
only place those two things can be told apart — before the first file disappears. `--max-delete`
remains as a second line of defence.

### Restic in two phases

The repository is sent first without `index/` and `snapshots/`; only the second pass adds them.
A snapshot names an index, an index names packs; the opposite order leaves a snapshot on the
replica pointing at data that is not there — a repository restic opens and then cannot restore
from. That is worse than a missing backup, because it looks like a backup.

The repository of an instance that has a job running right now is postponed. It is not an error
(the attempt counter is not increased), just not now.

### A single item goes through its parent directory

`rsync` creates only the last component of the destination path. A single run's directory
addressed directly would therefore have nowhere to land until something else produced `runs/`.
Items are therefore sent as "this subtree of `runs/`" (`--include=/run-42/*** --exclude=*`),
which at the same time keeps `--delete` inside what is included.

### What protects the backups on this server

Replication never writes into `backup_dir` — the rsync source is always local, the destination
always remote, the opposite direction is not in the code. Queueing happens only after
`FinishRun` and `data.part` is in `--exclude` on top of that, so a dump that is not a backup yet
is not transferred. Failures are written **exclusively** into `replica_queue` and
`replica_state`: a broken link does not turn a successful backup into a failed one. And a
missing `rsync` or a bad configuration leaves the subsystem idle but the server comes up —
replication is the last thing that should stop a backup server from starting.

### Visibility

A run's status in the listing is derived with a `LEFT JOIN` onto the queue (`replicaStatusFor`
in `store.go`), so the "offsite" column does not rest on a second query per row. An empty value
means "unknown" (replication is off, or the run is older than it) — a dash, not a failure.

`GET /api/v1/replica` returns the health of the link including `down_since`. That is set only on
the **first** error; restarting it with every attempt would turn a six-hour outage into a
two-minute one, which is precisely the number somebody would make a decision on. The web UI
builds a warning from it that is visible on every tab.

### Verified

Tests against **real `rsync`** (the destination on this machine, the "remote" end through a
local shell instead of ssh, which is why `rshCommand` is a variable — the same indirection as
`config.systemDir`): a dump arrives bit for bit, an in-progress upload does not, a deletion is
mirrored, without `mirror` the off-site copy survives a deletion here, a pass above the ceiling
leaves the replica untouched and marks the item as failing, a repository mid-backup is postponed
without counting an attempt, the replicated database can be opened and a key arrives with mode
`0600`.
Negatively: a transfer against an unreachable destination leaves both the local backup and the
run status unchanged.
Further unit tests: assembling the arguments (the deletion ceiling, `data.part`, `partial-dir`,
ssh options, filter ordering for restic), the queue (idempotence, backoff with a ceiling,
`down_since`), a `viewer` gets 403 on `sync`/`retry` and 200 on the status.
