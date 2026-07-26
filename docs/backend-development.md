# Návod: vývoj a ladění backendu

Praktický návod pro práci na Go kódu serveru a runneru: jak si postavit lokální
prostředí, kudy tečou data, kam přidat novou věc, jak to otestovat a jak ladit, když to
nefunguje.

Zpětné souvislosti a *proč* jsou věci tak, jak jsou, patří do [architecture.md](architecture.md).
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

Modul se jmenuje `arcatum`, takže importy jsou `arcatum/internal/server`, `arcatum/pkg/proto`, …
Závislosti jsou záměrně minimální: `BurntSushi/toml` a `modernc.org/sqlite`. **Server běží
bez CGO** — SQLite je čistě v Go, výsledkem je statický binár. Přidat závislost, která CGO
vyžaduje, tuhle vlastnost zruší.

Konvence: **komentáře a identifikátory anglicky, dokumentace česky.** Komentáře vysvětlují
*proč*, ne co řádek dělá — drž se stylu okolního kódu.

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

[storage]
backup_dir = "./local/backup"
EOF

cp data/instances.example.json local/instances.json
# runner_id v instances.json musí být hostname tohohle stroje:
hostname
```

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
curl http://127.0.0.1:8443/api/v1/runs/1/output
```

Užitečné přepínače:

| Přepínač | Komponenta | K čemu |
|---|---|---|
| `-config` | server, runner | cesta ke konfiguraci |
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
                                         ├─ sched.Due                internal/server/scheduler.go
                                         └─ buildDispatch            internal/server/http.go:207
                                              ├─ catalog.Get + readArtifact  (SHA-256 artefaktu)
                                              ├─ store.CreateRun     → status "pending"
                                              └─ signer.Sign(d.SigningBytes())
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
2. Registrace v `Server.Handler()` v [internal/server/http.go](../internal/server/http.go#L101).
   Routy používají Go 1.22 vzory (`"GET /api/v1/runs/{id}"`).
3. **Admin endpointy obal `s.adminOnly(...)`.** Bez toho je endpoint dostupný i runnerům.
   Runnerové endpointy si identitu vytáhnou přes `s.activeRunnerIdentity(r, "")` a musí
   kontrolovat, že runner sahá jen na své vlastní věci (vzor: `ownsRun`).
4. Odpověď přes `writeJSON`. Hodnoty secrets **nikdy** do odpovědi.
5. Řádek do tabulky API v [README](../README.md#http-api) — je to jediný soupis endpointů.

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

23 testovacích souborů, `go test ./...` dnes prochází. Testy jsou v témž balíku jako kód
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

**Log serveru** je hlavní zdroj. Server hlásí každý dispatch (`dispatch: instance=… run=… -> runner=…`),
odmítnutí (`checkin denied: …`) i chyby instancí. Chybná instance nezastaví checkin — jen se
přeskočí s logem, takže když se úloha „nespustila a nikdo nic neřekl", hledej tady.

**Stav v DB.** Databáze je obyčejné SQLite (WAL):

```sh
sqlite3 local/data/arcatum.db 'SELECT id,instance_id,status,exit_code,bytes FROM runs ORDER BY id DESC LIMIT 10;'
sqlite3 local/data/arcatum.db 'SELECT id,script,runner_id,schedule FROM instances;'
sqlite3 local/data/arcatum.db 'SELECT id,status,last_seen,cert_not_after FROM runners;'
```

Hodnoty secrets uvidíš jako `enc:v1:…` — to je správně; čitelné jsou jen názvy.

**Výstup běhu** leží na disku, nezávisle na API: `backup_dir/runs/<run_id>/{stdout,stderr}.log`.
Přírůstkové čtení, které používá živý tail ve webu:

```sh
curl "http://127.0.0.1:8443/api/v1/runs/1/tail?offset=0&stream=stdout"
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
```

Do `local/server.toml` přidej `[tls]`, `[signing]` a `[secrets]` s cestami do `local/pki`
(vzor v [config/server.example.toml](../config/server.example.toml)), pro runner analogicky
`local/runner.toml` s `runner-<hostname>` párem a `dispatch-signing.pub`.

```sh
# runner certifikát pro tenhle stroj
go run ./cmd/arcatum-ca runner -dir local/pki -id "$(hostname -s)"

# volání API
A=(--cacert local/pki/ca.pem --cert local/pki/admin-dev.pem --key local/pki/admin-dev.key)
curl "${A[@]}" https://127.0.0.1:8443/api/v1/whoami
```

Testování enrollmentu: přidej `[bootstrap]` s `listen = "127.0.0.1:8080"`, `ca_key` a
`api_url`, pak smaž runnerův `cert`/`key` a spusť runner — vygeneruje si klíč, pošle CSR
a bude čekat na schválení (`POST /api/v1/runners/{id}/approve`).

> Testuješ-li **rotaci CA**, drž se pořadí z [README → Rotace klíčů](../README.md#rotace-klíčů).
> Vydat certifikát serveru pod novou CA předčasně je jediný krok, který si umí zamknout
> runnery z vlastního systému — a v testu to vypadá stejně jako „nefunguje TLS".

---

## 8. Web UI

`web/index.html`, `app.js`, `style.css` — bez build stepu, bez frameworku, bez závislostí.
Assety jsou v binárce přes `embed.FS` ([web/web.go](../web/web.go)), takže se nemůžou
rozejít s verzí serveru.

Důsledek pro vývoj: **po změně v `web/` je nutný restart serveru** (u `go run` stačí
zabít a spustit znovu, `//go:embed` čte soubory při kompilaci). Nový soubor v `web/`
přidej i do direktivy `//go:embed` a do seznamu obsluhovaných assetů v `Handler()`.

Živý tail je polling — `GET /api/v1/runs/{id}/tail?offset=N` vrací jen přírůstek. Žádné
websockety: přežije to odpadnutí spojení a nepotřebuje nic navíc na serveru.

---

## 9. Pasti, které stojí čas

- **Scheduler je v paměti.** `next_run` se po restartu přepočítá od aktuálního času, takže
  běh, který měl padnout během restartu, se přeskočí. Není to chyba — persistence rozvrhu
  je záměrně mimo, ale při testování rozvrhů to plete.
- **Katalog skriptů se načítá jen při startu.** Změna `scripts/*.toml` bez restartu se
  neprojeví; vadný manifest naopak start rovnou shodí.
- **Seed instancí nepřepisuje existující.** Úprava `instances.json` bez `-import-force` se
  „neděje" — a tak to má být, jinak by restart mazal změny z webu.
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
```

- [ ] test na nové chování — u autorizace, podpisů a migrací povinně
- [ ] `Validate()` doplněný, když nové pole umí něco tiše vypnout
- [ ] nový endpoint obalený `adminOnly` (nebo má vlastní kontrolu vlastnictví) a je v tabulce API v README
- [ ] žádné secrets v logu ani v odpovědích
- [ ] změny v protokolu zvládne starý runner, nebo je popsané pořadí nasazení
- [ ] dokumentace: [README](../README.md) pro postup, [architecture.md](architecture.md) pro rozhodnutí a *proč*

Související: [architektura](architecture.md) · [nasazení produkce](production.md) ·
[vývoj skriptů](script-development.md)
