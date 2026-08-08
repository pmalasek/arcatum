# Arcatum — architektura

Zálohovací systém pro interní síť Xtuning. Monorepo, jazyk **Go** pro runner i server.

Stav implementace: fáze A–J hotové — scaffold, protokol, SQLite, mTLS a podpis úloh,
šifrování secrets, restic zálohy, web UI, instalace a enrollment, životní cyklus
certifikátů, obnova z webu, rotace klíčů, správa instancí z webu, auto-update runnerů,
přihlášení do webu jménem a heslem. Přehled v §10, detaily v §11–18.

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

Install skript stáhne statický binár, založí systemd službu a runner si vygeneruje
vlastní identitu (klíč + CSR); certifikát dostane po schválení operátorem — viz §11.

---

## 2. Klíčová rozhodnutí

| # | Rozhodnutí | Volba | Důvod |
|---|-----------|-------|-------|
| 1 | Směr komunikace | **Pull** (runner → server, odchozí HTTPS) | Žádný příchozí port na zálohovaných serverech, přátelské k firewallu, menší útočná plocha |
| 2 | Souborový backup | **Orchestrace restic** | Dedup, inkrementální zálohy, šifrování at-rest a integrita jsou vyřešené; my řešíme scheduling a UI |
| 3 | Restore | **2. fáze** (po MVP zálohování) | Rychlejší první použitelná verze. Hotovo: procházení a stažení z webu, běží na serveru — §13 |
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
- Krok 5 je implementovaný jako **restic REST backend na serveru** (`/restic/<instance>/`,
  `internal/server/restic.go`). Runner spouští restic s repozitářem
  `rest:https://<server>/restic/<instance>/`, takže pack soubory jdou přímo na server;
  na zálohovaném hostu zůstane jen lokální cache resticu. Každá instance má **vlastní
  repozitář** a runner se dostane jen k repozitářům instancí cílených na něj — jeden
  zálohovaný server tedy nemůže číst ani poškodit zálohy jiného.

---

## 4. Struktura monorepa

