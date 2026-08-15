# Arcatum

Central backup system for servers on an internal network. Monorepo, written in Go
for both the server and the runner.

Arcatum runs backup scripts on remote servers on a schedule, collects their output and
stores it **centrally** — nothing is supposed to stay behind on the backed-up server.

---

## Contents

- [How it works](#how-it-works)
- [Key concepts](#key-concepts)
- [Repository layout](#repository-layout)
- [Quick start (trying it locally)](#quick-start-trying-it-locally)
- [Configuration](#configuration)
- [Security (mTLS and job signing)](#security-mtls-and-job-signing)
- [Key rotation](#key-rotation)
- [File backups (restic)](#file-backups-restic)
- [Web UI](#web-ui)
- [Config backup and server reset](#config-backup-and-server-reset)
- [Writing your own backup script](#writing-your-own-backup-script)
- [Adding an instance](#adding-an-instance)
- [HTTP API](#http-api)
- [Debugging scripts](#debugging-scripts)
- [Installing the runner on a backed-up server](#installing-the-runner-on-a-backed-up-server)
- [Updating runners](#updating-runners)
- [Development](#development)
- [Guides](#guides)
- [Status and roadmap](#status-and-roadmap)

---

## Guides

The README is a reference overview. Step-by-step procedures have their own documents:

| Guide | When to open it |
|---|---|
| [Production deployment](docs/production.md) | from a clean server to a running Arcatum with security enabled — PKI, systemd, publishing builds, runner rollout, operations, backing up Arcatum itself |
| [Backend development and debugging](docs/backend-development.md) | working on the Go code: local environment, the flow of data through a single run, where to add things, tests, debugging, mTLS locally |
| [Script development and debugging](docs/script-development.md) | writing backup scripts: manifest, passing parameters, the development loop, error catalogue |
| [Restoring from a dump](docs/restore.md) | how to get a dump back into MySQL or PostgreSQL or a KVM host, what is not in it, and a trial restore |

---

## How it works

Two components:

- **arcatum-server** — the central brain. Holds the schedules, script definitions, the run
  database and the storage for backed-up data. Provides the API (and later the web UI).
- **arcatum-runner** — a lightweight service on every backed-up server. Runs scripts and
  streams the result to the server.

Communication is **pull** — the runner asks for work itself:

```
 runner                                    server
   │  1. POST /api/v1/checkin               │  "I'm web-01, got anything for me?"
   │ ─────────────────────────────────────► │
   │  2. list of jobs to run                │
   │ ◄───────────────────────────────────── │
   │  3. runs the script locally            │
   │  4. streams stdout/stderr + status     │
   │ ─────────────────────────────────────► │  stores into DB + backup_dir
```

**Why pull:** backed-up servers don't have to open any inbound port (outbound connections
only), which is firewall-friendly and shrinks the attack surface.

---

## Key concepts

### Script vs. instance

The most important distinction in the whole system — a **template** and its **deployment**:

| | **Script** (definition) | **Instance** (deployment) | **Schedule** (timing) |
|---|---|---|---|
| What it is | template: code/binary + parameter manifest | a concrete deployment onto a single target | when that deployment runs |
| Where it lives | `scripts/` — versioned in git | database (SQLite) | database (SQLite) |
| Contains secrets | **no, never** | yes (encrypted in the DB) | no |
| How many | one per kind of backup | any number per script | **any number per instance** |
| Example | `mysql-backup` | `mysql-web01`, `mysql-web02`, … | `nightly` 02:30, `monthly full` day 1 |

So a single "MySQL backup" script serves any number of MySQL servers — each as a separate
instance with its own credentials and database.

An instance targets **exactly one runner**. One runner can host multiple instances.
**More databases = more instances** (that gives each one an independent status and retry).

**When** a task runs is deliberately not part of the instance. One task often wants more
than one timetable — a nightly dump *and* a full copy on the first of the month — and
pausing the night run for a week must not mean editing the backup itself. So a schedule is
a thing of its own, and an instance may have several, or none at all: an instance with no
schedule is perfectly legal and runs when somebody presses **run now**.

If two schedules of the same task come due in the same minute, the server dispatches
**one** run, not two: they describe the same work, and two processes in one repository is
not something a backup system should ever arrange for itself.

### Three levels of configuration

1. **Script definition** — `scripts/<name>/<name>.toml` (git, no secrets)
2. **Instance and its schedules** — in the DB, seeded from `data/instances.json`
3. **Host level** — `config/server.toml` and `config/runner.toml`

---

## Repository layout

```
cmd/server            arcatum-server binary
cmd/runner            arcatum-runner binary
cmd/arcatum-ca        PKI management (CA, certificates, signing key)
internal/server       HTTP API, scheduler, SQLite store, authorization, restic REST backend
internal/runner       checkin loop, executor, signature verification, restic orchestration
pkg/proto             protocol messages + canonical serialization for signing
pkg/jobspec           script manifest parser + validation
pkg/schedule          "next run" computation (daily/weekly/monthly)
pkg/config            server config (server.toml) and runner config (runner.toml)
pkg/crypto            PKI, mTLS configuration, job signatures, secret encryption
web/                  web UI embedded into the binary (embed.FS)
scripts/              script DEFINITIONS — code + manifest, no secrets
data/                 instances.example.json
config/               server.example.toml, runner.example.toml
deploy/gen-certs.sh   generates the whole PKI with a single command
justfile              shortcuts for build, tests and local runs (see Development)
docs/architecture.md  architecture and decisions
```

---

## Quick start (trying it locally)

Prerequisite: Go 1.26+. If Go is not on `PATH`:

```sh
export PATH=/usr/local/go/bin:$PATH
```

**1) Prepare the config and an instance**

```sh
cp config/server.example.toml server.toml
cp data/instances.example.json data/instances.json
# in instances.json set "runner_id" to the hostname of the machine where the runner will run:
hostname
```

The config belongs in the root of the checkout: the server looks for `./server.toml` first
and only then `/etc/arcatum/server.toml`, so running from the repository picks up this one.
It does not belong in git (it's in `.gitignore`) — only `config/server.example.toml` is
versioned.

For a local test, adjust the paths in `server.toml` so nothing touches `/opt/arcatum` and
`/central_backup`:

```toml
[server]
listen   = "127.0.0.1:8443"
scripts  = "scripts"
data_dir = "./local/data"

[web]
listen = "127.0.0.1:8080"

[storage]
backup_dir = "./local/backup"
```

**2) Start the server**

```sh
go run ./cmd/server -instances data/instances.json
```

On the first start the log prints the generated password for the `admin` account — use it to
log into the web UI at `http://127.0.0.1:8080/`.

**3) Trigger a job manually and let the runner finish it**

```sh
# in another terminal — forces a run at the next checkin
curl -X POST http://127.0.0.1:8443/api/v1/instances/hello-demo/run

# the runner checks in once, runs the job and submits the result
go run ./cmd/runner -server http://127.0.0.1:8443 -once
```

**4) Check the result**

```sh
curl http://127.0.0.1:8443/                              # plain-text status page
curl http://127.0.0.1:8443/api/v1/runs                   # list of runs
curl http://127.0.0.1:8443/api/v1/runs/run-1/output      # captured output
```

Or in the web UI at `http://127.0.0.1:8080/` — the **Dashboard** shows what is running and
what failed in the last day; a task's full history is under **Instances** → the task →
**run now** or the **history** button beside its schedule.

The runner as a service (without `-once`) checks in repeatedly according to `poll_interval`.

> **With `just`** the whole procedure is four commands — `just dev-init` (prepares `local/`
> with a config and a seed), `just server`, `just trigger` and `just runner-once`. See
> [just shortcuts](#just-shortcuts).

> This quick start runs **without security** (plain HTTP, no authentication). For a real
> deployment continue with the [Security](#security-mtls-and-job-signing) section or go
> straight to the [Production deployment](docs/production.md) guide.

---

## Configuration

### Server — `server.toml`

`./server.toml` is looked up first, then `/etc/arcatum/server.toml`; `-config` overrides
both. In production it therefore lives in `/etc/arcatum`, in development in the root of the
checkout — and a binary started outside the checkout will reach for the production config,
and with it the production PKI.

```toml
[server]
listen    = "0.0.0.0:8443"                  # API for runners (mTLS)
scripts   = "/opt/arcatum/scripts"          # directory with script definitions
data_dir  = "/central_backup/arcatum/data"  # arcatum.db is created here
timezone  = "Europe/Prague"                 # default TZ for schedules without their own
log_level = "info"

[web]
listen      = "0.0.0.0:8080"                # web UI (plain HTTP, username and password)
session_ttl = "12h"                         # how long a login survives without activity
# secure_cookie = true                      # only when an HTTPS proxy sits in front of the web

[storage]
backup_dir = "/central_backup/arcatum"      # where backed-up data is stored

[tls]
# ca_cert / cert / key — mTLS, wired up later
```

Missing fields fall back to defaults (`pkg/config.Default`). A missing **file** does not:
a server without a config exits with an error, because the built-in defaults mean plain HTTP
and instance passwords in plaintext — that is not a state you should fall into because of a
typo in a path.

**Two ports, two kinds of caller.** `[server] listen` is for runners and authenticates them
by certificate; `[web] listen` is for humans and authenticates them by password. An empty
`[web] listen` turns the web UI off. The config rejects an address collision (two listeners
on one port) at startup, including the bootstrap port — otherwise one of them would die on
"address already in use" and it would not be obvious which.

### Runner — `runner.toml`

```toml
[runner]
server        = "https://172.24.0.60:8443"   # where to check in
poll_interval = "30s"
data_dir      = "/var/lib/arcatum-runner"
```

> **Which address lives where:** the server's `listen` is in `server.toml`; the address the
> **runner calls** is held by `runner.toml`. The server does not know its own reachable
> address and does not need to. When installing a runner, `install.sh` fills it in from the
> URL the installer was downloaded from.

---

## Security (mTLS and job signing)

Without the `[tls]` and `[signing]` sections Arcatum runs **insecurely** — plain HTTP, the
server does not authenticate callers and the runner executes anything it receives. That is
meant **for local development only**; both components warn about it at startup.

Protection has three independent layers:

1. **mTLS** — who is on the wire. The server and the runner both hold a certificate from a
   shared Arcatum CA and authenticate each other. An unknown host does not even get through
   the TLS handshake.
2. **Job signing** — where the work comes from. The server signs every job with an Ed25519
   key and the runner **verifies the signature before running anything**. If it does not
   match, it does not execute the code and reports a failure back. The signature also covers
   the SHA‑256 of the artifact, so it is bound to a specific piece of code.
3. **Secret encryption at rest** — what sits in the database. Instance passwords are
   encrypted in `arcatum.db` (AES‑256‑GCM), so a copy of the database on its own reveals no
   credentials.

Why this is not a single layer: mTLS protects the connection, the signature protects the
*job*, encryption protects the *stored data*. If the server's TLS key leaked, the signing
key is a different file and the attacker cannot slip code to a runner; if a database backup
leaked, the master key is a different file too.

**People, however, log in with a username and password**, not a certificate — that is what
the [web UI](#web-ui) and the separate plain-HTTP `[web] listen` port are for. A browser
certificate has to be exported, imported on every computer and replaced after a year; a
password is more convenient and costs nothing for an operator who only watches backup
results: the web UI does not touch keys, and *jobs* are protected by a signature the server
produces itself. Runners stay on certificates — a machine is installed once, and the
certificate is what makes the server let it in (or not) already at the TLS handshake.

### Logging into the web UI (username and password)

Accounts live in `arcatum.db` in the `users` table and have two roles:

| Role | What it may do |
|---|---|
| `admin` | everything — trigger jobs, edit instances, approve runners, rotate keys, manage accounts |
| `viewer` | read only — runs, outputs, instances, runners, restore. No buttons that change anything |

Only a **PBKDF2-HMAC-SHA256 verifier** is stored (600,000 iterations, a separate salt per
account), never the password. So a copy of the database logs nobody in and does not reveal
what anyone chose — and guessing hashes is deliberately slow. A login is held by the
`arcatum_session` cookie (`HttpOnly`, `SameSite=Strict`); the database stores only its
SHA‑256, so not even the session table lets anyone impersonate a logged-in operator.

**The server creates the first account itself.** When there is none in the database, an
`admin` is created at startup and its generated password is printed to the log **once**:

```
  ┌─ first start: created the web account ─────────────────────
  │   user:     admin
  │   password: k4m2ftq7hn3bwzla
  │ Log in and change it (Account → change password). A forgotten
  │ password is reset with: arcatum-server -passwd admin
  └───────────────────────────────────────────────────────────
```

Further accounts are added from the web UI (the **Users** tab). If even the last admin loses
their password, the way back is from a shell on the server:

```sh
arcatum-server -passwd petr
#   → prints a newly generated password; creates the account if it does not exist
ARCATUM_PASSWORD='your own password' arcatum-server -passwd petr
#   → sets a specific password (environment variable, so it does not end up in shell history)
arcatum-server -passwd colleague -passwd-role viewer
```

What the web UI enforces on its own:

- **Changing a password, disabling or deleting an account immediately ends its sessions** —
  no waiting for the cookie to expire.
- **The last working admin cannot be deleted, disabled or demoted to viewer.** Unlocking the
  system again would only be possible from a shell, so the web UI does not let it happen.
- **Failed logins are throttled after five attempts** (1 min, doubling further for
  15 minutes) — the password check is deliberately expensive and must not be callable in a
  loop.
- **A non-existent username is rejected for just as long as a wrong password**, so response
  time does not reveal which accounts exist.
- **Requests that change something must come from Arcatum** (an `Origin` check), so a foreign
  page cannot act on the cookie of a logged-in operator.

### Secret encryption at rest

Every value is encrypted separately, so the **names** of secrets stay readable (the web UI
can show which ones are set) and the **values** do not. In the database it looks like this:

```
"secrets": {"password": "enc:v1:VzeO2eeBNYagsYJ1HiiMlle5ERZk…"}
```

The ciphertext is cryptographically bound to the **specific instance and parameter name**.
So whoever can write to the database cannot copy a password from one instance to another —
verification fails.

> **Back up the master key** off the machine it protects. Losing it makes all stored
> passwords unreadable. Swapping it, on the other hand, is noticed immediately — reads fail
> with an error rather than a silently empty password.

Turning encryption on for an existing installation breaks nothing: values stored earlier in
plaintext are still read and get encrypted at the next instance import.

### Roles in certificates

The role sits in the certificate's `OU` and the server splits access by it:

| Role | Who | What it may do |
|---|---|---|
| `runner` | a backed-up server | only `checkin` and reporting **its own** runs |
| `admin` | an operator calling the API from a shell | the rest of the API — triggering jobs, listings, reading outputs |

The admin certificate is only needed today for calling the API on the `[server] listen` port
(typically from `curl` or a script). Nobody has to import it into a browser — the web UI has
its [own login](#logging-into-the-web-ui-username-and-password).

**Identity is determined by the certificate, not by the request.** A runner identifies
itself by the `CN` of its certificate, which must match the `runner_id` in the instances. If
a runner with a valid certificate tries to impersonate another host, the server rejects it
(403) — it does not stay silent.

### Generating certificates

A single command creates the whole PKI — the CA, the signing key, the server certificate, an
admin certificate and runner certificates:

```sh
deploy/gen-certs.sh -H 172.24.0.60,arcatum.xtuning.local -a petr web-01 db-01
```

A `pki/` directory is created:

| File | Where it belongs |
|---|---|
| `ca.pem` | the server **and every runner** |
| `ca.key` | **the server only** — the CA private key |
| `server.pem` / `server.key` | the server |
| `dispatch-signing.key` | **the server only** — signs jobs |
| `dispatch-signing.pub` | **every runner** — verifies jobs |
| `secrets-master.key` | **the server only** — encrypts stored secrets (**back it up!**) |
| `admin-petr.pem` / `.key` | your computer (API/web access) |
| `runner-web-01.pem` / `.key` | the runner in question |

> `-H` must contain **all** addresses the runners connect to (IP and DNS alike), otherwise
> TLS verification fails. Running the script again overwrites neither an existing CA nor the
> signing key.

Finer control is available through `arcatum-ca` (`init`, `server`, `runner`, `admin`,
`signing`, `master-key`, `sign-csr` — the last one is the basis for future enrollment, where
a runner generates its key itself and only sends a CSR):

```sh
go run ./cmd/arcatum-ca runner -dir pki -id web-02       # add a runner
go run ./cmd/arcatum-ca admin  -dir pki -name colleague  # add an operator
```

An existing CA, signing key or master key is never overwritten implicitly — the command
fails with an error instead.

### Wiring it into the configuration

```toml
# server.toml
[tls]
ca_cert = "/opt/arcatum/pki/ca.pem"
cert    = "/opt/arcatum/pki/server.pem"
key     = "/opt/arcatum/pki/server.key"

[signing]
key = "/opt/arcatum/pki/dispatch-signing.key"

[secrets]
master_key = "/opt/arcatum/pki/secrets-master.key"
```

```toml
# runner.toml (on the backed-up server)
[tls]
ca_cert = "/var/lib/arcatum-runner/pki/ca.pem"
cert    = "/var/lib/arcatum-runner/pki/runner-web-01.pem"
key     = "/var/lib/arcatum-runner/pki/runner-web-01.key"

[signing]
public_key = "/var/lib/arcatum-runner/pki/dispatch-signing.pub"
```

All three paths in `[tls]` must be given together — a half-configuration is an error the
server rejects, so it cannot silently fall back to insecure HTTP.

### Certificate lifecycle

**Automatic renewal.** A runner requests a new certificate itself when expiry approaches
(30 days ahead). It needs no approval for that — the request goes over mTLS, so it has
already proven itself with the very certificate it is replacing. Without this, all your
runners would stop working at once on the day the original certificates expire. Renewal
**replaces the key as well**.

The runner then restarts itself so it starts using the new certificate (the systemd unit has
`Restart=always`).

**Revocation on compromise.** In the web UI, click **revoke** on the runner:

1. The certificate stops being valid **everywhere** immediately — checkin, result reporting
   and access to the restic repository
2. The runner moves to the **`pending`** state
3. The runner notices at the next checkin, discards the certificate **and the key**, and
   sends a new request itself
4. You approve it — or hand it a certificate manually (`arcatum-ca runner -id <id>`)

If the **CA** is suspected of being compromised, there is a **revoke certificates of all
runners** button at the bottom of the Runners tab. It stops backups until you approve the
runners again.

> The difference between **revoke** and **reject**: revocation means "start over" and the
> runner asks for a new certificate itself. Rejection means "no" — the runner then stops
> calling, so it does not fill your queue with requests.

**Expiry warnings.** The web UI reports at the top when the validity of your admin
certificate (default **1 year** — it expires first), the server certificate, or runner
certificates is coming to an end. The date is also in a column next to every runner.

Certificates that do not renew themselves are renewed like this:

```sh
go run ./cmd/arcatum-ca admin  -dir pki -name petr           # your web access
go run ./cmd/arcatum-ca server -dir pki -hosts 172.24.0.60   # the server certificate
```

### Key rotation

All three long-lived keys can be replaced without touching individual hosts. The procedure
is the same for all of them: **a window in which both the old and the new key are valid**,
runners pick the new one up themselves, and you close the window once the server confirms
everyone has moved over. The **Keys** tab tracks the state.

| What | Who distributes it | Cutover |
|---|---|---|
| secrets master key | nobody — the server only | remove the old one from `previous_keys` |
| job signing key | the runners themselves (a signed set) | remove the old one from `previous_keys` |
| certificate authority | the runners themselves (a signed bundle) | narrow the bundle down to the new CA |

**Secrets master key** — no distribution, entirely on the server:

```sh
arcatum-ca master-key -dir pki -name secrets-master-2      # 1. new key
# 2. server.toml: master_key = the new one, previous_keys = ["…/secrets-master.key"]
# 3. restart, then in the UI "Keys" → re-encrypt (or POST /api/v1/secrets/rekey)
# 4. once pending is 0, remove previous_keys and restart
```

Re-encryption is **safe to run repeatedly** — values already on the current key are skipped,
so an interrupted run simply finishes on the next attempt.

**Job signing key** — runners take the new set themselves:

```sh
arcatum-ca signing -dir pki -name dispatch-signing-2
# server.toml: [signing] key = the new one, previous_keys = ["…/dispatch-signing.key"]
# restart → runners accept the set at the next checkin → then remove previous_keys
```

> `previous_keys` under `[signing]` are **private** keys, not public ones. The server signs
> the published set with **all** the keys it holds — otherwise a runner that only knows the
> old key would reject the new set and rotation would never get going.

**Certificate authority** — the most steps, because this is the trust anchor:

```sh
arcatum-ca init   -dir pki -name ca-new -cn "Arcatum CA 2026"   # 1. new CA
arcatum-ca bundle -dir pki -out pki/ca-bundle.pem ca.pem ca-new.pem
# 2. server.toml: [tls] ca_cert = the bundle; [bootstrap] ca_cert/ca_key = ca-new
#    CAREFUL: for now KEEP the server certificate under the old CA
# 3. restart → runners accept the bundle and move to the new CA on renewal
# 4. once GET /api/v1/rotation reports safe_to_drop_old_ca:
arcatum-ca server -dir pki -ca ca-new -hosts 172.24.0.60
arcatum-ca admin  -dir pki -ca ca-new -name petr
arcatum-ca bundle -dir pki -out pki/ca-bundle.pem ca-new.pem
```

> **Step 2 is easy to get wrong.** If you issued the server certificate under the new CA
> right away, a runner that only knows the old one **will not connect** — and therefore
> cannot download the bundle that would fix it. Rotation gets stuck. The server warns about
> it: the `warning` field in the rotation status and a message in the UI.

**What deliberately is not automatic:** closing the window. Removing a trust anchor is the
one operation that can lock you out of your own system — an unattended job that gets it wrong
at night leaves you with runners that trust neither the old nor the new CA. Routine
**certificate renewal**, on the other hand, is automatic, because its failure is safe: the
old certificate remains valid.

### Why not CRL/OCSP

They are not in place — and we did consider them. Revocation is enforced by **checking state
in the database** at every authorization point, which for a closed system is better than a
CRL: it takes effect immediately and has no cache or propagation delay.

One gap remains: if the **server's TLS key** leaked, runners would not detect it themselves.
But the impact is limited — job signing uses a different key, so an attacker cannot slip code
in to be executed, and pack files are encrypted with the repository password. And above all:
this gap is addressed by **CA rotation** above, with less machinery than a CRL
infrastructure.

### Calling the API with a certificate

```sh
curl --cacert pki/ca.pem --cert pki/admin-petr.pem --key pki/admin-petr.key \
  https://172.24.0.60:8443/api/v1/runs
```

---

## File backups (restic)

For file backups Arcatum does not invent its own format — it drives **restic**.
Deduplication, incremental snapshots, compression, encryption and integrity checking are
exactly the parts that take years to get right.

The repository, however, **lives on the server**: Arcatum itself exposes a restic REST
backend, so pack files flow from the backed-up host to the server and do not pile up on it.
Only restic's local cache stays on the host.

```
 backed-up host                           arcatum-server
   restic backup                          /restic/<instance>/
   │  pack files (mTLS)                   │
   │ ───────────────────────────────────► │ backup_dir/restic/<instance>/
```

Every instance has **its own repository** and a runner only reaches the repositories of
instances targeted at it. One backed-up server therefore cannot read or damage another's
backups.

### Prerequisites

`restic` must be installed on the backed-up server (`apt install restic`). When it is
missing, the job fails with a clear message rather than mysteriously.

### Definition and instance

A `restic`-type script has no entrypoint — the runner runs restic itself according to the
parameters. Example:
[scripts/example/files_backup.toml](scripts/example/files_backup.toml).

```toml
name    = "files-backup"
type    = "restic"
timeout = "6h"
```

The instance then determines what is backed up and for how long it is kept:

```json
{
  "id": "files-web01",
  "script": "files-backup",
  "runner_id": "web-01",
  "params": {
    "paths": "/etc,/var/www",
    "excludes": "*.tmp,/var/www/cache",
    "keep_daily": "7",
    "keep_weekly": "4",
    "keep_monthly": "6"
  },
  "secrets": { "restic_password": "long-random-password" },
  "schedules": [{ "frequency": "daily", "time": "01:30" }]
}
```

| Parameter | Meaning |
|---|---|
| `paths` | **required** — what to back up, comma-separated |
| `excludes` | restic exclude patterns, comma-separated |
| `tags` | additional snapshot tags |
| `keep_last`, `keep_daily`, `keep_weekly`, `keep_monthly`, `keep_yearly` | retention (GFS) |
| `restic_password` | secret — the repository password; if left empty it is stored as `password` |

The runner initializes the repository itself on first use. Snapshots get the tags `arcatum`
and `instance:<id>`.

### Retention

When any `keep_*` is set, `forget --prune` runs after a **successful** backup, restricted by
tag to this instance's snapshots. Two deliberate decisions: a failed backup never deletes old
snapshots, and one instance's policy cannot wipe out another's snapshots. An empty value
means "not set", not "keep nothing".

> **The repository password cannot be replaced.** Restic cannot recover it — without it the
> backups are unreadable. It is encrypted in the DB (see
> [Security](#security-mtls-and-job-signing)), but keep a copy outside Arcatum as well.
>
> The default `password` is just a filler so an instance can be created without inventing a
> password — the repository is indeed encrypted with it, but anyone who reaches `backup_dir`
> on the server can decrypt it. For data that matters, set your own.

### Restoring data

**From the web UI** — the **Restore** tab: you pick an instance and a snapshot, browse the
tree and download a single file or a whole directory as a `.tar`.

The restore runs **on the server** against the repository that is already there, and the
server decrypts the password itself. The runner is not involved — and that is intentional:
needing a restore often means the backed-up machine is unavailable, so the restore must not
depend on it.

> The server needs `restic` installed for this (`apt install restic`). Without it a restore
> returns a clear error.

Data is streamed straight from the repository to the browser (`restic dump`), so nothing is
staged on disk anywhere and a large archive starts arriving immediately.

The same over the API:

```sh
A=(--cacert pki/ca.pem --cert pki/admin-petr.pem --key pki/admin-petr.key)
I=https://172.24.0.60:8443/api/v1/instances/files-web01

curl "${A[@]}" $I/snapshots                                  # what is available
curl "${A[@]}" "$I/snapshots/latest/ls?path=/etc"             # browsing
curl "${A[@]}" "$I/snapshots/latest/download?path=/etc/nginx/nginx.conf" -o nginx.conf
curl "${A[@]}" "$I/snapshots/latest/download?path=/etc&archive=tar" -o etc.tar
```

Instead of `latest` you can use a specific snapshot ID — that is how you go back to the data
at a point in time.

**Missing:** restoring **back onto the backed-up server** (today you download the data to
yourself and copy it over by hand). For a full disaster recovery you can still use restic
directly:

```sh
cat pki/admin-petr.pem pki/admin-petr.key > /tmp/admin-combined.pem
export RESTIC_PASSWORD='long-random-password'
R="restic -r rest:https://172.24.0.60:8443/restic/files-web01/ \
     --cacert pki/ca.pem --tls-client-cert /tmp/admin-combined.pem"

$R snapshots                       # what is available
$R ls latest                       # contents of the latest snapshot
$R restore latest --target /tmp/restore
$R restore latest --target /tmp/restore --include /etc/nginx   # only a part
$R check                           # repository integrity check
```

The repository size and the number of snapshots are available from the API as well:

```sh
curl --cacert pki/ca.pem --cert pki/admin-petr.pem --key pki/admin-petr.key \
  https://172.24.0.60:8443/api/v1/instances/files-web01/repo
```

---

## Web UI

The web UI has **its own port** — open `http://172.24.0.60:8080/` and log in with a username
and password (`[web] listen` in the config, see
[Logging into the web UI](#logging-into-the-web-ui-username-and-password)). It is
**embedded in the binary** (`embed.FS`), so nothing is installed separately and it cannot
drift apart from the server version.

Overviews and run detail:

| Tab | What it shows |
|---|---|
| **Dashboard** | the landing page: what is **running now**, what **failed in the last 24 hours**, and the **next runs** — plus a strip of counts (instances, schedules and how many are paused, running, failures, offline runners). Every row leads somewhere: to the failing run, or to the history of the task that is about to run |
| **Instances** | script, runner, how many **schedules** the task has (or "on demand"), next run, restic repository size, **run now**; a click opens the editor, plus a **new instance** button |
| **Schedules** | one row per schedule: which task, **when** it runs, its **state** (running / ok / failed / never / paused), its **last run** and its **next run**; **pause** or resume without losing the settings, **history** for that task, and a **new schedule** button |
| **Restore** | snapshots, tree browsing, downloading a file or directory as a `.tar` |
| **Keys** | rotation status of all three keys, secret re-encryption, CA migration progress |
| **Runners** | status, platform, **build version**, certificate expiry, when it last checked in; **approve / reject / revoke** |
| **Users** | web accounts: role, status, last login; **add / new password / change role / disable / delete** (role `admin` only) |
| **Administration** | [off-site replica](#off-site-replica), [config backup, its restore and wiping the server](#config-backup-and-server-reset) (role `admin` only) |

The logged-in user, their role, **change password** and **log out** are on the right in the
header. A viewer is not shown the buttons that change anything at all — and the server
rejects them anyway (403), so the UI matching real permissions is not a matter of trusting
the browser.

**A run is found beside the task it belongs to.** There is no flat list of everything the
server has ever done: from an instance, a schedule row or a "run now" you land in that
task's **run history**, newest first, loading more on request. That answers "how has this
backup been doing" in one place, which a single mixed list never did. The flat list still
exists over the API (`GET /api/v1/runs`) for the shell and the `/status` page.

Clicking a run opens a **detail with a live tail of the output** — for a job in progress the
log keeps filling in as it arrives. There is a `stdout`/`stderr` switch and a "follow"
checkbox (automatic scrolling). Exactly what you were after when you asked to make script
debugging easier: run it by hand and see immediately what the script prints.

The UI is **responsive**. On a tablet the forms and metadata fold into one column and the
tabs become a scrolling strip; on a phone the tables stop being tables and each row becomes
a card with its column headings beside the values — because scrolling a table sideways to
find out whether last night succeeded is exactly what makes a UI useless on the device you
happen to have with you at seven in the morning.

The live tail does not use websockets — the browser asks
`GET /api/v1/runs/{id}/tail?offset=N` and the server sends only what has been added since the
last query. Simpler, it survives a dropped connection, and it needs nothing extra on the
server.

### Access from a browser

Nothing needs installing — open `http://<server>:8080/` and log in. The web UI is plain HTTP
and therefore belongs on the internal network; anyone who wants to expose it further should
put an HTTPS reverse proxy in front of it and enable `[web] secure_cookie = true` in the
config, so the session cookie only travels over HTTPS.

The web port is deliberately different from the API port: mTLS would force the browser to
send a client certificate, and that is exactly the inconvenience password login removes.
Runners keep going to the API port with a certificate.

The plain-text overview for the shell stays at `/status` — on the web port behind a login, on
the API port with an admin certificate:

```sh
curl --cacert pki/ca.pem --cert pki/admin-petr.pem --key pki/admin-petr.key \
  https://172.24.0.60:8443/status
```

---

## Off-site replica

Otherwise Arcatum is **the last place the backups sit** — one fire, ransomware or a typo in
`rm` and there is nowhere to restore from. Replication sends everything the server stores to
a second machine (typically over WireGuard) using `rsync` over `ssh`.

It is enabled by the `[replica]` section in `server.toml`; without it nothing changes and
nothing new runs.

```toml
[replica]
enabled      = true
host         = "172.26.0.2"
user         = "arcatum"
path         = "/data"
ssh_key      = "/opt/arcatum/pki/replica-ssh.key"
known_hosts  = "/opt/arcatum/pki/replica-known_hosts"
mirror       = true      # propagate deletions too
max_delete   = 100       # safety catch: more deletions in one pass = refuse
include_keys = true      # PKI, master key and a database snapshot
```

Preparing the second machine is covered in
[docs/production.md](docs/production.md#off-site-replica).

### What flows over there

```
/data/runs/<run-id>/       database dumps and run logs
/data/restic/<instance>/   restic repositories
/data/config-backups/      configuration archives
/data/meta/arcatum.db      a consistent database snapshot (VACUUM INTO, not a file copy)
/data/meta/server.toml     the server configuration, for reference
/data/keys/                PKI, signing key, secrets master key (with include_keys)
```

The keys are what makes the difference between a **recovery point** and a pile of unreadable
files: without the master key the repository passwords cannot be decrypted and the restic
repository on the replica will not open. The price is that **the replica is just as sensitive
as this server** — whoever reaches `/data` opens every repository and can issue a certificate
to any host. The server warns loudly about this at startup. Keep `/data` in mode `0700` under
a dedicated account.

### When the link goes down

The unit of work is **a row in the queue, not an event**. An item that did not transfer keeps
its place and is retried (30 s, then doubling up to 30 min) — so a repaired link finds the
work where it left it. Nothing is thrown away because a transfer failed.

You can see it in three places:

- **The "Offsite" column on every run** — `sent` / `queued` / `sending` / `error`. A dash
  means "unknown" (replication is off or the run is older), not a fault.
- **Run detail** — besides the status, the text of the last error and the number of attempts.
- **A warning above the table** on any tab when the replica is unreachable (with the time it
  has been down **since**) or items are failing to transfer.

The **Off-site replica** card under Administration shows the target, link status, queue size
and a list of failing items, plus **sync now** and **retry failed** buttons.

### Why it cannot endanger the backups here

| | |
|---|---|
| Replication only **reads** `backup_dir` | the `rsync` source is always a local path and the destination always the replica — the opposite direction is not in the code |
| An unfinished dump is not transferred | it is queued only after `FinishRun`; `data.part` is in `--exclude` on top of that |
| At most **one transfer at a time** | with `nice`/`ionice`, an optional `--bwlimit` and a hard timeout that kills the whole process group |
| A replication failure **does not change the run status** | it is only written to its own tables; a broken link does not turn a successful backup into a failed one |
| Deletions are **counted in a dry run first** | a pass that would delete more than `max_delete` is refused before the first file disappears |
| A missing `rsync` or a bad configuration **does not hold up startup** | the subsystem stays idle, the server comes up normally |

The last row deserves an explanation: `--max-delete` on its own is not enough, because
`rsync` deletes up to the limit and only then stops — so it limits the damage but does not
prevent it. That is why every mirroring pass is preceded by a `--dry-run` that counts the
planned deletions; if the ceiling is exceeded, the pass does not start at all. An unmounted
volume or a wrong `backup_dir` therefore cannot empty the off-site copy.

A restic repository is sent **in two phases**: packs and keys first, and only then the index
and snapshots. An interrupted transfer therefore never leaves a snapshot on the replica
pointing at data that is not there yet — which is the worst possible state, because it looks
like a backup. A repository whose instance has a job running right now is postponed (not an
error, just not now).

---

## Config backup and server reset

The **Administration** tab in the web UI (role `admin` only) handles three things concerning
Arcatum itself rather than what it backs up.

### Downloading the configuration

A single zip with everything that together forms the server's settings:

```
manifest.json     format, time, host, which secrets master keys are needed, checksums
instances.json    instances including secrets — exactly as they sit in the database
schedules.json    when each instance runs, including paused ones
users.json        web accounts including password verifiers (PBKDF2), not the passwords
runners.json      the runner registry: enrollment status, certificate, fingerprint
server.toml       a copy of the config — for reference only, the import does not use it
```

The manifest's `format` is **2**. Format 1 — written before timing moved out of the instance
— is still importable: its inline schedules are lifted into schedules of their own on the way
in. The archive somebody reaches for is the one exported before the server was lost, and
refusing to read it because the layout has moved on would defeat the entire feature.

What is **not** in the archive: runs, logs, dumps, restic repositories — and above all **no
keys**. The CA key, the signing key and `secrets-master.key` do not belong in it: one such
file would unlock every repository and could issue a certificate to any host. Keys are backed
up separately together with `pki/` (see [docs/production.md](docs/production.md)).

A consequence of that decision: **secrets travel encrypted**, so the archive can only be
imported where the same master key is present. The server checks this beforehand and
otherwise refuses the import — better than finding out at three in the morning that a
repository password cannot be read. On a server without `[secrets] master_key` the secrets are
in plaintext in the database, and therefore in the archive too.

The archive nevertheless contains operator password hashes. **It is not a public file.**

### Restoring the configuration

The import **replaces the whole configuration**: `instances`, `schedules`, `users` and
`runners` are emptied and filled with the archive's contents, so whatever is not in the
archive will not be on the server after the import either. All sessions are invalidated — after the import
everyone, you included, logs in again.

Runs, logs and backup data are **not deleted**. When an instance disappears from the
configuration, its restic repository stays put in `backup_dir` — nothing deletes itself.

The procedure has two steps. An uploaded archive is first only checked and the web UI shows
**what will change**: what will be added, what will change, what will disappear, plus
warnings about consequences nobody expects — for instance that a runner the archive does not
know about will lose access and will have to enroll again. Only then do you confirm. Before
anything is written, the server **saves the current configuration itself** into
`backup_dir/config-backups/` — the way back is to import that file.

The import refuses an archive that would leave behind a state there is no way out of:

- no enabled `admin` account — nobody could get into the web UI
- an instance referring to a script that is not on the server (the server would not come up
  after a restart)
- a schedule the scheduler does not understand, or one that parses but never comes due
- a schedule belonging to an instance the archive does not contain — a row that could never
  run anything
- secrets encrypted with a key this server does not have
- a corrupted archive (the checksum does not match)

`server.toml` is **never applied**. Overwriting your `listen` by an import means locking your
own door; whoever wants to change it does so by hand, with a restart at the ready.

### Wiping the server

Deletes **all backups, dumps, logs and the entire run history** — that is `backup_dir/runs`,
`backup_dir/restic` and the cache. Keys, users, instances, their schedules and approved
runners stay, so the server is immediately operational again, just without anything
collected; run numbering starts over from `run-1`.

Stored configuration archives (`backup_dir/config-backups`) are not deleted — they are ways
back, and a reset is not the moment to throw them away.

The only action in Arcatum that deletes backups. It is confirmed by typing a word, runs only
with `confirm` in the URL, and **cannot be undone** — this data is nowhere else. As long as
any job is running, the reset is refused: deleting a directory a runner is streaming into is
not a good idea.

---

## Writing your own backup script

A script = two files in `scripts/<name>/`: the **code** and the **manifest**.

### 1) Manifest — declares the parameters

```toml
# scripts/example/mysql_backup.toml
name       = "mysql-backup"
type       = "bash"            # bash | python | binary | restic (see below)
entrypoint = "mysql_backup.sh" # relative to the manifest
timeout    = "1h"              # default, an instance may override it

[[param]]
name = "host"
type = "string"
required = true

[[param]]
name = "port"
type = "int"
default = "3306"

[[param]]
name = "password"
type = "string"
required = true
secret = true                  # the value is passed in a file, not through env
```

Declaring parameters is not a formality — the server validates instances against it and
(later) generates the form in the web UI from it.

### 2) The code — how it receives the parameters

- **Non-secret parameters** → environment variables `ARCATUM_<NAME>` (uppercase).
- **Secrets** → a temporary sourced file whose path is in `ARCATUM_SECRETS_FILE`. The runner
  deletes it once the script finishes. Secrets are deliberately kept out of env — env is
  readable from `/proc/<pid>/environ`.
- **Output on stdout** is streamed to the server. Write the data to stdout so it does not
  stay behind on the backed-up server.

```bash
#!/usr/bin/env bash
set -euo pipefail

: "${ARCATUM_HOST:?missing host}"
PORT="${ARCATUM_PORT:-3306}"

# shellcheck disable=SC1090
[ -n "${ARCATUM_SECRETS_FILE:-}" ] && source "${ARCATUM_SECRETS_FILE}"
# $ARCATUM_PASSWORD is available now

exec mysqldump --host="$ARCATUM_HOST" --port="$PORT" \
  --single-transaction --quick "$ARCATUM_DATABASE"
```

Examples: [scripts/example/](scripts/example/) — `hello` (a dependency-free demo) and
`mysql_backup` (a realistic template).

> **Binary scripts** (`type = "binary"`) work too — the runner executes the artifact
> directly. The runner reports its platform at checkin (`linux/amd64`), so the server can
> pick the right artifact. With binaries, integrity verification (SHA-256, later a signature)
> matters all the more.
>
> **The `restic` type** has no script at all — the runner drives restic itself according to
> the instance parameters. See [File backups](#file-backups-restic).

---

## Adding an instance

**From the web UI** — the **Instances** tab → **new instance**. The form is assembled from
the parameters the selected script declares, and the values are **validated against the
manifest on save**: a missing password or a typo in a parameter name shows up right away, not
during the nightly backup.

The form has no time fields: a new instance starts with **no schedule** and runs when you
press **run now**. Give it one under **Schedules** → **new schedule** — as many as the task
needs.

Changes take effect **immediately, without restarting the server**, for the instance and for
its schedules alike. Passwords are encrypted on save, so they are never left in plaintext
anywhere.

Clicking an instance row opens it for editing. A stored secret shows as `(unchanged)`; if you
leave the field empty, the old value stays.

**Copying an existing instance** — the **copy** button on a row opens the form prefilled from
it. A second database on the same server is then a matter of two fields: a new `id` and a
different database name. The server takes the passwords from the source instance — it does
not let them out even into the form, so there is nowhere to copy them from.

The same over the API:

```sh
A=(--cacert pki/ca.pem --cert pki/admin-petr.pem --key pki/admin-petr.key)
API=https://172.24.0.60:8443/api/v1

curl "${A[@]}" $API/scripts                    # what the scripts declare (the basis of the form)
curl "${A[@]}" -X POST -H 'Content-Type: application/json' $API/instances -d '{
  "id": "mysql-web01",
  "script": "mysql-backup",
  "runner_id": "web-01",
  "params":  { "host": "127.0.0.1", "port": "3306", "database": "shop", "user": "backup" },
  "secrets": { "password": "…" },
  "timeout": "2h"
}'
# when it runs is a second, separate call — and a task may have several
curl "${A[@]}" -X POST -H 'Content-Type: application/json' $API/schedules -d '{
  "instance_id": "mysql-web01", "name": "nightly",
  "frequency": "weekly", "time": "02:30",
  "weekdays": ["mon","thu"], "timezone": "Europe/Prague"
}'
curl "${A[@]}" -X PUT    $API/instances/mysql-web01 -d '…'   # edit
curl "${A[@]}" -X DELETE $API/instances/mysql-web01          # delete, schedules included

# copy: "copy_from" fills in the secrets from the source instance, so you do not need to know them
curl "${A[@]}" -X POST -H 'Content-Type: application/json' $API/instances -d '{
  "id": "mysql-web01-orders", "copy_from": "mysql-web01",
  "script": "mysql-backup", "runner_id": "web-01",
  "params":  { "host": "127.0.0.1", "port": "3306", "database": "orders", "user": "backup" },
  "secrets": { "password": "***" }
}'
```

`copy_from` applies to creation only. A secret sent as `"***"` or empty is taken from the
source, any other value overwrites it; a secret the request does not mention at all is not
carried over (the manifest default applies, or it fails validation). Everything else is
always what came in the request — so a copy can run on a different runner. **Schedules are
not copied**: a copy starts with none, and its timetable is a decision of its own.

> **Deleting an instance does not delete the backups.** The configuration is removed and its
> schedules go with it — a schedule outliving its task is a row that can never run anything.
> The restic repository stays on disk; when you really want it gone, delete it manually from
> `backup_dir/restic/<instance>/`.

### Schedules

```sh
curl "${A[@]}" $API/schedules                              # all of them, with state and next run
curl "${A[@]}" $API/instances/mysql-web01/schedules        # just this task's
curl "${A[@]}" -X PUT $API/schedules/sch-1 -d '{"enabled": false}'   # pause
curl "${A[@]}" -X DELETE $API/schedules/sch-1                        # delete
```

`frequency` is `daily` | `weekly` | `monthly`; `weekdays` applies to `weekly`, `day` (1–28)
to `monthly` — never above 28, or the schedule would skip February. `timezone` is optional;
otherwise the default from `server.toml` applies. `name` is a label, so several schedules of
one task can be told apart. A frequency the server does not recognise is **refused on save**
rather than accepted into a schedule that silently never comes due.

**Pausing** (`enabled: false`) keeps the definition and stops it coming due. Prefer it to
deleting and retyping a week later: that is how a schedule comes back subtly different from
the one that was working.

Each schedule reports a **state**: `running` (a run of that task is in flight), `ok`,
`failed`, `never` (it has not run yet) or `paused`. Runs record which schedule caused them,
so a task's history says whether it was the nightly or the monthly one — and a manual "run
now" belongs to no schedule and is never reported as one's outcome.

### The seed file `data/instances.json`

It remains as the **initial** fill: at startup only instances that do not exist yet are
created from it. Existing ones are **not overwritten**, otherwise a server restart would
revert changes made from the web UI every time. Overwriting can be forced with the
`-import-force` flag.

An entry may carry the schedules it starts with:

```json
{
  "id": "hello-demo", "script": "hello", "runner_id": "web-01",
  "params": { "name": "hello-demo" },
  "schedules": [
    { "frequency": "daily",   "time": "03:00", "timezone": "Europe/Prague" },
    { "frequency": "monthly", "time": "23:00", "day": 1 }
  ]
}
```

The older singular `"schedule": { … }` is still accepted, so a seed file sitting on an
existing server does not have to be rewritten.

> **Schedules are only applied to an instance the import actually created.** For an instance
> that already exists nothing happens — deliberately, because otherwise every restart would
> re-create a schedule an operator had deleted. With `-import-force` the instance's schedules
> are **replaced** by the seed's rather than added to them.

Careful: the file contains passwords in plaintext, which is why it is in `.gitignore`. When
you manage instances from the web UI, you can happily delete it once it has been imported.

---

## HTTP API

The API is on two ports and **the same operator endpoints are on both** — they differ only in
how the caller proves who they are:

| Port | Who goes there | How they authenticate |
|---|---|---|
| `[server] listen` (mTLS) | runners and calls from a shell | a certificate (`OU` = `runner`/`admin`) |
| `[web] listen` (plain HTTP) | the web UI and people | a session cookie after logging in with username and password |

The "role" column therefore means: **runner** = a runner certificate; **admin** = an admin
certificate, or a logged-in user with the `admin` role; **read** = the same plus the `viewer`
role. Without `[tls]` nothing is checked on the API port (development mode); a login on the
web port always applies.

| Method and path | Role | Purpose |
|---|---|---|
| `POST /api/v1/checkin` | runner | the runner checks in and receives jobs to run |
| `POST /api/v1/runs/updates` | runner | receives the ndjson progress and **log** stream |
| `POST /api/v1/runs/{id}/data` | runner | receives the **backup payload** (raw body, one request) |
| `POST /api/v1/instances/{id}/run` | admin | **manual trigger** ("run now") |
| `POST /api/v1/runs/{id}/cancel` | admin | **stops a run** — the runner picks it up within a few seconds |
| `GET /api/v1/runs/{id}/cancel` | runner | a running job asking whether it should stop |
| `GET /api/v1/dashboard` | read | the overview in one request: counts, what is running, failures in the last 24 h, the next runs |
| `GET /api/v1/stats?days=N` | read | the period view: how the last N days went (7 by default, 90 at most) — totals, one bucket per day, a per-instance breakdown and what is currently on disk |
| `GET /api/v1/instances` | read | instances with `next_run` (the earliest across their **enabled** schedules) and how many schedules each has (secrets masked) |
| `POST /api/v1/instances` | admin | creates an instance (validated against the manifest) — no timing, that is a schedule |
| `PUT /api/v1/instances/{id}` | admin | edits an instance |
| `DELETE /api/v1/instances/{id}` | admin | deletes an instance **and its schedules** (the backups stay) |
| `GET /api/v1/schedules` | read | every schedule with its state, last run and next run |
| `POST /api/v1/schedules` | admin | adds a schedule to an instance |
| `GET /api/v1/schedules/{id}` | read | one schedule |
| `PUT /api/v1/schedules/{id}` | admin | edits a schedule, or pauses it with `{"enabled": false}` |
| `DELETE /api/v1/schedules/{id}` | admin | deletes a schedule (the task and its backups stay) |
| `GET /api/v1/instances/{id}/schedules` | read | the schedules of one task |
| `GET /api/v1/instances/{id}/runs?limit=&offset=` | read | **one task's run history**, newest first, with `has_more` for the next page |
| `GET /api/v1/scripts` | read | scripts and the parameters they declare |
| `GET /api/v1/runs?limit=N` | read | the flat run history, newest first — for the shell and `/status`; the UI reads history per task |
| `GET /api/v1/runs/{id}` | read | detail of a single run |
| `GET /api/v1/runs/{id}/output?stream=stdout\|stderr` | read | captured output of a run |
| `GET /api/v1/runs/{id}/tail?offset=N&stream=` | read | the increment of the output — the basis of the live tail |
| `GET /api/v1/runs/{id}/data` | read | download the backup payload (only after a successful run) |
| `GET /api/v1/instances/{id}/dumps` | read | an instance's stored dumps — the database counterpart of snapshots |
| `GET /api/v1/runners` | read | registered runners (status, platform, `last_seen`) |
| `GET /api/v1/install` | read | the command that installs a new runner (the address is composed from the request host and the bootstrap port) |
| `GET /api/v1/whoami` | read | who you are, how you logged in, certificate expiries |
| `GET /api/v1/rotation` | read | rotation status of all three keys |
| `POST /api/v1/secrets/rekey` | admin | re-encrypts secrets with the current master key |
| `GET /api/v1/replica` | read | status of the [off-site replica](#off-site-replica): link availability, queue, failing items |
| `POST /api/v1/replica/sync` | admin | queues a full pass right away, without waiting for the sweep |
| `POST /api/v1/replica/retry` | admin | returns failing items to the queue and skips the backoff |
| `GET /api/v1/config/export` | admin | [config backup](#config-backup-and-server-reset) as a zip (no keys, no backup data) |
| `POST /api/v1/config/import` | admin | the archive in the request body; **without** `?confirm=replace-all` it only returns what it would change |
| `GET /api/v1/reset` | admin | what [wiping the server](#wiping-the-server) would delete |
| `POST /api/v1/reset?confirm=delete-all-backups` | admin | deletes all backups, dumps, logs and run history |
| `GET /api/v1/trust` | runner / admin | the signed set of signing keys and the CA bundle |
| `GET /api/v1/update` | runner / admin | the signed manifest of published runner builds |
| `GET /api/v1/update/{name}` | runner / admin | the runner binary (over mTLS only) |
| `POST /api/v1/runners/{id}/approve` | admin | approves a request and signs a certificate |
| `POST /api/v1/runners/{id}/reject` | admin | rejects a request |
| `POST /api/v1/runners/{id}/revoke` | admin | revokes the certificate, runner → `pending` |
| `POST /api/v1/runners/revoke-all` | admin | revokes the certificates of all runners |
| `POST /api/v1/renew` | runner | certificate renewal (no approval needed) |
| `GET /api/v1/instances/{id}/repo` | read | restic repository size and snapshot count |
| `GET /api/v1/instances/{id}/snapshots` | read | list of snapshots, newest first |
| `GET /api/v1/instances/{id}/snapshots/{snap}/ls?path=` | read | directory contents inside a snapshot |
| `GET /api/v1/instances/{id}/snapshots/{snap}/download?path=&archive=tar` | read | **restore** — a file or directory as a tar |
| `/restic/{instance}/…` | runner (its own) / admin | the restic REST backend for file backups |
| `GET /status` | read | plain-text status page for the shell |

Only on the **web port** (`[web] listen`) — login and accounts:

| Method and path | Role | Purpose |
|---|---|---|
| `POST /api/v1/login` | — | log in with `{username, password}`, sets the session cookie |
| `POST /api/v1/logout` | — | ends the session and invalidates the cookie |
| `POST /api/v1/password` | read | change **your own** password `{current, new}`; ends all sessions |
| `GET /api/v1/users` | admin | list of accounts (never passwords or hashes) |
| `POST /api/v1/users` | admin | a new account; without a password the server generates one and returns it once |
| `PUT /api/v1/users/{name}` | admin | role, disable/enable, new password (`generate_password`) |
| `DELETE /api/v1/users/{name}` | admin | deletes an account |
| `GET /` | — | the [web UI](#web-ui) (embedded in the binary; login is handled by the API above) |

On the **bootstrap port** (plain HTTP, see
[installing the runner](#installing-the-runner-on-a-backed-up-server)) only this runs —
available without a certificate too, because a new host does not have one:

| Method and path | Purpose |
|---|---|
| `GET /arcatum_runner/install.sh` | the installer, generated with the server address |
| `GET /arcatum_runner/arcatum-runner-<os>-<arch>` | the runner binary |
| `GET /arcatum_runner/ca.pem`, `…/dispatch-signing.pub` | public trust material |
| `POST /api/v1/enroll` | submitting a certificate request (CSR) |
| `GET /api/v1/enroll/{id}` | collecting the signed certificate |

The API **never returns** secret values (only the names, masked as `***`). Real values leave
the server only inside a job delivered to the runner they belong to.

A runner may only report progress for runs assigned to it — so one backed-up server cannot
overwrite another's results.

---

## Debugging scripts

The most convenient way is the [web UI](#web-ui): the **Instances** tab → **run now**, which drops you into that task's history — then
click the run and watch the live tail of the output. The same from a shell:

```sh
# 1) run immediately, without waiting for the schedule
curl -X POST http://127.0.0.1:8443/api/v1/instances/hello-demo/run

# 2) the runner once, with the log in the terminal
go run ./cmd/runner -server http://127.0.0.1:8443 -once

# 3) read exactly what the script printed
curl http://127.0.0.1:8443/api/v1/runs/run-1/output
curl "http://127.0.0.1:8443/api/v1/runs/run-1/output?stream=stderr"
```

With [`just`](#just-shortcuts) that is `just trigger hello-demo`, `just runner-once` and
`just run-output 1` (the recipe accepts both `run-1` and a bare number — over the API the
correct form is `run-1`, because the path to the log is built from the ID).

The output is stored in `backup_dir/runs/<run_id>/{stdout,stderr}.log`, so it can be looked at
directly on the server at any time. A dry-run mode is on the way.

**The log and the data are not the same thing.** A script with `capture = "stream"` in its
manifest (`mysql-backup`, for instance) writes the dump itself to stdout — that does not
belong in the log and does not go there. It is stored next to it as `runs/<run_id>/data.bin`
and offered for download in the web UI, whereas the log contains just one summary line and
stderr. Logs are capped at 4 MiB per stream and are deleted according to
`[storage] log_retention_success` / `log_retention_failed`. Details in
[the architecture, §17](docs/architecture.md).

**Dumps are rotated, not deduplicated.** A database backup is a single artifact that is
restored as a whole, so it does not go into restic — the last N are kept (`keep_last`) plus
everything younger than D days (`keep_days`), both configured **per instance**. Zero for both
means keep everything; the new instance form prefills 7. Deletion runs right after a
successful backup and, to be safe, once an hour on top of that. See
[§19](docs/architecture.md).

The whole development loop, including running a script dry outside Arcatum and a catalogue of
error messages: [Script development and debugging](docs/script-development.md).

---
## Installing the runner on a backed-up server

On the backed-up server a single command is enough:

```sh
curl -LsSf http://172.24.0.60/arcatum_runner/install.sh | sudo sh
```

> The exact wording, including this server's address, is also in the web UI: the **Runners**
> tab → **Add runner**. It is the same page where you then approve the runner.

The script downloads the binary for the given platform, `ca.pem` and the signing public key,
writes out `runner.toml`, installs a systemd service and starts it. **It derives the server
address from the URL it was downloaded from** — so you do not enter it twice. Running it
again updates the binary but leaves an existing `runner.toml` alone.

Then all that is left is to **approve the host in the web UI** (the Runners tab). Until then
the runner keeps polling and does nothing — that is fine.

```sh
systemctl status arcatum-runner
journalctl -u arcatum-runner -f
```

### How a runner obtains its certificate (enrollment)

The private key **never leaves the backed-up server**:

1. On first start the runner generates its own key and sends only a **signing request** (CSR)
2. The server records it as **`pending`** — nothing trusts it yet and no work is assigned
3. You approve it in the web UI; you see the **IP address and the fingerprint of the
   request**, so you can tell it is the genuine host
4. The server signs the CSR and the runner collects its certificate
5. From that moment everything goes over mTLS

Approval is the main security safeguard. A forged request achieves nothing without your
click, and **for an already approved runner the server rejects a further request** (HTTP 409)
— so nobody can overwrite the certificate of a running host. Rejecting a runner in the web UI
cuts it off immediately, even if it still holds a valid certificate.

### What the server needs for this

Bootstrap runs on a **separate plain-HTTP port**. It cannot share the main one: the mTLS
listener requires a client certificate and a new host has none — the connection would not
even get through the handshake.

```toml
# server.toml
[bootstrap]
listen   = "0.0.0.0:80"
dist_dir = "/opt/arcatum/dist"       # arcatum-runner-linux-amd64, …
api_url  = "https://172.24.0.60:8443"           # where the runner will check in
ca_key   = "/opt/arcatum/pki/ca.key" # signs approved requests
```

The binaries for publishing are built like this:

```sh
GOOS=linux GOARCH=amd64 go build -o /opt/arcatum/dist/arcatum-runner-linux-amd64 ./cmd/runner
GOOS=linux GOARCH=arm64 go build -o /opt/arcatum/dist/arcatum-runner-linux-arm64 ./cmd/runner
```

The bootstrap port serves **only** `install.sh`, the binaries, `ca.pem`, the signing public
key and the enrollment endpoints. None of that is secret and the administrative API is not
available there.

> **Careful with `curl … | sh`:** the backed-up server runs a script downloaded from the
> network as root. Over plain HTTP anyone with access to internal network traffic can swap it
> out. For the Xtuning internal network that is a common trade-off; anyone who wants more can
> distribute `ca.pem` in advance (e.g. with a configuration tool) and download over fully
> verified HTTPS.

### Issuing a certificate manually

You do not need enrollment if you issue the certificate yourself — then it is enough to copy
the files over and the runner does not deal with enrollment at all:

```sh
go run ./cmd/arcatum-ca runner -dir pki -id web-01
```

---

## Updating runners

Runners update themselves. Publishing means copying the binaries into `dist_dir` and writing
the version next to them:

```sh
V=2026.07.26
for A in amd64 arm64; do
  GOOS=linux GOARCH=$A go build -ldflags "-X arcatum/pkg/version.Version=$V" \
    -o /opt/arcatum/dist/arcatum-runner-linux-$A ./cmd/runner
done
echo "$V" > /opt/arcatum/dist/VERSION
```

With [`just`](#just-shortcuts) the same in one command — it builds both architectures and the
`VERSION` file:

```sh
V=2026.07.26 just dist-runner /opt/arcatum/dist
```

At its next checkin the runner finds out it is running an older version, downloads the new
one, replaces itself and restarts. The current version of every host is visible in the
**Runners** tab — that is how you tell how far the rollout has got.

**Without `VERSION` nothing is offered.** Binaries in the directory do not trigger an update
on their own; the version is what says "this is released".

### Why this is safe

Replacing its own binary is the riskiest thing a runner does — a bad or forged update breaks
(or takes over) all backed-up servers at once. Therefore:

- **The manifest is signed with the job signing key.** Publishing a build therefore requires
  that key, not just control over the server — and definitely not access to the plain-HTTP
  bootstrap.
- **The binary is downloaded over mTLS**, never from the bootstrap port, and its **SHA‑256 is
  verified** against the signed manifest before anything is overwritten.
- **The new binary is written alongside and renamed** (atomically); the previous one stays as
  `.old` for diagnostics.
- **A development build is not updated** — a binary without a baked-in version reports `dev`
  and is left alone.
- **One attempt per version.** If the version does not change after a restart, the runner does
  not try again and reports it to the log — a broken build therefore cannot throw a host into
  a restart loop.

### Pinning a host

When you want a particular server on a fixed version:

```toml
# runner.toml
[runner]
auto_update = false
```

You then update it manually by running `install.sh` again.

> **After signing key rotation** the `dispatch-signing.pub` on the host is stale — the
> authority is the downloaded set in `data_dir/pki/signing-keys.pem`. If that set were lost
> (a disk reinstall, deleted by mistake), the runner cannot verify anything and refuses to
> work. The fix is to download the current key from the bootstrap:
> `curl -LsSf http://172.24.0.60/arcatum_runner/dispatch-signing.pub -o <data_dir>/pki/dispatch-signing.pub`

---

## Development

```sh
export PATH=/usr/local/go/bin:$PATH   # Go is not on PATH in this environment

go build ./...      # compile
go vet ./...        # static checks
go test ./...       # tests
```

The server runs without CGO (SQLite via `modernc.org/sqlite`), so the result is a single
static binary with no runtime dependencies.

### `just` shortcuts

There is a `justfile` in the root of the repository — [just](https://just.systems) is a task
runner, a "makefile without depending on `make`". It is **optional**: every recipe is just a
wrapper around the `go`/`curl` commands from this README, so you can do without `just`, you
will just type more.

```sh
cargo install just     # or: apt install just
just                   # lists all recipes with their descriptions
```

**Build and checks**

| Recipe | What it does |
|---|---|
| `just build` | `go build ./...` |
| `just build-all` | the server, runner and `arcatum-ca` binaries into `./bin` |
| `just release` | the same, but with the version baked in through `-ldflags` |
| `just dist-runner [dir]` | the runner for `linux/amd64` and `arm64` + a `VERSION` file (default `local/dist`) |
| `just bundle` | a production bundle: binaries, runners, `scripts/` and the installer in a single `bin/arcatum-<version>.tar.gz` |
| `just test` / `just test-race` / `just vet` | tests, tests with the race detector, `go vet` |
| `just fmt` | `gofmt -w` over the whole tree |
| `just check` | gofmt + vet + test + build — what must pass before submitting a change |
| `just clean` | deletes `bin/` and `local/dist` (does not touch data or backups) |

**Running and debugging locally**

| Recipe | What it does |
|---|---|
| `just dev-init` | creates `local/{data,backup}`, `local/server.toml` and `local/instances.json` if missing (and fills the hostname into the seed) |
| `just server` | starts the server against the `local/` config |
| `just runner-once` / `just runner` | one runner cycle, or running as a service |
| `just passwd [user]` | changes a web account password and prints it (default `admin`) |
| `just user-add <user> [role]` | creates a web account and prints its password (default role `viewer`) |
| `just trigger [instance]` | forces an instance to run (default `hello-demo`) |
| `just runs`, `just instances`, `just runners`, `just status` | overviews from the API |
| `just run-output <id> [stream]` | the captured output of a run (accepts `run-1` and `1` alike) |
| `just run-tail <id> [offset]` | the increment of the output — the same thing the live tail uses |

**PKI for local development**

| Recipe | What it does |
|---|---|
| `just dev-certs [hosts] [admin]` | the whole PKI into `local/pki` (default `127.0.0.1`, admin `dev`) |
| `just dev-runner-cert [id]` | a runner certificate from `local/pki` (default: the machine hostname) |
| `just ca <args…>` | any `arcatum-ca` command, e.g. `just ca admin -dir local/pki -name colleague` |

Behaviour is tuned with environment variables, not by editing the file:

```sh
GO=/usr/local/go/bin/go just build          # Go outside PATH
V=2026.07.26 just release                   # version baked into the binary (default: today's date)
SERVER_URL=https://127.0.0.1:8443 just runs # a different API target
SERVER_CONFIG=local/server-mtls.toml just server
LISTEN=0.0.0.0:8443 just dev-init           # reachable from another machine too (see below)
WEB_LISTEN=0.0.0.0:8080 just dev-init       # the same for the web UI
ARCATUM_PASSWORD=secretpass just passwd petr # a specific password instead of a generated one
ARCATUM_PASSWORD=secretpass just user-add colleague viewer # the same when creating an account
```

> The development config listens on `127.0.0.1` only, so you cannot connect to the server
> from another machine and no trace of the attempt is left in its log. `0.0.0.0`, however,
> means plain HTTP **without authenticating the caller** — for anything more than an
> experiment, turn [security](#security-mtls-and-job-signing) on.

Recipes that take an argument accept it positionally: `just trigger mysql-web01`,
`just run-output 42`, `just dist-runner /opt/arcatum/dist`.

In more detail — the local environment, the flow of data through a single run, where to add
an endpoint / column / script type, tests and debugging:
[Backend development and debugging](docs/backend-development.md).

---

## Status and roadmap

**Done:**
- The pull protocol end to end: checkin → job delivery → execution → output streamed to the server
- Schedules as an entity of their own: several per task (daily/weekly/monthly), pausable, plus a manual trigger
- Persistence in SQLite (instances, schedules, runs, the runner registry) — survives a restart
- Three levels of configuration, a manifest declaring parameters
- **mTLS** between the server and the runners, identity and role from the certificate, PKI tooling
- **Job signing** (Ed25519) — the runner verifies before running, and refuses otherwise
- **Secret encryption at rest** (AES-256-GCM, bound to the instance and parameter name)
- **File backups through restic** — the repository on the server (our own restic REST
  backend), dedup and incremental snapshots, repository isolation between runners
- **Retention (GFS)** — `forget --prune` after a successful backup, restricted to its own snapshots
- **A web UI** embedded in the binary, **responsive** — a dashboard, instances, schedules, per-task run history, runners, a **live tail of the output**, "run now"
- **Web login with a username and password** on its own port, `admin`/`viewer` roles, account
  management from the web UI (PBKDF2 hashes, sessions in a cookie); runners stay on certificates
- **One-command installation** (`install.sh`) and **enrollment** — the runner generates its own
  key, sends only a CSR and waits for approval in the web UI
- **Automatic certificate renewal** before expiry (including key replacement) and **revocation
  on compromise** — the runner moves to `pending` and asks for a new one itself; warnings about
  approaching expiry in the web UI
- **Rotation of all three long-lived keys** (secrets master key, job signing key, CA) with a
  dual-validity window; runners pick up the new trust material themselves
- **Instance management from the web UI/API** — a form built from the parameter declaration,
  validation on save, changes take effect without a restart, passwords encrypted from the moment
  they are stored
- **Runner auto-update** — a signed build manifest, download over mTLS, SHA-256 verification
- Safe secret delivery (a temporary file, not env), masking in the API, SHA-256 artifact verification

- **Restore from the web UI** — browsing snapshots and downloading a file or directory, runs on
  the server (independently of the backed-up host)
- **Config backup and restore in a single file** — instances, accounts and runners as a zip,
  import with a preview of the changes and an automatic backup of the previous state; keys are
  deliberately not transferred
- **Wiping the server** — deleting all backups, dumps, logs and history while keeping the
  configuration
- **[Off-site replica](#off-site-replica)** — everything the server stores flows over `rsync`
  through `ssh` to a second machine; a link outage is visible on every run and as a warning, and
  the queue catches up on its own once it is fixed

**Missing (later phases):**
- **Restore back onto the backed-up server** (today you download the data to yourself and copy it over)
- **Notifications** on failure (e-mail/Slack) and a **dry-run** mode
- **CRL/OCSP** — [deliberately not implemented](#why-not-crlocsp)

Detailed architecture and decisions: [docs/architecture.md](docs/architecture.md).
