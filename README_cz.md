# Arcatum

Centrální zálohovací systém pro servery ve vnitřní síti. Monorepo, jazyk Go
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
- [Rotace klíčů](#rotace-klíčů)
- [Zálohování souborů (restic)](#zálohování-souborů-restic)
- [Web UI](#web-ui)
- [Záloha konfigurace a reset serveru](#záloha-konfigurace-a-reset-serveru)
- [Jak napsat vlastní zálohovací skript](#jak-napsat-vlastní-zálohovací-skript)
- [Jak přidat instanci](#jak-přidat-instanci)
- [HTTP API](#http-api)
- [Ladění skriptů](#ladění-skriptů)
- [Instalace runneru na zálohovaný server](#instalace-runneru-na-zálohovaný-server)
- [Aktualizace runnerů](#aktualizace-runnerů)
- [Vývoj](#vývoj)
- [Návody](#návody)
- [Stav a roadmapa](#stav-a-roadmapa)

---

## Návody

README je referenční přehled. Postupy krok za krokem mají vlastní dokumenty:

| Návod | Kdy ho otevřít |
|---|---|
| [Nasazení produkční verze](docs/production_cz.md) | od čistého serveru k běžícímu Arcatum se zapnutým zabezpečením — PKI, systemd, publikování buildů, rollout runnerů, provoz, záloha samotného Arcatum |
| [Vývoj a ladění backendu](docs/backend-development_cz.md) | práce na Go kódu: lokální prostředí, tok dat jedním během, kam co přidat, testy, ladění, mTLS lokálně |
| [Vývoj a ladění skriptů](docs/script-development_cz.md) | psaní zálohovacích skriptů: manifest, předání parametrů, vývojová smyčka, katalog chyb |
| [Obnova databáze z dumpu](docs/restore_cz.md) | jak dump dostat zpět do MySQL nebo PostgreSQL, co v něm není, a zkušební obnova |

---

## Jak to funguje

Dvě komponenty:

- **arcatum-server** — centrální mozek. Drží rozvrhy, definice skriptů, databázi běhů
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

| | **Skript** (definice) | **Instance** (nasazení) | **Rozvrh** (kdy) |
|---|---|---|---|
| Co to je | šablona: kód/binárka + manifest parametrů | konkrétní nasazení na jeden cíl | kdy se to nasazení spouští |
| Kde žije | `scripts/` — verzováno v gitu | databáze (SQLite) | databáze (SQLite) |
| Obsahuje secrets | **ne, nikdy** | ano (šifrované v DB) | ne |
| Kolik jich je | jeden na druh zálohy | libovolně na skript | **libovolně na instanci** |
| Příklad | `mysql-backup` | `mysql-web01`, `mysql-web02`, … | `nightly` 02:30, `monthly full` 1. den |

Jeden skript „záloha MySQL" tak obsluhuje libovolný počet MySQL serverů — každý jako
samostatná instance s vlastními přihlašovacími údaji a databází.

Instance míří na **právě jeden runner**. Jeden runner může hostit víc instancí.
**Víc databází = víc instancí** (dá to nezávislý status a retry na každou).

**Kdy** se úloha spouští, záměrně není součástí instance. Jedna úloha často chce víc než
jeden časový plán — noční dump *a* k tomu plnou kopii prvního v měsíci — a pozastavení
nočního běhu na týden nesmí znamenat editaci samotné zálohy. Rozvrh je proto samostatná
věc a instance jich může mít víc, nebo taky žádný: instance bez rozvrhu je zcela legitimní
a spustí se, když někdo zmáčkne **run now**.

Když dva rozvrhy téže úlohy dozrají ve stejnou minutu, server odešle **jeden** běh, ne dva:
popisují tutéž práci a dva procesy v jednom repozitáři není nic, co by si zálohovací systém
měl zařídit sám.

### Tři úrovně konfigurace

1. **Definice skriptu** — `scripts/<name>/<name>.toml` (git, bez secrets)
2. **Instance a její rozvrhy** — v DB, seedované z `data/instances.json`
3. **Host-level** — `config/server.toml` a `config/runner.toml`

---

## Struktura repozitáře

```
cmd/server            binárka arcatum-server
cmd/runner            binárka arcatum-runner
cmd/arcatum-ca        správa PKI (CA, certifikáty, podepisovací klíč)
internal/server       HTTP API, scheduler, SQLite store, autorizace, restic REST backend
internal/runner       checkin smyčka, executor, ověření podpisu, orchestrace resticu
pkg/proto             zprávy protokolu + kanonická serializace pro podpis
pkg/jobspec           parser manifestu skriptu + validace
pkg/schedule          výpočet „next run" (denní/týdenní/měsíční)
pkg/config            config serveru (server.toml) i runneru (runner.toml)
pkg/crypto            PKI, mTLS konfigurace, podpisy úloh, šifrování secrets
web/                  web UI zabalené do binárky (embed.FS)
scripts/              DEFINICE skriptů — kód + manifest, bez secrets
data/                 instances.example.json
config/               server.example.toml, runner.example.toml
deploy/gen-certs.sh   vygeneruje celé PKI jedním příkazem
justfile              zkratky pro build, testy a lokální běh (viz Vývoj)
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
cp config/server.example.toml server.toml
cp data/instances.example.json data/instances.json
# v instances.json nastav "runner_id" na hostname stroje, kde poběží runner:
hostname
```

Config patří do kořene checkoutu: server hledá `./server.toml` a až pak
`/etc/arcatum/server.toml`, takže spuštění z repozitáře vezme tenhle. Do gitu nepatří
(je v `.gitignore`) — verzovaný je jen `config/server.example.toml`.

Pro lokální test uprav v `server.toml` cesty, ať se nesahá do `/opt/arcatum`
a `/central_backup`:

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

**2) Spustit server**

```sh
go run ./cmd/server -instances data/instances.json
```

V logu se při prvním startu objeví vygenerované heslo účtu `admin` — s ním se přihlásíš do
webu na `http://127.0.0.1:8080/`.

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

Nebo ve webu na `http://127.0.0.1:8080/` — **Dashboard** ukazuje, co běží a co za poslední
den selhalo; celou historii jedné úlohy najdeš přes **Instances** → úloha → **run now**,
nebo tlačítkem **history** vedle jejího rozvrhu.

Runner jako služba (bez `-once`) se hlásí opakovaně podle `poll_interval`.

> **Se `just`** je celý tenhle postup na čtyři příkazy — `just dev-init` (připraví `local/`
> s configem i seedem), `just server`, `just trigger` a `just runner-once`. Viz
> [Zkratky přes just](#zkratky-přes-just).

> Tento rychlý start běží **bez zabezpečení** (plain HTTP, žádné ověřování). Pro reálné
> nasazení pokračuj sekcí [Zabezpečení](#zabezpečení-mtls-a-podpis-úloh) nebo přímo
> návodem [Nasazení produkční verze](docs/production_cz.md).

---

## Konfigurace

### Server — `server.toml`

Hledá se `./server.toml`, pak `/etc/arcatum/server.toml`; `-config` obojí přebije. Na
produkci tedy leží v `/etc/arcatum`, ve vývoji v kořeni checkoutu — a binárka spuštěná
mimo checkout sáhne po produkční konfiguraci, tedy i po produkční PKI.

```toml
[server]
listen    = "0.0.0.0:8443"                  # API pro runnery (mTLS)
scripts   = "/opt/arcatum/scripts"          # adresář s definicemi skriptů
data_dir  = "/central_backup/arcatum/data"  # zde vzniká arcatum.db
timezone  = "Europe/Prague"                 # default TZ pro rozvrhy bez vlastní
log_level = "info"

[web]
listen      = "0.0.0.0:8080"                # web UI (plain HTTP, jméno a heslo)
session_ttl = "12h"                         # jak dlouho vydrží přihlášení bez aktivity
# secure_cookie = true                      # jen když před webem stojí HTTPS proxy

[storage]
backup_dir = "/central_backup/arcatum"      # kam se ukládají zálohovaná data

[tls]
# ca_cert / cert / key — mTLS, zapojíme později
```

Chybějící pole padají na defaulty (`pkg/config.Default`). Chybějící **soubor** ale ne:
server bez konfigurace skončí chybou, protože vestavěné defaulty znamenají plain HTTP
a hesla instancí v plaintextu — to není stav, do kterého se má spadnout překlepem v cestě.

**Dva porty, dva druhy volajících.** `[server] listen` je pro runnery a ověřuje je
certifikátem; `[web] listen` je pro lidi a ověřuje je heslem. Prázdné `[web] listen` web
vypne. Kolizi adres (dva listenery na jednom portu) config odmítne při startu, i s bootstrap
portem — jinak by jeden z nich spadl na „address already in use" a nebylo by zřejmé který.

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

Ochrana má tři nezávislé vrstvy:

1. **mTLS** — kdo je na drátě. Server i runner mají certifikát od společné Arcatum CA
   a ověřují se navzájem. Neznámý host neprojde ani TLS handshakem.
2. **Podpis úloh** — odkud pochází práce. Server podepisuje každou úlohu Ed25519 klíčem
   a runner podpis **ověří ještě před spuštěním**. Nesouhlasí-li, kód nespustí
   a nahlásí selhání zpět. Podpis pokrývá i SHA‑256 artefaktu, takže je svázán
   s konkrétním kódem.
3. **Šifrování secrets at-rest** — co leží v databázi. Hesla instancí jsou v `arcatum.db`
   šifrovaná (AES‑256‑GCM), takže kopie databáze sama o sobě žádné přihlašovací údaje
   neprozradí.

Proč to není jedna vrstva: mTLS chrání spojení, podpis chrání *úlohu*, šifrování chrání
*uložená data*. Kdyby unikl TLS klíč serveru, podepisovací klíč je jiný soubor a útočník
runneru kód nepodstrčí; kdyby unikla záloha databáze, master klíč je také jiný soubor.

**Lidé se ale hlásí jménem a heslem**, ne certifikátem — na to je [web UI](#web-ui)
a samostatný plain-HTTP port `[web] listen`. Certifikát do prohlížeče se musí vyexportovat,
naimportovat v každém počítači a po roce vyměnit; heslo je pohodlnější a u operátora, který
jen kouká na výsledky záloh, ničemu neubírá: web nesahá na klíče a *úlohy* chrání podpis,
který si server dělá sám. Runnery na certifikátech zůstávají — stroj se instaluje jednou
a certifikát je to, čím ho server pustí (nebo nepustí) už na TLS handshaku.

### Přihlášení do webu (jméno a heslo)

Účty žijí v `arcatum.db` v tabulce `users` a mají dvě role:

| Role | Co smí |
|---|---|
| `admin` | všechno — spouštět úlohy, editovat instance, schvalovat runnery, rotovat klíče, spravovat účty |
| `viewer` | jen čtení — běhy, výstupy, instance, runnery, obnova. Žádná tlačítka, která něco mění |

Ukládá se jen **PBKDF2-HMAC-SHA256 verifikátor** (600 000 iterací, samostatná sůl na
každý účet), nikdy heslo. Kopie databáze tedy nikoho nepřihlásí ani neprozradí, co si kdo
zvolil — a hádat hashe je pomalé záměrně. Přihlášení drží cookie `arcatum_session`
(`HttpOnly`, `SameSite=Strict`), v databázi je z ní jen SHA‑256, takže se ani z tabulky
sezení nedá vydávat za přihlášeného operátora.

**První účet vytvoří server sám.** Když v databázi není žádný, při startu vznikne `admin`
a jeho vygenerované heslo se **jednou** vypíše do logu:

```
  ┌─ first start: created the web account ─────────────────────
  │   user:     admin
  │   password: k4m2ftq7hn3bwzla
  │ Log in and change it (Account → change password). A forgotten
  │ password is reset with: arcatum-server -passwd admin
  └───────────────────────────────────────────────────────────
```

Další účty se přidávají z webu (záložka **Users**). Když se heslo ztratí i tomu
poslednímu adminovi, cesta zpět je ze shellu na serveru:

```sh
arcatum-server -passwd petr
#   → vypíše nové vygenerované heslo; účet vytvoří, pokud neexistuje
ARCATUM_PASSWORD='vlastní heslo' arcatum-server -passwd petr
#   → nastaví konkrétní heslo (proměnná prostředí, ať nekončí v historii shellu)
arcatum-server -passwd kolega -passwd-role viewer
```

Co web hlídá sám:

- **Změna hesla, vypnutí nebo smazání účtu okamžitě ukončí jeho sezení** — nestačí čekat,
  než vyprší cookie.
- **Posledního funkčního admina nelze smazat, vypnout ani degradovat na viewera.** Odemknout
  systém zpátky by šlo jen ze shellu, tak k tomu web nedá dojít.
- **Neúspěšná přihlášení se po pěti pokusech zdržují** (1 min, dál se zdvojnásobuje po
  15 minut) — kontrola hesla je záměrně drahá a nesmí se dát volat ve smyčce.
- **Neexistující jméno se odmítá stejně dlouho jako špatné heslo**, takže z rychlosti
  odpovědi nejde vyčíst, které účty existují.
- **Požadavky, které něco mění, musí přijít z Arcatum** (kontrola `Origin`), aby cizí
  stránka nemohla jednat cookie přihlášeného operátora.

### Šifrování secrets at-rest

Každá hodnota se šifruje samostatně, takže **názvy** secrets zůstávají čitelné (web UI
umí zobrazit, které jsou nastavené) a **hodnoty** ne. V databázi to vypadá takto:

```
"secrets": {"password": "enc:v1:VzeO2eeBNYagsYJ1HiiMlle5ERZk…"}
```

Ciphertext je kryptograficky svázán s **konkrétní instancí a názvem parametru**. Kdo umí
do databáze zapisovat, nemůže tedy zkopírovat heslo z jedné instance do druhé — ověření
selže.

> **Master klíč si zazálohuj** mimo stroj, který chrání. Jeho ztrátou se všechna uložená
> hesla stanou nečitelnými. Naopak jeho záměna se pozná okamžitě — čtení skončí chybou,
> nikoli tichým prázdným heslem.

Zapnutí šifrování na existující instalaci nic nerozbije: hodnoty uložené dříve
v plaintextu se dál načtou a při nejbližším importu instancí se zašifrují.

### Role v certifikátech

Role je v `OU` certifikátu a server podle ní dělí přístup:

| Role | Kdo | Co smí |
|---|---|---|
| `runner` | zálohovaný server | jen `checkin` a hlášení **vlastních** běhů |
| `admin` | operátor volající API ze shellu | ostatní API — spouštění úloh, výpisy, čtení výstupů |

Admin certifikát je dnes potřeba jen pro volání API na portu `[server] listen` (typicky
z `curl` nebo skriptu). Do prohlížeče ho nikdo naimportovat nemusí — web má
[vlastní přihlášení](#přihlášení-do-webu-jméno-a-heslo).

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
| `secrets-master.key` | **jen server** — šifruje uložené secrets (**zazálohovat!**) |
| `admin-petr.pem` / `.key` | tvůj počítač (přístup k API/webu) |
| `runner-web-01.pem` / `.key` | příslušný runner |

> `-H` musí obsahovat **všechny** adresy, na které se runnery připojují (IP i DNS),
> jinak ověření TLS selže. Opakované spuštění skriptu existující CA ani podepisovací
> klíč nepřepíše.

Jemnější kontrola přes `arcatum-ca` (`init`, `server`, `runner`, `admin`, `signing`,
`master-key`, `sign-csr` — poslední je základ pro budoucí enrollment, kdy si runner klíč
vygeneruje sám a posílá jen CSR):

```sh
go run ./cmd/arcatum-ca runner -dir pki -id web-02      # přidat runner
go run ./cmd/arcatum-ca admin  -dir pki -name kolega    # přidat operátora
```

Existující CA, podepisovací klíč ani master klíč se nikdy nepřepíší implicitně — příkaz
místo toho skončí chybou.

### Zapojení do konfigurace

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

### Životní cyklus certifikátů

**Automatická obnova.** Runner si sám vyžádá nový certifikát, když se blíží expirace
(30 dní předem). Nepotřebuje na to schválení — žádost jde přes mTLS, takže se prokázal
tím certifikátem, který mění. Bez toho by ti všechny runnery přestaly fungovat naráz
v den, kdy vyprší původní certifikáty. Obnova zároveň **vymění i klíč**.

Runner se pak sám restartuje, aby nový certifikát začal používat (systemd unit má
`Restart=always`).

**Zneplatnění při kompromitaci.** Ve webu u runneru klikneš na **revoke**:

1. Certifikát okamžitě přestane platit **všude** — checkin, hlášení výsledků i přístup
   k restic repozitáři
2. Runner přejde do stavu **`pending`**
3. Runner to při dalším checkinu pozná, zahodí certifikát **i klíč** a sám pošle novou
   žádost
4. Ty ho schválíš — nebo mu certifikát předáš ručně (`arcatum-ca runner -id <id>`)

Při podezření na kompromitaci **CA** je ve spodní části záložky Runners tlačítko
**revoke the certificates of all runners**. Zastaví to zálohování, dokud runnery znovu
neschválíš.

> Rozdíl mezi **revoke** a **reject**: zneplatnění znamená „začni znovu" a runner
> sám požádá o nový certifikát. Zamítnutí je „ne" — runner se pak už neozývá, aby ti
> nezaplňoval frontu žádostmi.

**Varování před expirací.** Web hlásí nahoře, když se blíží konec platnosti tvého
admin certifikátu (default **1 rok** — vyprší první), certifikátu serveru, nebo
certifikátů runnerů. Datum je i ve sloupci u každého runneru.

Obnovu certifikátů, které se neobnovují samy, uděláš takto:

```sh
go run ./cmd/arcatum-ca admin  -dir pki -name petr           # tvůj přístup k webu
go run ./cmd/arcatum-ca server -dir pki -hosts 172.24.0.60   # certifikát serveru
```

### Rotace klíčů

Všechny tři dlouhodobé klíče jde vyměnit bez zásahu na jednotlivých hostech. Postup je
u všech stejný: **okno, kdy platí starý i nový**, runnery si nové převezmou samy, a okno
zavřeš, až server potvrdí, že jsou všichni přeneseni. Stav sleduje záložka **Keys**.

| Co | Kdo to roznese | Cutover |
|---|---|---|
| master klíč secrets | nic — jen server | odebrat starý z `previous_keys` |
| podepisovací klíč úloh | runnery samy (podepsaná sada) | odebrat starý z `previous_keys` |
| certifikační autorita | runnery samy (podepsaný bundle) | zúžit bundle na novou CA |

**Master klíč secrets** — žádná distribuce, celé na serveru:

```sh
arcatum-ca master-key -dir pki -name secrets-master-2      # 1. nový klíč
# 2. server.toml: master_key = nový, previous_keys = ["…/secrets-master.key"]
# 3. restart, pak v UI „Keys" → re-encrypt (nebo POST /api/v1/secrets/rekey)
# 4. až je pending 0, odeber previous_keys a restartuj
```

Přešifrování je **bezpečné spustit opakovaně** — hodnoty už na aktuálním klíči přeskočí,
takže přerušený průběh se prostě dokončí dalším spuštěním.

**Podepisovací klíč úloh** — runnery si novou sadu vezmou samy:

```sh
arcatum-ca signing -dir pki -name dispatch-signing-2
# server.toml: [signing] key = nový, previous_keys = ["…/dispatch-signing.key"]
# restart → runnery při dalším checkinu sadu přijmou → pak odeber previous_keys
```

> `previous_keys` u `[signing]` jsou **privátní** klíče, ne veřejné. Server totiž
> publikovanou sadu podepisuje **všemi** klíči, které drží — jinak by runner, který zná
> jen starý klíč, novou sadu odmítl a rotace by se nikdy nerozjela.

**Certifikační autorita** — nejvíc kroků, protože jde o kotvu důvěry:

```sh
arcatum-ca init   -dir pki -name ca-new -cn "Arcatum CA 2026"   # 1. nová CA
arcatum-ca bundle -dir pki -out pki/ca-bundle.pem ca.pem ca-new.pem
# 2. server.toml: [tls] ca_cert = bundle; [bootstrap] ca_cert/ca_key = ca-new
#    POZOR: certifikát serveru zatím NECHAT pod starou CA
# 3. restart → runnery přijmou bundle a při obnově přejdou na novou CA
# 4. až GET /api/v1/rotation hlásí safe_to_drop_old_ca:
arcatum-ca server -dir pki -ca ca-new -hosts 172.24.0.60
arcatum-ca admin  -dir pki -ca ca-new -name petr
arcatum-ca bundle -dir pki -out pki/ca-bundle.pem ca-new.pem
```

> **Krok 2 je snadné pokazit.** Kdybys certifikát serveru vydal pod novou CA hned, runner,
> který zná jen starou, se **nepřipojí** — a tím si nemůže stáhnout bundle, který by to
> spravil. Rotace se zasekne. Server na to upozorní: pole `warning` ve stavu rotace
> a hlášení v UI.

**Co záměrně není automatické:** zavření okna. Odebrání kotvy důvěry je jediná operace,
která tě umí zamknout z vlastního systému — neobsluhovaná úloha, která to v noci pokazí,
nechá runnery, kteří nevěří ani staré, ani nové CA. Rutinní **obnova certifikátů**
naopak automatická je, protože její selhání je bezpečné: starý certifikát dál platí.

### Proč ne CRL/OCSP

Nejsou zavedené — a zvážili jsme to. Zneplatnění se vynucuje **kontrolou stavu
v databázi** na každém autorizačním bodu, což je pro uzavřený systém lepší než CRL:
platí okamžitě, nemá cache ani propagační zpoždění.

Zbývá jedna mezera: kdyby unikl **TLS klíč serveru**, runnery to samy nezjistí. Ale dopad
je omezený — podpis úloh je jiný klíč, takže útočník nedokáže podstrčit kód ke spuštění,
a pack soubory jsou šifrované heslem repozitáře. A hlavně: tuhle mezeru řeší **rotace
CA** výše, s menším aparátem než CRL infrastruktura.

### Volání API s certifikátem

```sh
curl --cacert pki/ca.pem --cert pki/admin-petr.pem --key pki/admin-petr.key \
  https://172.24.0.60:8443/api/v1/runs
```

---

## Zálohování souborů (restic)

Pro souborové zálohy Arcatum nevymýšlí vlastní formát — řídí **restic**. Deduplikace,
inkrementální snapshoty, komprese, šifrování a kontrola integrity jsou přesně ty části,
které se ladí roky.

Repozitář ale **leží na serveru**: Arcatum sám vystavuje restic REST backend, takže pack
soubory tečou ze zálohovaného hostu na server a nekupí se na něm. Na hostu zůstane jen
lokální cache resticu.

```
 zálohovaný host                          arcatum-server
   restic backup                          /restic/<instance>/
   │  pack soubory (mTLS)                 │
   │ ───────────────────────────────────► │ backup_dir/restic/<instance>/
```

Každá instance má **vlastní repozitář** a runner se dostane jen k repozitářům instancí,
které jsou cílené na něj. Jeden zálohovaný server tak nemůže číst ani poškodit zálohy
jiného.

### Předpoklady

Na zálohovaném serveru musí být `restic` (`apt install restic`). Když chybí, úloha selže
s jasnou zprávou, nikoli záhadně.

### Definice a instance

Skript typu `restic` nemá entrypoint — runner spouští restic sám podle parametrů. Ukázka:
[scripts/example/files_backup.toml](scripts/example/files_backup.toml).

```toml
name    = "files-backup"
type    = "restic"
timeout = "6h"
```

Instance pak určuje, co se zálohuje a jak dlouho se to drží:

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
  "secrets": { "restic_password": "dlouhé-náhodné-heslo" },
  "schedules": [{ "frequency": "daily", "time": "01:30" }]
}
```

| Parametr | Význam |
|---|---|
| `paths` | **povinné** — co zálohovat, oddělené čárkou |
| `excludes` | restic exclude vzory, oddělené čárkou |
| `tags` | další tagy snapshotu |
| `keep_last`, `keep_daily`, `keep_weekly`, `keep_monthly`, `keep_yearly` | retence (GFS) |
| `restic_password` | secret — heslo repozitáře; nevyplněné se uloží jako `password` |

Runner repozitář při prvním použití sám inicializuje. Snapshoty dostanou tagy `arcatum`
a `instance:<id>`.

### Retence

Když je nastavený kterýkoli `keep_*`, spustí se po **úspěšné** záloze `forget --prune`,
omezený tagem na snapshoty téhle instance. Dvě záměrná rozhodnutí: neúspěšná záloha
nikdy nesmaže staré snapshoty, a politika jedné instance nemůže zlikvidovat snapshoty
jiné. Prázdná hodnota znamená „nenastaveno", ne „nedrž nic".

> **Heslo repozitáře je nenahraditelné.** Restic ho neumí obnovit — bez něj jsou zálohy
> nečitelné. V DB je šifrované (viz [Zabezpečení](#zabezpečení-mtls-a-podpis-úloh)),
> ale kopii si ulož i mimo Arcatum.
>
> Výchozí `password` je jen výplň, aby šlo instanci založit bez vymýšlení hesla —
> repozitář jím sice zašifrovaný je, ale kdokoli se dostane k `backup_dir` na serveru,
> si ho rozšifruje. U dat, na kterých záleží, nastav vlastní.

### Obnova dat

**Z webu** — záložka **Restore**: vybereš instanci a snapshot, procházíš strom a stáhneš
jednotlivý soubor nebo celý adresář jako `.tar`.

Obnova běží **na serveru** proti repozitáři, který už tam je, a heslo si server
dešifruje sám. Runner do toho není zapojený — a to je záměr: potřeba obnovy často
znamená, že zálohovaný stroj je nedostupný, takže obnova na něm nesmí být závislá.

> Server k tomu potřebuje nainstalovaný `restic` (`apt install restic`). Bez něj vrátí
> obnova jasnou chybu.

Data se streamují přímo z repozitáře do prohlížeče (`restic dump`), takže se nikde
nestagují na disk a velký archiv začne přicházet hned.

Totéž přes API:

```sh
A=(--cacert pki/ca.pem --cert pki/admin-petr.pem --key pki/admin-petr.key)
I=https://172.24.0.60:8443/api/v1/instances/files-web01

curl "${A[@]}" $I/snapshots                                  # co je k dispozici
curl "${A[@]}" "$I/snapshots/latest/ls?path=/etc"             # procházení
curl "${A[@]}" "$I/snapshots/latest/download?path=/etc/nginx/nginx.conf" -o nginx.conf
curl "${A[@]}" "$I/snapshots/latest/download?path=/etc&archive=tar" -o etc.tar
```

Místo `latest` jde použít ID konkrétního snapshotu — tím se vracíš k datům v čase.

**Chybí:** obnova **zpět na zálohovaný server** (dnes stáhneš data k sobě a nakopíruješ
je sám). Pro plnou katastrofickou obnovu se dá pořád použít restic přímo:

```sh
cat pki/admin-petr.pem pki/admin-petr.key > /tmp/admin-combined.pem
export RESTIC_PASSWORD='dlouhé-náhodné-heslo'
R="restic -r rest:https://172.24.0.60:8443/restic/files-web01/ \
     --cacert pki/ca.pem --tls-client-cert /tmp/admin-combined.pem"

$R snapshots                       # co je k dispozici
$R ls latest                       # obsah posledního snapshotu
$R restore latest --target /tmp/obnova
$R restore latest --target /tmp/obnova --include /etc/nginx   # jen část
$R check                           # kontrola integrity repozitáře
```

Velikost repozitáře a počet snapshotů zjistíš i z API:

```sh
curl --cacert pki/ca.pem --cert pki/admin-petr.pem --key pki/admin-petr.key \
  https://172.24.0.60:8443/api/v1/instances/files-web01/repo
```

---

## Web UI

Web má **vlastní port** — otevři `http://172.24.0.60:8080/` a přihlas se jménem a heslem
(`[web] listen` v configu, viz [Přihlášení do webu](#přihlášení-do-webu-jméno-a-heslo)).
Je **zabalený v binárce** (`embed.FS`), takže se nic zvlášť neinstaluje a nemůže se rozejít
s verzí serveru.

Přehledy a detail běhu:

| Záložka | Co ukazuje |
|---|---|
| **Dashboard** | úvodní stránka: co **právě běží**, co **za posledních 24 hodin selhalo** a **nejbližší běhy** — plus proužek počtů (instance, rozvrhy a kolik jich je pozastavených, běžící, chyby, nedostupné runnery). Každý řádek někam vede: na chybující běh, nebo do historie úlohy, která se chystá běžet |
| **Instances** | skript, runner, kolik má úloha **rozvrhů** (nebo „on demand"), příští běh, velikost restic repozitáře, **run now**; klik otevře úpravu, tlačítko **new instance** |
| **Schedules** | řádek na každý rozvrh: která úloha, **kdy** běží, jeho **stav** (running / ok / failed / never / paused), **poslední** a **příští běh**; **pause** i obnovení bez ztráty nastavení, **history** dané úlohy a tlačítko **new schedule** |
| **Restore** | snapshoty, procházení stromu, stažení souboru nebo adresáře jako `.tar` |
| **Keys** | stav rotace všech tří klíčů, přešifrování secrets, postup migrace CA |
| **Runners** | stav, platforma, **verze buildu**, expirace certifikátu, kdy se naposledy ohlásil; **approve / reject / revoke** |
| **Users** | účty webu: role, stav, poslední přihlášení; **add / new password / change role / disable / delete** (jen pro roli `admin`) |
| **Administration** | [odlehlá kopie](#odlehlá-kopie), [záloha konfigurace, její obnova a vyprázdnění serveru](#záloha-konfigurace-a-reset-serveru) (jen pro roli `admin`) |

Vpravo v hlavičce je přihlášený uživatel, jeho role, **change password** a **sign out**.
Viewerovi se tlačítka, která něco mění, vůbec nezobrazí — a server je stejně odmítne (403),
takže shoda UI se skutečnými právy není otázka důvěry v prohlížeč.

**Běh se hledá u úlohy, ke které patří.** Plochý seznam všeho, co kdy server udělal, už
není: z instance, z řádku rozvrhu i po „run now" se dostaneš do **historie té úlohy**, od
nejnovějšího, s donačítáním na vyžádání. To odpovídá na otázku „jak se té záloze vede" na
jednom místě, což jeden promíchaný seznam nikdy nedělal. Plochý seznam zůstává v API
(`GET /api/v1/runs`) pro shell a stránku `/status`.

Klikem na běh se otevře **detail s živým tailem výstupu** — u probíhající úlohy se log
dosypává, jak přichází. Přepínač `stdout`/`stderr` a zaškrtávátko „follow"
(automatické odscrollování). Přesně to, na co jsi mířil požadavkem usnadnit ladění
skriptů: spustit ručně a hned vidět, co skript píše.

Web je **responzivní**. Na tabletu se formuláře a metadata složí do jednoho sloupce a
záložky se změní na posuvný pruh; na telefonu tabulky přestanou být tabulkami a každý řádek
je karta s názvy sloupců u hodnot — protože posouvat tabulku doprava, abys zjistil, jestli
noční záloha dopadla, je přesně to, co udělá UI nepoužitelným na zařízení, které máš zrovna
u sebe v sedm ráno.

Živý tail nepoužívá websockety — prohlížeč se ptá `GET /api/v1/runs/{id}/tail?offset=N`
a server pošle jen to, co od posledního dotazu přibylo. Jednodušší, přežije to odpadnutí
spojení a nepotřebuje to nic navíc na serveru.

### Přístup z prohlížeče

Nic instalovat netřeba — otevřít `http://<server>:8080/` a přihlásit se. Web je plain HTTP
a patří proto do vnitřní sítě; kdo ho chce vystavit dál, ať před něj postaví HTTPS reverse
proxy a v configu zapne `[web] secure_cookie = true`, aby cookie sezení chodila jen po HTTPS.

Port webu je záměrně jiný než port API: mTLS by prohlížeč nutil poslat klientský certifikát,
a to je přesně ta nepohodlnost, kterou přihlášení heslem odstraňuje. Runnery na port API
chodí dál s certifikátem.

Textový přehled pro shell zůstává na `/status` — na webovém portu za přihlášením, na portu
API s admin certifikátem:

```sh
curl --cacert pki/ca.pem --cert pki/admin-petr.pem --key pki/admin-petr.key \
  https://172.24.0.60:8443/status
```

---

## Odlehlá kopie

Arcatum je jinak **poslední místo, kde zálohy leží** — jeden požár, ransomware nebo
překlep v `rm` a není odkud obnovovat. Replikace posílá všechno, co server uloží, na druhý
stroj (typicky přes WireGuard) pomocí `rsync` přes `ssh`.

Zapíná se sekcí `[replica]` v `server.toml`; bez ní se nic nemění a nic nového neběží.

```toml
[replica]
enabled      = true
host         = "172.26.0.2"
user         = "arcatum"
path         = "/data"
ssh_key      = "/opt/arcatum/pki/replica-ssh.key"
known_hosts  = "/opt/arcatum/pki/replica-known_hosts"
mirror       = true      # propagovat i mazání
max_delete   = 100       # pojistka: víc smazání v jednom průchodu = odmítnout
include_keys = true      # PKI, master klíč a snapshot databáze
```

Příprava druhého stroje je v [docs/production.md](docs/production_cz.md#off-site-replika).

### Co tam odtéká

```
/data/runs/<run-id>/       dumpy databází a logy běhů
/data/restic/<instance>/   restic repozitáře
/data/config-backups/      archivy konfigurace
/data/meta/arcatum.db      konzistentní snapshot databáze (VACUUM INTO, ne kopie souboru)
/data/meta/server.toml     konfigurace serveru pro referenci
/data/keys/                PKI, podepisovací klíč, master klíč secrets (při include_keys)
```

Klíče jsou to, co dělá rozdíl mezi **bodem obnovy** a hromadou nečitelných souborů: bez
master klíče se hesla repozitářů nedají rozšifrovat a restic repozitář na replice se
neotevře. Cena za to je, že **replika je stejně citlivá jako tenhle server** — kdo se
dostane k `/data`, otevře každý repozitář a vydá certifikát libovolnému hostu. Server na
to při startu hlasitě upozorní. Drž `/data` v módu `0700` pod vyhrazeným účtem.

### Když linka vypadne

Jednotkou práce je **řádek ve frontě, ne událost**. Položka, která se nepřenesla, si své
místo drží a zkouší se dál (30 s, pak dvojnásobek do 30 min) — takže spravená linka najde
práci, jak ji nechala. Nic se nezahazuje proto, že přenos selhal.

Vidět je to na třech místech:

- **Sloupec „Offsite" u každého běhu** — `sent` / `queued` / `sending` / `error`.
  Pomlčka znamená „neví se" (replikace je vypnutá nebo je běh starší), ne poruchu.
- **Detail běhu** — kromě stavu i text poslední chyby a počet pokusů.
- **Varování nad tabulkou** na kterékoli záložce, když je replika nedostupná (s časem,
  **od kdy**) nebo se položky nedaří přenést.

Karta **Off-site replica** v Administraci ukazuje cíl, stav linky, velikost fronty a soupis
chybných položek, plus tlačítka **sync now** a **retry failed**.

### Proč to nemůže ohrozit zálohy tady

| | |
|---|---|
| Replikace `backup_dir` jen **čte** | zdrojem `rsync`u je vždy lokální cesta, cílem vždy replika — opačný směr v kódu není |
| Nedokončený dump se nepřenáší | zařazuje se až po `FinishRun`; `data.part` je navíc v `--exclude` |
| Nejvýš **jeden přenos naráz** | s `nice`/`ionice`, volitelným `--bwlimit` a tvrdým timeoutem, který zabíjí celou procesní skupinu |
| Selhání replikace **nemění stav běhu** | zapisuje se jen do vlastních tabulek; rozbitá linka neudělá z úspěšné zálohy neúspěšnou |
| Mazání se nejdřív **spočítá nanečisto** | průchod, který by smazal víc než `max_delete`, se odmítne dřív, než zmizí první soubor |
| Chybějící `rsync` nebo špatná konfigurace **nezdrží start** | podsystém zůstane nečinný, server naběhne normálně |

Poslední řádek stojí za zdůvodnění: `--max-delete` sám o sobě nestačí, protože `rsync`
maže až do limitu a teprve pak se zastaví — omezí tedy škodu, ale nezabrání jí. Proto
každému zrcadlícímu průchodu předchází `--dry-run`, který spočítá plánovaná mazání; při
překročení stropu se průchod vůbec nespustí. Odpojený svazek nebo špatný `backup_dir` tak
nemůže vyprázdnit odlehlou kopii.

Restic repozitář se posílá **na dvě fáze**: nejdřív packy a klíče, teprve pak index
a snapshoty. Přerušený přenos tak nikdy nenechá na replice snapshot odkazující na data,
která tam ještě nejsou — což je nejhorší možný stav, protože vypadá jako záloha.
Repozitář, pro jehož instanci zrovna běží úloha, se odloží (není to chyba, jen ne teď).

---

## Záloha konfigurace a reset serveru

Záložka **Administration** ve webu (jen pro roli `admin`) obsluhuje tři věci, které se týkají
Arcatum samotného, ne toho, co zálohuje.

### Stažení konfigurace

Jeden zip s tím, co dohromady tvoří nastavení serveru:

```
manifest.json     formát, čas, host, které master klíče secrets potřebují, kontrolní součty
instances.json    instance včetně secrets — v té podobě, v jaké leží v databázi
schedules.json    kdy se která instance spouští, včetně pozastavených
users.json        účty webu včetně ověřovačů hesel (PBKDF2), ne hesel samotných
runners.json      evidence runnerů: stav enrollmentu, certifikát, otisk
server.toml       kopie configu — jen pro referenci, import ji nepoužije
```

`format` v manifestu je **2**. Formát 1 — zapsaný ještě dřív, než se čas odstěhoval z
instance — jde pořád naimportovat: jeho inline rozvrhy se cestou dovnitř přesypou do
samostatných. Archiv, po kterém člověk sáhne, je ten vyexportovaný předtím, než server
zmizel, a odmítnout ho číst kvůli tomu, že se rozvržení posunulo, by celou tuhle funkci
připravilo o smysl.

Co v archivu **není**: běhy, logy, dumpy, restic repozitáře — a hlavně **žádné klíče**.
CA klíč, podepisovací klíč ani `secrets-master.key` do něj nepatří: jeden takový soubor by
odemykal všechny repozitáře a uměl vyrobit certifikát libovolnému hostu. Klíče se zálohují
zvlášť spolu s `pki/` (viz [docs/production.md](docs/production_cz.md)).

Důsledek toho rozhodnutí: **secrets cestují zašifrované**, takže archiv jde naimportovat
jen tam, kde je stejný master klíč. Server to zkontroluje předem a jinak import odmítne —
lepší než zjistit ve tři ráno, že heslo k repozitáři nejde přečíst. Na serveru bez
`[secrets] master_key` jsou secrets v databázi v plaintextu, a tedy i v archivu.

Archiv přesto obsahuje hashe hesel operátorů. **Není to veřejný soubor.**

### Obnova konfigurace

Import **nahradí celou konfiguraci**: `instances`, `schedules`, `users` a `runners` se
vyprázdní a naplní obsahem archivu, takže co v archivu není, po importu nebude ani na
serveru. Sezení se ruší
všechna — po importu se všichni včetně tebe přihlásí znovu.

Běhy, logy ani data záloh se **nemažou**. Když z konfigurace zmizí instance, její restic
repozitář zůstane ležet v `backup_dir` — nic se nemaže samo.

Postup je dvoukrokový. Nahraný archiv se nejdřív jen zkontroluje a web ukáže, **co se
změní**: co přibude, co se změní, co zmizí, plus varování na následky, které nikdo nečeká —
třeba že runner, kterého archiv nezná, přijde o přístup a bude se muset znovu naenrollovat.
Teprve pak se potvrzuje. Než se něco zapíše, server si **současnou konfiguraci sám uloží**
do `backup_dir/config-backups/` — cesta zpátky je naimportovat ten soubor.

Import odmítne archiv, který by po sobě nechal stav, ze kterého už není cesty ven:

- žádný povolený `admin` účet — nikdo by se do webu nedostal
- instance odkazující na skript, který na serveru není (server by po restartu nenaběhl)
- rozvrh, kterému scheduler nerozumí, nebo takový, co se sice rozparsuje, ale nikdy nedozraje
- rozvrh patřící instanci, kterou archiv neobsahuje — řádek, který by nikdy nic nespustil
- secrets zašifrované klíčem, který tenhle server nemá
- poškozený archiv (nesedí kontrolní součet)

`server.toml` se **nikdy neaplikuje**. Přepsat si `listen` importem znamená zamknout si
dveře; kdo ho chce změnit, udělá to ručně a s restartem po ruce.

### Vyprázdnění serveru

Smaže **všechny zálohy, dumpy, logy a celou historii běhů** — tedy `backup_dir/runs`,
`backup_dir/restic` a cache. Zůstanou klíče, uživatelé, instance i s rozvrhy a schválené runnery, takže
server je hned zase provozuschopný, jen bez čehokoli nasbíraného; číslování běhů začne
znovu od `run-1`.

Uložené konfigurační archivy (`backup_dir/config-backups`) se nemažou — jsou to cesty zpět
a reset není chvíle, kdy je zahazovat.

Jediná akce v Arcatum, která maže zálohy. Potvrzuje se slovem, běží jen s `confirm` v URL,
a **nejde vzít zpět** — data odsud nejsou nikde jinde. Dokud běží nějaká úloha, reset se
odmítne: mazat adresář, do kterého runner právě streamuje, není dobrý nápad.

---

## Jak napsat vlastní zálohovací skript

Skript = dva soubory ve `scripts/<jmeno>/`: **kód** a **manifest**.

### 1) Manifest — deklaruje parametry

```toml
# scripts/example/mysql_backup.toml
name       = "mysql-backup"
type       = "bash"            # bash | python | binary | restic (viz níže)
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
>
> **Typ `restic`** žádný skript nemá — runner řídí restic sám podle parametrů instance.
> Viz [Zálohování souborů](#zálohování-souborů-restic).

---

## Jak přidat instanci

**Z webu** — záložka **Instances** → **new instance**. Formulář se sestaví z parametrů,
které vybraný skript deklaruje, a hodnoty se proti manifestu **zvalidují při uložení**:
chybějící heslo nebo překlep v názvu parametru se pozná hned, ne až při noční záloze.

Formulář nemá časová pole: nová instance vzniká **bez rozvrhu** a spustí se, když zmáčkneš
**run now**. Rozvrh jí dej v záložce **Schedules** → **new schedule** — a klidně víc, kolik
jich úloha potřebuje.

Změny platí **okamžitě, bez restartu serveru**, jak u instance, tak u jejích rozvrhů. Hesla
se šifrují už při uložení, takže nikde nezůstávají v plaintextu.

Klik na řádek instance ji otevře k úpravě. U uloženého secretu se zobrazí `(unchanged)`;
když pole necháš prázdné, stará hodnota zůstane.

**Kopie hotové instance** — tlačítko **copy** u řádku otevře formulář předvyplněný
podle ní. Druhá databáze na stejném serveru je pak otázka dvou políček: nové `id` a jiný
název databáze. Hesla přebere server ze zdrojové instance — ven je nepustí ani do
formuláře, takže je není kde opsat.

Totéž přes API:

```sh
A=(--cacert pki/ca.pem --cert pki/admin-petr.pem --key pki/admin-petr.key)
API=https://172.24.0.60:8443/api/v1

curl "${A[@]}" $API/scripts                    # co skripty deklarují (základ formuláře)
curl "${A[@]}" -X POST -H 'Content-Type: application/json' $API/instances -d '{
  "id": "mysql-web01",
  "script": "mysql-backup",
  "runner_id": "web-01",
  "params":  { "host": "127.0.0.1", "port": "3306", "database": "shop", "user": "backup" },
  "secrets": { "password": "…" },
  "timeout": "2h"
}'
# kdy to běží je druhé, samostatné volání — a úloha jich může mít víc
curl "${A[@]}" -X POST -H 'Content-Type: application/json' $API/schedules -d '{
  "instance_id": "mysql-web01", "name": "nightly",
  "frequency": "weekly", "time": "02:30",
  "weekdays": ["mon","thu"], "timezone": "Europe/Prague"
}'
curl "${A[@]}" -X PUT    $API/instances/mysql-web01 -d '…'   # úprava
curl "${A[@]}" -X DELETE $API/instances/mysql-web01          # smazání, i s rozvrhy

# kopie: "copy_from" doplní secrety ze zdrojové instance, ať je není nutné znát
curl "${A[@]}" -X POST -H 'Content-Type: application/json' $API/instances -d '{
  "id": "mysql-web01-orders", "copy_from": "mysql-web01",
  "script": "mysql-backup", "runner_id": "web-01",
  "params":  { "host": "127.0.0.1", "port": "3306", "database": "orders", "user": "backup" },
  "secrets": { "password": "***" }
}'
```

`copy_from` platí jen pro vytvoření. Secret poslaný jako `"***"` nebo prázdný se vezme ze
zdroje, jakákoli jiná hodnota ho přepíše; secret, který požadavek vůbec nezmíní, se
nepřebírá (uplatní se default z manifestu, nebo to neprojde validací). Všechno ostatní je
vždy to, co přišlo v požadavku — kopie tedy může běžet na jiném runneru. **Rozvrhy se
nekopírují**: kopie vzniká bez nich a její časový plán je samostatné rozhodnutí.

> **Smazání instance nemaže zálohy.** Odstraní se konfigurace a s ní i její rozvrhy — rozvrh,
> který přežije svou úlohu, je řádek, co nikdy nic nespustí. Restic repozitář zůstane na
> disku; když ho chceš opravdu zahodit, smaž ho ručně z `backup_dir/restic/<instance>/`.

### Rozvrhy

```sh
curl "${A[@]}" $API/schedules                              # všechny, se stavem a příštím během
curl "${A[@]}" $API/instances/mysql-web01/schedules        # jen té jedné úlohy
curl "${A[@]}" -X PUT $API/schedules/sch-1 -d '{"enabled": false}'   # pozastavit
curl "${A[@]}" -X DELETE $API/schedules/sch-1                        # smazat
```

`frequency` je `daily` | `weekly` | `monthly`; `weekdays` platí pro `weekly`, `day` (1–28)
pro `monthly` — nikdy víc než 28, jinak by rozvrh přeskočil únor. `timezone` je nepovinná,
jinak platí default ze `server.toml`. `name` je popisek, aby šlo víc rozvrhů jedné úlohy
rozlišit. Frekvenci, kterou server nezná, **odmítne při uložení** — místo aby ji přijal do
rozvrhu, který pak potichu nikdy nedozraje.

**Pozastavení** (`enabled: false`) drží definici a přestane dozrávat. Použij ho radši než
smazat a za týden napsat znovu: přesně tak se rozvrh vrátí nenápadně jiný, než jaký fungoval.

Každý rozvrh hlásí **stav**: `running` (běh té úlohy je rozjetý), `ok`, `failed`, `never`
(ještě neběžel) nebo `paused`. Běhy si pamatují, který rozvrh je způsobil, takže historie
úlohy řekne, jestli šlo o noční nebo měsíční — a ruční „run now" nepatří žádnému rozvrhu a
nikdy se nevydává za jeho výsledek.

### Seed soubor `data/instances.json`

Zůstává jako **počáteční** naplnění: při startu se z něj vytvoří jen instance, které
ještě neexistují. Existující se **nepřepisují**, jinak by restart serveru pokaždé vrátil
změny udělané z webu. Vynutit přepsání jde přepínačem `-import-force`.

Položka může nést rozvrhy, se kterými instance vzniká:

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

Starší jednotné `"schedule": { … }` se pořád přijímá, takže seed soubor ležící na existujícím
serveru se kvůli tomu nemusí přepisovat.

> **Rozvrhy se uplatní jen na instanci, kterou import skutečně založil.** U instance, která
> už existuje, se nestane nic — záměrně, protože jinak by každý restart znovu vytvořil rozvrh,
> který operátor smazal. S `-import-force` se rozvrhy instance seedem **nahradí**, ne doplní.

Pozor: soubor obsahuje hesla v plaintextu, proto je v `.gitignore`. Když instance
spravuješ z webu, můžeš ho po naplnění klidně smazat.

---

## HTTP API

API je na dvou portech a **stejné operátorské endpointy jsou na obou** — liší se jen tím,
čím se volající prokáže:

| Port | Kdo tam chodí | Čím se ověří |
|---|---|---|
| `[server] listen` (mTLS) | runnery a volání ze shellu | certifikát (`OU` = `runner`/`admin`) |
| `[web] listen` (plain HTTP) | web UI a lidé | cookie sezení po přihlášení jménem a heslem |

Sloupec „role" tedy znamená: **runner** = certifikát runneru; **admin** = admin certifikát,
nebo přihlášený uživatel s rolí `admin`; **čtení** = totéž plus role `viewer`. Bez `[tls]`
se na portu API nekontroluje nic (vývojový režim); přihlášení na webovém portu platí vždy.

| Metoda a cesta | Role | Účel |
|---|---|---|
| `POST /api/v1/checkin` | runner | runner se hlásí, dostane úlohy k spuštění |
| `POST /api/v1/runs/updates` | runner | příjem ndjson streamu průběhu a **logu** |
| `POST /api/v1/runs/{id}/data` | runner | příjem **payloadu zálohy** (surové tělo, jeden request) |
| `POST /api/v1/instances/{id}/run` | admin | **manuální spuštění** („spusť teď") |
| `POST /api/v1/runs/{id}/cancel` | admin | **zastavení běhu** — runner ho vyzvedne do pár sekund |
| `GET /api/v1/runs/{id}/cancel` | runner | dotaz běžící úlohy, jestli má skončit |
| `GET /api/v1/dashboard` | čtení | přehled v jednom požadavku: počty, co běží, chyby za 24 h, nejbližší běhy |
| `GET /api/v1/stats?days=N` | čtení | pohled za období: jak dopadlo posledních N dní (výchozí 7, nejvýš 90) — souhrny, jeden kbelík na den, rozpad po instancích a co je právě na disku |
| `GET /api/v1/instances` | čtení | instance s `next_run` (nejbližší přes jejich **povolené** rozvrhy) a počtem rozvrhů (secrets maskované) |
| `POST /api/v1/instances` | admin | vytvoří instanci (validuje se proti manifestu) — bez času, ten je rozvrh |
| `PUT /api/v1/instances/{id}` | admin | upraví instanci |
| `DELETE /api/v1/instances/{id}` | admin | smaže instanci **i s jejími rozvrhy** (zálohy zůstanou) |
| `GET /api/v1/schedules` | čtení | všechny rozvrhy se stavem, posledním a příštím během |
| `POST /api/v1/schedules` | admin | přidá instanci rozvrh |
| `GET /api/v1/schedules/{id}` | čtení | jeden rozvrh |
| `PUT /api/v1/schedules/{id}` | admin | upraví rozvrh, nebo ho pozastaví přes `{"enabled": false}` |
| `DELETE /api/v1/schedules/{id}` | admin | smaže rozvrh (úloha i zálohy zůstanou) |
| `GET /api/v1/instances/{id}/schedules` | čtení | rozvrhy jedné úlohy |
| `GET /api/v1/instances/{id}/runs?limit=&offset=` | čtení | **historie běhů jedné úlohy**, nejnovější první, s `has_more` pro další stránku |
| `GET /api/v1/scripts` | čtení | skripty a jejich deklarované parametry |
| `GET /api/v1/runs?limit=N` | čtení | plochá historie běhů, nejnovější první — pro shell a `/status`; web čte historii po úlohách |
| `GET /api/v1/runs/{id}` | čtení | detail jednoho běhu |
| `GET /api/v1/runs/{id}/output?stream=stdout\|stderr` | čtení | zachycený výstup běhu |
| `GET /api/v1/runs/{id}/tail?offset=N&stream=` | čtení | přírůstek výstupu — základ živého tailu |
| `GET /api/v1/runs/{id}/data` | čtení | stažení payloadu zálohy (jen po úspěšném běhu) |
| `GET /api/v1/instances/{id}/dumps` | čtení | uložené dumpy instance — obdoba snapshotů pro databáze |
| `GET /api/v1/runners` | čtení | evidované runnery (stav, platforma, `last_seen`) |
| `GET /api/v1/install` | čtení | příkaz, kterým se instaluje nový runner (adresa se skládá z hostu dotazu a bootstrap portu) |
| `GET /api/v1/whoami` | čtení | kdo jsi, jak jsi se přihlásil, expirace certifikátů |
| `GET /api/v1/rotation` | čtení | stav rotace všech tří klíčů |
| `POST /api/v1/secrets/rekey` | admin | přešifruje secrets aktuálním master klíčem |
| `GET /api/v1/replica` | čtení | stav [odlehlé kopie](#odlehlá-kopie): dostupnost linky, fronta, chybné položky |
| `POST /api/v1/replica/sync` | admin | zařadí úplný průchod hned, bez čekání na sweep |
| `POST /api/v1/replica/retry` | admin | vrátí chybné položky do fronty a přeskočí backoff |
| `GET /api/v1/config/export` | admin | [záloha konfigurace](#záloha-konfigurace-a-reset-serveru) jako zip (bez klíčů a bez dat záloh) |
| `POST /api/v1/config/import` | admin | archiv v těle requestu; **bez** `?confirm=replace-all` jen vrátí, co by změnil |
| `GET /api/v1/reset` | admin | co by [vyprázdnění serveru](#vyprázdnění-serveru) smazalo |
| `POST /api/v1/reset?confirm=delete-all-backups` | admin | smaže všechny zálohy, dumpy, logy a historii běhů |
| `GET /api/v1/trust` | runner / admin | podepsaná sada podepisovacích klíčů a CA bundle |
| `GET /api/v1/update` | runner / admin | podepsaný manifest publikovaných buildů runneru |
| `GET /api/v1/update/{name}` | runner / admin | binárka runneru (jen přes mTLS) |
| `POST /api/v1/runners/{id}/approve` | admin | schválí žádost a podepíše certifikát |
| `POST /api/v1/runners/{id}/reject` | admin | zamítne žádost |
| `POST /api/v1/runners/{id}/revoke` | admin | zneplatní certifikát, runner → `pending` |
| `POST /api/v1/runners/revoke-all` | admin | zneplatní certifikáty všech runnerů |
| `POST /api/v1/renew` | runner | obnova certifikátu (bez schvalování) |
| `GET /api/v1/instances/{id}/repo` | čtení | velikost restic repozitáře a počet snapshotů |
| `GET /api/v1/instances/{id}/snapshots` | čtení | seznam snapshotů, nejnovější první |
| `GET /api/v1/instances/{id}/snapshots/{snap}/ls?path=` | čtení | obsah adresáře ve snapshotu |
| `GET /api/v1/instances/{id}/snapshots/{snap}/download?path=&archive=tar` | čtení | **obnova** — soubor nebo adresář jako tar |
| `/restic/{instance}/…` | runner (vlastní) / admin | restic REST backend pro souborové zálohy |
| `GET /status` | čtení | textová status stránka pro shell |

Jen na **webovém portu** (`[web] listen`) — přihlášení a účty:

| Metoda a cesta | Role | Účel |
|---|---|---|
| `POST /api/v1/login` | — | přihlášení `{username, password}`, nastaví cookie sezení |
| `POST /api/v1/logout` | — | ukončí sezení a cookie zneplatní |
| `POST /api/v1/password` | čtení | změna **vlastního** hesla `{current, new}`; ukončí všechna sezení |
| `GET /api/v1/users` | admin | seznam účtů (nikdy hesla ani hashe) |
| `POST /api/v1/users` | admin | nový účet; bez hesla ho server vygeneruje a jednou vrátí |
| `PUT /api/v1/users/{name}` | admin | role, vypnutí/zapnutí, nové heslo (`generate_password`) |
| `DELETE /api/v1/users/{name}` | admin | smaže účet |
| `GET /` | — | [web UI](#web-ui) (zabalené v binárce; přihlášení řeší až API výše) |

Na **bootstrap portu** (plain HTTP, viz [instalace runneru](#instalace-runneru-na-zálohovaný-server))
běží jen tohle — dostupné i bez certifikátu, protože nový host žádný nemá:

| Metoda a cesta | Účel |
|---|---|
| `GET /arcatum_runner/install.sh` | instalátor, generovaný s adresou serveru |
| `GET /arcatum_runner/arcatum-runner-<os>-<arch>` | binárka runneru |
| `GET /arcatum_runner/ca.pem`, `…/dispatch-signing.pub` | veřejné trust materiály |
| `POST /api/v1/enroll` | podání žádosti o certifikát (CSR) |
| `GET /api/v1/enroll/{id}` | vyzvednutí podepsaného certifikátu |

Hodnoty secrets API **nikdy nevrací** (jen názvy, maskované `***`). Skutečné hodnoty
opouštějí server pouze v úloze doručené vlastnímu runneru.

Runner smí hlásit průběh jen u běhů, které byly přiděleny jemu — jeden zálohovaný
server tak nemůže přepsat výsledky jiného.

---

## Ladění skriptů

Nejpohodlnější cesta je [web UI](#web-ui): záložka **Instances** → **run now**, což tě
přepne do historie té úlohy — pak
klik na běh a sleduješ živý tail výstupu. Ze shellu totéž:

```sh
# 1) spustit hned, bez čekání na rozvrh
curl -X POST http://127.0.0.1:8443/api/v1/instances/hello-demo/run

# 2) runner jednorázově, s logem v terminálu
go run ./cmd/runner -server http://127.0.0.1:8443 -once

# 3) přečíst přesně to, co skript vypsal
curl http://127.0.0.1:8443/api/v1/runs/run-1/output
curl "http://127.0.0.1:8443/api/v1/runs/run-1/output?stream=stderr"
```

Se [`just`](#zkratky-přes-just) je to `just trigger hello-demo`, `just runner-once`
a `just run-output 1` (recept přijme `run-1` i holé číslo — přes API je správný tvar
`run-1`, protože z ID se skládá cesta k logu).

Výstup se ukládá do `backup_dir/runs/<run_id>/{stdout,stderr}.log`, takže do něj lze
kdykoli nahlédnout i přímo na serveru. Chystá se dry-run režim.

**Log a data nejsou totéž.** Skript, který má v manifestu `capture = "stream"` (třeba
`mysql-backup`), píše na stdout samotný dump — ten do logu nepatří a neputuje tam.
Uloží se vedle něj jako `runs/<run_id>/data.bin` a ve webu se nabídne ke stažení, kdežto
log obsahuje jen jednu shrnující řádku a stderr. Logy mají strop 4 MiB na stream a mažou
se podle `[storage] log_retention_success` / `log_retention_failed`. Detaily
v [architektuře, §17](docs/architecture_cz.md).

**Dumpy se rotují, nededuplikují.** Databázová záloha je jeden artefakt, který se
obnovuje celý, takže se nedává do resticu — drží se posledních N (`keep_last`) a všechno
mladší než D dnů (`keep_days`), obojí nastavené **na instanci**. Nula u obou znamená
držet všechno; formulář nové instance předvyplňuje 7. Mazání běží hned po úspěšné záloze
a pro jistotu ještě jednou za hodinu. Viz [§19](docs/architecture_cz.md).

Celá vývojová smyčka včetně spuštění skriptu nasucho mimo Arcatum a katalogu chybových
zpráv: [Vývoj a ladění skriptů](docs/script-development_cz.md).

---

## Instalace runneru na zálohovaný server

Na zálohovaném serveru stačí jeden příkaz:

```sh
curl -LsSf http://172.24.0.60/arcatum_runner/install.sh | sudo sh
```

> Přesné znění včetně adresy tohohle serveru najdeš i ve web UI: záložka **Runners** →
> **Add runner**. Je to tatáž stránka, na které runner pak schvaluješ.

Skript stáhne binárku pro danou platformu, `ca.pem` a podepisovací veřejný klíč, vypíše
`runner.toml`, nainstaluje systemd službu a spustí ji. **Adresu serveru si odvodí z URL,
ze které se sám stáhl** — nezadáváš ji tedy dvakrát. Opakované spuštění binárku
aktualizuje, ale existující `runner.toml` nechá být.

Pak už zbývá jen **schválit hosta ve webu** (záložka Runners). Do té doby runner
opakovaně dotazuje a nic nedělá — to je v pořádku.

```sh
systemctl status arcatum-runner
journalctl -u arcatum-runner -f
```

### Jak runner získá certifikát (enrollment)

Privátní klíč **nikdy neopustí zálohovaný server**:

1. Runner si při prvním startu vygeneruje vlastní klíč a pošle jen **žádost o podpis** (CSR)
2. Server ji zapíše jako **`pending`** — nic jí zatím nevěří a žádnou práci nepřidělí
3. Ty ji schválíš ve webu; vidíš přitom **IP adresu a fingerprint žádosti**, takže poznáš,
   že jde o pravý host
4. Server CSR podepíše a runner si certifikát vyzvedne
5. Od té chvíle jde všechno přes mTLS

Schválení je hlavní bezpečnostní pojistka. Podvržená žádost bez tvého kliknutí nic nezmůže,
a **u už schváleného runneru server další žádost odmítne** (HTTP 409) — nikdo ti tedy
nemůže přepsat certifikát běžícího hosta. Zamítnutí runneru ve webu ho odřízne okamžitě,
i kdyby ještě držel platný certifikát.

### Co k tomu server potřebuje

Bootstrap běží na **samostatném plain-HTTP portu**. Nemůže sdílet ten hlavní: mTLS
listener vyžaduje klientský certifikát a nový host žádný nemá — spojení by neprošlo už
při handshaku.

```toml
# server.toml
[bootstrap]
listen   = "0.0.0.0:80"
dist_dir = "/opt/arcatum/dist"       # arcatum-runner-linux-amd64, …
api_url  = "https://172.24.0.60:8443"           # kam se runner bude hlásit
ca_key   = "/opt/arcatum/pki/ca.key" # podepisuje schválené žádosti
```

Binárky pro publikování se sestaví takto:

```sh
GOOS=linux GOARCH=amd64 go build -o /opt/arcatum/dist/arcatum-runner-linux-amd64 ./cmd/runner
GOOS=linux GOARCH=arm64 go build -o /opt/arcatum/dist/arcatum-runner-linux-arm64 ./cmd/runner
```

Bootstrap port vydává **jen** `install.sh`, binárky, `ca.pem`, podepisovací veřejný klíč
a enrollment endpointy. Nic z toho není tajné a administrátorské API tam není dostupné.

> **Pozor na `curl … | sh`:** zálohovaný server si spustí jako root skript stažený ze
> sítě. Přes plain HTTP ho může kdokoli s přístupem k provozu vnitřní sítě vyměnit. Pro
> interní síť Xtuning je to běžný kompromis; kdo chce víc, může `ca.pem` rozdat předem
> (např. konfiguračním nástrojem) a stahovat přes plně ověřené HTTPS.

### Ruční vydání certifikátu

Enrollment nepotřebuješ, když certifikát vydáš sám — pak stačí soubory nakopírovat
a runner se enrollmentem vůbec nezabývá:

```sh
go run ./cmd/arcatum-ca runner -dir pki -id web-01
```

---

## Aktualizace runnerů

Runnery se aktualizují samy. Publikování je zkopírovat binárky do `dist_dir` a napsat
vedle nich verzi:

```sh
V=2026.07.26
for A in amd64 arm64; do
  GOOS=linux GOARCH=$A go build -ldflags "-X arcatum/pkg/version.Version=$V" \
    -o /opt/arcatum/dist/arcatum-runner-linux-$A ./cmd/runner
done
echo "$V" > /opt/arcatum/dist/VERSION
```

Se [`just`](#zkratky-přes-just) totéž jedním příkazem — postaví obě architektury i soubor
`VERSION`:

```sh
V=2026.07.26 just dist-runner /opt/arcatum/dist
```

Runner při dalším checkinu zjistí, že běží na starší verzi, novou stáhne, nahradí sám
sebe a restartuje se. Aktuální verze každého hostu je vidět v záložce **Runners** —
podle toho poznáš, jak daleko je rozjezd.

**Bez `VERSION` se nic nenabízí.** Binárky v adresáři samy o sobě aktualizaci nespustí;
verze je to, co říká „tohle je vydané".

### Proč je to bezpečné

Nahrazení vlastní binárky je nejrizikovější věc, kterou runner dělá — špatná nebo
podvržená aktualizace rozbije (nebo ovládne) všechny zálohované servery naráz. Proto:

- **Manifest je podepsaný podepisovacím klíčem úloh.** Publikovat build tedy vyžaduje ten
  klíč, ne jen kontrolu nad serverem — a hlavně ne přístup k plain-HTTP bootstrapu.
- **Binárka se stahuje přes mTLS**, nikdy z bootstrap portu, a její **SHA‑256 se ověří**
  proti podepsanému manifestu, teprve pak se cokoli přepisuje.
- **Nová binárka se zapíše vedle a přejmenuje** (atomicky), předchozí zůstane jako
  `.old` pro diagnostiku.
- **Vývojový build se neaktualizuje** — binárka bez vypálené verze hlásí `dev` a nechá se
  na pokoji.
- **Jeden pokus na verzi.** Když se po restartu verze nezmění, runner to nezkouší znovu
  a nahlásí to do logu — rozbitý build tedy nedokáže hosta uvrhnout do restart smyčky.

### Přišpendlení hosta

Když chceš mít nějaký server na pevné verzi:

```toml
# runner.toml
[runner]
auto_update = false
```

Pak ho aktualizuješ ručně opakovaným `install.sh`.

> **Po rotaci podepisovacího klíče** je `dispatch-signing.pub` na hostu zastaralý —
> autoritou je stažená sada v `data_dir/pki/signing-keys.pem`. Kdyby se ta sada ztratila
> (přeinstalace disku, omylem smazaná), runner nedokáže nic ověřit a odmítne pracovat.
> Náprava je stáhnout aktuální klíč z bootstrapu:
> `curl -LsSf http://172.24.0.60/arcatum_runner/dispatch-signing.pub -o <data_dir>/pki/dispatch-signing.pub`

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

### Zkratky přes `just`

V kořeni repozitáře je `justfile` — [just](https://just.systems) je task runner, tedy
„makefile bez závislostí na `make`". Je **nepovinný**: každý recept je jen obal nad
`go`/`curl` příkazy z tohohle README, takže bez `just` se obejdeš, jen si víc napíšeš.

```sh
cargo install just     # nebo: apt install just
just                   # vypíše všechny recepty i s popisem
```

**Build a kontroly**

| Recept | Co dělá |
|---|---|
| `just build` | `go build ./...` |
| `just build-all` | binárky serveru, runneru a `arcatum-ca` do `./bin` |
| `just release` | totéž, ale s verzí vypálenou přes `-ldflags` |
| `just dist-runner [dir]` | runner pro `linux/amd64` i `arm64` + soubor `VERSION` (default `local/dist`) |
| `just bundle` | balík pro produkci: binárky, runnery, `scripts/` a instalátor v jednom `bin/arcatum-<verze>.tar.gz` |
| `just test` / `just test-race` / `just vet` | testy, testy s race detektorem, `go vet` |
| `just fmt` | `gofmt -w` nad celým stromem |
| `just check` | gofmt + vet + test + build — co má projít před odesláním změny |
| `just clean` | smaže `bin/` a `local/dist` (data ani zálohy nesahá) |

**Lokální běh a ladění**

| Recept | Co dělá |
|---|---|
| `just dev-init` | vytvoří `local/{data,backup}`, `local/server.toml` a `local/instances.json`, pokud chybí (a doplní do seedu hostname) |
| `just server` | spustí server nad `local/` configem |
| `just runner-once` / `just runner` | jeden cyklus runneru, nebo běh jako služba |
| `just passwd [user]` | změní heslo účtu webu a vypíše ho (default `admin`) |
| `just user-add <user> [role]` | vytvoří účet webu a vypíše jeho heslo (default role `viewer`) |
| `just trigger [instance]` | vynutí spuštění instance (default `hello-demo`) |
| `just runs`, `just instances`, `just runners`, `just status` | přehledy z API |
| `just run-output <id> [stream]` | zachycený výstup běhu (přijme `run-1` i `1`) |
| `just run-tail <id> [offset]` | přírůstek výstupu — totéž, co používá živý tail |

**PKI pro lokální vývoj**

| Recept | Co dělá |
|---|---|
| `just dev-certs [hosts] [admin]` | celé PKI do `local/pki` (default `127.0.0.1`, admin `dev`) |
| `just dev-runner-cert [id]` | certifikát runneru z `local/pki` (default hostname stroje) |
| `just ca <args…>` | libovolný příkaz `arcatum-ca`, např. `just ca admin -dir local/pki -name kolega` |

Chování se ladí proměnnými prostředí, ne úpravou souboru:

```sh
GO=/usr/local/go/bin/go just build          # Go mimo PATH
V=2026.07.26 just release                   # verze do binárky (default: dnešní datum)
SERVER_URL=https://127.0.0.1:8443 just runs # jiný cíl API
SERVER_CONFIG=local/server-mtls.toml just server
LISTEN=0.0.0.0:8443 just dev-init           # dostupné i z jiného stroje (viz níže)
WEB_LISTEN=0.0.0.0:8080 just dev-init       # totéž pro web UI
ARCATUM_PASSWORD=tajneheslo just passwd petr # konkrétní heslo místo vygenerovaného
ARCATUM_PASSWORD=tajneheslo just user-add kolega viewer # totéž při zakládání účtu
```

> Vývojový config naslouchá jen na `127.0.0.1`, takže z jiného stroje se na server
> nepřipojíš a v jeho logu po tom nezůstane žádná stopa. `0.0.0.0` ale znamená plain HTTP
> **bez ověřování volajícího** — pro víc než pokus zapni
> [zabezpečení](#zabezpečení-mtls-a-podpis-úloh).

Recepty, které berou argument, ho přijímají pozičně: `just trigger mysql-web01`,
`just run-output 42`, `just dist-runner /opt/arcatum/dist`.

Podrobněji — lokální prostředí, tok dat jedním během, kam přidat endpoint / sloupec /
typ skriptu, testy a ladění: [Vývoj a ladění backendu](docs/backend-development_cz.md).

---

## Stav a roadmapa

**Hotovo:**
- Pull protokol end-to-end: checkin → doručení úlohy → spuštění → stream výstupu na server
- Rozvrhy jako samostatná entita: víc na jednu úlohu (denní/týdenní/měsíční), pozastavitelné, plus manuální trigger
- Persistence v SQLite (instance, rozvrhy, běhy, evidence runnerů) — přežije restart
- Tři úrovně konfigurace, manifest s deklarací parametrů
- **mTLS** mezi serverem a runnery, identita a role z certifikátu, PKI nástroje
- **Podpis úloh** (Ed25519) — runner ověřuje před spuštěním, jinak odmítne
- **Šifrování secrets at-rest** (AES-256-GCM, vázané na instanci a název parametru)
- **Zálohování souborů přes restic** — repozitář na serveru (vlastní restic REST backend),
  dedup a inkrementální snapshoty, izolace repozitářů mezi runnery
- **Retence (GFS)** — `forget --prune` po úspěšné záloze, omezené na vlastní snapshoty
- **Web UI** zabalené v binárce, **responzivní** — dashboard, instance, rozvrhy, historie běhů po úlohách, runnery, **živý tail výstupu**, „run now"
- **Přihlášení do webu jménem a heslem** na vlastním portu, role `admin`/`viewer`, správa
  účtů z webu (PBKDF2 hashe, sezení v cookie); runnery zůstávají na certifikátech
- **Instalace jedním příkazem** (`install.sh`) a **enrollment** — runner si vygeneruje vlastní
  klíč, pošle jen CSR a čeká na schválení ve webu
- **Automatická obnova certifikátů** před expirací (včetně výměny klíče) a **zneplatnění
  při kompromitaci** — runner přejde do `pending` a sám požádá o nový; varování na
  blížící se expiraci ve webu
- **Rotace všech tří dlouhodobých klíčů** (master klíč secrets, podepisovací klíč úloh, CA)
  s oknem dvojí platnosti; runnery si nový trust materiál převezmou samy
- **Správa instancí z webu/API** — formulář z deklarace parametrů, validace při uložení,
  změny platí bez restartu, hesla šifrovaná od uložení
- **Auto-update runneru** — podepsaný manifest buildů, stažení přes mTLS, ověření SHA-256
- Bezpečné předání secrets (dočasný soubor, ne env), maskování v API, ověření SHA-256 artefaktu

- **Obnova z webu** — procházení snapshotů a stažení souboru či adresáře, běží na serveru
  (nezávisle na zálohovaném hostu)
- **Záloha a obnova konfigurace jedním souborem** — instance, účty a runnery jako zip, import
  s náhledem změn a automatickou zálohou předchozího stavu; klíče se záměrně nepřenášejí
- **Vyprázdnění serveru** — smazání všech záloh, dumpů, logů a historie se zachováním
  konfigurace
- **[Odlehlá kopie](#odlehlá-kopie)** — všechno, co server uloží, odtéká `rsync`em přes
  `ssh` na druhý stroj; výpadek linky je vidět u každého běhu i jako varování a fronta se
  po nápravě sama dožene

**Chybí (další fáze):**
- **Obnova zpět na zálohovaný server** (dnes stáhneš data k sobě a nakopíruješ je sám)
- **Notifikace** při selhání (e-mail/Slack) a **dry-run** režim
- **CRL/OCSP** — [záměrně nezavedeno](#proč-ne-crlocsp)

Podrobná architektura a rozhodnutí: [docs/architecture.md](docs/architecture_cz.md).
