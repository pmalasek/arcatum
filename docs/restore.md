# Guide: restoring a database from a dump

How to get a dump out of Arcatum and back into a running database — MySQL and PostgreSQL
alike. This is hardest to read at the moment you need it, so walk through the whole procedure
once as a dry run before you need it for real.

- [1. What Arcatum guarantees and what it does not](#1-what-arcatum-guarantees-and-what-it-does-not)
- [2. How to get hold of the dump](#2-how-to-get-hold-of-the-dump)
- [3. MySQL / MariaDB](#3-mysql--mariadb)
- [4. PostgreSQL](#4-postgresql)
- [5. What is not in the dump](#5-what-is-not-in-the-dump)
- [6. Error catalogue](#6-error-catalogue)
- [7. A trial restore](#7-a-trial-restore)

---

## 1. What Arcatum guarantees and what it does not

**It guarantees:** a downloadable dump only exists when the backup script finished with exit
code 0. The server writes the received data into `data.part` and renames it to `data.bin`
only when the run completes successfully, so neither a truncated transfer nor a crashed
`mysqldump` leaves behind a file that looks like a finished backup
([architecture §17](architecture.md)).

**It guarantees nothing about the contents.** The server does not look inside the dump: it
checks neither format, extension, header nor size — it accepts any stream of bytes. A script
that exits zero and prints nonsense produces a backup indistinguishable from a correct one
right up to the moment you restore it.

> The only proof that a backup is restorable is **a restore you have performed**. Not the file
> size, not a green row in the run overview. See [§7](#7-a-trial-restore).

Dumps are also **rotated** (`keep_last`, `keep_days` per instance) — older ones may no longer
be available even though the run's row stays in the history
([architecture §19](architecture.md)).

---

## 2. How to get hold of the dump

**From the web UI** — the **Restore** tab, where you pick an instance; for a streamed
instance the stored dumps are listed for download instead of a browsable tree. The file is
named `<instance>-<run>.dump` regardless of what is inside.

The same over the API:

```sh
A=(--cacert pki/ca.pem --cert pki/admin-petr.pem --key pki/admin-petr.key)
API=https://172.24.0.60:8443/api/v1

curl "${A[@]}" $API/instances/mysql-web01/dumps        # what is available, newest first
curl "${A[@]}" -O -J $API/runs/run-42/data             # download a specific run
```

The download can also go **straight into the database client**, with no intermediate step on
disk — with a multi-gigabyte dump that makes a difference:

```sh
curl "${A[@]}" $API/runs/run-42/data | mysql --host=… --user=… shop
```

| Response | Means |
|---|---|
| `200` | the dump is there |
| `404 this run has no backup data` | the run produced no data (or did not finish successfully) |
| `410 Gone` | the dump **was rotated away** by retention — the run went fine, the file no longer exists |

Downloading requires a read role (a client certificate or a web login) and supports `Range`,
so an interrupted download of a large dump can be resumed rather than restarted from zero.

---

## 3. MySQL / MariaDB

The dump from [scripts/example/mysql_backup.sh](../scripts/example/mysql_backup.sh) is
**plain SQL for a single database**. It contains neither `CREATE DATABASE` nor `USE`, so you
choose the target database at restore time — and you can restore it under a different name.

```sh
# 1) the target database (must exist; character set matching the original)
mysql --host=db01 --user=root -e \
  "CREATE DATABASE shop_restore CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"

# 2) restore; the password via MYSQL_PWD so it is not in the process list
export MYSQL_PWD='…'
mysql --host=db01 --user=root --default-character-set=utf8mb4 \
      shop_restore < mysql-web01-run-42.dump
echo "exit=$?"
```

By default the dump has `DROP TABLE IF EXISTS` for every table, so a restore **over an
existing database works** and overwrites the tables. Anything extra in the target database (a
table that is not in the dump) stays, though — for a clean restore always use an empty
database.

`mysql` exits with a non-zero code on the first error, so `exit=0` here really does mean the
whole file went through.

**Careful with GTID.** If the source server has GTID enabled, `mysqldump` puts
`SET @@GLOBAL.GTID_PURGED=…` into the dump. Restoring into a server that already knows some
transactions fails on it. The fix is to dump with `--set-gtid-purged=OFF`, or to remove that
statement from the dump before restoring.

---

## 4. PostgreSQL

Postgres is backed up by **two instances**: `postgres-backup` dumps a single database and
`postgres-globals-backup` the roles and tablespaces of the whole cluster. When restoring onto
an empty cluster you need both dumps, **in that order** — the roles must exist before the
database dump hits them in `ALTER … OWNER TO`.

The dump from [scripts/example/postgres_backup.sh](../scripts/example/postgres_backup.sh) is
**plain SQL for a single database** without `CREATE DATABASE` — it is restored with `psql`
(not `pg_restore`, which only handles the `custom`/`directory`/`tar` formats).

```sh
export PGPASSWORD='…'

# 1) roles and tablespaces (only on a cluster that does not have them yet)
psql --host=db01 --username=postgres --dbname=postgres \
     --set=ON_ERROR_STOP=1 \
     --file=pg-globals-web01-run-41.dump

# 2) the target database; -T template0 because of the encoding and collation from the dump
createdb --host=db01 --username=postgres --template=template0 --encoding=UTF8 shop_restore

# 3) restoring the data
psql --host=db01 --username=postgres --dbname=shop_restore \
     --set=ON_ERROR_STOP=1 --single-transaction \
     --file=postgres-web01-run-42.dump
echo "exit=$?"
```

Step 1 is written for an **empty cluster** — there `ON_ERROR_STOP=1` is right, because every
error is a real one. On a cluster that already has some roles, skip it instead: the dump has
`CREATE ROLE` even for roles that exist there (`postgres` practically always), and the first
collision would throw away the rest. But then **go through stderr by hand** — `role "x"
already exists` is fine, anything else is not.

> The globals dump contains **password hashes for all roles**
> (`ALTER ROLE … PASSWORD 'SCRAM-…'`). Arcatum does not encrypt payloads, so it sits in the
> open on the server like any other dump — treat it as a sensitive file.

**`ON_ERROR_STOP=1` is not there for decoration.** By default `psql` carries on after an
error and exits with code 0 — so a restore missing half the tables looks successful.
`--single-transaction` adds to that: on an error nothing is committed and the target database
stays empty instead of half-filled.

**Owners and privileges are in the dump.** Into a plain-format file `pg_dump` writes
`ALTER … OWNER TO <role>` and `GRANT`. Such a dump will not restore into a **different**
cluster where those roles do not exist — you either have to create the roles beforehand:

```sh
psql --host=db01 --username=postgres -c "CREATE ROLE shop_app LOGIN"
```

…or take the dump with `--no-owner --no-privileges`. That is a choice **at dump time**, not at
restore time; ownership cannot conveniently be taken out of a finished plain dump.

Into the same cluster the backup came from, this problem does not arise.

**Versions.** `pg_dump` must be the same version as the backed-up server or newer; restoring a
dump into an **older** major version of Postgres does not work reliably.

---

## 5. What is not in the dump

A dump of one database captures the database, not the server. Do not look for these in it:

| | MySQL | PostgreSQL |
|---|---|---|
| functions, procedures, triggers, views | yes | yes |
| sequences / AUTO_INCREMENT | yes | yes |
| scheduled events | yes (`--events`) | n/a |
| extensions | n/a | yes (`CREATE EXTENSION`, must be installed on the target) |
| large objects | n/a | yes |
| users, passwords, grants | **no** (`mysql.user`) | yes, but **from a different instance** — `postgres-globals-backup` |
| other databases on the server | no | no |
| `CREATE DATABASE` | no | no (`--create` is missing) |
| server configuration | no (`my.cnf`) | no (`postgresql.conf`, `pg_hba.conf`) |
| point-in-time recovery | no (binlogs) | no (WAL archive) |

What follows from that in practice:

- **MySQL users and grants are not backed up at all.** After restoring onto another server
  you have to create them by hand, otherwise nobody will connect to the restored database.
  Arcatum has no counterpart yet to what `postgres-globals-backup` does for Postgres.
- **With Postgres you need both instances.** One `postgres-globals-backup` per server, one
  `postgres-backup` per database. Neither restores anything on its own.
- `--single-transaction` in `mysqldump` gives a consistent snapshot only for **InnoDB**;
  MyISAM tables are dumped in whatever state they happen to be caught.

---

## 6. Error catalogue

| Message | Where | What to do |
|---|---|---|
| `410 this run's dump has been rotated away by retention` | download | retention deleted the dump — pick a newer run, or raise `keep_last`/`keep_days` |
| `404 this run has no backup data` | download | the run did not finish successfully, or the script has no `capture = "stream"` |
| `ERROR 1840 … @@GLOBAL.GTID_PURGED can only be set when @@GLOBAL.GTID_EXECUTED is empty` | MySQL restore | GTID — see [§3](#3-mysql--mariadb) |
| `ERROR 1049 Unknown database` | MySQL restore | the dump has no `CREATE DATABASE`, create it by hand |
| `role "x" does not exist` | PG restore | restore the globals dump **before** the database dump, or create the roles by hand |
| `role "x" already exists` | PG globals restore | the cluster already has the roles — drop `ON_ERROR_STOP` and go through stderr by hand |
| `permission denied for table pg_authid` | `postgres-globals-backup` run | the instance is not running as a superuser; role passwords cannot be read otherwise |
| `you need (at least one of) the EVENT privilege(s)` | `mysql-backup` run | the backup account has no `EVENT` — add it, or drop `--events` from the script |
| `relation "x" already exists` | PG restore | you are restoring into a non-empty database; `pg_dump` without `--clean` deletes nothing |
| `unsupported version … in file header` | `pg_restore` | a plain dump is restored with `psql`, not `pg_restore` |
| `server version mismatch` | PG restore | a dump from a newer Postgres cannot go into an older one |
| the restore exits 0 but data is missing | PG restore | a forgotten `ON_ERROR_STOP=1` — `psql` only printed the errors and carried on |
| the dump is suspiciously small | — | the script may have failed without `pipefail`; compare `data_bytes` with previous runs |

---

## 7. A trial restore

A backup nothing has ever been restored from is an unconfirmed assumption. A procedure that
fits into half an hour and gives a real answer:

1. Download the **newest** dump of the instance
   (`/api/v1/instances/<id>/dumps` → `/runs/<run>/data`).
2. Restore it into a **new, empty database** on a test server, not over production.
3. Check that the restore exited with code 0 — with Postgres **with `ON_ERROR_STOP=1`**,
   otherwise that code means nothing.
4. Compare the contents against the original. Run **the same command** against the source and
   the restored database and pipe the outputs through `diff`:

   ```sh
   # MySQL — checksums of the contents, not just row counts
   mysql -N -B shop -e "SHOW TABLES" |
     xargs -I% mysql -N -B shop -e 'CHECKSUM TABLE `%`' | sed 's/^[^.]*\.//'

   # PostgreSQL — exact row counts; n_live_tup from the statistics is only an estimate
   psql -At -d shop -c "
     SELECT relname, (xpath('/row/c/text()',
              query_to_xml(format('SELECT count(*) AS c FROM %I.%I', schemaname, relname),
                           false, true, '')))[1]::text::bigint
     FROM pg_stat_user_tables ORDER BY relname"
   ```

   (The `sed` in the MySQL case cuts off the database name so the diff does not report a
   difference merely because the restored copy has a different name.)

5. Verify what the tables will not show: that functions, triggers and EVENTs exist, that
   sequences / `AUTO_INCREMENT` are right, that the application connects to the restored
   database.

A checklist for a new database instance:

- [ ] the instance has at least one enabled **schedule** — without one it runs only when
      somebody presses "run now"
- [ ] a trial restore was done, `exit = 0`, checksums / row counts match
- [ ] the target database could be created with the **same encoding and collation**
- [ ] with Postgres: the server also has a `postgres-globals-backup` instance and you
      restored its dump **first**
- [ ] with Postgres: the globals instance runs as a superuser (otherwise the run fails on
      `pg_authid`)
- [ ] with MySQL: the backup account has the `EVENT` privilege — the script requires it since
      `--events` was added
- [ ] with MySQL: you know where you will get the users and grants — they are not in the dump
      and Arcatum does not back them up
- [ ] `keep_last` / `keep_days` on the instance cover the time it takes to notice silent data
      corruption, not just yesterday

---

Related: [script development and debugging](script-development.md) ·
[architecture §17 (payload vs. log)](architecture.md) ·
[architecture §19 (dump retention)](architecture.md) ·
[README → Restoring data](../README.md#restoring-data) (file backups through restic)