```
arcatum/
├── cmd/
│   ├── server/          # main pro arcatum-server
│   ├── runner/          # main pro arcatum-runner
│   └── arcatum-ca/      # správa PKI (CA, certifikáty, podepisovací a master klíč)
├── pkg/                  # SDÍLENÝ kód (hlavní přínos monorepa)
│   ├── proto/            # zprávy protokolu, verzování
│   ├── crypto/           # mTLS, podpis/ověření úloh, enrollment
│   ├── jobspec/          # parsování a validace config (TOML/YAML)
│   └── schedule/         # výpočet „next run" (cron-like)
├── internal/
│   ├── server/           # scheduler, API, DB vrstva, storage
│   └── runner/           # executor skriptů, restic wrapper, streaming
├── web/                  # web UI zabalené do binárky (embed.FS)
├── scripts/              # DEFINICE skriptů (kód + manifest) – verzované v gitu, bez secrets
│   └── example/
│       ├── mysql_backup.sh       # nebo binárka (type=binary)
│       └── mysql_backup.toml      # manifest: deklarace parametrů
│   # POZOR: instance (konkrétní hodnoty + secrets) NEjsou tady, žijí v DB / web UI
├── config/               # server.example.toml, runner.example.toml
├── data/                 # instances.example.json
├── deploy/
│   └── gen-certs.sh      # vygeneruje celé PKI jedním příkazem
│                          # (install.sh generuje server za běhu — §11)
├── docs/
│   └── architecture.md
├── justfile              # zkratky nad go/curl příkazy z dokumentace (just je nepovinný)
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
capture     = "stream"            # co je stdout: "log" (výchozí) nebo "stream" = payload zálohy (§17)

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
- `users` — účty web UI: username, PBKDF2 verifikátor hesla, role (`admin`/`viewer`),
  disabled, created_at, updated_at, last_login
- `sessions` — přihlášení do webu: **SHA‑256 tokenu** z cookie (ne token sám), username,
  created_at, expires_at, last_seen, ip

**Skripty nejsou v DB** — definice zůstávají soubory ve `scripts/` (verzované v gitu),
server je čte do katalogu při startu.

**Výstup běhů také není v DB** — streamuje se do
`backup_dir/runs/<run_id>/{stdout,stderr}.log`. Payload zálohy patří do úložiště, ne
do tabulky; v DB je jen metadata a počet bajtů.

**Log a data jsou dvě různé věci.** Skript deklarovaný v manifestu jako
`capture = "stream"` (např. `mysql-backup`) píše na stdout samotný dump. Ten neputuje
kanálem pro výstup — jde vlastním requestem do `runs/<run_id>/data.bin` a v logu se
neobjeví. Kdyby šel logem, byl by base64 v ndjson streamu, po chuncích, s vlastní SQLite
transakcí na každý z nich, a webová stránka běhů by nabízela k prohlížení gigabajtový
SQL dump. Podrobněji v §17.

Logy mají strop **4 MiB na stream a běh** (přetečení se označí markerem) a **retenci**:
`[storage] log_retention_success` / `log_retention_failed`. Data se retencí nemažou —
smazat zálohu není výchozí chování, které by měl kdokoli zdědit bez vlastního rozhodnutí.

Časy jsou unix millis (0 = nenastaveno). **Secrets jsou v `instances.secrets` šifrované**
(viz §7). Přechod na Postgres zůstává otevřený.

---

## 7. Bezpečnost

Implementováno v `pkg/crypto` (PKI, mTLS, podpisy, šifrování secrets) a
`internal/server/auth.go` (autorizace). Tři **nezávislé** vrstvy:

- **mTLS** — *kdo je na drátě*. Každý runner má klientský cert od Arcatum CA, server
  vyžaduje `RequireAndVerifyClientCert`. Tím je splněno „autorizace oběma směry".
  Klíče jsou ECDSA P‑256 (ne Ed25519), aby stejný cert serveru zvládly i prohlížeče
  pro web UI.
- **Podpis úloh** — *odkud pochází práce*. Server podepisuje `JobDispatch` klíčem
  Ed25519, runner ověří **před spuštěním**; při nesouhlasu kód nespustí a nahlásí
  selhání. Podpis je záměrně **jiný klíč než TLS** — kdyby unikl TLS klíč serveru,
  útočník stále nepodstrčí runneru kód.
- **Šifrování secrets at-rest** — *co leží v databázi*. AES‑256‑GCM, každá hodnota
  samostatně (názvy secrets zůstávají čitelné pro UI, hodnoty ne). Kopie `arcatum.db`
  sama o sobě neprozradí žádné přihlašovací údaje. Master klíč je opět **jiný soubor**
  než TLS i podepisovací klíč.

**Co podpis pokrývá:** kanonická serializace v `pkg/proto/signing.go` — všechna pole
s délkovými prefixy (aby hodnota nemohla imitovat hranici pole) a mapy se seřazenými
klíči (jinak by verifikace selhávala nedeterministicky). Obsah artefaktu se podepisuje
**přes jeho SHA‑256**; runner přijaté bajty proti hashi povinně ověří, takže je podpis
svázán s konkrétním kódem.

**Role v certifikátu (`OU`):**

| Role | Kdo | Oprávnění |
|---|---|---|
| `runner` | zálohovaný server | `checkin` a hlášení **vlastních** běhů |
| `admin` | operátor volající API ze shellu | ostatní API (trigger, výpisy, čtení výstupů) |

**Lidé se hlásí heslem, stroje certifikátem** (`internal/server/users.go`). Web UI má
vlastní plain-HTTP listener (`[web] listen`) a účty v tabulce `users`; port API zůstává
mTLS pro runnery a pro volání s admin certifikátem. Důvod je provozní: certifikát se
musí vyexportovat, naimportovat v každém prohlížeči a po roce vyměnit, což u člověka,
který se jen dívá na výsledky záloh, nic nechrání — *úlohy* stejně chrání podpis a
*secrets* šifrování, a web na žádný z těch klíčů nesahá. Naproti tomu u runneru je
certifikát to, čím ho server pustí (nebo nepustí) už na TLS handshaku, a stroj se
instaluje jednou.

Ukládá se PBKDF2‑HMAC‑SHA256 verifikátor (600 000 iterací, sůl na účet) ze standardní
knihovny — žádná nová závislost, ze stejného důvodu jako `modernc.org/sqlite`. Z tokenu
sezení je v DB jen SHA‑256, takže ani kopie `sessions` nedovolí vydávat se za
přihlášeného. Role webu jsou `admin` (mění) a `viewer` (jen čte); stejné operátorské
endpointy tak mají na mTLS listeneru guard `adminOnly` a na webovém `webRead`/`webAdmin`
(`registerOperatorRoutes` v `http.go`). Změna hesla, vypnutí i smazání účtu okamžitě
ruší jeho sezení; posledního funkčního admina web odmítne odstranit, protože zpět by
cesta vedla jen přes shell (`arcatum-server -passwd`).

**Identitu určuje certifikát, ne požadavek** — runner je identifikován `CN`, které musí
odpovídat `runner_id` instance. Pokus vydávat se za jiný host končí 403 (neselhává
tiše). Runner také nemůže hlásit výsledky běhu, který nebyl přidělen jemu.

- **Enrollment** — automatický: runner si vygeneruje vlastní klíč (ten **nikdy neopustí
  host**), pošle jen CSR, server ho zapíše jako `pending`, operátor schválí ve webu
  a runner si podepsaný certifikát vyzvedne. Detail v §11. Ruční vydání přes
  `arcatum-ca runner` zůstává jako alternativa.
- **Secrets** — hesla nikdy v plaintextu v `scripts/`; předávají se v úloze a na
  runneru dočasným souborem (ne env, viz §5.3). V DB jsou **šifrovaná** (`enc:v1:` +
  base64) a ciphertext je přes AAD svázán s **instancí a názvem parametru** — kdo umí
  do DB zapisovat, nemůže heslo přesunout mezi instancemi. Bez master klíče je uložená
  hodnota nečitelná a čtení skončí chybou (`ErrSealed`), ne tichým prázdným heslem.
  Hodnoty uložené před zapnutím šifrování se dál načtou a při dalším importu se zašifrují.
  **Ztráta master klíče = ztráta secrets** → zálohovat mimo chráněný stroj.
- **Bez `[tls]`** server běží plain HTTP a nikoho neautentizuje, bez `[secrets]` ukládá
  hesla v plaintextu. Je to režim pro lokální vývoj; obě komponenty na to při startu
  upozorní. Poloviční konfiguraci `[tls]` config odmítne a se zapnutým mTLS vyžaduje
  i `[signing]` a `[secrets]`, aby nedošlo k tichému propadnutí na nezabezpečený režim.
- **Životní cyklus certifikátů** — automatická obnova a zneplatnění, viz §12.
- **Rotace klíčů** — všechny tři dlouhodobé klíče, viz §14.
- **Přihlášení do webu** — jméno a heslo, role `admin`/`viewer`, správa účtů z webu; viz
  výše a §16. CRL/OCSP záměrně ne (§14).

---

## 8. Ladění skriptů (priorita zadavatele)

- ✓ **Manuální trigger** z web UI („spustit teď") i přes API.
- ✓ **Živý tail** stdout/stderr ve webu během běhu.
- ✓ Uchování běhů a jejich výstupů pro srovnání (`backup_dir/runs/<run_id>/`).
- **Dry-run** režim — zbývá.

**Jak je živý tail udělaný:** žádné websockety ani SSE. Prohlížeč se ptá
`GET /api/v1/runs/{id}/tail?offset=N` a server vrátí jen přírůstek plus nový offset
(`handleRunTail` + `Store.ReadOutputFrom`). Je to jednodušší, přežije to odpadnutí
spojení a nepotřebuje to na serveru žádný stav.

Jedna subtilita, která rozhoduje o tom, jestli se neztratí poslední řádky: stav běhu se
čte **před** výstupem. Když úloha dobehne mezi tím, odpověď ještě říká „running", takže
si klient vyžádá ještě jeden dotaz. Při obráceném pořadí by mohl dostat `done=true`
a přijít o výstup zapsaný po čtení.

---

## 9. Otevřené otázky / backlog

- ~~**Retence a rotace** záloh (GFS)~~ — hotovo, parametry `keep_*` u restic instancí (§10, fáze F).
- ~~**Restore flow**~~ — hotovo pro procházení a stažení z webu (§13); obnova **zpět na
  zálohovaný host** chybí.
- **Notifikace** při selhání (e-mail/Slack).
- **Storage backend** serveru — dnes lokální disk (`backup_dir`); NAS/S3 zatím ne.
  Šifrování zálohovaných dat řeší restic sám, secrets v DB šifrujeme (§7).
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

### Fáze D — mTLS a podpis úloh ✓
Bezpečnostní model z §7 implementován: `pkg/crypto` (PKI, TLS konfigurace, Ed25519
podpisy), `pkg/proto/signing.go` (kanonická serializace), `internal/server/auth.go`
(identita a role z certifikátu), ověření podpisu v runneru před spuštěním.
Nástroje: `cmd/arcatum-ca` a `deploy/gen-certs.sh`.

**Ověřeno E2E s reálným mTLS:** podepsaná úloha proběhne; bez certifikátu spojení
neprojde; runner cert na admin endpoint → 403; cert od cizí CA → odmítnut při
handshaku; platný cert hlásící se za jiný host → 403 s jasnou zprávou v logu.
Jednotkové testy: PKI (chainování, SAN, oddělení rolí, cizí CA), podpisy (poškozená
data/podpis, cizí klíč, prázdný podpis), kanonická serializace (determinismus,
nezávislost na pořadí map, detekce změny každého pole, odolnost proti posunu hranic
polí), autorizace (role, mismatch identity), ověření v runneru.

### Fáze E — šifrování secrets at-rest ✓
`pkg/crypto/secretbox.go` (AES‑256‑GCM, master klíč, `SealToString`/`OpenFromString`
s markerem `enc:v1:`), zapojeno ve `internal/server/store.go` (šifrování při zápisu,
dešifrování při čtení). Master klíč generuje `arcatum-ca master-key` / `init`
a `deploy/gen-certs.sh`. Nový config `[secrets] master_key`.

**Ověřeno E2E:** v `arcatum.db` (ani ve `-wal`/`-shm`) plaintext hesla není — je tam
jen `enc:v1:VzeO2ee…`; API vrací `***`; runner přitom dostane skutečnou hodnotu.
Jednotkové testy: round-trip, různý ciphertext pro stejnou hodnotu, poškozený
ciphertext, cizí master klíč, **přesun ciphertextu na jinou instanci/parametr**,
vadné soubory klíče, legacy plaintext, chybějící klíč (`ErrSealed`), zachovaná redakce.

### Fáze F — souborové zálohy přes restic ✓
Server: restic REST backend (`internal/server/restic.go`) — objekty se validují proti
`^[0-9a-f]{16,64}$`, pack soubory se shardují po prvních dvou znacích, zápis jde do
temp souboru a přejmenuje se (žádné poloviční packy), existující objekt je immutable.
Podporuje API v1 i v2 listing, HEAD a Range (přes `http.ServeContent`).
Runner: `internal/runner/restic.go` — parsování parametrů, inicializace repozitáře při
prvním použití, `backup` s tagy `arcatum`/`instance:<id>`, retence `forget --prune`
až **po úspěšné** záloze a omezená tagem. Pro restic se skládá klientský cert a klíč
do jednoho souboru (`--tls-client-cert`), CA přes `--cacert`.
Manifest typu `restic` nemá entrypoint (runner řídí restic sám).

**Ověřeno E2E se skutečným resticem přes mTLS:** repozitář se sám inicializoval,
zálohovaly se 3 soubory (`.tmp` vynechán dle `excludes`), snapshot uložen, retence
aplikována. **Obnovená data jsou bit za bit identická s originálem** včetně binárního
souboru (shodný md5). Druhá záloha přenesla jen 2,3 KiB místo 48 KiB (dedup funguje).
Cizí runner do repozitáře → 403; traversal → 400.
Jednotkové testy: validace cest a traversal, lifecycle objektů, immutabilita, listing
v1/v2, skrytí rozpracovaných uploadů, autorizace per repozitář, sestavení příkazů
`backup`/`forget`, prázdná retence ≠ „nedrž nic", skládání TLS souboru pro restic,
a **test, že se router poskládá bez kolizí** (tuhle chybu unit testy volající handlery
přímo minuly a odhalil ji až E2E běh).

### Fáze G — web UI ✓
UI je v `web/` a zabalené do binárky přes `embed.FS` (balíček `arcatum/web`), servírované
z `/` — původně na portu API pod admin certifikátem, dnes na vlastním portu za přihlášením
jménem a heslem (§16). Textový přehled se přesunul na `/status`.
Vanilla JS bez build stepu — cílem je, aby server zůstal jeden samostatný soubor.
Nové endpointy: `GET /api/v1/runs/{id}` a `GET /api/v1/runs/{id}/tail?offset=`.

Obsah: záložky Běhy / Instance / Runnery, detail běhu se **živým tailem**, přepínač
stdout/stderr, „sledovat" (autoscroll) a tlačítko **spustit teď** (§8).

**Ověřeno E2E:** UI i assety se servírují (HTTP 200), bez certifikátu spojení neprojde,
každý endpoint, který UI volá, odpovídá 200. Živý tail odsimulován proti skutečně běžící
úloze: přírůstky bez duplikátů i mezer, `done=true` teprve s posledním řádkem.
Jednotkové testy: `ReadOutputFrom` (offset, prázdný soubor, cap, offset za koncem,
oddělení stdout/stderr), tail přes celý životní cyklus běhu, servírování assetů
a jejich Content-Type, admin ochrana UI.

**Nevykresleno v prohlížeči** — v tomhle prostředí není headless browser, takže vzhled
UI ověřen nebyl, jen že se soubory servírují a API pod nimi funguje.

### Fáze H — instalace jedním příkazem a enrollment ✓
Viz §11.

**Zatím vědomě chybí (další fáze):** restore přes API/web (dnes přímo resticem s admin
certifikátem), notifikace, dry-run, správa instancí přes API (dnes seed z JSON),
revokace a rotace klíčů, auto-update runneru.

---

## 11. Instalace runneru a enrollment

### Proč samostatný plain-HTTP listener

Nový host nemá klientský certifikát, a hlavní listener má
`RequireAndVerifyClientCert` — spojení by neprošlo už při TLS handshaku. Bootstrap
soubory proto obsluhuje **druhý listener** (`[bootstrap] listen`, typicky `:80`,
`internal/server/bootstrap.go`) a vydává **jen**:

```
/arcatum_runner/install.sh              generovaný instalátor
/arcatum_runner/arcatum-runner-<os>-<arch>
/arcatum_runner/ca.pem
/arcatum_runner/dispatch-signing.pub
POST /api/v1/enroll        podání CSR
GET  /api/v1/enroll/{id}   vyzvednutí certifikátu
```

Nic z toho není tajné: CA certifikát a podepisovací **veřejný** klíč jsou veřejné svou
povahou, CSR nese jen veřejný klíč a vydaný certifikát je bez privátního klíče
nepoužitelný. Administrátorské API na tomto portu **není** (pokryto testem).

### install.sh se generuje za běhu

Šablona `internal/server/install.sh.tmpl` (embedded) se renderuje per-request a adresu
serveru bere z **Host hlavičky requestu** — runner se tak konfiguruje na tu adresu, ze
které se právě stáhl, a nikde se nezadává dvakrát. `api_url` (mTLS port) je z configu.
Opakovaná instalace binárku aktualizuje, ale existující `runner.toml` nechá být.

### Tok enrollmentu

```
 runner (nový host)                       server
   1. vygeneruje vlastní klíč (zůstává)
   2. POST /api/v1/enroll  {CSR}          → status pending, uloží IP + fingerprint
   3. GET /api/v1/enroll/{id} … pending      (operátor vidí žádost ve webu)
                                          ← operátor schválí → SignCSR
   4. GET /api/v1/enroll/{id} → cert      zapíše cert, přejde na mTLS
