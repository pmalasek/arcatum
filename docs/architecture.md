# Arcatum — architektura

Zálohovací systém pro interní síť Xtuning. Monorepo, jazyk **Go** pro runner i server.

Stav dokumentu: **návrh / draft** (2026-07-12). Implementace: fáze A (scaffold) +
fáze B (protokol end-to-end přes plain HTTP) hotové — viz §10.

---

## 1. Přehled

Arcatum se skládá ze dvou komponent:

- **arcatum-server** — centrální mozek. Drží scheduler (časovač), definice úloh
  (skripty + config), databázi běhů a výsledků, úložiště zálohovaných dat, web UI a API.
- **arcatum-runner** — lehká služba na každém zálohovaném serveru. Sama se hlásí
  serveru (pull), stahuje si podepsané úlohy, spouští je a streamuje výstup/data zpět.

Instalace runneru:

```sh
curl -LsSf http://172.24.0.60/arcatum_runner/install.sh | sh
```

Install skript stáhne statický binár, založí systemd službu a vygeneruje runneru
identitu (klíč + CSR), kterou server při prvním kontaktu schválí (enrollment).

---

## 2. Klíčová rozhodnutí

| # | Rozhodnutí | Volba | Důvod |
|---|-----------|-------|-------|
| 1 | Směr komunikace | **Pull** (runner → server, odchozí HTTPS) | Žádný příchozí port na zálohovaných serverech, přátelské k firewallu, menší útočná plocha |
| 2 | Souborový backup | **Orchestrace restic** | Dedup, inkrementální zálohy, šifrování at-rest a integrita jsou vyřešené; my řešíme scheduling a UI |
| 3 | Restore | **2. fáze** (po MVP zálohování) | Rychlejší první použitelná verze |
| 4 | Jazyk serveru | **Go** (ne Python) | Sdílený kód s runnerem v monorepu, jeden statický binár, embed web UI, concurrency |
| 5 | Autorizace | **mTLS + podpis úloh** | Vzájemná autentizace server↔runner; runner spustí jen podepsané úlohy. Ne per-script certifikát |
| 6 | Konfigurace | **Dvojí: definice skriptu + instance** | Skript = šablona (git, bez secrets); instance = nasazení na konkrétní cíl s parametry a rozvrhem (DB). Viz §5 |
| 7 | Typy skriptů | **bash / python / binary / restic** | Počítat i s binárkami (výběr dle archu runneru, důraz na podpis) |

---

## 3. Model komunikace (pull)

```
 runner                              server
   │  1. POST /api/v1/checkin (mTLS) │   „jsem host web-01, co pro mě máš?"
   │ ───────────────────────────────► │
   │  2. seznam due úloh (podepsané)  │
   │ ◄─────────────────────────────── │
   │  3. spustí restic / bash / py    │
   │  4. stream stdout+stderr+status  │
   │ ───────────────────────────────► │   ukládá do DB
   │  5. stream dat (restic → REST)   │
   │ ───────────────────────────────► │   ukládá do storage
```

- Runner poll interval konfigurovatelný (default např. 30 s). Pro okamžitý „spusť teď"
  z webu stačí krátký interval nebo long-polling.
