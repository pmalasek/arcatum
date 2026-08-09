# Guide: developing and debugging backup scripts

How to write a new backup script, how to try it out before letting it loose on production,
and how to debug it when it fails. Debugging scripts is the thing you will do most often —
"run now" and the live tail in the web UI are built around it.

- [1. What a script consists of](#1-what-a-script-consists-of)
- [2. The manifest](#2-the-manifest)
- [3. How the script receives its parameters](#3-how-the-script-receives-its-parameters)
- [4. Rules worth sticking to](#4-rules-worth-sticking-to)
- [5. The development loop](#5-the-development-loop)
- [6. Debugging a run](#6-debugging-a-run)
- [7. Error catalogue](#7-error-catalogue)
- [8. File backups (the `restic` type)](#8-file-backups-the-restic-type)
- [9. Pre-production checklist](#9-pre-production-checklist)

---

## 1. What a script consists of

Two files under `scripts/`:

```
scripts/example/mysql_backup.toml    manifest — name, type, entrypoint, parameter declarations
scripts/example/mysql_backup.sh      code
```

The server walks the `scripts` directory **recursively** and treats every `*.toml` as a
manifest. The subdirectory structure is therefore up to you; the only firm rule is that
**`name` in the manifest must be unique across the whole catalogue** and that `entrypoint` is
a path **relative to the manifest**.

Two things to remember right away so you do not waste time:

- **A new or changed manifest only takes effect after a server restart.** The catalogue is
  loaded at startup.
- **A broken manifest prevents the server from starting.** An unknown type, a missing
  `entrypoint` or a duplicate name is a fatal error — deliberately, so it shows up right away
  and not at night.

Changing the script's **code** needs no restart: the artifact is read from disk on every
dispatch (and its SHA‑256 is signed along with the job).

A script **never contains passwords or the addresses of specific servers** — it is a template.
Values belong in the instance (see
[README → Script vs. instance](../README.md#script-vs-instance)).

---

## 2. The manifest

```toml
name       = "mysql-backup"      # unique name; this is what an instance refers to
type       = "bash"              # bash | python | binary | restic
entrypoint = "mysql_backup.sh"   # relative to this file
timeout    = "1h"                # default; an instance may override it
platforms  = ["linux/amd64"]     # only for type = "binary"
capture    = "stream"            # what stdout is: "log" (default) | "stream" = the backup payload

[[param]]
name     = "host"
type     = "string"              # string | int | bool
required = true

[[param]]
name    = "port"
type    = "int"
default = "3306"

[[param]]
name     = "password"
type     = "string"
required = true
secret   = true                  # passed in a file, not through env
```

| Type | How the runner executes the artifact |
|---|---|
| `bash` | `bash <artifact>` |
| `python` | `python3 <artifact>` |
| `binary` | directly — the artifact is a binary for the runner's platform |
| `restic` | **no artifact**, no `entrypoint`; the runner drives restic itself ([§8](#8-file-backups-the-restic-type)) |

**Declaring parameters is not a formality.** The server builds the web form from it and
validates the instance **on save**, not during the nightly backup. What is checked:

- an unknown parameter name → error (a typo like `datbase` would otherwise silently do nothing)
- `required` with no value and no `default` → error
- a secret sent as an ordinary parameter (and vice versa) → error
- `type = "int"` / `"bool"` with a non-numeric / non-boolean value → error

An empty value counts as not given. The `default` from the manifest is filled into the stored
value, so the script receives it in env like any other parameter. In the web UI it is
prefilled in the field — so what gets stored is visible on screen. A value you enter always
wins; a default never overwrites what you filled in.

> This applies to instances saved through the API and the web UI. The seed file
> `instances.json` is imported outside that path and takes values as they are — write the
> default into the JSON yourself there.

---

## 3. How the script receives its parameters

**Non-secret parameters → environment variables `ARCATUM_<NAME>`.** The name is uppercased
and everything that is not `A–Z` or `0–9` becomes `_`. So `keep_daily` →
`ARCATUM_KEEP_DAILY`, `db-name` → `ARCATUM_DB_NAME`.

**Secrets → a temporary sourceable file** whose path is in `ARCATUM_SECRETS_FILE`. Its
contents are lines of `export ARCATUM_<NAME>='value'` (the value is safely quoted, so
apostrophes in a password break nothing). The runner deletes the file once the script
finishes.

```bash
#!/usr/bin/env bash
set -euo pipefail

: "${ARCATUM_HOST:?missing host}"       # required → fail hard rather than carry on
PORT="${ARCATUM_PORT:-3306}"

# shellcheck disable=SC1090
[ -n "${ARCATUM_SECRETS_FILE:-}" ] && source "$ARCATUM_SECRETS_FILE"
# $ARCATUM_PASSWORD is available now

exec mysqldump --host="$ARCATUM_HOST" --port="$PORT" \
  --single-transaction --quick "$ARCATUM_DATABASE"
```

Why secrets do not go through env: a process's env is readable from `/proc/<pid>/environ`, so
by anyone close enough on the backed-up machine. The file has mode `0600` and a short life.

**The run environment:**

| | |
|---|---|
| working directory | a temporary `run-<id>-*` under `data_dir/work`, **deleted after the run** |
| other env | inherited from the runner process (a systemd service — expect a minimal `PATH`) |
| stdout | according to `capture` in the manifest: either the log, or **the backed-up data** (see below) |
| stderr | streamed separately — diagnostics belong here, always as a log |
| `bytes` on a run | how much **log** the server received (both streams together) |
| `data_bytes` on a run | how much **backed-up data** arrived, only with `capture = "stream"` |
| timeout | from the instance, otherwise from the manifest, otherwise 1 h; once it expires the **whole process group** is killed |
| stopping | an operator may stop a run at any time; the runner asks every 5 s and then kills the whole group |
| exit code | 0 = success, anything else = the run failed |

---

## 4. Rules worth sticking to

- **Write the data to stdout — and say so in the manifest.** The whole point of the system is
  that nothing stays behind on the backed-up server; a script that stores the dump in
  `/var/backups` circumvents that model. For the server to treat stdout as the backup rather
  than the log, the manifest must have `capture = "stream"`. Without it your dump ends up in
  the log, where the 4 MiB cap truncates it and retention deletes it after a while.
- **`set -euo pipefail`** in every bash script. Without `pipefail`, `mysqldump | gzip` passes
  as a success even when the dump fails — and you have a backup containing half a database.
- **Check required inputs yourself** (`: "${ARCATUM_HOST:?}"`). The server's validation catches
  missing and mistyped values, not nonsensical ones.
- **Diagnostics on stderr, not stdout.** The backup is in stdout.
- **Never log a password** — not even truncated, not even "just this once". The output is
  stored centrally and stays there.
- **Clean up on failure** — `trap 'rm -f "$tmp"' EXIT` for anything temporary you create. The
  runner deletes the working directory, but whatever you write outside it is your problem.
- **`exec` the final command** when it is just a stream of data: it saves a process and
  preserves the exit code.
- **Idempotence and safety on interruption.** A run can end in a timeout or a runner restart at
  any moment.

---

## 5. The development loop

### a) Dry first, outside Arcatum

The fastest round is to run the script by hand with the same contract the runner gives it. No
server, no instance:

```sh
cat > /tmp/secrets.env <<'EOF'
export ARCATUM_PASSWORD='secret'
EOF
chmod 600 /tmp/secrets.env

env -i PATH=/usr/bin:/bin \
  ARCATUM_HOST=127.0.0.1 ARCATUM_PORT=3306 ARCATUM_DATABASE=shop ARCATUM_USER=backup \
  ARCATUM_SECRETS_FILE=/tmp/secrets.env \
  bash scripts/example/mysql_backup.sh > /tmp/out 2> /tmp/err
echo "exit=$?  bytes=$(stat -c%s /tmp/out)"

shred -u /tmp/secrets.env
```

`env -i PATH=…` is there on purpose: it mimics the lean environment of a systemd service and
exposes a script that relies on your interactive shell (`~/.my.cnf`, aliases, a full `PATH`).

```sh
shellcheck scripts/**/*.sh      # when available — worth it
```

### b) Then inside the system, on a local server

```sh
# manifest + code in place → the server must get through startup
go run ./cmd/server -config local/server.toml -instances local/instances.json
```

Create the instance from the web UI (**Instances → new instance** — the form is built from
your declarations, so you see immediately whether the manifest makes sense; it has no time
fields, timing lives under **Schedules**), or over the API:

```sh
curl -X POST -H 'Content-Type: application/json' http://127.0.0.1:8443/api/v1/instances -d '{
  "id": "myscript-test", "script": "my-script", "runner_id": "'"$(hostname -s)"'",
  "params": {"host": "127.0.0.1"}, "secrets": {"password": "secret"},
  "timeout": "5m"
}'
```

Do not give it a schedule at all while you are developing. An instance without one runs only
when you start it, which is precisely what you want here:

```sh
curl -X POST http://127.0.0.1:8443/api/v1/instances/myscript-test/run
go run ./cmd/runner -server http://127.0.0.1:8443 -once
```

If [`just`](../README.md#just-shortcuts) is at hand, this round is `just trigger
myscript-test && just runner-once` — and `just dev-init` prepares `local/` the first time.

The local development environment (`local/server.toml`) is described in the
[backend guide](backend-development.md#2-the-local-development-loop).

### c) Finally on the target machine

A script that depends on the environment (the version of `mysqldump`, permissions, socket
availability) has to pass **on the host where it is meant to run** — with the real runner and
through "run now" in the web UI. Step (a) on your laptop is no substitute for this.

Short test scripts for verifying that the system itself works are in
[scripts/example/](../scripts/example/): `hello` (no dependencies, demonstrates passing a
parameter and a secret) and `slow-demo` (prints a line per second — for the live tail).

---

## 6. Debugging a run

**The web route is the most convenient:** the **Instances** tab → **run now**, which opens
that task's history — then click the
run. A detail with a **live tail** opens — for a job in progress the log keeps filling in as
it arrives, with a `stdout`/`stderr` switch and a "follow" checkbox. For a script with
`capture = "stream"` the `stdout` holds only a summary ("streamed N bytes…"); the data itself
is not tailed, but after a successful run it is available for download in the detail.

The same from a shell:

```sh
curl -X POST http://127.0.0.1:8443/api/v1/instances/myscript-test/run
go run ./cmd/runner -server http://127.0.0.1:8443 -once     # the runner log in the terminal

curl http://127.0.0.1:8443/api/v1/runs                      # status, exit code, bytes, duration
curl http://127.0.0.1:8443/api/v1/runs/run-1/output         # what the script printed
curl "http://127.0.0.1:8443/api/v1/runs/run-1/output?stream=stderr"
curl "http://127.0.0.1:8443/api/v1/runs/run-1/tail?offset=0"   # incrementally, mid-run too
```

> **The run ID is `run-1`.** With a bare number the output endpoints return an empty body with
> status 200 — easily mistaken for "the script printed nothing". The
> [`just`](../README.md#just-shortcuts) recipes work around this trap: `just run-output 1` and
> `just run-output run-1` point at the same thing.

With `just` the same trio is `just trigger myscript-test`, `just runner-once`, `just runs` and
`just run-output 1 stderr`.

On the server the output is also on disk, so it can be looked at without the API:

```sh
tail -f /central_backup/arcatum/runs/run-1/stderr.log
```

What to read from the run header before diving into the logs:

| Field | Reads as |
|---|---|
| `exit_code` > 0 | the script itself failed → look in `stderr` |
| `exit_code` = -1 with `err` | the environment failed: a missing interpreter, a wrong artifact hash, a timeout |
| `data_bytes` = 0 with `capture = "stream"` | the script printed nothing to stdout — often a forgotten `exec`/redirect |
| `data_bytes` suspiciously small | the data went somewhere other than stdout (stored locally?), or the dump failed without `pipefail` |
| the dump is in the log instead of the data | the manifest has no `capture = "stream"`, so the server treats stdout as the log |
| `bytes` = 0 for a log script | the script printed nothing at all |
| `status` = `pending` | the job was assigned but the runner never called back — the problem is on the runner's side |
| `status` = `cancelled` | an operator stopped the run with the **stop** button; there is nothing to investigate |
| `err` = `runner did not report completion…` | the runner died mid-run (restart, reboot, OOM) and the server cleaned the run up |

On the backed-up host:

```sh
journalctl -u arcatum-runner -f
```

---

## 7. Error catalogue

| Message | Where | What to do |
|---|---|---|
| `manifest "x": invalid type "y"` | server startup | `type` must be `bash`, `python`, `binary` or `restic` |
| `manifest "x": entrypoint is required` | server startup | add an `entrypoint` (except for `type = "restic"`) |
| `script "x": entrypoint not found` | server startup | the path is relative **to the manifest**, not to the repository |
| `duplicate script name "x"` | server startup | two manifests have the same `name` |
| `script "x" has no parameter "y"` | saving an instance | a typo in the name, or `[[param]]` is missing from the manifest |
| `parameter "y" is a secret and must be given as a secret` | saving an instance | it belongs in `secrets`, not `params` |
| `parameter "y" is not declared as a secret` | saving an instance | the opposite mistake — add `secret = true`, or move the value to `params` |
| `parameter "y" is required` | saving an instance | an empty value counts as not given |
| `parameter "y" must be a whole number` | saving an instance | `type = "int"` with a non-numeric value |
| `unknown script "x"` | server log at checkin | the instance points at a script the catalogue does not know (a rename without a restart?) |
| `artifact hash mismatch` | runner | the contents do not match the signed SHA‑256 — do not touch the script file during a dispatch |
| `unsupported script type` | runner | an old runner does not know the new script type |
| `restic not found on this host` | runner | `apt install restic` on the backed-up host |
| `line 12: mysqldump: command not found` | run stderr | the service has a lean `PATH` — use an absolute path, or set `PATH` in the script |
| the run ends right at the timeout boundary | — | raise `timeout` on the instance (it overrides the manifest) |

---

## 8. File backups (the `restic` type)

A `restic`-type script **has no code** — the runner runs restic itself according to the
instance parameters and the repository lives on the server. The manifest is therefore short
(template: [scripts/example/files_backup.toml](../scripts/example/files_backup.toml)):

```toml
name    = "files-backup"
type    = "restic"
timeout = "6h"
```

The parameters the runner reads:

| Parameter | Meaning |
|---|---|
| `paths` | **required** — what to back up, comma-separated |
| `excludes` | restic exclude patterns, comma-separated |
| `tags` | additional snapshot tags (`arcatum` and `instance:<id>` are always added) |
| `keep_last`, `keep_daily`, `keep_weekly`, `keep_monthly`, `keep_yearly` | retention; if any of them is set, a `forget --prune` restricted to this instance's snapshots runs after a **successful** backup |
| `restic_password` | secret — the repository password; if left empty it is stored as `password` (see the default in the manifest) |

So do not write your own backup script for files. Where it pays to bypass the `restic` type
with your own `bash` script: when you need to pre-process the data (a database dump, a
consistent snapshot through LVM) — then send the dump to stdout from an ordinary script, or
prepare the files in the script and back them up with a second, `restic` instance.

> The repository password cannot be replaced — restic cannot recover it. Generate a long
> random one and keep a copy outside Arcatum as well. Debug a new file backup on a test
> instance with its own repository, not on the production one.

---

## 9. Pre-production checklist

- [ ] `set -euo pipefail` (bash), required inputs verified in the code
- [ ] `shellcheck` clean of findings that matter
- [ ] all parameters declared in the manifest, passwords with `secret = true`
- [ ] no password or secret value in the output
- [ ] data goes to stdout, diagnostics to stderr, nothing stays behind on the backed-up host
- [ ] `timeout` matches the real run time on the **largest** instance, not the test one
- [ ] it passes with `env -i` (a lean `PATH`), not just in your shell
- [ ] it passes on the **target host** with a manual trigger, `exit_code = 0` and `bytes > 0`
- [ ] failure is detectable: try it with a wrong password too and verify the run ends with a
      non-zero code
- [ ] for a file backup: **restoring a single file has been tried** from the Restore tab
- [ ] the manifest and the code are committed (the instance and passwords are not — those are
      in the DB)

Related: [README → Writing your own backup script](../README.md#writing-your-own-backup-script) ·
[backend development](backend-development.md) · [production deployment](production.md)