```

**Schválení je bezpečnostní pojistka**, ne formalita — endpoint musí být dostupný bez
autentizace, takže žádost může podat kdokoli ze sítě, ale bez schválení nedostane nic.
Aby šla podvržená žádost poznat, ukládá se **IP adresa** a **fingerprint** a UI je
zobrazuje.

Rozhodnutá pravidla:
- **Opakované podání během `pending` je povolené** — přeinstalace je běžná věc.
- **U schváleného runneru se další žádost odmítne (409)** — jinak by kdokoli ze sítě
  přepsal certifikát běžícího hosta.
- **CN v CSR musí odpovídat `runner_id`**, jinak by operátor schvaloval jinou identitu,
  než jaká se vydá.
- **Zamítnutý runner je odmítnut i s platným certifikátem** (kontrola při checkinu), takže
  zamítnutí platí okamžitě a nečeká na revokaci.
- Runnery s ručně vydaným certifikátem mají stav `approved` **defaultně** (migrace), aby
  upgrade nerozbil existující instalace.

### Databáze

Sloupce `status`, `csr`, `cert_pem`, `cert_fingerprint`, `enroll_ip`, `enrolled_at`,
`approved_at`, `cert_not_after`, `revoked_at`, `renewed_at` v `runners`. Přidávají se
**migrací** (`addColumns` + `migrate()` v `store.go`), protože
`CREATE TABLE IF NOT EXISTS` by existující DB neupravil.

### Ověřeno E2E
Celý tok proti běžícímu serveru: stažení `install.sh` (syntaxe ověřena `bash -n`,
obsahuje správné adresy) → runner si vygeneroval klíč `0600` a poslal CSR → server ho
vedl jako `pending` s IP a fingerprintem → schválení přes admin API → runner si vyzvedl
certifikát (`CN=backup-cental, OU=runner`, podepsaný Arcatum CA) → přešel na mTLS →
**proběhla skutečná restic záloha s podepsanou úlohou**. Negativně: útočníkova žádost
o už schváleného runnera → 409, admin API na bootstrap portu → 404.

**Neověřeno:** root části `install.sh` (zápis do `/usr/local/bin`, `/etc`, instalace
systemd unit) se v tomto prostředí nespouštěly — ověřena je syntaxe skriptu a jeho
vygenerovaná konfigurace, kterou runner reálně použil.

**Vyzkoušení lokálně:** viz [README — Rychlý start](../README.md#rychlý-start-lokální-vyzkoušení)
a [Zabezpečení](../README.md#zabezpečení-mtls-a-podpis-úloh).

---

## 12. Životní cyklus certifikátů

### Vynucování je aplikační, ne TLS

Zneplatněný certifikát je pořád kryptograficky platný a projde handshakem, dokud
nevyprší. Odmítnutí je tedy **rozhodnutí aplikace** a musí být na **všech** cestách,
kudy runner může jít: `requireApprovedRunner` v `auth.go` se volá z checkinu, z příjmu
výsledků i z autorizace restic repozitáře.

> Tohle byla reálná mezera po fázi H: stav se kontroloval jen při checkinu, takže
> zamítnutý runner sice nedostal práci, ale **do repozitáře se dostal** — mohl zálohy
> čist i přepsat. Opraveno; pokryto testy.

Sémantika stavů:

| Stav | Význam | Runner dělá |
|---|---|---|
| `""` (bez záznamu) | ručně vydaný certifikát, ještě se neohlásil | funguje — certifikát **je** autorizace, řádek vznikne prvním checkinem jako `approved` |
| `approved` | v pořádku | pracuje |
| `pending` | čeká na schválení, **nebo byl zneplatněn** | zahodí certifikát a požádá o nový |
| `rejected` | operátor odmítl | jen loguje, znovu nežádá |

Rozlišení `pending` vs. `rejected` je důležité: server posílá 403 se **strojově
čitelným důvodem** (`enroll_required` / `rejected`, v těle i v hlavičce
`X-Arcatum-Reason`). Bez toho by zamítnutý runner navěky plnil frontu žádostmi.

### Automatická obnova

Certifikáty se vydávají hromadně, takže by hromadně i vypršely — všechny runnery by
zhasly týž den. Runner proto 30 dní před expirací požádá o nový přes
`POST /api/v1/renew` **na mTLS listeneru**: identitu prokazuje certifikátem, který mění,
takže schvalování operátorem tu nemá co přidat. Obnova generuje **nový klíč**, ne jen
nový certifikát.

Runner smí obnovit **jen vlastní** identitu (CN v CSR musí odpovídat CN volajícího) a
zneplatněný runner obnovit nemůže — musí projít enrollmentem.

### Přepnutí na nový certifikát

Runner po obnově (nebo po zahození zneplatněného certifikátu) **čistě skončí** a nechá
se restartovat službou (`Restart=always` v unit). Je to jednodušší a spolehlivější než
přehazovat TLS stav za běhu — hot-swap TLS konfigurace v poloběhu je zdroj subtilních
chyb, restart je triviálně správný.

### Viditelnost expirace

`cert_not_after` se plní z certifikátu na **živém spojení** při checkinu, takže je znám
i u ručně vydaných certifikátů. `GET /api/v1/whoami` vrací expiraci certifikátu
volajícího **a** serveru; UI z toho staví varování 30 dní předem (7 dní = ostřejší).
Admin certifikát má default platnost 1 rok, takže vyprší jako první — a bez varování by
se to projevilo jako prohlížeč, který se prostě nepřipojí.

### Ověřeno E2E
Zneplatnění → runner dostal 403 s `enroll_required`, zahodil certifikát i klíč a skončil
→ po restartu sám požádal → schválení → **nový fingerprint** a záloha proběhla.
Automatická obnova: ručně předaný certifikát s platností 10 dní → runner si při startu
sám vyžádal nový (825 dní), **klíč se vyměnil**, server obnovu zaznamenal. `whoami`
hlásilo 364 dní u admin certifikátu a 824 u serveru.

---

## 13. Obnova dat

### Běží na serveru, ne na runneru

Obnova se spouští **na serveru** proti repozitáři, který už tam leží, a heslo si server
dešifruje z instance (má master klíč). Runner do toho vůbec nevstupuje.

To je záměrné rozhodnutí, ne zjednodušení: **potřeba obnovy často znamená, že zálohovaný
stroj je nedostupný.** Kdyby obnova vedla přes runnera, nefungovala by právě ve chvíli,
kdy ji nejvíc potřebuješ.

Cenou je závislost: server musí mít nainstalovaný `restic`. Když chybí, endpointy vrátí
jasnou chybu (`restic is not installed on the server`).

### Endpointy (vše admin only)

| Cesta | Co dělá |
|---|---|
| `GET …/snapshots` | `restic snapshots --json`, přeřazeno nejnovějším napřed |
| `GET …/snapshots/{snap}/ls?path=` | obsah **jedné úrovně** stromu |
| `GET …/snapshots/{snap}/download?path=&archive=tar` | `restic dump`, streamem do prohlížeče |

`restic dump` píše na stdout, takže se nic nestaguje na disk — velký archiv začne
přicházet okamžitě (`copyStream` flushuje průběžně).

### Procházení po jedné úrovni

`restic ls` vypisuje snapshot **rekurzivně** a nemá volbu pro hloubku. Filtrování na
přímé potomky proto dělá `parseResticLS`. Dvě věci, které to musí zvládnout:

- **Odvodit chybějící adresáře** — když restic vypíše `/data/sub/deep.txt`, ale ne
  `/data/sub`, musí se `sub` v listingu objevit, jinak by část stromu byla nedosažitelná.
- **Starší formát** — restic dřív značil řádky `struct_type` místo `message_type`.

Výpis je zastropovaný (`maxListEntries`) a příznak `truncated` se posílá do UI, aby
zkrácení nevypadalo jako prázdný adresář.

### Bezpečnost

Cesty i ID snapshotů se validují (`cleanSnapshotPath`, `resticSnapshotIDPattern`) —
`..` se normalizuje pryč a ID musí být hex. Argumenty jdou do `exec` bez shellu, takže
injekce přes `;` nebo backtick nemá kudy. Heslo repozitáře se předává **souborem**, ne
argumentem, takže není vidět v seznamu procesů.

### Chyby: kdo je způsobil

`ErrNoRepository` (neexistující instance, chybějící heslo, repozitář ještě nevznikl) →
**404** s vysvětlením. Selhání resticu → **502**.

U downloadu je jedna subtilita: hlavičky se posílají **až po přečtení prvních bajtů**.
Neexistující cesta ve snapshotu totiž nechá restic selhat okamžitě bez výstupu, a kdyby
hlavičky odešly dřív, výsledkem by byla **HTTP 200 s prázdným tělem** — což v prohlížeči
vypadá jako prázdný soubor, ne jako chyba. (Tohle E2E odhalilo a je to opravené.)

### Ověřeno E2E
Proti skutečnému repozitáři: seznam snapshotů, procházení stromu do hloubky, stažení
textového souboru, binárního souboru (50 kB) a celého adresáře jako tar — **vše bit za
bit identické s originálem** (shodné md5 všech souborů). Obnova ze **staršího snapshotu**
vrátila stav bez později přidaného souboru, tedy point-in-time funguje. Chybové stavy:
neexistující cesta → 404 se zprávou od resticu, neexistující instance → 404, instance bez
hesla → 404 s vysvětlením.

**Neověřeno:** vzhled záložky Obnova v prohlížeči — v tomto prostředí není headless
browser. Ověřeno je, že všechny endpointy, které UI volá, fungují.

---

## 14. Rotace dlouhodobých klíčů

Tři klíče žijí dlouho a všechny je jde vyměnit, aniž by někdo obcházel hosty: master klíč
secrets, podepisovací klíč úloh a CA. Postup je u všech stejný — **okno, kdy platí starý
i nový**, runnery si nové převezmou samy po ověřeném kanálu, a okno zavře operátor, až
server potvrdí, že jsou všichni přeneseni.

### Co je automatické a co ne

| | Automatické? |
|---|---|
| **distribuce** nového materiálu | ano — runnery si ho stahují samy |
| **provedení** (přešifrování, obnova certifikátů) | ano |
| **zavření okna** (odebrání starého klíče) | **ne, operátor** |

Kritérium: **operace, jejíž selhání je bezpečné, může běžet bez dozoru.** Obnova
certifikátů (§12) je proto automatická — když selže, starý certifikát dál platí. Odebrání
kotvy důvěry naopak umí operátora zamknout z vlastního systému, a neobsluhovaná úloha,
která to v noci pokazí, nechá runnery, kteří nevěří ani staré, ani nové CA. Systém proto
odmakává práci a hlásí, **jestli je bezpečné dokončit** (`safe_to_drop_old_ca`).

### Master klíč secrets — `pkg/crypto/keyring.go`

Keyring drží primární klíč a předchůdce. Uložené hodnoty nesou **ID klíče**:

```
enc:v2:<keyid>:<base64(nonce||ciphertext)>
```

ID dělá dvě věci: dešifrování sáhne přímo po správném klíči, a přešifrování pozná, co už
je hotové — takže je **opakovatelné a přerušitelné**. Starší formát `enc:v1:` (bez ID) se
čte zkoušením všech klíčů. `RekeySecrets` commituje po instancích, takže selhání v půlce
nechá dřívější hotové; smíšený stav je čitelný, protože keyring má oba klíče.

Distribuce žádná — celé na serveru.

### Podepisovací klíč úloh — `pkg/crypto/signingset.go`

Runner drží **sadu** akceptovaných klíčů, ne jeden. Sada se publikuje na
`GET /api/v1/trust` a runner ji přijme, jen když je **podepsaná klíčem, kterému už věří**.
Autorita k rotaci tedy leží na držení klíče, který se mění — ne na kontrole serveru.
Kdyby stačil ověřený kanál, převzetí serveru by umožnilo přidat vlastní podepisovací klíč
a spustit libovolný kód, tedy přesně to, čemu podpis úloh brání.

**Sada se podepisuje všemi klíči, které server drží.** Tohle E2E odhalilo jako chybu
v prvním návrhu: podepsat ji jen novým klíčem znamená, že runner na starém klíči ji nikdy
nepřijme a rotace se **zasekne**. Proto jsou `[signing] previous_keys` **privátní** klíče.

Kanonická forma (`SigningSetBytesToSign`) je seřazená a délkově prefixovaná, takže podpis
nezávisí na pořadí, ale žádný klíč nelze přidat ani odebrat nepozorovaně.

### Certifikační autorita

Trust bundle je jeden PEM s víc autoritami (`LoadCAPool` to umí přes
`AppendCertsFromPEM`). Během rotace `[tls] ca_cert` = bundle a `[bootstrap] ca_cert/ca_key`
= nová CA: **verifikace přijímá obě, vydává se pod novou**. Runnery bundle převezmou
(podepsaný, stejně jako sada klíčů) a při obnově přejdou na novou CA. `cert_issuer` se
plní z živého spojení při checkinu, takže server ví, kdo už přešel.

**Pořadí je tady zrádné a E2E to odhalilo:** certifikát serveru musí zůstat pod **starou**
CA, dokud runnery bundle nemají. Kdyby ho vydala nová CA hned, runner se starou `ca.pem`
neprojde handshakem — a tím si nemůže stáhnout bundle, který by to spravil. Server na to
proto sám upozorní (`warning` ve stavu rotace).

### Proč ne CRL/OCSP

Zvažovali jsme je a **záměrně nejsou**. Zneplatnění se vynucuje kontrolou stavu v DB na
každém autorizačním bodu (§12), což je pro uzavřený systém lepší než CRL: platí okamžitě,
nemá cache ani propagační zpoždění. CRL by to jen nahradila něčím pomalejším.

Zbývá jedna mezera — únik **TLS klíče serveru** runnery samy nezjistí. Dopad je ale
omezený: podpis úloh je jiný klíč, takže útočník nespustí kód, a pack soubory jsou
šifrované heslem repozitáře. A tuhle mezeru řeší **rotace CA** výše, s menším aparátem než
CRL infrastruktura (kterou by navíc Go v TLS samo nekontrolovalo, takže by ji bylo nutné
dopsat do runneru včetně řešení stárnutí seznamu).

### Ověřeno E2E
Všechny tři rotace provedeny proti běžícímu systému, včetně cutoveru:
**master klíč** — okno se dvěma klíči, zálohy běží dál, přešifrování 2 hodnot, opakování
no-op, po odebrání starého klíče vše čitelné novým.
**podepisovací klíč** — runner na starém klíči přijal sadu se dvěma klíči, úloha podepsaná
novým prošla, po cutoveru sada zúžena na jeden a zálohy dál běží.
**CA** — bundle se dvěma autoritami, runner ho přijal, obnovou přešel na novou CA
(`CN=Arcatum CA 2026`), stav ohlásil `safe_to_drop_old_ca`, po zúžení bundle záloha
proběhla. Odhaleny a opraveny **dvě reálné chyby v pořadí**, obě popsané výše.

---

## 15. Správa instancí a aktualizace runnerů

### Instance: z JSON do databáze

Instance se zakládají a mění přes API (`internal/server/instances.go`), takže seed soubor
přestal být zdroj pravdy. Tři věci, které to musí splňovat:

**Validace proti manifestu.** `Manifest.ValidateParams` kontroluje povinné hodnoty, typy
a **odmítá neznámé názvy** — překlep `datbase` by jinak tiše ležel v konfiguraci a skript
by padal z důvodu, který s ním nikdo nespojí. Tohle je to, k čemu byly deklarace
parametrů v §5.1 navržené; z nich se zároveň staví formulář ve webu.

**Maskovaný secret se nesmí uložit.** API vrací hesla jako `***`. Kdyby se formulář
odeslal zpátky tak, jak přišel, přepsalo by to každé heslo tím maskováním. Hodnota, která
přijde jako `***`, prázdná, nebo v payloadu vůbec není, proto **zachová uloženou**.

**Seed už nepřepisuje.** `ImportInstances` s `overwrite=false` vytvoří jen to, co
neexistuje. Předtím upsertoval při každém startu, takže restart serveru by vrátil zpět
každou změnu udělanou z webu. Vynutit staré chování jde `-import-force`.

Scheduler se aktualizuje za běhu (`Track` znovu, `Untrack` při smazání), takže změna
rozvrhu platí okamžitě. Smazání instance **nemaže restic repozitář**: zahodit
konfiguraci nesmí zahodit zálohy.

### Auto-update: nejrizikovější funkce v systému

Špatná nebo podvržená aktualizace rozbije všechny zálohované servery naráz, a bootstrap
port je plain HTTP. Bez ochran by auto-update byl ideální cesta, jak podstrčit kód —
tedy přesně to, čemu podpis úloh brání. Proto:

| Ochrana | Proč |
|---|---|
| Manifest **podepsaný podepisovacím klíčem úloh** | publikovat build vyžaduje ten klíč, ne kontrolu serveru |
| Stahování **přes mTLS**, ne z bootstrapu | aktualizace nesmí přijít neautentizovaným kanálem |
| Ověření **SHA‑256** před zápisem | manifest pin­uje konkrétní bajty |
| Zápis vedle + `rename` | atomické; pád v půlce nenechá poloviční binárku |
| Předchozí build jako `.old` | když nová nejde spustit, je s čím srovnávat |
| `dev` build se neaktualizuje | vývojářskou binárku fleet nepřepisuje |
| Jeden pokus na verzi (`update-attempted`) | rozbitý build nezpůsobí restart smyčku |

Kanonická forma manifestu je seřazená a délkově prefixovaná (`updateManifestBytesToSign`,
shodně na serveru i v runneru), takže build nelze přidat, odebrat ani zaměnit
nepozorovaně. Bez souboru `VERSION` se nic nenabízí — binárky v adresáři samy o sobě
aktualizaci nespustí.

Runner hlásí verzi v checkinu (`runners.version`), takže je v UI vidět postup rozjezdu.
Vypnout to jde per host (`[runner] auto_update = false`).

### Ověřeno E2E
**Instance:** vytvořena přes API → záloha proběhla **bez restartu serveru** → validace
odmítla chybějící povinný parametr (400 s jménem parametru) → maskovaný secret se při
úpravě zachoval → seed soubor nepřepsal instanci spravovanou z API.
**Auto-update:** runner 1.0.0 → publikováno 2.0.0 → sám stáhl, ověřil hash, nahradil se
a restartoval; `.old` zůstala. Negativně: **podvržená binárka odmítnuta** (hash
nesouhlasil, binárka zůstala nedotčená) a **manifest podepsaný cizím klíčem odmítnut**.

Cestou to odhalilo provozní past, která je teď v README: po rotaci podepisovacího klíče je
`dispatch-signing.pub` na hostu zastaralý a autoritou je stažená sada. Ztráta té sady
runnera zablokuje (fail closed, správně) — náprava je stáhnout aktuální klíč z bootstrapu.

---

## 16. Přihlášení do webu a účty operátorů

Web UI původně stálo na portu API a chránil ho **admin certifikát** (§7, fáze G). Provozně
to nesedělo: certifikát se musí vyexportovat do PKCS#12, naimportovat v každém prohlížeči
každého operátora, přidat CA mezi důvěryhodné, a po roce to celé zopakovat. Runnerům
certifikát smysl dává — stroj se instaluje jednou a server ho bez certifikátu nepustí ani
na TLS handshaku. U člověka, který se dívá na výsledky záloh, dává smysl heslo.

**Dva listenery, dva druhy volajících:**

| Listener | Konfig | Kdo | Ověření | Router |
|---|---|---|---|---|
| API | `[server] listen` | runnery, volání ze shellu | mTLS certifikát (`OU`) | `Server.Handler()` |
| Web | `[web] listen` | lidé v prohlížeči | cookie sezení po přihlášení | `Server.WebHandler()` |
| Bootstrap | `[bootstrap] listen` | hosti bez certifikátu | nic (§11) | `Server.BootstrapHandler()` |

Operátorské endpointy jsou **na obou** — registruje je `registerOperatorRoutes`, které
dostane guard podle listeneru (`adminOnly`, nebo `webRead`/`webAdmin`). Jeden soupis rout
tedy platí pro oba světy a nový endpoint nemůže dostat ochranu jen na jednom z nich.
Runnerové endpointy (`checkin`, `runs/updates`, `renew`, `trust`, `update`, `/restic/`)
zůstávají výhradně na mTLS listeneru; UI a `login`/`users` výhradně na webovém.

### Co se ukládá

- **Heslo nikdy** — jen PBKDF2‑HMAC‑SHA256 verifikátor (600 000 iterací dle OWASP, náhodná
  sůl na účet, formát `pbkdf2-sha256$<iter>$<salt>$<hash>`). Iterace jsou uvnitř hodnoty,
  takže se dají později zvednout bez odhlášení všech. KDF je ze standardní knihovny
  (`crypto/pbkdf2`), takže nepřidává závislost — stejná úvaha jako u `modernc.org/sqlite`.
- **Token sezení nikdy** — v tabulce `sessions` je jeho SHA‑256. Kopie DB tedy nedovolí
  vydávat se za přihlášeného operátora, jen ho (nejvýš) odhlásit.
- Cookie `arcatum_session` je `HttpOnly`, `SameSite=Strict` a bez `Expires` (zavření
  prohlížeče ji zahodí); `[web] secure_cookie` ji přepne na HTTPS‑only pro nasazení za
  reverse proxy.

### Role a hranice

`admin` mění, `viewer` jen čte. Rozdělení je v routách (`read`/`write`), ne v UI — web sice
viewerovi tlačítka skryje, ale rozhodující je server (403). Ochrany, které stojí za zmínku:

| Situace | Chování | Proč |
|---|---|---|
| Změna hesla, vypnutí, smazání účtu | okamžitě mizí i jeho sezení | jinak by odebrání přístupu platilo až za 12 h |
| Poslední funkční admin | nelze smazat, vypnout ani degradovat (409) | zpět by cesta vedla jen přes shell |
| 5+ neúspěšných přihlášení | prodleva 1 min, dál dvojnásobek do 15 min, per účet | kontrola hesla je záměrně drahá; nesmí jít volat ve smyčce |
| Neexistující jméno | ověří se proti *decoy* hashi, pak stejná chyba | z doby odpovědi nejde zjistit, které účty existují |
| Požadavek mimo Arcatum (`Origin`) | 403 u všeho, co mění stav | cizí stránka nesmí jednat cookie operátora |
| Vlastní účet a mazání | 409 | odhlásit se uprostřed akce je matoucí, ne užitečné |
| První start bez účtů | vznikne `admin`, heslo **jednou** do logu | server, ke kterému se nikdo nepřihlásí, je k ničemu |
| Ztracené heslo posledního admina | `arcatum-server -passwd <user>` | jediná cesta zpět, záměrně mimo web |

Vygenerované heslo (nový účet bez zadaného hesla, nebo reset) se vrací v odpovědi API
**jednou** a nikde se v čitelné podobě neukládá — UI ho zobrazí k opsání.

### Ověřeno E2E

Přihlášení heslem → cookie → `whoami` hlásí `auth: password` a roli; API bez cookie 401,
s vymyšlenou cookie 401, po odhlášení 401. Viewer: čtení 200, každý zápis 403, `users` 403,
vlastní heslo si změnit smí. Cizí `Origin` i `Sec-Fetch-Site: cross-site` na zápisu 403,
stejný origin projde. Throttling: po šesti chybných pokusech 429 i pro správné heslo,
druhý účet nedotčen. Účty: vytvoření s vygenerovaným heslem (funguje k přihlášení), výpis
neobsahuje hash ani heslo, promotion, reset, smazání; degradace posledního admina 409.
Runnery přes mTLS listener nedotčené (`checkin` funguje dál), UI se na portu API neservíruje.

**Nevykresleno v prohlížeči** — v tomhle prostředí není headless browser, takže přihlašovací
obrazovka a záložka Uživatelé jsou ověřené jen na úrovni API a servírování assetů.

---

## 17. Payload vs. log

Skript může na stdout psát dvě zcela různé věci: **log** (co se dělo) nebo **data**
(samotnou zálohu). `mysql_backup.sh` píše na stdout dump databáze — proto ho manifest
deklaruje jako `capture = "stream"`.

Dokud se obojí posílalo stejným kanálem, mělo to dva důsledky:

1. **Stránka běhů nabízela k prohlížení gigabajtový SQL dump** místo logu. Log se tím
   stal nepoužitelným a `runs/<run_id>/stdout.log` neomezeně rostl.
2. **Bylo to pomalé.** 1,2 GB dumpu trvalo ~20 minut (~1 MB/s). Cesta byla:
   stdout → chunky po 32 KiB → `RunUpdate` s base64 v ndjson → server: `MkdirAll` +
   `open` + `write` + `close` na chunk + `UPDATE runs SET bytes = bytes + ?` na chunk.
   Na 1,2 GB to je ~39 000 SQLite transakcí (WAL se `synchronous = FULL`, tedy ~39 000
   fsyncy) a stejný počet otevření souboru.

### Rozdělení

| | log (`capture = "log"`, výchozí) | data (`capture = "stream"`) |
|---|---|---|
| kanál | `POST /api/v1/runs/updates` (ndjson `RunUpdate`) | `POST /api/v1/runs/{id}/data` (raw tělo) |
| uloží se do | `runs/<run_id>/{stdout,stderr}.log` | `runs/<run_id>/data.bin` |
| ve webu | živý tail v detailu běhu | odkaz „stáhnout“, `GET …/runs/{id}/data` |
| v DB | `runs.bytes` | `runs.data_bytes` |
| strop | 4 MiB na stream | žádný |
| retence | `[storage] log_retention_*` | nikdy se nemaže automaticky |

Runner předá `cmd.StdoutPipe()` rovnou jako tělo requestu — dump tedy nikde nečeká,
nechunkuje se, neprochází JSONem a na zálohovaném hostu se nestaguje.

### Kdo rozhoduje, co je stdout

**Manifest, ne instance** (`effectiveCapture` v `internal/server/catalog.go`). Jestli
skript tiskne dump nebo průběh, je vlastnost skriptu, ne cíle, na kterém běží. Navíc
instance jsou starší než ta deklarace a nesou `capture = "stream"` i u skriptů, které
odjakživa jen tiskly text — respektovat to by znamenalo poslat výstup `hello` do
datového souboru, kde ho nikdo nehledá. Instance smí streamování jen **vypnout**
(`capture = "local"`), pro skript, který si data odloží sám.

### Nedokončený dump není záloha

Upload padá do `data.part` a přejmenuje se na `data.bin` až ve `FinishRun`, a jen když
běh skončil úspěšně. Neúspěšný běh svůj částečný soubor zahodí. Runner navíc počká na
dokončení uploadu, než ohlásí `finished`, takže verdikt nikdy nepředběhne data; když
upload selže a skript sám skončil v pořádku, běh se přesto označí za chybný — jinak by
se jako úspěšná záloha tvářilo něco, co nedorazilo.

### Zápis logu

Log zůstává na `runs/<run_id>/{stdout,stderr}.log`, ale už se pro každý chunk
neotevírá a nezavírá: handle je otevřený po dobu, kdy běh produkuje výstup (idle 5 min
ho zavře, `FinishRun` taky). Počítadlo bajtů se bufferuje a do DB jde po 2 s a při
dokončení běhu. `PRAGMA synchronous = NORMAL` (s WAL bezpečné) ruší fsync na každý
commit.

### Kompatibilita při upgradu

**Nejdřív server, pak runnery.** Starý runner proti novému serveru posílá stdout postaru
přes ndjson — funguje to, jen bez zrychlení. Nový runner proti starému serveru by dostal
na `/runs/{id}/data` 404 a běh by selhal. Auto-update pořadí drží sám.

---

## 18. Zastavení běhu a osiřelé běhy

Běh opustí stav `running` jenom tím, že to runner ohlásí. Když runner přestane
existovat uprostřed úlohy — zabitý, restartovaný systemd, reboot hosta — neohlásí to
nikdo a řádek zůstane, jako by se pořád pracovalo. Tak vypadá záloha, která „běží od
rána": ne pomalý dump, ale řádek, který nemá kdo dokončit.

### Zastavení (`cancel.go`)

Server nemůže běh zastavit sám. Komunikace je jen pull — runner volá ven, server nikdy
dovnitř — takže není co přerušit a není vidět žádný proces. Zrušení je proto příznak,
který operátor nastaví a runner si ho vyzvedne:

```
operátor → POST /api/v1/runs/{id}/cancel     nastaví cancel_requested
runner   → GET  /api/v1/runs/{id}/cancel     ptá se po dobu běhu (co 5 s)
```

Runner se během úlohy jinak neozve vůbec (úlohy spouští synchronně v checkin smyčce),
takže se ptá samostatná goroutina — `watchForCancel` v `internal/runner/loop.go`. Když
příznak uvidí, zruší kontext běhu a proces zemře.

**Zabíjí se celá procesní skupina.** Artefakt je shell skript, ale zálohu dělá jeho
potomek — `mysqldump`, nebo celá roura. Zabít jen interpret nestačí: potomci běží dál,
drží zapisovací konec rour, ze kterých runner čte, čtení tedy nikdy neskončí, `cmd.Wait`
se nevrátí a běh visí „zastavený" — a dump mezitím dál tluče do databáze, na kterou se
už nikdo nedívá. Proto `Setpgid` a signál celé skupině (`setupProcessGroup`), s
`WaitDelay` jako pojistkou proti tomu, kdo signál ignoruje.

Stav se hlásí jako `cancelled`, ne `failed`: zabitý proces je odsud k nerozeznání od
pádu a „failed" by posílalo někoho hledat závadu, která byla záměrem. Přeznačuje se jen
neúspěšný konec — úloha, která mezi požadavkem a všimnutím doběhla, vyrobila platnou
zálohu a zahodit ji by bylo pro nic.

**Nedokončený payload se zahazuje**, stejně jako u každého jiného neúspěšného běhu (§17).

### Osiřelé běhy (`reaper.go`)

Rozhodnutelné to dělá timeout. Runner ho vynucuje přes `exec.CommandContext`, takže živý
běh ho nemůže přežít; běh, který ho přežil, je tedy běh bez runnera. `CreateRun` proto
ukládá `timeout_sec` na řádek — server si ho po odeslání dispatche nesmí zapomenout.

Sweeper běží co minutu (a jednou při startu, protože běh přerušený restartem *serveru*
je nejčastější sirotek) a běh, kterému uplynulo `started_at + timeout + 5 min`, označí
jako `error` s vysvětlením. U běhu, který se nikdy nerozeběhl, se počítá od dispatche.
Řádky bez `timeout_sec` (zapsané dřív, než sloupec existoval) dostanou výchozí hodinu.

Reaper jen zapisuje, co se už stalo: nic nezabíjí (není co) a běhu, který je jen pomalý,
se nedotkne.

### Když upload nemá kam

Selhaný upload payloadu běh ukončí hned. Nechat skript doběhnout by znamenalo držet dump
na databázi kvůli bajtům, které se stejně zahazují.
