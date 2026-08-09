# Návod: vývoj a ladění backendu

Praktický návod pro práci na Go kódu serveru a runneru: jak si postavit lokální
prostředí, kudy tečou data, kam přidat novou věc, jak to otestovat a jak ladit, když to
nefunguje.

Zpětné souvislosti a *proč* jsou věci tak, jak jsou, patří do [architecture.md](architecture_cz.md).
Tenhle dokument je o *jak*.

- [1. Prostředí a základní příkazy](#1-prostředí-a-základní-příkazy)
- [2. Lokální vývojová smyčka](#2-lokální-vývojová-smyčka)
- [3. Kudy tečou data](#3-kudy-tečou-data)
- [4. Kam co přidat](#4-kam-co-přidat)
- [5. Testy](#5-testy)
- [6. Ladění](#6-ladění)
- [7. Vývoj se zapnutým mTLS](#7-vývoj-se-zapnutým-mtls)
- [8. Web UI](#8-web-ui)
- [9. Pasti, které stojí čas](#9-pasti-které-stojí-čas)
- [10. Než pošleš změnu](#10-než-pošleš-změnu)

---

## 1. Prostředí a základní příkazy

Go v tomhle prostředí není na `PATH`:

```sh
export PATH=/usr/local/go/bin:$PATH
```

```sh
go build ./...          # kompilace všeho
go vet ./...            # statická kontrola
go test ./...           # testy (dnes projdou čisté)
go test -race ./...     # data race detektor — hlavně pro executor a scheduler
gofmt -l .              # co není naformátované
```

**Task runner `just`** (nepovinný, `cargo install just` nebo `apt install just`) má tyhle
příkazy jako recepty v kořenovém `justfile`. `just` bez argumentu vypíše všechny:

```sh
just build     just vet     just test     just test-race     just fmt
just check                  # gofmt + vet + test + build, tj. celá brána z §10
just build-all              # binárky do ./bin
just release                # totéž s verzí vypálenou přes -ldflags (V=…)
just clean                  # smaže bin/ a local/dist
```

Recepty jsou tenký obal — nic, co by nešlo napsat ručně. Volají `go` z `PATH`; když ho
tam nemáš, předej cestu proměnnou místo exportu:

```sh
GO=/usr/local/go/bin/go just test     # gofmt se odvodí jako <GO>fmt, přebít jde GOFMT=
```

`just check` na rozdíl od `gofmt -l .` **selže**, když něco není naformátované — `gofmt`
sám vrací nulu a jen vypíše seznam, což se v CI i v rychlém běhu snadno přehlédne.

Modul se jmenuje `arcatum`, takže importy jsou `arcatum/internal/server`, `arcatum/pkg/proto`, …
Závislosti jsou záměrně minimální: `BurntSushi/toml` a `modernc.org/sqlite`. **Server běží
bez CGO** — SQLite je čistě v Go, výsledkem je statický binár. Přidat závislost, která CGO
vyžaduje, tuhle vlastnost zruší.

Konvence: **komentáře a identifikátory anglicky, dokumentace česky i anglicky.** Každý
dokument existuje ve dvou verzích — `*_cz.md` (česky) a holý název (anglicky) — a obě se
udržují v souladu v rámci jedné změny. Komentáře vysvětlují *proč*, ne co řádek dělá — drž
se stylu okolního kódu.

---

## 2. Lokální vývojová smyčka

Vývojový režim je bez TLS a bez podpisů: plain HTTP, server neověřuje volající, runner
spustí, co dostane. Obě komponenty na to při startu upozorní varováním. Pro práci na
logice je to nejrychlejší cesta.

**Jednorázová příprava** (nesahá se do `/central_backup`):

```sh
mkdir -p local/{data,backup}
cat > local/server.toml <<'EOF'
[server]
listen   = "127.0.0.1:8443"
scripts  = "scripts"
data_dir = "./local/data"
timezone = "Europe/Prague"

[web]
listen = "127.0.0.1:8080"

[storage]
backup_dir = "./local/backup"
EOF

cp data/instances.example.json local/instances.json
# runner_id v instances.json musí být hostname tohohle stroje:
hostname
```

Totéž udělá `just dev-init` — vytvoří adresáře, zkopíruje oba vzorové soubory a přepíše
v configu oba `listen`, `data_dir` a `backup_dir` na lokální cesty a v seedu zástupný
`REPLACE-WITH-RUNNER-HOSTNAME` na `hostname -s`. Existující soubory **nepřepisuje**,
takže se dá pustit kdykoli.

Web je pak na `http://127.0.0.1:8080/`. Při prvním startu server vypíše do logu heslo
vygenerované pro účet `admin`; kdykoli později ho přenastaví `just passwd admin`.

`local/` je v `.gitignore`.

**Smyčka:**

```sh
# terminál 1 — server
go run ./cmd/server -config local/server.toml -instances local/instances.json

# terminál 2 — vynutit úlohu a nechat runner jednou doběhnout
curl -X POST http://127.0.0.1:8443/api/v1/instances/hello-demo/run
go run ./cmd/runner -server http://127.0.0.1:8443 -once

# výsledek
curl http://127.0.0.1:8443/api/v1/runs
curl http://127.0.0.1:8443/api/v1/runs/run-1/output
```

> **ID běhu je `run-1`, ne `1`.** Endpointy pro výstup skládají z ID cestu na disk
> (`backup_dir/runs/run-1/stdout.log`), takže s holým číslem vrátí prázdné tělo
> a HTTP 200 — vypadá to jako „skript nic nevypsal". Recepty `just run-output`
> a `just run-tail` si číslo doplní samy.

Přes `just` je táž smyčka kratší:

```sh
just server                    # terminál 1
just trigger                   # terminál 2 — default instance je hello-demo
just runner-once
just runs
just run-output 1
```

Recepty míří na `local/server.toml`, `local/instances.json` a `http://127.0.0.1:8443`.
Přepsat je jde proměnnými prostředí, takže ani pro mTLS variantu ([§7](#7-vývoj-se-zapnutým-mtls))
nemusíš do `justfile` sahat:

| Proměnná | Ovlivňuje | Default |
|---|---|---|
| `GO` | čím se překládá a spouští | `go` z `PATH` |
| `SERVER_CONFIG` | `just server` | `local/server.toml` |
| `INSTANCES` | seed pro `just server` | `local/instances.json` |
| `SERVER_URL` | `just trigger`, `runner-once`, `runs`, `run-output` | `http://127.0.0.1:8443` |
| `LISTEN` | adresa zapsaná do configu, který zakládá `just dev-init` | `127.0.0.1:8443` |
| `V` | verze v `just release` / `dist-runner` | dnešní datum |

```sh
SERVER_CONFIG=local/server-mtls.toml INSTANCES=/dev/null just server
just runner-once http://127.0.0.1:8443 local/runner.toml   # runner s vlastním configem
```

> **Vývojový server poslouchá jen na loopbacku.** Z jiného stroje (typicky prohlížeč na
> tvém notebooku proti serveru ve VM) spojení skončí „Chyba spojení" a v logu serveru
> **není po požadavku ani stopa** — odmítne ho jádro, ne Arcatum. Náprava je
> `listen = "0.0.0.0:8443"` v configu a restart; `LISTEN=0.0.0.0:8443 just dev-init` to
> rovnou takhle založí. Takový server je ale plain HTTP **bez ověřování volajícího**,
> takže admin API má otevřené celé síti — na cokoli delšího než pokus zapni
> [mTLS](#7-vývoj-se-zapnutým-mtls).

> `just runs` a spol. jsou holé `curl` bez klientského certifikátu — proti serveru
> se zapnutým mTLS skončí chybou handshaku. Tam používej `curl` s `-A` sadou z [§7](#7-vývoj-se-zapnutým-mtls).

Užitečné přepínače:

| Přepínač | Komponenta | K čemu |
|---|---|---|
| `-config` | server, runner | cesta ke konfiguraci; u serveru nepovinná — bez ní se bere `./server.toml`, pak `/etc/arcatum/server.toml` |
| `-instances` | server | seed soubor; `/dev/null` když nechceš seedovat |
| `-import-force` | server | přepíše i existující instance ze seedu |
| `-server` | runner | přebije `runner.server` z configu — hodí se pro rychlý test |
| `-once` | runner | **jeden plný cyklus** a konec, s logem v terminálu |

`-once` není zkratka — dělá přesně to, co jedno kolo smyčky, včetně převzetí rotovaného
trust materiálu. Proto se `-once` chová jako produkce, ne jako testovací zjednodušení.

Server se **nerestartuje sám**. Po změně Go kódu, `web/` assetů nebo `scripts/*.toml` ho
zastav a spusť znovu.

---

## 3. Kudy tečou data

Jeden běh úlohy, od tiknutí runneru k zapsanému výstupu. Tohle je mapa, podle které se dá
hledat, kde se něco rozbilo:

```
runner: Agent.Tick                      internal/runner/loop.go
  └─ POST /api/v1/checkin ──────────►  server: handleCheckin        internal/server/http.go:151
                                         ├─ activeRunnerIdentity     internal/server/auth.go
                                         │    identitu bere z CN certifikátu, ne z požadavku
                                         ├─ store.RecordCheckin      internal/server/store.go
                                         ├─ store.InstancesForRunner
                                         ├─ sched.DueFor             internal/server/scheduler.go
                                         │    → id dozrálých rozvrhů + příznak ručního běhu;
                                         │      víc dozrálých naráz dá pořád JEDEN běh, připsaný
                                         │      tomu nejstaršímu, a posunou se všechny
                                         └─ buildDispatch(in, scheduleID)  internal/server/http.go
                                              ├─ catalog.Get + readArtifact  (SHA-256 artefaktu)
                                              ├─ store.CreateRun     → status "pending", schedule_id
                                              └─ signer.Sign(d.SigningBytes())
                                         (pak sched.MarkDispatched na každý rozvrh,
                                          sched.ClearManual u „run now")
  ◄──── CheckinResponse{Due: […]} ─────┘
  ├─ verifyDispatch                     internal/runner/loop.go:56   podpis PŘED spuštěním
  ├─ Execute                            internal/runner/executor.go
  │    ├─ kontrola SHA-256 obsahu artefaktu
  │    ├─ ARCATUM_<PARAM> do env, secrets do souboru (ARCATUM_SECRETS_FILE)
  │    └─ bash | python3 | binárka, cwd = dočasný workdir (po běhu smazán)
  └─ POST /api/v1/runs/updates ──────►  handleUpdates                internal/server/http.go:252
       ndjson stream RunUpdate            ├─ ownsRun — runner smí hlásit jen své běhy
       (started, output, finished)        └─ applyUpdate
                                              ├─ store.StartRun / FinishRun
                                              └─ store.AppendOutput  → backup_dir/runs/<id>/*.log
```

Podpis je nad **kanonickou serializací** v [pkg/proto/signing.go](../pkg/proto/signing.go):
délkově prefixovaná pole, seřazené klíče map, a obsah artefaktu pokrytý svým SHA‑256.
Server podepisuje a runner ověřuje **stejnou funkcí** — proto jsou v `pkg/proto`, ne
u jedné strany.

Vrstvy zabezpečení, které do toho zasahují:

| Vrstva | Server | Runner |
|---|---|---|
| mTLS, role z `OU` | `internal/server/auth.go` | `pkg/crypto/tls.go` |
| podpis úloh (Ed25519) | `pkg/crypto/sign.go`, `signingset.go` | `internal/runner/trust.go` |
| šifrování secrets | `pkg/crypto/secretbox.go`, `keyring.go` | — (dostane plaintext v úloze) |

---

## 4. Kam co přidat

### Nový endpoint

1. Handler do vhodného souboru v `internal/server/` (`instances.go`, `restore.go`, `update.go`, …).
2. Registrace v [internal/server/http.go](../internal/server/http.go). Server má **dva
   routery**, protože má dva druhy volajících:
   - `Server.Handler()` — port `[server] listen` (mTLS): runnery a volání s admin certifikátem.
   - `Server.WebHandler()` — port `[web] listen` (plain HTTP): web UI a přihlášení heslem.

   Operátorské endpointy patří do `registerOperatorRoutes`, které se registruje **do obou**
   — dostane guard podle listeneru (`adminOnly` u mTLS, `webRead`/`webAdmin` u webu), takže
   nový endpoint funguje z curl i z webu a nikde se nezapomene ohlídat. Runnerové endpointy
   registruj jen v `Handler()`. Routy používají Go 1.22 vzory (`"GET /api/v1/runs/{id}"`).
3. **Rozhodni, jestli endpoint jen čte, nebo mění.** Do `registerOperatorRoutes` ho zabal
   `read(...)` (čte — pustí i roli `viewer`) nebo `write(...)` (mění — jen admin). Runnerové
   endpointy si identitu vytáhnou přes `s.activeRunnerIdentity(r, "")` a musí kontrolovat, že
   runner sahá jen na své vlastní věci (vzor: `ownsRun`). Přihlášeného člověka vrací
   `userFrom(r)` (nil na mTLS listeneru), jméno pro log dá `actor(r)` pro oba případy.
4. Odpověď přes `writeJSON`. Hodnoty secrets **nikdy** do odpovědi.
5. Řádek do tabulky API v [README](../README_cz.md#http-api) — je to jediný soupis endpointů.

### Nové pole v konfiguraci

`pkg/config/config.go` (server) nebo `runner.go` (runner) → struktura + `toml` tag →
`Default()` → `Validate()` → zakomentovaný příklad do `config/*.example.toml`.

`Validate()` je tu na to, aby **poloviční konfigurace byla chyba, ne režim**. Vzor:
`[tls]` vyžaduje všechny tři cesty, `[tls]` vynucuje `[signing]` i `[secrets]`. Když nové
pole umí něco tiše vypnout, přidej i kontrolu.

### Nový sloupec v DB

Do `addColumns` v [internal/server/schema.go](../internal/server/schema.go) — přidá se jen
tam, kde chybí, takže existující databáze se upgraduje na místě. **Neměň historii**
v `schemaSQL` a dej sloupci default, který nechá starší data funkční (viz enrollment
sloupce: default `approved`, aby ručně vydané certifikáty dál fungovaly).

### Nový index a nová tabulka

Index nad sloupcem, který přidal `addColumns`, patří do **`postMigrateSQL`**, nikdy do
`schemaSQL`: schéma se aplikuje *před* migrací, takže takový index by spadl na každé databázi
starší než ten sloupec. Celá nová tabulka jde do `schemaSQL` jako dřív — je to
`CREATE TABLE IF NOT EXISTS`, což je bezpečné všude.

### Přesun dat, ne jen schématu

Když změna musí *přesunout* data — rozdělení rozvrhů bylo první taková — dej ji do vlastního
souboru (viz [internal/server/migrate_schedules.go](../internal/server/migrate_schedules.go))
a pusť z `Open` po `postMigrateSQL`.

Dvě pravidla, která si ta migrace vysloužila:

- **Pojistka per řádek, ne per databáze.** Globální příznak „hotovo" je správně přesně jednou.
  Značka na migrovaném řádku je správně i pro řádek obnovený později ze staršího archivu — a
  hlavně nikdy nevzkřísí něco, co operátor mezitím smazal. Každý zapisovatel značku nastavuje
  rovnou při vkládání, takže sken čerstvý řádek nikdy neuvidí.
- **Neshazuj start kvůli jednomu špatnému řádku.** Zaloguj ho, označ za vyřízený a jeď dál.
  Server, který odmítne nastartovat kvůli jedné zdeformované hodnotě, s sebou vezme všechny
  ostatní zálohy.

### Nová zpráva nebo pole v protokolu

`pkg/proto/proto.go`. Pokud pole vstupuje do podpisu, přidej ho i do `SigningBytes()`
v `signing.go` — a mysl na pořadí nasazení: **nejdřív server, pak runnery.** Změna
kanonické podoby znamená, že starý runner novou podepsanou zprávu neověří, takže
nekompatibilní změna se do produkce dostane jen s aktualizací runnerů (a ty se aktualizují
samy až po restartu serveru s novým manifestem).

### Nový typ skriptu

`proto.ScriptType` → povolení v `jobspec.Manifest.Validate()` → `switch d.Type` v
`prepare()` v [internal/runner/executor.go](../internal/runner/executor.go). Typ bez
entrypointu (jako `restic`) musí dostat výjimku v `Validate()` i v `Catalog`.

---

## 5. Testy

42 testovacích souborů, `go test ./...` dnes prochází. Testy jsou v témž balíku jako kód
(`package server`), takže vidí i neexportované věci.

Užitečné vzory, které v repozitáři už jsou — používej je, nevymýšlej nové:

```go
// store nad temp DB, uklidí se sám       internal/server/store_test.go
st, dir := openTestStore(t)

// server s katalogem skriptů „na papíře" — bez souborů na disku
catalog := &Catalog{byName: map[string]*ScriptEntry{
    "mysql-backup": {Manifest: &jobspec.Manifest{ /* … */ }},
}}
```

HTTP endpointy se testují přes `httptest` proti `srv.Handler()` (viz
[instances_test.go](../internal/server/instances_test.go)); krypto a rozvrhy tabulkovými
testy v `pkg/`.

```sh
go test ./internal/server -run TestImportInstances -v    # jeden test
go test ./internal/server -count=1                        # bez cache
go test -race ./internal/runner                           # executor a smyčka
```

Co si zaslouží test vždycky: **autorizace** (může runner A hlásit běh runnera B?),
**kanonická serializace** podpisu, **migrace** (starý řádek po přidání sloupce), a
**odmítnutí** (chybný podpis, neschválený runner, neznámý parametr). Tyhle věci se v ručním
testu snadno přehlédnou, protože „to funguje".

---

## 6. Ladění

**Log serveru** je hlavní zdroj. Server hlásí každý dispatch (`dispatch: instance=… run=… schedule=… -> runner=…`, u ručního
běhu je `schedule=manual`),
odmítnutí (`checkin denied: …`) i chyby instancí. Chybná instance nezastaví checkin — jen se
přeskočí s logem, takže když se úloha „nespustila a nikdo nic neřekl", hledej tady.

**Stav v DB.** Databáze je obyčejné SQLite (WAL):

```sh
sqlite3 local/data/arcatum.db 'SELECT id,instance_id,status,exit_code,bytes FROM runs ORDER BY id DESC LIMIT 10;'
sqlite3 local/data/arcatum.db 'SELECT id,script,runner_id FROM instances;'
# kdy co běží — instances.schedule je pozůstatkový sloupec a u všeho vzniklého po rozdělení
# je prázdný, takže číst ho tě jen svede z cesty
sqlite3 local/data/arcatum.db 'SELECT id,instance_id,name,frequency,time,enabled FROM schedules;'
sqlite3 local/data/arcatum.db 'SELECT id,instance_id,schedule_id,status FROM runs ORDER BY id DESC LIMIT 10;'
sqlite3 local/data/arcatum.db 'SELECT id,status,last_seen,cert_not_after FROM runners;'
```

Hodnoty secrets uvidíš jako `enc:v1:…` — to je správně; čitelné jsou jen názvy.

**Výstup běhu** leží na disku, nezávisle na API: `backup_dir/runs/<run_id>/{stdout,stderr}.log`.
Přírůstkové čtení, které používá živý tail ve webu:

```sh
curl "http://127.0.0.1:8443/api/v1/runs/run-1/tail?offset=0&stream=stdout"
just run-tail 1                     # totéž, ID si doplní
```

**Strana runneru.** V lokálním vývoji `go run ./cmd/runner … -once` a čti terminál. Na
hostu:

```sh
journalctl -u arcatum-runner -f
```

Typické chyby a co znamenají:

| Co vidíš | Kde se to děje | Příčina |
|---|---|---|
| `checkin denied: certificate … has role …` | server, `auth.go` | certifikát není `runner` (např. admin cert) |
| 403 při checkinu | server | `CN` certifikátu ≠ `runner_id` — runner se vydává za jiný host |
| runner opakovaně dotazuje a nic nedělá | server | runner je `pending` — schval ho v UI |
| `artifact hash mismatch` | runner, `executor.go` | obsah artefaktu nesouhlasí s podepsaným SHA‑256 |
| runner odmítne úlohu kvůli podpisu | runner, `trust.go` | rozešel se podepisovací klíč nebo kanonická serializace |
| `unknown script "x"` v logu serveru | server, `buildDispatch` | instance míří na skript, který katalog nezná |
| server vůbec nenastartuje | `LoadCatalog` | vadný manifest nebo chybějící entrypoint ve `scripts/` |

**Ověřování mimo běh.** `GET /status` vypíše i seznam skriptů, které katalog načetl —
nejrychlejší kontrola, že server vidí to, co si myslíš.

---

## 7. Vývoj se zapnutým mTLS

Na věci jako autorizace, enrollment, obnova či rotace klíčů je plain HTTP nepoužitelné.
Lokální PKI:

```sh
deploy/gen-certs.sh -d local/pki -H 127.0.0.1 -a dev
just dev-certs                       # totéž; jiné hodnoty: just dev-certs 127.0.0.1,localhost petr
```

Do `local/server.toml` přidej `[tls]`, `[signing]` a `[secrets]` s cestami do `local/pki`
(vzor v [config/server.example.toml](../config/server.example.toml)), pro runner analogicky
`local/runner.toml` s `runner-<hostname>` párem a `dispatch-signing.pub`.

```sh
# runner certifikát pro tenhle stroj
go run ./cmd/arcatum-ca runner -dir local/pki -id "$(hostname -s)"
just dev-runner-cert                        # totéž; jiný host: just dev-runner-cert web-02
# libovolný jiný příkaz CA: just ca admin -dir local/pki -name kolega

# volání API
A=(--cacert local/pki/ca.pem --cert local/pki/admin-dev.pem --key local/pki/admin-dev.key)
curl "${A[@]}" https://127.0.0.1:8443/api/v1/whoami
```

Testování enrollmentu: přidej `[bootstrap]` s `listen = "127.0.0.1:8080"`, `ca_key` a
`api_url`, pak smaž runnerův `cert`/`key` a spusť runner — vygeneruje si klíč, pošle CSR
a bude čekat na schválení (`POST /api/v1/runners/{id}/approve`).

> Testuješ-li **rotaci CA**, drž se pořadí z [README → Rotace klíčů](../README_cz.md#rotace-klíčů).
> Vydat certifikát serveru pod novou CA předčasně je jediný krok, který si umí zamknout
> runnery z vlastního systému — a v testu to vypadá stejně jako „nefunguje TLS".

---

## 8. Web UI

`web/index.html`, `app.js`, `style.css` — bez build stepu, bez frameworku, bez závislostí.
Assety jsou v binárce přes `embed.FS` ([web/web.go](../web/web.go)), takže se nemůžou
rozejít s verzí serveru.

Důsledek pro vývoj: **po změně v `web/` je nutný restart serveru** (u `go run` stačí
zabít a spustit znovu, `//go:embed` čte soubory při kompilaci). Nový soubor v `web/`
přidej i do direktivy `//go:embed` a do seznamu obsluhovaných assetů ve `WebHandler()`.

Views jsou sekce přepínané třídou `hidden`: `dashboard` (úvodní stránka), `instances`,
`instance-form`, `schedules`, `schedule-form`, `history` (běhy jedné úlohy), `restore`,
`runners`, `rotation`, `users`, `admin`, `account` a `detail` (běh se živým tailem). Které se
obnovují pětisekundovým časovačem, rozhoduje mapa `loaders` — formuláře, stránka účtu a detail
běhu v ní záměrně nejsou, jinak by refresh přepsal rozdělaný formulář pod rukama.

**Každé `<td>`, které render vyrobí, nese `data-label`.** Pod 620 px tabulky přestanou být
tabulkami: každý řádek je karta a `data-label` buňky se vykreslí vedle hodnoty — protože
posouvat tabulku doprava, abys zjistil, jestli noční záloha dopadla, je přesně to, co udělá UI
na telefonu nepoužitelným. Nová tabulka bez `data-label` vypadá na desktopu dobře a na telefonu
se z ní stane nepopsaný sloupec hodnot, což je věc, které si nikdo nevšimne, dokud nesedí ve
vlaku.

Views jsou sekce přepínané třídou `hidden`: `dashboard` (úvodní stránka), `instances`,
`instance-form`, `schedules`, `schedule-form`, `history` (běhy jedné úlohy), `restore`,
`runners`, `rotation`, `users`, `admin`, `account` a `detail` (běh se živým tailem). Které se
obnovují pětisekundovým časovačem, rozhoduje mapa `loaders` — formuláře, stránka účtu a detail
běhu v ní záměrně nejsou, jinak by refresh přepsal rozdělaný formulář pod rukama.

**Každé `<td>`, které render vyrobí, nese `data-label`.** Pod 620 px tabulky přestanou být
tabulkami: každý řádek je karta a `data-label` buňky se vykreslí vedle hodnoty — protože
posouvat tabulku doprava, abys zjistil, jestli noční záloha dopadla, je přesně to, co udělá UI
na telefonu nepoužitelným. Nová tabulka bez `data-label` vypadá na desktopu dobře a na telefonu
se z ní stane nepopsaný sloupec hodnot, čehož si nikdo nevšimne, dokud nesedí ve vlaku.

Web běží na vlastním portu (`[web] listen`) a přihlašuje se jménem a heslem — assety samy
jsou dostupné bez přihlášení (je to ta stránka, která o přihlášení *žádá*), všechna data
jdou přes API za cookie sezení. Na 401 z API `app.js` ukáže přihlašovací obrazovku, takže
vypršené sezení nekončí prázdnými tabulkami. Účty, sezení a middleware jsou
v [internal/server/users.go](../internal/server/users.go) a `users_store.go`; heslo se
ukládá jako PBKDF2 verifikátor z [pkg/crypto/password.go](../pkg/crypto/password.go).

> Testy v `internal/server` snižují počet iterací PBKDF2 (`TestMain` v `users_test.go`).
> Bez toho by každé vytvoření účtu stálo skoro půl sekundy.

Živý tail je polling — `GET /api/v1/runs/{id}/tail?offset=N` vrací jen přírůstek. Žádné
websockety: přežije to odpadnutí spojení a nepotřebuje nic navíc na serveru.

Texty ve webu jsou **jen anglicky**; dvojjazyčná je dokumentace, ne UI.

---

## 9. Pasti, které stojí čas

- **Scheduler je v paměti.** `next_run` se po restartu přepočítá u každého rozvrhu od
  aktuálního času, takže běh, který měl padnout během restartu, se přeskočí. Není to chyba —
  persistence časů příštích běhů je záměrně mimo, ale při testování rozvrhů to plete.
  Vypnutý rozvrh je trackovaný, ale nikdy nedozraje, takže zapnutí je přehození příznaku, ne
  nové parsování, které by mohlo selhat v nejhorší chvíli.
- **`instances.schedule` není zdroj pravdy.** Je to sloupec z doby před rozdělením, migrace ho
  jednou přečte a u všeho novějšího je prázdný. Ptej se tabulky `schedules`.
- **Katalog skriptů se načítá jen při startu.** Změna `scripts/*.toml` bez restartu se
  neprojeví; vadný manifest naopak start rovnou shodí.
- **Seed instancí nepřepisuje existující.** Úprava `instances.json` bez `-import-force` se
  „neděje" — a tak to má být, jinak by restart mazal změny z webu. Totéž platí pro `schedules`
  v něm: přidat rozvrh instanci, která už existuje, neudělá vůbec nic — záměrně, aby se rozvrh,
  který operátor smazal, nevytvářel znovu při každém restartu. S `-import-force` se rozvrhy
  instance **nahradí**, ne doplní.
- **Identitu určuje certifikát, ne požadavek.** `req.RunnerID` z těla se zahodí a přepíše
  hodnotou z `CN`. Kód, který věří tělu, obchází autorizaci.
- **Secrets nikdy do logu ani do odpovědi API.** Do env taky ne (env je čitelné
  z `/proc/<pid>/environ`) — proto ten dočasný soubor v `executor.go`.
- **`ErrRestartRequired` není chyba.** Znamená, že se změnil certifikát nebo trust materiál;
  proces má skončit a service manager ho nastartuje s novým stavem.
- **`log_level` v configu se dnes nepoužívá** — server loguje jednou úrovní. Kdo čeká na
  `debug` víc, čeká zbytečně.

---

## 10. Než pošleš změnu

```sh
gofmt -l . && go vet ./... && go test ./... && go build ./...
# se just: gofmt -l . && just vet && just test && just build
```

- [ ] test na nové chování — u autorizace, podpisů a migrací povinně
- [ ] `Validate()` doplněný, když nové pole umí něco tiše vypnout
- [ ] nový endpoint má guard — `read`/`write` v `registerOperatorRoutes`, nebo `adminOnly`,
      nebo vlastní kontrolu vlastnictví u runnerů — a je v tabulce API v README
- [ ] žádné secrets v logu ani v odpovědích
- [ ] změny v protokolu zvládne starý runner, nebo je popsané pořadí nasazení
- [ ] dokumentace **v obou jazycích**: [README](../README_cz.md) / [README (EN)](../README.md)
      pro postup, [architecture_cz.md](architecture_cz.md) / [architecture.md](architecture.md)
      pro rozhodnutí a *proč*

Související: [architektura](architecture_cz.md) · [nasazení produkce](production_cz.md) ·
[vývoj skriptů](script-development_cz.md)