- Scheduler (kdy je úloha „due") žije **na serveru**; runner se jen ptá.

---

## 4. Struktura monorepa

```
arcatum/
├── cmd/
│   ├── server/          # main pro arcatum-server
│   └── runner/          # main pro arcatum-runner
├── pkg/                  # SDÍLENÝ kód (hlavní přínos monorepa)
│   ├── proto/            # zprávy protokolu, verzování
│   ├── crypto/           # mTLS, podpis/ověření úloh, enrollment
│   ├── jobspec/          # parsování a validace config (TOML/YAML)
│   └── schedule/         # výpočet „next run" (cron-like)
├── internal/
│   ├── server/           # scheduler, API, DB vrstva, storage
│   └── runner/           # executor skriptů, restic wrapper, streaming
├── web/                  # embed.FS – jednoduché web UI
├── scripts/              # DEFINICE skriptů (kód + manifest) – verzované v gitu, bez secrets
│   └── example/
│       ├── mysql_backup.sh       # nebo binárka (type=binary)
│       └── mysql_backup.toml      # manifest: deklarace parametrů
│   # POZOR: instance (konkrétní hodnoty + secrets) NEjsou tady, žijí v DB / web UI
├── deploy/
│   └── install.sh        # instalátor runneru
├── docs/
│   └── architecture.md
└── go.mod
```

---

## 5. Dvojí konfigurace: definice skriptu vs. instance

Vzor **šablona + nasazení**. Jeden skript (např. záloha MySQL) se spouští proti více
serverům; každý server je samostatná **instance** s vlastními parametry a rozvrhem.

### 5.1 Skript (definice) — `scripts/<name>/<name>.toml`

Kód (bash / python / **binárka**) + *manifest* deklarující parametry. Verzuje se v gitu,
**neobsahuje žádné konkrétní hodnoty ani hesla**.

```toml
# scripts/example/mysql_backup.toml
name        = "mysql-backup"
type        = "bash"              # bash | python | binary | restic
entrypoint  = "mysql_backup.sh"   # cesta relativně k tomuto configu
platforms   = ["linux/amd64"]     # jen pro type=binary (výběr artefaktu dle archu runneru)
timeout     = "1h"                # default, instance může přepsat

# Deklarace parametrů → server z toho generuje formulář ve webu a validuje instanci
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

### 5.2 Instance — v DB, spravováno přes web UI

Konkrétní vázání skriptu na cíl. Tady žijí hodnoty parametrů (secrets šifrované at-rest)
a **rozvrh** (ten patří sem, ne do definice — každý MySQL server může zálohovat v jiný čas).

```jsonc
// koncepčně (v DB, ne soubor):
{
  "instance": "mysql-web01",
  "script":   "mysql-backup",
  "target":   "web-01",            // který runner
  "params":   { "host": "127.0.0.1", "port": 3306, "database": "shop", "user": "backup" },
  "secrets":  { "password": "<enc:age...>" },   // šifrováno master klíčem
  "schedule": { "frequency": "weekly", "time": "02:30", "weekdays": ["mon","thu"],
                "timezone": "Europe/Prague" },
  "run":      { "timeout": "1h", "on_failure": "notify", "capture": "stream" }
}
```

**Víc databází → víc instancí.** Dá to nezávislý rozvrh, status, retry a granularitu
restoru na každou DB. Nepoužívat jeden skript s listem databází.

### 5.3 Předání parametrů skriptu

- **Non-secret** parametry → env proměnné (`ARCATUM_HOST`, `ARCATUM_PORT`…).
- **Secrets** → krátkodobý config soubor předaný cestou v argumentu, smazaný po doběhnutí
  (ne env — je vidět v `/proc/<pid>/environ`). Sedí to i s cílem „minimum lokálních dat".

### 5.4 `target`

Instance míří na **právě jeden runner** (bez skupin). Z pohledu runneru je to **N:1** —
jeden runner může hostit víc instancí (např. MySQL záloha + záloha souborů), ale každá
instance má jeden cíl. V DB tedy prostý `instances.runner_id` FK, žádná M:N tabulka.

### 5.5 Konfigurace serveru — `config/server.toml`

Třetí, samostatná úroveň konfigurace (vedle definic skriptů a instancí): host-level
nastavení serveru. Chybějící pole padají na defaulty (`pkg/config.Default`).

```toml
[server]
listen    = "0.0.0.0:8443"
scripts   = "scripts"
data_dir  = "/central_backup/arcatum/data"   # DB a runtime stav
timezone  = "Europe/Prague"                    # default TZ pro rozvrhy bez vlastní
log_level = "info"

[storage]
backup_dir = "/central_backup/arcatum"         # kam se ukládají zálohovaná data

[tls]
# ca_cert / cert / key — mTLS, zapojíme později
```

### 5.6 Konfigurace runneru — `runner.toml`

Runner má **vlastní** config na zálohovaném hostu. Klíčové pole je `server` — kam se
hlásit. **install.sh ho vyplní** z URL, ze které se instalátor stáhl (`172.24.0.60`),
takže operátor nic ručně nepíše.

> Kde která adresa žije: **listen** serveru je v `server.toml`; **adresu, kam runner volá**,
> drží `runner.toml`. Server svou vlastní dosažitelnou adresu nezná ani znát nemá.

```toml
[runner]
server        = "https://172.24.0.60:8443"   # kam se hlásit (arcatum-server)
poll_interval = "30s"
data_dir      = "/var/lib/arcatum-runner"

[tls]
# ca_cert / cert / key — klientský mTLS, později
```

---

## 6. Databáze (server)

**SQLite** (jeden soubor v `data_dir/arcatum.db`), driver `modernc.org/sqlite` — čistě
v Go, **bez CGO**, takže binárka zůstává statická. Schéma v `internal/server/schema.go`,
aplikuje se idempotentně při každém startu.

Implementované tabulky:

- `runners` — id, hostname, os, arch, first_seen, last_seen *(cert fingerprint a stav
  pending/approved přidáme s enrollmentem)*
- `instances` — script, runner_id, params (JSON), secrets (JSON), capture, timeout,
  schedule (JSON)
- `runs` — id (rowid → `run-<n>`), instance_id, runner_id, script, status, exit_code,
  bytes, started_at, ended_at, err, created_at

**Skripty nejsou v DB** — definice zůstávají soubory ve `scripts/` (verzované v gitu),
server je čte do katalogu při startu.

**Výstup běhů také není v DB** — streamuje se do
`backup_dir/runs/<run_id>/{stdout,stderr}.log`. Payload zálohy patří do úložiště, ne
do tabulky; v DB je jen metadata a počet bajtů.

Časy jsou unix millis (0 = nenastaveno). Secrets jsou zatím v plaintextu — šifrování
at-rest přijde s `pkg/crypto.SecretBox`. Přechod na Postgres zůstává otevřený.

---

## 7. Bezpečnost

- **mTLS** — každý runner má vlastní klientský cert vydaný Arcatum CA. Server ověří
  runner, runner ověří server. Tím je splněno „autorizace oběma směry".
- **Enrollment** — nový runner pošle CSR, admin ho ve webu schválí, teprve pak dostane cert.
- **Podpis úloh** — server podepisuje payload úlohy svým klíčem; runner spustí jen
  ověřený podpis. Šifrování zajišťuje samotné mTLS spojení.
- **Secrets** — hesla (DB apod.) nikdy v plaintextu v `scripts/`; injektují se přes
  prostředí/secret store při doručení úlohy. (Detailně dořešit.)

---

## 8. Ladění skriptů (priorita zadavatele)

- **Manuální trigger** z web UI: „spusť teď na hostu X".
- **Živý tail** stdout/stderr ve webu během běhu.
- **Dry-run** režim.
- Verbose log a uchování posledních N běhů pro srovnání.

---

## 9. Otevřené otázky / backlog

- **Retence a rotace** záloh (GFS: denní/týdenní/měsíční).
- **Restore flow** (2. fáze) — návrh dřív než implementace zálohování dokončena.
- **Notifikace** při selhání (e-mail/Slack).
- **Storage backend** serveru (lokální disk / NAS / S3) + šifrování at-rest.
- **Chování při nedostupnosti** (server dole v čase zálohy → dohnat / přeskočit).
- **Auto-update runneru**.
- **Autentizace k webu/API** + audit log.
- **Souběžnost / zamykání** a resumability velkých přenosů.

---

## 10. Stav implementace

### Fáze A — scaffold ✓
Kostra monorepa, config (server + runner), parser manifestu, výpočet rozvrhu, proto zprávy.

### Fáze B — protokol end-to-end (plain HTTP) ✓
Runner se přihlásí, dostane úlohu, spustí ji a **streamuje výstup zpět na server**, kde
se ukládá do `backup_dir/runs/<run_id>/` — na zálohovaném hostu nezůstává. Ověřeno E2E.

**HTTP API (`internal/server`)** — kompletní přehled v [README](../README.md#http-api).

**Runner (`internal/runner`):** checkin smyčka dle `poll_interval`, executor materializuje
artefakt (ověří SHA-256), non-secret params → `ARCATUM_*` env, secrets → dočasný sourcovaný
soubor (mazán po běhu), stdout/stderr streamuje jako `RunUpdate`.

### Fáze C — persistence v SQLite ✓
In-memory store nahrazen SQLite (`internal/server/store.go`, schéma v `schema.go`).
Instance, běhy i evidence runnerů přežijí restart serveru; číslování běhů pokračuje.
Přidány endpointy `GET /api/v1/instances` (s `next_run`), `GET /api/v1/runs/{id}/output`
a `GET /api/v1/runners`.

**Ověřeno E2E:** běh → restart serveru → data i výstup zachovány → další běh je `run-2`.
Testy v `internal/server/store_test.go` (upsert instancí, životní cyklus běhu, mapování
statusů, persistence přes reopen, maskování secrets).

**Bezpečnostní detail:** `Instance.Redacted()` maskuje hodnoty secrets pro API a logy;
skutečné hodnoty opouštějí server jen v `JobDispatch` vlastnímu runneru.

**Zatím vědomě chybí (další fáze):** mTLS + podpis úloh (běží plain HTTP, runner podpis
neověřuje), šifrování secrets at-rest, restic orchestrace, web UI, retence, notifikace,
restore, správa instancí přes API (dnes seed z JSON).

**Vyzkoušení lokálně:** viz [README — Rychlý start](../README.md#rychlý-start-lokální-vyzkoušení).
