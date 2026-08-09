# Bringing the server up in `/opt/arcatum`

A guide for a **test machine**: you are logged in on the computer where Arcatum is meant to
run, and a checkout of the repository is sitting here. Nothing is transferred anywhere — it is
built here and installed straight from here into `/opt/arcatum`.

Security is enabled here too (mTLS, signed jobs, encrypted secrets) — not out of strictness,
but because with it turned off this would be something other than what will run for real. The
development mode over `local/` (plain HTTP, no authentication) is a different thing and is
covered in [README → Quick start](../README.md#quick-start-trying-it-locally).

> **Deployment onto a remote machine (a bundle + `scp`) is not here yet** — once we have this
> debugged, a second chapter will be added. For now: `/opt/arcatum` and the checkout are on the
> same machine.

- [Prerequisites](#prerequisites)
- [The whole procedure](#the-whole-procedure)
- [1. Build the binaries](#1-build-the-binaries)
- [2. Install into `/opt/arcatum`](#2-install-into-optarcatum)
- [3. The first start in the foreground](#3-the-first-start-in-the-foreground)
- [4. Run it as a service](#4-run-it-as-a-service)
- [5. Verify](#5-verify)
- [6. The web UI](#6-the-web-ui)
- [7. A runner on the same machine](#7-a-runner-on-the-same-machine)
- [8. A test instance and the first run](#8-a-test-instance-and-the-first-run)
- [The debugging loop](#the-debugging-loop)
- [Starting again from scratch](#starting-again-from-scratch)
- [Where things live](#where-things-live)
- [`server.toml`](#servertoml)
- [The systemd unit](#the-systemd-unit)
- [When it does not work](#when-it-does-not-work)

---

## Prerequisites

| What | Why | How to check |
|---|---|---|
| Go 1.26+ | it is built here | `go version` |
| `just` | build recipes | `just --version` |
| systemd | the `arcatum-server` service | — |
| `restic` | **data restore and repository sizes run on the server** | `restic version` |
| `rsync` (optional) | the installer syncs `scripts/` with it | `command -v rsync` |

Go is not on `PATH` in this environment:

```sh
export PATH=/usr/local/go/bin:$PATH
```

Without `rsync` the installer merely copies `scripts/` (`cp -a`) and says so. The only
difference is that a deleted manifest stays behind in `/opt/arcatum/scripts` — you will run
into this while debugging scripts, so either `apt install rsync` or delete that file by hand.

The address the runners will connect to goes into the server certificate and into `api_url`.
On a test machine that is its own IP and hostname:

```sh
hostname -s        # backup-central
hostname -I        # 172.24.0.60
```

Substitute them into `-H` in step 2. Throughout this guide it is `172.24.0.60,backup-central`.

## The whole procedure

Six commands; the rest of the document spells them out.

```sh
export PATH=/usr/local/go/bin:$PATH
cd /root/src/backup_central

just release && just dist-runner bin                      # 1. the binaries
deploy/install-server.sh -H 172.24.0.60,backup-central    # 2. into /opt/arcatum
cd /root && arcatum-server -instances /dev/null           # 3. a trial start, Ctrl-C ends it
systemctl enable --now arcatum-server                     # 4. the service
journalctl -u arcatum-server -n 30
```

## 1. Build the binaries

```sh
cd /root/src/backup_central
just release          # bin/arcatum-server, bin/arcatum-ca, bin/arcatum-runner
just dist-runner bin  # bin/arcatum-runner-linux-{amd64,arm64} + bin/VERSION
```

The installer builds nothing — it only distributes what it finds in `bin/`. Hence both
recipes; the second produces the binaries the backed-up hosts download, and the `VERSION`
file, without which no update is offered to the runners.

The version is baked into the binaries through `-ldflags` and taken from `V` (today's date by
default). You force your own with `V=test1 just release`; an unstamped build reports itself as
`dev`.

## 2. Install into `/opt/arcatum`

```sh
deploy/install-server.sh -H 172.24.0.60,backup-central -a petr
```

It is run **from the checkout** and installs from it (`bin/` and `scripts/` side by side). It
must run as root; `-n` is a dry run that only prints what it would do.

What it does:

- creates `/opt/arcatum/{bin,pki,dist,scripts}`, `/etc/arcatum` and `/central_backup/arcatum`,
- installs `arcatum-server` and `arcatum-ca` into `/opt/arcatum/bin` and symlinks them into
  `/usr/local/bin` (which is why from here on we just write `arcatum-server`),
- copies `bin/arcatum-runner-linux-*` and `VERSION` into `dist/`,
- syncs `scripts/` — **script definitions are the only thing the server reads from disk**; the
  web UI and `install.sh` are compiled into the binary,
- generates the PKI, writes `/etc/arcatum/server.toml` and the systemd unit.

**Whatever already exists is left alone.** The config, the PKI and the unit are never
overwritten after the first write — so your edits survive every subsequent run, and the same
command is also the procedure for reinstalling after a code change
([the debugging loop](#the-debugging-loop)).

Handy flags: `-b` for a different `backup_dir`, `-p` for a different prefix (a second instance
alongside, say), `-n` for a dry run.

> **`-a petr` only applies when the PKI is being created.** It is the name in the client
> certificate for calling the API from a shell (`pki/admin-petr.pem` and `.key`) — it is not
> used in a browser, where a username and password apply. When the PKI already exists, the
> installer leaves it alone and `-a` **has no effect**; on this machine the certificate is
> therefore `admin-admin.*`. You can issue another one at any time:
>
> ```sh
> arcatum-ca admin -dir /opt/arcatum/pki -name petr
> ```

At the end the installer prints the path to the client certificate and a `Check:` section with
what is missing. Read it.

## 3. The first start in the foreground

The installer does **not** start the service. The first start belongs in the foreground, where
it is immediately visible whether the config and the PKI hold together:

```sh
cd /root && arcatum-server -instances /dev/null
```

`-config` is deliberately absent here: the server finds its config itself — first
`./server.toml`, then `/etc/arcatum/server.toml`. The `cd /root` is a reassurance that it is
started from somewhere other than a directory with its own `server.toml` (the checkout has one
such for development in `local/`).

This is what you want to see:

```
configuration from /etc/arcatum/server.toml
  server certificate valid until 2028-…
  new certificates are issued under "Arcatum CA"
arcatum-server listening on 0.0.0.0:8443
  scripts=/opt/arcatum/scripts  db=…/data/arcatum.db  backup_dir=/central_backup/arcatum
  instance secrets are encrypted at rest
  mTLS enabled (CA …/ca.pem); job dispatches are signed
  bootstrap (plain HTTP) on 0.0.0.0:80 — install.sh and enrollment
  web UI (plain HTTP, password login) on 0.0.0.0:8080
```

**Always check the first line** — if the certificates are different from what you expect, the
config is usually different too. There must be no `WARNING` there; `no [tls]` or
`no [secrets] master_key` means the server is running insecurely and the config is wrong.

When the database is **empty** (the very first start), the generated password of the `admin`
web account is printed here once — write it down:

```
  ┌─ first start: created the web account ─────────────────────
  │   user:     admin
  │   password: k4m2ftq7hn3bwzla
```

When the database already exists (on this machine it does, with runs from earlier in it), the
account is there and nothing is printed. Reset the password:

```sh
arcatum-server -passwd admin                      # generates and prints a new one
ARCATUM_PASSWORD='secretpass' arcatum-server -passwd admin   # or a specific one
```

Ctrl-C ends it.

> A server without a config **does not start at all**, and that is intentional — the built-in
> defaults mean plain HTTP and instance passwords in plaintext, so a typo in a path must not
> silently bring up an insecure server. A broken manifest under `scripts/` is fatal too.
> Forgetting the whole `scripts/` directory, on the other hand, **does not stop the startup**:
> the catalogue simply stays empty, which shows up as the web UI offering no scripts.

## 4. Run it as a service

```sh
systemctl enable --now arcatum-server
journalctl -u arcatum-server -n 30
```

The unit is already on disk, written by the installer ([contents and why](#the-systemd-unit)).
Look for the same things in the log as in step 3.

## 5. Verify

```sh
A=(--cacert /opt/arcatum/pki/ca.pem
   --cert /opt/arcatum/pki/admin-admin.pem
   --key /opt/arcatum/pki/admin-admin.key)

curl "${A[@]}" https://172.24.0.60:8443/api/v1/whoami     # {"role":"admin","secured":true,…}
curl "${A[@]}" https://172.24.0.60:8443/status            # a text overview + the script catalogue
curl "${A[@]}" https://172.24.0.60:8443/api/v1/scripts    # what it loaded from scripts/
curl -k       https://172.24.0.60:8443/api/v1/runs        # MUST fail at the handshake
curl -sS      http://172.24.0.60/                         # bootstrap: runner instructions
```

That a call **without** a certificate fails matters just as much as that one with it goes
through — it is the proof that mTLS really applies. An empty response from `/api/v1/scripts`
means a forgotten `scripts/`, not an error in the log.

## 6. The web UI

```
http://172.24.0.60:8080/
```

The `admin` account and the password from step 3. Nothing is installed or imported into the
browser — the web UI is plain HTTP with username-and-password login, the admin certificate is
only for `curl` against port 8443.

On a test machine this is enough as it is. For real operation the rule is that the web UI
belongs on the internal network, and when it has to be visible further out, an HTTPS reverse
proxy goes in front of it and `[web] secure_cookie = true` is enabled (do not enable that
option without a proxy, logging in would stop working).

## 7. A runner on the same machine

To have something to try, you need a runner. On a test machine the same computer will happily
do:

```sh
curl -LsSf http://172.24.0.60/arcatum_runner/install.sh | sh
```

The script downloads the binary for this platform, `ca.pem` and the signing public key, writes
`/etc/arcatum-runner/runner.toml`, installs a systemd service and starts it. It derives the
bootstrap address from the URL it was downloaded from; it takes the API address from
`[bootstrap] api_url` in the server config — which is why that has to be an address that is
also in the certificate's SAN.

The runner generates **its own** key (it never leaves the host) and sends only a signing
request. Then it waits, and that is the correct state:

```sh
systemctl status arcatum-runner
journalctl -u arcatum-runner -f
```

In the web UI, on the **Runners** tab, **approve** the request. Its `runner_id` is
`hostname -s`, i.e. `backup-central` — that name then belongs in the `runner_id` of the
instances.

```sh
curl "${A[@]}" https://172.24.0.60:8443/api/v1/runners   # status, platform, version, last_seen
```

## 8. A test instance and the first run

In the web UI, **Instances → new instance**, script `hello`. The form is assembled from the
manifest parameters ([`scripts/example/hello.toml`](../scripts/example/hello.toml)), so it is
enough to fill in `name`, `target` and `token` and pick `backup-central` as the runner.
`hello` deliberately needs no external tool — it goes the whole way from dispatch to the
captured output, so when it finishes, it is the chain that works, not the script.

A new instance has **no schedule** and runs only when you start it, which is exactly what you
want here. Then **run now**, which drops you into that task's run history; click the run and
watch the live tail. Once the chain works, give it a timetable under **Schedules → new
schedule**. The same from a shell:

```sh
curl "${A[@]}" -X POST https://172.24.0.60:8443/api/v1/instances/hello-demo/run
curl "${A[@]}" https://172.24.0.60:8443/api/v1/runs?limit=5
curl "${A[@]}" https://172.24.0.60:8443/api/v1/runs/run-1/output
```

A run ID has the form `run-1` — with a bare number the output endpoints return an empty body,
not an error. The output is also on disk in
`/central_backup/arcatum/runs/<run_id>/{stdout,stderr}.log`.

Once this passes, try `files-backup` — that one already reaches for restic and is the first
thing that really backs anything up.

---

## The debugging loop

This is what you will be running most often. What changed determines how much you have to do:

**A change in the Go code** — rebuild, reinstall, restart. The installer restarts a running
service itself:

```sh
cd /root/src/backup_central
just release && just dist-runner bin
deploy/install-server.sh
journalctl -u arcatum-server -n 30
```

`-H` is no longer written, the PKI and the config exist. A running binary cannot be written to
directly (`Text file busy`), which is why the installer stores it alongside and renames it into
place.

**A change in `scripts/*.toml` or in a script** — the catalogue is read at startup, so
`deploy/install-server.sh` (which copies them into `/opt/arcatum/scripts`) **and a restart**
are needed:

```sh
deploy/install-server.sh && systemctl restart arcatum-server
```

A new or changed script does not take effect earlier. A broken manifest, by contrast, stops the
startup, so always look into the log after a restart.

**A change to an instance or its parameters** — nothing. It takes effect immediately, no
restart needed.

**A change to a schedule** — nothing either. Adding, editing, pausing or deleting one under
**Schedules** (or `PUT /api/v1/schedules/{id}`) recomputes the next run at once.

**A change to `server.toml`** — a restart only, the installer does not touch the config:

```sh
systemctl restart arcatum-server
```

**The runner** updates itself from `dist/` when `VERSION` changes; while debugging it is faster
to restart it directly:

```sh
systemctl restart arcatum-runner
journalctl -u arcatum-runner -f
```

A server restart, incidentally, **skips** a run that was due at that moment — the scheduler is
in memory and schedules are computed from the current time after a start. On a test machine
that does not matter; on a production one it does.

## Starting again from scratch

When you want a clean slate (a new PKI, an empty database, no instances):

```sh
systemctl disable --now arcatum-server arcatum-runner

rm -rf /opt/arcatum /etc/arcatum /central_backup/arcatum
rm -f /etc/systemd/system/arcatum-server.service /usr/local/bin/arcatum-{server,ca}

# the runner on this machine, if you installed it
rm -rf /etc/arcatum-runner /var/lib/arcatum-runner /usr/local/bin/arcatum-runner
rm -f /etc/systemd/system/arcatum-runner.service

systemctl daemon-reload
```

Then back to [step 1](#1-build-the-binaries). The installation is a single directory precisely
so this works — no binary from a previous version is left lying anywhere.

> Deleting `/opt/arcatum/pki` also throws away `secrets-master.key`, which the instance
> passwords are encrypted with, and `ca.key`, which every already-approved runner trusts. On a
> test machine that is exactly what you want. On a production one it would mean you can no
> longer get at the data in the restic repositories — which is why these three keys
> (`secrets-master.key`, `ca.key`, `dispatch-signing.key`) are carried off the machine
> encrypted in a real deployment.

When you want to keep the PKI and throw away only the data, `rm -rf /central_backup/arcatum` is
enough — the server creates the database and the directories again at startup.

## Where things live

```
/opt/arcatum/                     the installation                       installer
  bin/                            arcatum-server, arcatum-ca             installer
  pki/                            CA, certificates, signing and master key (0700)
  dist/                           runner binaries + VERSION              installer
  scripts/                        script DEFINITIONS — the server reads them at runtime

/etc/arcatum/server.toml          configuration                          installer
/usr/local/bin/arcatum-server     symlink into /opt/arcatum/bin/
/usr/local/bin/arcatum-ca         symlink into /opt/arcatum/bin/

/central_backup/arcatum/          backup_dir — nothing but data
  data/arcatum.db                 SQLite (instances, schedules, runs, runners, accounts)  server
  runs/<run_id>/{stdout,stderr}.log   the captured output of runs             server
  restic/<instance>/              the restic repository of each instance       server
  config-backups/                 the configuration saved before every import  server
```

The dividing line is simple: **you never touch `backup_dir` by hand.** Everything under it is
created and written by the server itself. What you copy there does not belong there — it
belongs in `/opt/arcatum`.

The runner does not go into `bin/`: the central server does **not install** it, it only hands
it out, which is why it lives in `dist/` under the name `arcatum-runner-linux-<arch>`. The
bootstrap will not find any other name and installing a runner ends in a 404.

The PKI is deliberately not in `backup_dir`: `secrets-master.key` decrypts the passwords of the
restic repositories, so on the same volume as `restic/` a single carried-off copy would mean
the encrypted data and the key to it in one package.

### What to back up from Arcatum itself

Two things, each differently:

1. **`/opt/arcatum/pki/`** — the CA key, the signing key and `secrets-master.key`. By hand, off
   this machine, and in the knowledge that whoever has this has access to every repository.
   Without `secrets-master.key` the instance passwords are worthless, so **this is the part
   whose loss is irreversible**.
2. **The configuration** — the web UI, the **Administration** tab → *download configuration*,
   or `GET /api/v1/config/export`. A single zip with the instances, their schedules, accounts
   and runners, with no keys and no backup data. That same file can be uploaded back at any time to replace the
   configuration; the details are in the
   [README](../README.md#config-backup-and-server-reset).

The split is deliberate: keys change once per rotation and do not belong in any automatic
export, the configuration changes continuously and downloading it may well be a matter of one
click. Recovery on a new machine is therefore: copy `pki/` over, set up `server.toml`, import
the zip.

The backup data (`runs/`, `restic/`) is the largest part. It is either backed up at the volume
level or — and this is the recommended route — the **off-site replica** below is enabled, which
pours it continuously onto a second machine together with the keys and a database snapshot.
Without it, Arcatum is the last place it sits.

## Off-site replica

A second machine everything the server stores flows onto continuously. The design and the
behaviour are in the [README](../README.md#off-site-replica) and
[architecture.md](architecture.md#21-off-site-replica); this is only the procedure for setting
it up.

### On the replica

```sh
# a dedicated account that should do nothing else
useradd -r -m -d /var/lib/arcatum-replica -s /bin/sh arcatum
install -d -o arcatum -g arcatum -m 700 /data
apt-get install -y rsync                       # or dnf install rsync
```

Mode `0700` is not cosmetic: with `include_keys = true` the master key and the CA key sit in
`/data/keys/`, so whoever gets there opens every repository and can issue a certificate to any
host. Ideally on an encrypted volume.

### The key and its restrictions

On the **Arcatum server** create a dedicated key just for this (without a passphrase — the
transfer runs unattended):

```sh
ssh-keygen -t ed25519 -N '' -C arcatum-replica -f /opt/arcatum/pki/replica-ssh.key
chmod 600 /opt/arcatum/pki/replica-ssh.key
```

> **The key and `known_hosts` must live outside `/root`.** The systemd unit has
> `ProtectHome=yes`, so `/root` and `/home` are empty directories as far as the service is
> concerned — `ssh_key = "/root/.ssh/id_ed25519"` works when you test it by hand from a
> root shell and fails for the service, with `rsync exit 255` and nothing else to go on.
> `/opt/arcatum/pki/` is the right place: readable by the service, outside `backup_dir`.

Write the public part into `~arcatum/.ssh/authorized_keys` on the replica, **restricted**:

```
from="172.26.0.1",restrict,command="rrsync /data" ssh-ed25519 AAAA… arcatum-replica
```

- `restrict` turns off port forwarding, the agent and a pty — the key can only run `command`.
- `command="rrsync /data"` keeps the transfer inside `/data`, even if someone took over the
  Arcatum server. (`rrsync` is usually in `/usr/share/rsync/scripts/` or
  `/usr/share/doc/rsync/scripts/`.)
- `from=` limits its use to an address inside the WireGuard tunnel.

Then pin the replica's host key so a transfer never hangs on a prompt:

```sh
ssh-keyscan -H 172.26.0.2 > /opt/arcatum/pki/replica-known_hosts
# compare the fingerprint with what the replica reports: ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
ssh -i /opt/arcatum/pki/replica-ssh.key \
    -o UserKnownHostsFile=/opt/arcatum/pki/replica-known_hosts \
    arcatum@172.26.0.2 true && echo OK
```

### In `server.toml`

Take the `[replica]` section from [`config/server.example.toml`](../config/server.example.toml).
The minimum that makes sense for us:

```toml
[replica]
enabled      = true
host         = "172.26.0.2"
user         = "arcatum"
path         = "/data"
ssh_key      = "/opt/arcatum/pki/replica-ssh.key"
known_hosts  = "/opt/arcatum/pki/replica-known_hosts"
mirror       = true
max_delete   = 100
include_keys = true
```

The server needs `rsync` installed; when it is missing, replication turns itself off with a
message in the log and everything else keeps running.

After a restart the startup log shows the target and two warnings that are not errors, just a
reminder of what you enabled (keys on the replica, and possibly an unpinned host key).
Progress is visible under Administration → **Off-site replica**.

### What to watch out for

- **`max_delete`** must be higher than the number of files ordinary retention deletes in an
  hour, and lower than the number of them in the whole `backup_dir`. A hundred is a reasonable
  start. A pass above the ceiling is refused as a whole and shows up as a failing item — that
  is intended behaviour, not a fault, and it means "check that `backup_dir` is mounted and
  full".
- **Space on the replica.** With `mirror = true` it holds roughly as much as `backup_dir`;
  with mirroring off it grows without limit.
- **Restoring from the replica** goes: copy `/data/keys/` into `pki/`, `/data/meta/arcatum.db`
  into `data_dir`, `/data/restic/` and `/data/runs/` into `backup_dir`, adjust `server.toml`
  and start it. Try it as a dry run before you need it.

## `server.toml`

The installer wrote it and does not touch it from then on. An example with every option
described is in [`config/server.example.toml`](../config/server.example.toml). This is what is
on this machine:

```toml
[server]
listen    = "0.0.0.0:8443"                  # API for runners (mTLS)
scripts   = "/opt/arcatum/scripts"          # an absolute path, not a relative one
data_dir  = "/central_backup/arcatum/data"
timezone  = "Europe/Prague"
log_level = "info"

[web]
listen      = "0.0.0.0:8080"                # web UI: plain HTTP, username and password
session_ttl = "12h"

[storage]
backup_dir = "/central_backup/arcatum"

[tls]
ca_cert = "/opt/arcatum/pki/ca.pem"
cert    = "/opt/arcatum/pki/server.pem"
key     = "/opt/arcatum/pki/server.key"

[signing]
key = "/opt/arcatum/pki/dispatch-signing.key"

[secrets]
master_key = "/opt/arcatum/pki/secrets-master.key"

[bootstrap]
listen   = "0.0.0.0:80"
dist_dir = "/opt/arcatum/dist"
api_url  = "https://172.24.0.60:8443"       # this is the address the runner gets in runner.toml
ca_key   = "/opt/arcatum/pki/ca.key"
```

**Where the config is looked for** without `-config`: first `./server.toml`, then
`/etc/arcatum/server.toml`. If neither is found, the server exits with an error. So do **not**
put a `server.toml` into `/opt/arcatum` — the service has its `WorkingDirectory` there and such
a file would override the service configuration.

What the config **refuses** at startup instead of silently working around it:

- `[tls]` filled in halfway — all three paths belong together, otherwise it would fall back to
  plain HTTP,
- `[tls]` without `[signing] key` — the runners would have nothing to verify,
- `[tls]` without `[secrets] master_key` — passwords would sit in `arcatum.db` in plaintext,
- `[bootstrap]` without `api_url` or `ca_key`, or without `[tls]`,
- two listeners on the same address (`[web]`, `[server]`, `[bootstrap]`),
- a nonsensical `[web] session_ttl` — a wrong value would silently mean "never expires".

Two things that are easy to mix up:

- **`listen` vs. `api_url`.** `listen` is where the server listens; `api_url` is the address
  the server writes into the generated `runner.toml`. It does not know its own reachable
  address — you have to tell it.
- **`log_level`** is read, but the server currently logs at a single level; `debug` prints no
  more.

## The systemd unit

```ini
[Unit]
Description=Arcatum backup server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/arcatum/bin/arcatum-server -config /etc/arcatum/server.toml -instances /dev/null
WorkingDirectory=/opt/arcatum
Restart=always
RestartSec=5

AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
ProtectSystem=full
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=/central_backup/arcatum
ReadOnlyPaths=/opt/arcatum

[Install]
WantedBy=multi-user.target
```

- **`-instances /dev/null`** — instances are managed from the web UI and live in the DB. The
  server passes over an empty or missing file without an error.
- **`-config` is given explicitly**, even though the lookup would find it anyway: the unit has
  `WorkingDirectory=/opt/arcatum` and a file slipped into the working directory must not
  override the service configuration. While debugging from a shell, leave `-config` out
  instead — you get the same config and there is nothing to overwrite.
- **`ReadOnlyPaths=/opt/arcatum`** — the server only reads from here and writes exclusively
  into `backup_dir`. This does not prevent updates, the installer runs outside the service. A
  different `backup_dir` also needs `ReadWritePaths` adjusted.
- Without `User=` the service runs as root: it reads private keys and binds port 80.
  `AmbientCapabilities` is prepared for when it runs under its own user.

## When it does not work

**`TLS handshake error` in the log is normal.** mTLS being on means every attempt to connect
without the right certificate looks like an error. The server is not reporting anything about
itself — it reports what went wrong on the client's side:

| Text in the log | Cause | Fix |
|---|---|---|
| `client sent an HTTP request to an HTTPS server` | `http://` against port 8443 | use `https://` |
| `remote error: tls: unknown certificate authority` | the client does not know `ca.pem` | `--cacert /opt/arcatum/pki/ca.pem` |
| `tls: client didn't provide a certificate` | it got to mTLS, but without an admin certificate | `--cert/--key` |
| `remote error: tls: unknown certificate` | **the client rejected the server certificate** — the address is not in its SAN | see below |
| `remote error: tls: bad certificate` | the certificate was issued by a different CA (typically after `rm -rf pki`) | issue a new one: `arcatum-ca admin …` |

`remote error` means the alert was sent by the **client** — the problem is in what it does not
trust.

**The address is not in the certificate's SAN.** What is in it:

```sh
openssl x509 -in /opt/arcatum/pki/server.pem -noout -ext subjectAltName -dates
```

If you issue the certificate for an IP only, you will not connect over the DNS name even with
the CA — and vice versa. It can be added at any time, the certificate is simply reissued:

```sh
arcatum-ca server -dir /opt/arcatum/pki -hosts 172.24.0.60,backup-central
systemctl restart arcatum-server
```

This breaks nothing for the runners, they verify the CA, which stays the same. Do change
`[bootstrap] api_url` as well, though, if it points at the address you were adding.

**The web UI offers no scripts** — `/opt/arcatum/scripts` is empty or `[server] scripts` points
somewhere else. An empty catalogue does not stop the startup; check with
`curl "${A[@]}" …/api/v1/scripts`.

**A deleted script is still showing** — `rsync` is missing on this machine, so the installer
copies without `--delete`. Delete it from `/opt/arcatum/scripts` by hand (or
`apt install rsync`).

**Replication fails with `rsync exit 255`** — ssh could not connect. Try the same command
by hand as root; if it works, the paths are the problem: with `ProtectHome=yes` in the unit
the service does not see `/root/.ssh`. Move the key and `known_hosts` into
`/opt/arcatum/pki/` and adjust `[replica]`.

**Installing a runner ends in a 404** — there is no `arcatum-runner-linux-<arch>` in `dist/`
for that architecture. `ls /opt/arcatum/dist`, and if need be `just dist-runner bin` and the
installer again.

**No update is offered to the runners** — `/opt/arcatum/dist/VERSION` is missing. The binaries
alone do not constitute a release; that is deliberate, so that copying and releasing can be
done separately.

**`Text file busy` during installation** — a running binary cannot be written to. The installer
handles this itself (it stores alongside and renames); when you hit it by hand, stop the
service.

Related: [architecture](architecture.md) · [backend development](backend-development.md) ·
[script development](script-development.md)
