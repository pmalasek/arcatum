# Arcatum

Centrální zálohovací systém pro servery ve vnitřní síti **Xtuning**. Monorepo, jazyk Go
pro server i runner.

Arcatum spouští zálohovací skripty na vzdálených serverech podle rozvrhu, sbírá jejich
výstup a ukládá ho **centrálně** — na zálohovaném serveru nemá zůstávat nic.

---

## Obsah

- [Jak to funguje](#jak-to-funguje)
- [Klíčové koncepty](#klíčové-koncepty)
- [Struktura repozitáře](#struktura-repozitáře)
- [Rychlý start (lokální vyzkoušení)](#rychlý-start-lokální-vyzkoušení)
- [Konfigurace](#konfigurace)
- [Zabezpečení (mTLS a podpis úloh)](#zabezpečení-mtls-a-podpis-úloh)
- [Jak napsat vlastní zálohovací skript](#jak-napsat-vlastní-zálohovací-skript)
- [Jak přidat instanci](#jak-přidat-instanci)
- [HTTP API](#http-api)
- [Ladění skriptů](#ladění-skriptů)
- [Instalace runneru na zálohovaný server](#instalace-runneru-na-zálohovaný-server)
- [Vývoj](#vývoj)
- [Stav a roadmapa](#stav-a-roadmapa)

---

## Jak to funguje

Dvě komponenty:

- **arcatum-server** — centrální mozek. Drží rozvrh, definice skriptů, databázi běhů
  a úložiště zálohovaných dat. Poskytuje API (a později web UI).
- **arcatum-runner** — lehká služba na každém zálohovaném serveru. Spouští skripty
  a streamuje výsledek na server.

Komunikace je **pull** — runner si o práci říká sám:

```
 runner                                    server
   │  1. POST /api/v1/checkin               │  „jsem web-01, máš pro mě něco?"
   │ ─────────────────────────────────────► │
   │  2. seznam úloh k spuštění             │
   │ ◄───────────────────────────────────── │
   │  3. spustí skript lokálně              │
   │  4. streamuje stdout/stderr + status   │
   │ ─────────────────────────────────────► │  ukládá do DB + backup_dir
```

**Proč pull:** zálohované servery nemusí otevírat žádný příchozí port (jen odchozí
spojení), což je přátelské k firewallu a zmenšuje útočnou plochu.

---

## Klíčové koncepty

### Skript vs. instance

Nejdůležitější rozdělení v celém systému — **šablona** a její **nasazení**:

| | **Skript** (definice) | **Instance** (nasazení) |
|---|---|---|
| Co to je | šablona: kód/binárka + manifest parametrů | konkrétní nasazení na jeden cíl |
| Kde žije | `scripts/` — verzováno v gitu | databáze (SQLite) |
| Obsahuje secrets | **ne, nikdy** | ano |
| Rozvrh | ne | **ano** |
| Příklad | `mysql-backup` | `mysql-web01`, `mysql-web02`, … |

Jeden skript „záloha MySQL" tak obsluhuje libovolný počet MySQL serverů — každý jako
samostatná instance s vlastními přihlašovacími údaji, databází a časem spouštění.

Instance míří na **právě jeden runner**. Jeden runner může hostit víc instancí.
**Víc databází = víc instancí** (dá to nezávislý rozvrh, status a retry na každou).

### Tři úrovně konfigurace

1. **Definice skriptu** — `scripts/<name>/<name>.toml` (git, bez secrets)
2. **Instance** — v DB, seedovaná z `data/instances.json`
3. **Host-level** — `config/server.toml` a `config/runner.toml`

---

## Struktura repozitáře

```
cmd/server            binárka arcatum-server
cmd/runner            binárka arcatum-runner
cmd/arcatum-ca        správa PKI (CA, certifikáty, podepisovací klíč)
internal/server       HTTP API, scheduler, SQLite store, katalog skriptů, autorizace
internal/runner       checkin smyčka, executor, ověření podpisu úloh
pkg/proto             zprávy protokolu + kanonická serializace pro podpis
pkg/jobspec           parser manifestu skriptu + validace
pkg/schedule          výpočet „next run" (denní/týdenní/měsíční)
pkg/config            config serveru (server.toml) i runneru (runner.toml)
pkg/crypto            PKI, mTLS konfigurace, Ed25519 podpisy úloh
scripts/              DEFINICE skriptů — kód + manifest, bez secrets
data/                 instances.example.json
config/               server.example.toml, runner.example.toml
deploy/gen-certs.sh   vygeneruje celé PKI jedním příkazem
docs/architecture.md  architektura a rozhodnutí
```

---

## Rychlý start (lokální vyzkoušení)

Předpoklad: Go 1.26+. Pokud Go není na `PATH`:

```sh
export PATH=/usr/local/go/bin:$PATH
```

**1) Připravit config a instanci**

```sh
cp config/server.example.toml config/server.toml
cp data/instances.example.json data/instances.json
# v instances.json nastav "runner_id" na hostname stroje, kde poběží runner:
hostname
```

Pro lokální test uprav v `config/server.toml` cesty, ať se nesahá do `/central_backup`:

```toml
[server]
listen   = "127.0.0.1:8443"
data_dir = "./local/data"

[storage]
backup_dir = "./local/backup"
```

**2) Spustit server**

```sh
go run ./cmd/server -config config/server.toml -instances data/instances.json
```

**3) Spustit úlohu ručně a nechat runner doběhnout**

```sh
# v jiném terminálu — vynutí spuštění při nejbližším checkinu
curl -X POST http://127.0.0.1:8443/api/v1/instances/hello-demo/run

# runner se jednou přihlásí, úlohu spustí a odešle výsledek
go run ./cmd/runner -server http://127.0.0.1:8443 -once
```

**4) Zkontrolovat výsledek**

```sh
curl http://127.0.0.1:8443/                              # textová status stránka
curl http://127.0.0.1:8443/api/v1/runs                   # seznam běhů
curl http://127.0.0.1:8443/api/v1/runs/run-1/output      # zachycený výstup
```

Runner jako služba (bez `-once`) se hlásí opakovaně podle `poll_interval`.

> Tento rychlý start běží **bez zabezpečení** (plain HTTP, žádné ověřování). Pro reálné
> nasazení pokračuj sekcí [Zabezpečení](#zabezpečení-mtls-a-podpis-úloh).

---

## Konfigurace

### Server — `config/server.toml`

```toml
[server]
listen    = "0.0.0.0:8443"                 # kde API/web naslouchá
scripts   = "scripts"                       # adresář s definicemi skriptů
data_dir  = "/central_backup/arcatum/data"  # zde vzniká arcatum.db
timezone  = "Europe/Prague"                 # default TZ pro rozvrhy bez vlastní
log_level = "info"

[storage]
backup_dir = "/central_backup/arcatum"      # kam se ukládají zálohovaná data

[tls]
# ca_cert / cert / key — mTLS, zapojíme později
```

Chybějící pole padají na defaulty (`pkg/config.Default`), chybějící soubor taky.

### Runner — `runner.toml`

```toml
[runner]
server        = "https://172.24.0.60:8443"   # kam se hlásit
poll_interval = "30s"
data_dir      = "/var/lib/arcatum-runner"
```

> **Kde která adresa žije:** `listen` serveru je v `server.toml`; adresu, **kam runner
> volá**, drží `runner.toml`. Server svou vlastní dosažitelnou adresu nezná ani znát nemá.
> Při instalaci runneru ji vyplní `install.sh` z URL, ze které se instalátor stáhl.

---

## Zabezpečení (mTLS a podpis úloh)

Bez sekcí `[tls]` a `[signing]` běží Arcatum **nezabezpečeně** — plain HTTP, server
neověřuje volající a runner spustí cokoli, co dostane. To je určeno **jen pro lokální
vývoj**; obě komponenty na to při startu upozorní.

Ochrana má dvě nezávislé vrstvy:

1. **mTLS** — kdo je na drátě. Server i runner mají certifikát od společné Arcatum CA
   a ověřují se navzájem. Neznámý host neprojde ani TLS handshakem.
2. **Podpis úloh** — odkud pochází práce. Server podepisuje každou úlohu Ed25519 klíčem
   a runner podpis **ověří ještě před spuštěním**. Nesouhlasí-li, kód nespustí
   a nahlásí selhání zpět. Podpis pokrývá i SHA‑256 artefaktu, takže je svázán
   s konkrétním kódem.

Proč obojí: mTLS chrání spojení, podpis chrání *úlohu*. Kdyby unikl TLS klíč serveru,
podepisovací klíč je jiný soubor a útočník stále nepodstrčí runneru kód.

### Role v certifikátech

Role je v `OU` certifikátu a server podle ní dělí přístup:

| Role | Kdo | Co smí |
|---|---|---|
| `runner` | zálohovaný server | jen `checkin` a hlášení **vlastních** běhů |
| `admin` | operátor / web UI | ostatní API — spouštění úloh, výpisy, čtení výstupů |

**Identitu určuje certifikát, ne požadavek.** Runner se identifikuje `CN` svého
certifikátu, které musí odpovídat `runner_id` v instancích. Když se runner s platným
certifikátem pokusí vydávat za jiný host, server to odmítne (403) — nemlčí.

### Vygenerování certifikátů

Jeden příkaz vytvoří celé PKI — CA, podepisovací klíč, cert serveru, admin cert
a certifikáty runnerů:

```sh
deploy/gen-certs.sh -H 172.24.0.60,arcatum.xtuning.local -a petr web-01 db-01
```

Vznikne adresář `pki/`:

| Soubor | Kam patří |
|---|---|
| `ca.pem` | server **i každý runner** |
| `ca.key` | **jen server** — soukromý klíč CA |
| `server.pem` / `server.key` | server |
| `dispatch-signing.key` | **jen server** — podepisuje úlohy |
| `dispatch-signing.pub` | **každý runner** — ověřuje úlohy |
| `admin-petr.pem` / `.key` | tvůj počítač (přístup k API/webu) |
| `runner-web-01.pem` / `.key` | příslušný runner |

> `-H` musí obsahovat **všechny** adresy, na které se runnery připojují (IP i DNS),
> jinak ověření TLS selže. Opakované spuštění skriptu existující CA ani podepisovací
> klíč nepřepíše.

Jemnější kontrola přes `arcatum-ca` (`init`, `server`, `runner`, `admin`, `signing`,
`sign-csr` — poslední je základ pro budoucí enrollment, kdy si runner klíč vygeneruje
sám a posílá jen CSR):

```sh
go run ./cmd/arcatum-ca runner -dir pki -id web-02      # přidat runner
go run ./cmd/arcatum-ca admin  -dir pki -name kolega    # přidat operátora
```

### Zapojení do konfigurace

```toml
# server.toml
[tls]
ca_cert = "/central_backup/arcatum/pki/ca.pem"
cert    = "/central_backup/arcatum/pki/server.pem"
key     = "/central_backup/arcatum/pki/server.key"

[signing]
key = "/central_backup/arcatum/pki/dispatch-signing.key"
```

```toml
# runner.toml (na zálohovaném serveru)
[tls]
ca_cert = "/var/lib/arcatum-runner/pki/ca.pem"
cert    = "/var/lib/arcatum-runner/pki/runner-web-01.pem"
key     = "/var/lib/arcatum-runner/pki/runner-web-01.key"

[signing]
public_key = "/var/lib/arcatum-runner/pki/dispatch-signing.pub"
```

Všechny tři cesty v `[tls]` musí být zadané společně — poloviční konfigurace je chyba,
kterou server odmítne, aby nedošlo k tichému propadnutí na nezabezpečené HTTP.

### Volání API s certifikátem

```sh
curl --cacert pki/ca.pem --cert pki/admin-petr.pem --key pki/admin-petr.key \
  https://172.24.0.60:8443/api/v1/runs
```

---

## Jak napsat vlastní zálohovací skript

Skript = dva soubory ve `scripts/<jmeno>/`: **kód** a **manifest**.

### 1) Manifest — deklaruje parametry

```toml
# scripts/example/mysql_backup.toml
name       = "mysql-backup"
type       = "bash"            # bash | python | binary | restic
entrypoint = "mysql_backup.sh" # relativně k manifestu
timeout    = "1h"              # default, instance může přepsat

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
secret = true                  # hodnota se předá souborem, ne přes env
```

Deklarace parametrů není formalita — server z ní validuje instance a (později)
vygeneruje formulář ve web UI.

### 2) Kód — jak dostane parametry

- **Non-secret parametry** → env proměnné `ARCATUM_<JMENO>` (velkými písmeny).
- **Secrets** → dočasný sourcovaný soubor, jeho cesta je v `ARCATUM_SECRETS_FILE`.
  Runner ho po doběhnutí smaže. Do env se secrets nedávají záměrně — env je čitelné
  z `/proc/<pid>/environ`.
- **Výstup na stdout** se streamuje na server. Pište data na stdout, ať nezůstávají
  na zálohovaném serveru.

```bash
#!/usr/bin/env bash
set -euo pipefail

: "${ARCATUM_HOST:?missing host}"
PORT="${ARCATUM_PORT:-3306}"

# shellcheck disable=SC1090
[ -n "${ARCATUM_SECRETS_FILE:-}" ] && source "${ARCATUM_SECRETS_FILE}"
# nyní je k dispozici $ARCATUM_PASSWORD

exec mysqldump --host="$ARCATUM_HOST" --port="$PORT" \
  --single-transaction --quick "$ARCATUM_DATABASE"
```

Ukázky: [scripts/example/](scripts/example/) — `hello` (demo bez závislostí)
a `mysql_backup` (realistická šablona).

> **Binární skripty** (`type = "binary"`) fungují taky — runner artefakt spustí přímo.
> Runner při checkinu hlásí svou platformu (`linux/amd64`), takže server umí vybrat
> správný artefakt. U binárek je ověření integrity (SHA-256, později podpis) o to důležitější.

---

## Jak přidat instanci

Instance žijí v DB; zatím se seedují z JSON při startu serveru (upsert podle `id`,
takže opakovaný import bezpečně aktualizuje):

```json
[
  {
    "id": "mysql-web01",
    "script": "mysql-backup",
    "runner_id": "web-01",
    "params": { "host": "127.0.0.1", "port": "3306", "database": "shop", "user": "backup" },
    "secrets": { "password": "…" },
    "capture": "stream",
    "timeout": "2h",
    "schedule": {
      "frequency": "weekly",
      "time": "02:30",
      "weekdays": ["mon", "thu"],
      "timezone": "Europe/Prague"
    }
  }
]
```

Rozvrh: `frequency` je `daily` | `weekly` | `monthly`; `weekdays` platí pro `weekly`,
`day` (1–28) pro `monthly`. `timezone` je nepovinná — jinak platí default ze `server.toml`.

Pozor: `data/instances.json` obsahuje secrets, proto je v `.gitignore`
(verzovaná je jen `instances.example.json`).

---

## HTTP API

Sloupec „role" platí při zapnutém mTLS — viz
[Zabezpečení](#zabezpečení-mtls-a-podpis-úloh).

| Metoda a cesta | Role | Účel |
|---|---|---|
| `POST /api/v1/checkin` | runner | runner se hlásí, dostane úlohy k spuštění |
| `POST /api/v1/runs/updates` | runner | příjem ndjson streamu průběhu a výstupu |
| `POST /api/v1/instances/{id}/run` | admin | **manuální spuštění** („spusť teď") |
| `GET /api/v1/instances` | admin | instance včetně `next_run` (secrets maskované) |
| `GET /api/v1/runs?limit=N` | admin | historie běhů, nejnovější první |
| `GET /api/v1/runs/{id}/output?stream=stdout\|stderr` | admin | zachycený výstup běhu |
| `GET /api/v1/runners` | admin | evidované runnery (platforma, `last_seen`) |
| `GET /` | admin | textová status stránka |

Hodnoty secrets API **nikdy nevrací** (jen názvy, maskované `***`). Skutečné hodnoty
opouštějí server pouze v úloze doručené vlastnímu runneru.

Runner smí hlásit průběh jen u běhů, které byly přiděleny jemu — jeden zálohovaný
server tak nemůže přepsat výsledky jiného.

---

## Ladění skriptů

Ladění bylo od začátku prioritou, takže:

```sh
# 1) spustit hned, bez čekání na rozvrh
curl -X POST http://127.0.0.1:8443/api/v1/instances/hello-demo/run

# 2) runner jednorázově, s logem v terminálu
go run ./cmd/runner -server http://127.0.0.1:8443 -once

# 3) přečíst přesně to, co skript vypsal
curl http://127.0.0.1:8443/api/v1/runs/run-1/output
curl "http://127.0.0.1:8443/api/v1/runs/run-1/output?stream=stderr"
```

Výstup se ukládá do `backup_dir/runs/<run_id>/{stdout,stderr}.log`, takže do něj lze
kdykoli nahlédnout i přímo na serveru. Chystá se živý tail ve web UI a dry-run režim.

---

## Instalace runneru na zálohovaný server

Zamýšlený tvar (instalátor se dopisuje):

```sh
curl -LsSf http://172.24.0.60/arcatum_runner/install.sh | sh
```

Instalátor stáhne statický binár, založí systemd službu, vytvoří `runner.toml`
s adresou serveru (odvozenou z instalační URL) a vygeneruje runneru identitu (klíč
+ CSR), kterou server po schválení podepíše.

---

## Vývoj

```sh
export PATH=/usr/local/go/bin:$PATH   # Go není v tomto prostředí na PATH

go build ./...      # kompilace
go vet ./...        # statická kontrola
go test ./...       # testy
```

Server běží bez CGO (SQLite přes `modernc.org/sqlite`), takže výsledkem je jeden
statický binár bez runtime závislostí.

---

## Stav a roadmapa

**Hotovo:**
- Pull protokol end-to-end: checkin → doručení úlohy → spuštění → stream výstupu na server
- Rozvrh (denní/týdenní/měsíční) + manuální trigger
- Persistence v SQLite (instance, běhy, evidence runnerů) — přežije restart
- Tři úrovně konfigurace, manifest s deklarací parametrů
- **mTLS** mezi serverem a runnery, identita a role z certifikátu, PKI nástroje
- **Podpis úloh** (Ed25519) — runner ověřuje před spuštěním, jinak odmítne
- Bezpečné předání secrets (dočasný soubor, ne env), maskování v API, ověření SHA-256 artefaktu

**Chybí (další fáze):**
- **Enrollment** runneru (dnes se certifikáty generují a rozdávají ručně; `sign-csr` už existuje)
- **Šifrování secrets at-rest** v DB
- **Orchestrace restic** pro souborové zálohy (dedup, inkrementální)
- **Web UI** včetně živého tailu výstupu
- **Retence a rotace** (GFS), **notifikace** při selhání
- **Restore** (2. fáze podle plánu)
- Správa instancí přes API/web (dnes seed z JSON), auto-update runneru
- Revokace certifikátů (CRL/OCSP) a rotace klíčů

Podrobná architektura a rozhodnutí: [docs/architecture.md](docs/architecture.md).
