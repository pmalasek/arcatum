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
- [Rotace klíčů](#rotace-klíčů)
- [Zálohování souborů (restic)](#zálohování-souborů-restic)
- [Web UI](#web-ui)
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
| Obsahuje secrets | **ne, nikdy** | ano (šifrované v DB) |
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
ca_cert = "/central_backup/arcatum/pki/ca.pem"
cert    = "/central_backup/arcatum/pki/server.pem"
key     = "/central_backup/arcatum/pki/server.key"

[signing]
key = "/central_backup/arcatum/pki/dispatch-signing.key"

[secrets]
master_key = "/central_backup/arcatum/pki/secrets-master.key"
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

**Zneplatnění při kompromitaci.** Ve webu u runneru klikneš na **zneplatnit**:

1. Certifikát okamžitě přestane platit **všude** — checkin, hlášení výsledků i přístup
   k restic repozitáři
2. Runner přejde do stavu **`pending`**
3. Runner to při dalším checkinu pozná, zahodí certifikát **i klíč** a sám pošle novou
   žádost
4. Ty ho schválíš — nebo mu certifikát předáš ručně (`arcatum-ca runner -id <id>`)

Při podezření na kompromitaci **CA** je ve spodní části záložky Runnery tlačítko
**zneplatnit certifikáty všech runnerů**. Zastaví to zálohování, dokud runnery znovu
neschválíš.

> Rozdíl mezi **zneplatnit** a **zamítnout**: zneplatnění znamená „začni znovu" a runner
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
zavřeš, až server potvrdí, že jsou všichni přeneseni. Stav sleduje záložka **Klíče**.

| Co | Kdo to roznese | Cutover |
|---|---|---|
| master klíč secrets | nic — jen server | odebrat starý z `previous_keys` |
| podepisovací klíč úloh | runnery samy (podepsaná sada) | odebrat starý z `previous_keys` |
| certifikační autorita | runnery samy (podepsaný bundle) | zúžit bundle na novou CA |

**Master klíč secrets** — žádná distribuce, celé na serveru:

```sh
arcatum-ca master-key -dir pki -name secrets-master-2      # 1. nový klíč
# 2. server.toml: master_key = nový, previous_keys = ["…/secrets-master.key"]
# 3. restart, pak v UI „Klíče" → přešifrovat (nebo POST /api/v1/secrets/rekey)
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
  "schedule": { "frequency": "daily", "time": "01:30" }
}
```

| Parametr | Význam |
|---|---|
| `paths` | **povinné** — co zálohovat, oddělené čárkou |
| `excludes` | restic exclude vzory, oddělené čárkou |
| `tags` | další tagy snapshotu |
| `keep_last`, `keep_daily`, `keep_weekly`, `keep_monthly`, `keep_yearly` | retence (GFS) |
| `restic_password` | **povinný secret** — heslo repozitáře |

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

### Obnova dat

**Z webu** — záložka **Obnova**: vybereš instanci a snapshot, procházíš strom a stáhneš
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

Web běží na stejné adrese jako API — otevři `https://172.24.0.60:8443/`. Je **zabalený
v binárce** (`embed.FS`), takže se nic zvlášť neinstaluje a nemůže se rozejít s verzí
serveru.

Tři přehledy a detail běhu:

| Záložka | Co ukazuje |
|---|---|
| **Běhy** | historie: stav, návratový kód, přenesená data, trvání |
| **Instance** | příští běh, velikost restic repozitáře, tlačítko **spustit teď** |
| **Obnova** | snapshoty, procházení stromu, stažení souboru nebo adresáře jako `.tar` |
| **Klíče** | stav rotace všech tří klíčů, přešifrování secrets, postup migrace CA |
| **Runnery** | stav (`pending`/`approved`/`rejected`), platforma, expirace certifikátu, kdy se naposledy ohlásil; u čekajících žádostí **schválit / zamítnout**, u schválených **zneplatnit** |

Klikem na běh se otevře **detail s živým tailem výstupu** — u probíhající úlohy se log
dosypává, jak přichází. Přepínač `stdout`/`stderr` a zaškrtávátko „sledovat"
(automatické odscrollování). Přesně to, na co jsi mířil požadavkem usnadnit ladění
skriptů: spustit ručně a hned vidět, co skript píše.

Živý tail nepoužívá websockety — prohlížeč se ptá `GET /api/v1/runs/{id}/tail?offset=N`
a server pošle jen to, co od posledního dotazu přibylo. Jednodušší, přežije to odpadnutí
spojení a nepotřebuje to nic navíc na serveru.

### Přístup z prohlížeče (klientský certifikát)

Web má **stejnou ochranu jako API** — vyžaduje admin certifikát. Aby ho prohlížeč
poslal, naimportuj ho jako PKCS#12:

```sh
openssl pkcs12 -export \
  -inkey pki/admin-petr.key -in pki/admin-petr.pem -certfile pki/ca.pem \
  -out admin-petr.p12
```

Vzniklý `.p12` naimportuj do prohlížeče (Firefox: *Nastavení → Certifikáty → Vaše
certifikáty*; Chrome/Windows: dvojklik na soubor). Aby prohlížeč nehlásil neznámou
autoritu, přidej `pki/ca.pem` mezi důvěryhodné CA.

> Bez certifikátu vrátí server 401 a spojení neprojde — to je záměr, ne chyba.

Textový přehled pro shell zůstává na `/status`:

```sh
curl --cacert pki/ca.pem --cert pki/admin-petr.pem --key pki/admin-petr.key \
  https://172.24.0.60:8443/status
```

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
| `GET /api/v1/runs/{id}` | admin | detail jednoho běhu |
| `GET /api/v1/runs/{id}/output?stream=stdout\|stderr` | admin | zachycený výstup běhu |
| `GET /api/v1/runs/{id}/tail?offset=N&stream=` | admin | přírůstek výstupu — základ živého tailu |
| `GET /api/v1/runners` | admin | evidované runnery (stav, platforma, `last_seen`) |
| `GET /api/v1/whoami` | admin | tvoje identita a expirace certifikátů |
| `GET /api/v1/rotation` | admin | stav rotace všech tří klíčů |
| `POST /api/v1/secrets/rekey` | admin | přešifruje secrets aktuálním master klíčem |
| `GET /api/v1/trust` | runner / admin | podepsaná sada podepisovacích klíčů a CA bundle |
| `POST /api/v1/runners/{id}/approve` | admin | schválí žádost a podepíše certifikát |
| `POST /api/v1/runners/{id}/reject` | admin | zamítne žádost |
| `POST /api/v1/runners/{id}/revoke` | admin | zneplatní certifikát, runner → `pending` |
| `POST /api/v1/runners/revoke-all` | admin | zneplatní certifikáty všech runnerů |
| `POST /api/v1/renew` | runner | obnova certifikátu (bez schvalování) |
| `GET /api/v1/instances/{id}/repo` | admin | velikost restic repozitáře a počet snapshotů |
| `GET /api/v1/instances/{id}/snapshots` | admin | seznam snapshotů, nejnovější první |
| `GET /api/v1/instances/{id}/snapshots/{snap}/ls?path=` | admin | obsah adresáře ve snapshotu |
| `GET /api/v1/instances/{id}/snapshots/{snap}/download?path=&archive=tar` | admin | **obnova** — soubor nebo adresář jako tar |
| `/restic/{instance}/…` | runner (vlastní) / admin | restic REST backend pro souborové zálohy |
| `GET /` | admin | [web UI](#web-ui) (zabalené v binárce) |
| `GET /status` | admin | textová status stránka pro shell |

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

Nejpohodlnější cesta je [web UI](#web-ui): záložka **Instance** → **spustit teď**, pak
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

Výstup se ukládá do `backup_dir/runs/<run_id>/{stdout,stderr}.log`, takže do něj lze
kdykoli nahlédnout i přímo na serveru. Chystá se dry-run režim.

---

## Instalace runneru na zálohovaný server

Na zálohovaném serveru stačí jeden příkaz:

```sh
curl -LsSf http://172.24.0.60/arcatum_runner/install.sh | sudo sh
```

Skript stáhne binárku pro danou platformu, `ca.pem` a podepisovací veřejný klíč, vypíše
`runner.toml`, nainstaluje systemd službu a spustí ji. **Adresu serveru si odvodí z URL,
ze které se sám stáhl** — nezadáváš ji tedy dvakrát. Opakované spuštění binárku
aktualizuje, ale existující `runner.toml` nechá být.

Pak už zbývá jen **schválit hosta ve webu** (záložka Runnery). Do té doby runner
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
dist_dir = "/central_backup/arcatum/dist"       # arcatum-runner-linux-amd64, …
api_url  = "https://172.24.0.60:8443"           # kam se runner bude hlásit
ca_key   = "/central_backup/arcatum/pki/ca.key" # podepisuje schválené žádosti
```

Binárky pro publikování se sestaví takto:

```sh
GOOS=linux GOARCH=amd64 go build -o /central_backup/arcatum/dist/arcatum-runner-linux-amd64 ./cmd/runner
GOOS=linux GOARCH=arm64 go build -o /central_backup/arcatum/dist/arcatum-runner-linux-arm64 ./cmd/runner
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
- **Šifrování secrets at-rest** (AES-256-GCM, vázané na instanci a název parametru)
- **Zálohování souborů přes restic** — repozitář na serveru (vlastní restic REST backend),
  dedup a inkrementální snapshoty, izolace repozitářů mezi runnery
- **Retence (GFS)** — `forget --prune` po úspěšné záloze, omezené na vlastní snapshoty
- **Web UI** zabalené v binárce — běhy, instance, runnery, **živý tail výstupu**, „spustit teď"
- **Instalace jedním příkazem** (`install.sh`) a **enrollment** — runner si vygeneruje vlastní
  klíč, pošle jen CSR a čeká na schválení ve webu
- **Automatická obnova certifikátů** před expirací (včetně výměny klíče) a **zneplatnění
  při kompromitaci** — runner přejde do `pending` a sám požádá o nový; varování na
  blížící se expiraci ve webu
- **Rotace všech tří dlouhodobých klíčů** (master klíč secrets, podepisovací klíč úloh, CA)
  s oknem dvojí platnosti; runnery si nový trust materiál převezmou samy
- Bezpečné předání secrets (dočasný soubor, ne env), maskování v API, ověření SHA-256 artefaktu

- **Obnova z webu** — procházení snapshotů a stažení souboru či adresáře, běží na serveru
  (nezávisle na zálohovaném hostu)

**Chybí (další fáze):**
- **Obnova zpět na zálohovaný server** (dnes stáhneš data k sobě a nakopíruješ je sám)
- **Notifikace** při selhání (e-mail/Slack)
- Správa instancí přes API/web (dnes seed z JSON), auto-update runneru
- **CRL/OCSP** — [záměrně nezavedeno](#proč-ne-crlocsp)

Podrobná architektura a rozhodnutí: [docs/architecture.md](docs/architecture.md).
