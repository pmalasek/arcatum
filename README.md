# Arcatum

Zálohovací systém pro interní síť Xtuning. Monorepo, jazyk Go.

- **arcatum-server** (`cmd/server`) — scheduler, API, úložiště, web UI, DB.
- **arcatum-runner** (`cmd/runner`) — služba na zálohovaném serveru; **pull model**
  (iniciuje odchozí spojení, žádný příchozí port), orchestruje `restic` a skripty,
  streamuje výstup i data na server.

Architektura a rozhodnutí: [docs/architecture.md](docs/architecture.md).

## Stav

**Fáze A (scaffold) + fáze B (protokol end-to-end přes plain HTTP) hotové.**
Runner se přihlásí, dostane úlohu, spustí ji a streamuje výstup zpět na server, kde se
ukládá. Zatím bez mTLS/podpisů, DB (in-memory), restic, web UI. Detail: [docs/architecture.md §10](docs/architecture.md).

## Struktura

```
cmd/{server,runner}   binárky
internal/server       HTTP API, scheduler, in-memory store, katalog skriptů
internal/runner       checkin smyčka, executor (spuštění úlohy + stream výstupu)
pkg/proto             zprávy protokolu (checkin, dispatch, stream výsledku)
pkg/jobspec           parser manifestu skriptu (definice + deklarace parametrů)
pkg/schedule          výpočet „next run"
pkg/config            config serveru (server.toml) i runneru (runner.toml)
pkg/crypto            mTLS, podpis úloh, šifrování secrets (placeholder)
scripts/              DEFINICE skriptů (kód + manifest) — bez secrets, verzované
data/                 instances.example.json (instance = nasazení skriptu na cíl)
docs/                 architektura
```

Konfigurace je dvojí: **definice skriptu** (šablona v `scripts/`) a **instance**
(nasazení na konkrétní cíl s parametry, secrets a rozvrhem — v DB). Detail v §5 architektury.

## Vývoj

```sh
go build ./...
go run ./cmd/server -config config/server.example.toml
go run ./cmd/runner -config config/runner.example.toml
# nebo bez configu, s adresou serveru z parametru:
go run ./cmd/runner -server https://172.24.0.60:8443
```
