# Návod: vývoj a ladění zálohovacích skriptů

Jak napsat nový zálohovací skript, jak ho vyzkoušet, než ho pustíš na produkci, a jak ho
ladit, když selže. Ladění skriptů je věc, kterou budeš dělat nejčastěji — kolem toho je
postavené „spustit teď" i živý tail ve webu.

- [1. Z čeho se skript skládá](#1-z-čeho-se-skript-skládá)
- [2. Manifest](#2-manifest)
- [3. Jak skript dostane parametry](#3-jak-skript-dostane-parametry)
- [4. Pravidla, která se vyplatí držet](#4-pravidla-která-se-vyplatí-držet)
- [5. Vývojová smyčka](#5-vývojová-smyčka)
- [6. Ladění běhu](#6-ladění-běhu)
- [7. Katalog chyb](#7-katalog-chyb)
- [8. Souborové zálohy (typ `restic`)](#8-souborové-zálohy-typ-restic)
- [9. Checklist před produkcí](#9-checklist-před-produkcí)

---

## 1. Z čeho se skript skládá

Dva soubory ve `scripts/`:

```
scripts/example/mysql_backup.toml    manifest — jméno, typ, entrypoint, deklarace parametrů
scripts/example/mysql_backup.sh      kód
```

Server prochází adresář `scripts` **rekurzivně** a bere každý `*.toml` jako manifest.
Struktura podadresářů je tedy na tobě; jediné pevné pravidlo je, že **`name` v manifestu
musí být v celém katalogu unikátní** a `entrypoint` je cesta **relativní k manifestu**.

Dvě věci si zapamatuj hned, ať se nezdržuješ:

- **Nový nebo změněný manifest se projeví až po restartu serveru.** Katalog se načítá při
  startu.
- **Vadný manifest zabrání startu serveru.** Neznámý typ, chybějící `entrypoint` nebo
  duplicitní jméno je fatální chyba — schválně, ať se to pozná hned a ne v noci.

Změna samotného **kódu** skriptu restart nepotřebuje: artefakt se čte z disku při každém
dispatchi (a jeho SHA‑256 se podepisuje s úlohou).

Skript **nikdy neobsahuje hesla ani adresy konkrétních serverů** — je to šablona. Hodnoty
patří do instance (viz [README → Skript vs. instance](../README.md#skript-vs-instance)).

---

## 2. Manifest

```toml
name       = "mysql-backup"      # unikátní jméno; na tohle se odkazuje instance
type       = "bash"              # bash | python | binary | restic
entrypoint = "mysql_backup.sh"   # relativně k tomuhle souboru
timeout    = "1h"                # default; instance smí přepsat
platforms  = ["linux/amd64"]     # jen pro type = "binary"

[[param]]
name     = "host"
type     = "string"              # string | int | bool
required = true

[[param]]
name    = "port"
type    = "int"
default = "3306"

[[param]]
name     = "password"
type     = "string"
required = true
secret   = true                  # předá se souborem, ne přes env
```

| Typ | Jak runner spustí artefakt |
|---|---|
| `bash` | `bash <artefakt>` |
| `python` | `python3 <artefakt>` |
| `binary` | přímo — artefaktem je binárka pro platformu runneru |
| `restic` | **žádný artefakt**, `entrypoint` se nezadává; runner řídí restic sám ([§8](#8-souborové-zálohy-typ-restic)) |

**Deklarace parametrů není formalita.** Server z ní staví formulář ve webu a validuje
instanci **při uložení**, ne až při noční záloze. Kontroluje se:

- neznámý název parametru → chyba (překlep `datbase` by jinak tiše nic nedělal)
- `required` bez hodnoty a bez `default` → chyba
- secret poslaný jako obyčejný parametr (a naopak) → chyba
- `type = "int"` / `"bool"` s nečíselnou / nepravdivostní hodnotou → chyba

Prázdná hodnota se počítá jako nezadaná. `default` z manifestu se doplní do uložené
hodnoty, takže ho skript dostane v env jako každý jiný parametr. Ve webu je předvyplněný
v políčku — co se uloží, je tedy vidět na obrazovce. Zadaná hodnota má vždy přednost;
default nikdy nepřepíše to, co jsi vyplnil.

> Platí pro instance ukládané přes API a web. Seed soubor `instances.json` se importuje
> mimo tuhle cestu a bere hodnoty tak, jak jsou — tam si default napiš do JSONu sám.

---

## 3. Jak skript dostane parametry

**Non-secret parametry → env proměnné `ARCATUM_<NÁZEV>`.** Název se převede na velká
písmena a všechno, co není `A–Z` nebo `0–9`, se změní na `_`. Takže `keep_daily` →
`ARCATUM_KEEP_DAILY`, `db-name` → `ARCATUM_DB_NAME`.

**Secrets → dočasný sourcovatelný soubor**, jeho cesta je v `ARCATUM_SECRETS_FILE`.
Obsah jsou řádky `export ARCATUM_<NÁZEV>='hodnota'` (hodnota je bezpečně uzávorkovaná,
takže apostrofy v hesle nic nerozbijí). Runner soubor po doběhnutí smaže.

```bash
#!/usr/bin/env bash
set -euo pipefail

: "${ARCATUM_HOST:?missing host}"       # povinné → tvrdě spadnout, ne pokračovat
PORT="${ARCATUM_PORT:-3306}"

# shellcheck disable=SC1090
[ -n "${ARCATUM_SECRETS_FILE:-}" ] && source "$ARCATUM_SECRETS_FILE"
# teď je k dispozici $ARCATUM_PASSWORD

exec mysqldump --host="$ARCATUM_HOST" --port="$PORT" \
  --single-transaction --quick "$ARCATUM_DATABASE"
```

Proč secrets nejdou přes env: env procesu je čitelné z `/proc/<pid>/environ`, tedy pro
kohokoli, kdo je na zálohovaném stroji dost blízko. Soubor má práva `0600` a krátký život.

**Prostředí běhu:**

| | |
|---|---|
| pracovní adresář | dočasný `run-<id>-*` pod `data_dir/work`, **po běhu smazán** |
| ostatní env | dědí se od procesu runneru (systemd služba — čekej minimální `PATH`) |
| stdout | streamuje se na server — tady mají tečt zálohovaná data |
| stderr | streamuje se zvlášť — sem patří diagnostika |
| `bytes` u běhu | součet **obou** streamů, jak je server přijal (upovídaný stderr se do něj počítá taky) |
| timeout | z instance, jinak z manifestu, jinak 1 h; po vypršení se proces zabije |
| návratový kód | 0 = úspěch, cokoli jiného = selhání běhu |

---

## 4. Pravidla, která se vyplatí držet

- **Data piš na stdout.** Celý smysl systému je, že na zálohovaném serveru nic nezůstává.
  Skript, který si dump uloží do `/var/backups`, tenhle model obchází.
- **`set -euo pipefail`** v každém bashi. Bez `pipefail` projde `mysqldump | gzip`
  jako úspěch, i když dump selže — a máš zálohu, která obsahuje půl databáze.
- **Povinné vstupy kontroluj sám** (`: "${ARCATUM_HOST:?}"`). Validace serveru chytá
  chybějící a špatně otypované hodnoty, ne nesmyslné.
- **Diagnostika na stderr, ne na stdout.** Ve stdout je záloha.
- **Nikdy neloguj heslo** — ani zkrácené, ani „jen pro tentokrát". Výstup se ukládá
  centrálně a zůstává tam.
- **Úklid při selhání** — `trap 'rm -f "$tmp"' EXIT` na cokoli dočasného, co si vyrobíš.
  Pracovní adresář runner smaže, ale co si napíšeš mimo něj, je tvoje starost.
- **`exec` na finální příkaz**, když jde jen o proud dat: ušetří proces a zachová
  návratový kód.
- **Idempotence a bezpečnost při přerušení.** Běh může skončit timeoutem nebo restartem
  runneru kdykoliv.

---

## 5. Vývojová smyčka

### a) Nejdřív nasucho, mimo Arcatum

Nejrychlejší kolo je spustit skript ručně se stejným kontraktem, jaký mu dá runner. Žádný
server, žádná instance:

```sh
cat > /tmp/secrets.env <<'EOF'
export ARCATUM_PASSWORD='tajne'
EOF
chmod 600 /tmp/secrets.env

env -i PATH=/usr/bin:/bin \
  ARCATUM_HOST=127.0.0.1 ARCATUM_PORT=3306 ARCATUM_DATABASE=shop ARCATUM_USER=backup \
  ARCATUM_SECRETS_FILE=/tmp/secrets.env \
  bash scripts/example/mysql_backup.sh > /tmp/out 2> /tmp/err
echo "exit=$?  bytes=$(stat -c%s /tmp/out)"

shred -u /tmp/secrets.env
```

`env -i PATH=…` je tam schválně: napodobí to hubené prostředí systemd služby a odhalí
skript, který se spoléhá na tvůj interaktivní shell (`~/.my.cnf`, aliasy, plné `PATH`).

```sh
shellcheck scripts/**/*.sh      # když je k dispozici — vyplatí se
```

### b) Pak v systému, na lokálním serveru

```sh
# manifest + kód na místě → server musí projít startem
go run ./cmd/server -config local/server.toml -instances local/instances.json
```

Instanci založ z webu (**Instance → nová instance** — formulář vznikne z tvých deklarací,
takže hned uvidíš, jestli manifest dává smysl), nebo přes API:

```sh
curl -X POST -H 'Content-Type: application/json' http://127.0.0.1:8443/api/v1/instances -d '{
  "id": "mujskript-test", "script": "muj-skript", "runner_id": "'"$(hostname -s)"'",
  "params": {"host": "127.0.0.1"}, "secrets": {"password": "tajne"},
  "timeout": "5m", "schedule": {"frequency": "daily", "time": "03:00"}
}'
```

Rozvrh dej klidně na noc — testovat budeš ručním spuštěním:

```sh
curl -X POST http://127.0.0.1:8443/api/v1/instances/mujskript-test/run
go run ./cmd/runner -server http://127.0.0.1:8443 -once
```

Je-li po ruce [`just`](../README.md#zkratky-přes-just), je tohle kolo `just trigger
mujskript-test && just runner-once` — a `just dev-init` připraví `local/` napoprvé.

Lokální vývojové prostředí (`local/server.toml`) je popsané v
[návodu na backend](backend-development.md#2-lokální-vývojová-smyčka).

### c) Nakonec na cílovém stroji

Skript, který závisí na prostředí (verze `mysqldump`, práva, dostupnost socketu), musí
projít **na tom hostu, kde má běžet** — s ostrým runnerem a přes web „spustit teď".
Krok (a) na tvém notebooku tohle nenahradí.

Krátké testovací skripty pro ověření, že systém sám funguje, jsou v
[scripts/example/](../scripts/example/): `hello` (bez závislostí, ukazuje předání
parametrů a secretu) a `slow-demo` (píše řádek za sekundu — pro živý tail).

---

## 6. Ladění běhu

**Cesta z webu je nejpohodlnější:** záložka **Instance** → **spustit teď**, pak klik na běh.
Otevře se detail s **živým tailem** — u probíhající úlohy se log dosypává, jak přichází,
s přepínačem `stdout`/`stderr` a zaškrtávátkem „sledovat".

Ze shellu totéž:

```sh
curl -X POST http://127.0.0.1:8443/api/v1/instances/mujskript-test/run
go run ./cmd/runner -server http://127.0.0.1:8443 -once     # log runneru v terminálu

curl http://127.0.0.1:8443/api/v1/runs                      # stav, exit code, bytes, trvání
curl http://127.0.0.1:8443/api/v1/runs/run-1/output         # co skript vypsal
curl "http://127.0.0.1:8443/api/v1/runs/run-1/output?stream=stderr"
curl "http://127.0.0.1:8443/api/v1/runs/run-1/tail?offset=0"   # přírůstkově, i za běhu
```

> **ID běhu je `run-1`.** S holým číslem vrátí endpointy výstupu prázdné tělo se stavem
> 200 — snadno se to splete s „skript nic nevypsal". Recepty
> [`just`](../README.md#zkratky-přes-just) tuhle past obcházejí: `just run-output 1`
> i `just run-output run-1` míří na totéž.

Se `just` je táž trojice `just trigger mujskript-test`, `just runner-once`, `just runs`
a `just run-output 1 stderr`.

Na serveru leží výstup i přímo na disku, takže do něj lze nahlédnout bez API:

```sh
tail -f /central_backup/arcatum/runs/run-1/stderr.log
```

Co si přečíst z hlavičky běhu, než se pustíš do logů:

| Pole | Čte se jako |
|---|---|
| `exit_code` > 0 | skript sám selhal → hledej ve `stderr` |
| `exit_code` = -1 s `err` | selhalo prostředí: chybí interpret, špatný hash artefaktu, timeout |
| `bytes` = 0 u úspěchu | skript nevypsal vůbec nic — často zapomenuté `exec`/přesměrování |
| `bytes` podezřele malé | data tekla jinam než na stdout (uložila se lokálně?), nebo dump selhal bez `pipefail` |
| `status` = `pending` | úloha byla přidělena, ale runner se neozval — problém je na straně runneru |

Na zálohovaném hostu:

```sh
journalctl -u arcatum-runner -f
```

---

## 7. Katalog chyb

| Zpráva | Kde | Co s tím |
|---|---|---|
| `manifest "x": invalid type "y"` | start serveru | `type` musí být `bash`, `python`, `binary` nebo `restic` |
| `manifest "x": entrypoint is required` | start serveru | doplň `entrypoint` (kromě `type = "restic"`) |
| `script "x": entrypoint not found` | start serveru | cesta je relativní **k manifestu**, ne k repozitáři |
| `duplicate script name "x"` | start serveru | dva manifesty mají stejné `name` |
| `script "x" has no parameter "y"` | uložení instance | překlep v názvu, nebo chybí `[[param]]` v manifestu |
| `parameter "y" is a secret and must be given as a secret` | uložení instance | patří do `secrets`, ne do `params` |
| `parameter "y" is not declared as a secret` | uložení instance | opačná chyba — přidej `secret = true`, nebo hodnotu přesuň do `params` |
| `parameter "y" is required` | uložení instance | prázdná hodnota se počítá jako nezadaná |
| `parameter "y" must be a whole number` | uložení instance | `type = "int"` a nečíselná hodnota |
| `unknown script "x"` | log serveru při checkinu | instance míří na skript, který katalog nezná (přejmenování bez restartu?) |
| `artifact hash mismatch` | runner | obsah nesouhlasí s podepsaným SHA‑256 — nesahej skriptu do souboru během dispatchi |
| `unsupported script type` | runner | starý runner nezná nový typ skriptu |
| `restic not found on this host` | runner | `apt install restic` na zálohovaném hostu |
| `line 12: mysqldump: command not found` | stderr běhu | služba má hubené `PATH` — použij absolutní cestu, nebo si `PATH` v skriptu nastav |
| běh skončí přesně na hraně timeoutu | — | zvedni `timeout` v instanci (přebíjí manifest) |

---

## 8. Souborové zálohy (typ `restic`)

Skript typu `restic` **žádný kód nemá** — runner spouští restic sám podle parametrů
instance a repozitář leží na serveru. Manifest je proto krátký (vzor:
[scripts/example/files_backup.toml](../scripts/example/files_backup.toml)):

```toml
name    = "files-backup"
type    = "restic"
timeout = "6h"
```

Parametry, které runner čte:

| Parametr | Význam |
|---|---|
| `paths` | **povinné** — co zálohovat, oddělené čárkou |
| `excludes` | restic exclude vzory, oddělené čárkou |
| `tags` | další tagy snapshotu (vždy se přidá `arcatum` a `instance:<id>`) |
| `keep_last`, `keep_daily`, `keep_weekly`, `keep_monthly`, `keep_yearly` | retence; je-li kterýkoli nastavený, po **úspěšné** záloze se pustí `forget --prune` omezené na snapshoty téhle instance |
| `restic_password` | secret — heslo repozitáře; nevyplněné se uloží jako `password` (viz default v manifestu) |

Vlastní zálohovací skript pro soubory tedy nepiš. Kde se `restic` typ vyplatí obejít
vlastním `bash` skriptem: když potřebuješ data předzpracovat (dump databáze, konzistentní
snapshot přes LVM) — pak dump pošli na stdout obyčejným skriptem, nebo si ve skriptu
připrav soubory a zálohuj je druhou, `restic` instancí.

> Heslo repozitáře je nenahraditelné — restic ho neumí obnovit. Vygeneruj dlouhé náhodné
> a ulož si kopii i mimo Arcatum. Ladění nového souborového backupu dělej na testovací
> instanci s vlastním repozitářem, ne na tom produkčním.

---

## 9. Checklist před produkcí

- [ ] `set -euo pipefail` (bash), povinné vstupy ověřené v kódu
- [ ] `shellcheck` bez nálezů, které mají smysl
- [ ] všechny parametry deklarované v manifestu, hesla s `secret = true`
- [ ] žádné heslo ani hodnota secretu ve výstupu
- [ ] data jdou na stdout, diagnostika na stderr, nic nezůstává na zálohovaném hostu
- [ ] `timeout` odpovídá reálné době běhu na **největší** instanci, ne na testovací
- [ ] projde s `env -i` (hubené `PATH`), ne jen v tvém shellu
- [ ] projde na **cílovém hostu** ručním spuštěním, `exit_code = 0` a `bytes > 0`
- [ ] selhání se pozná: zkus to i s chybným heslem a ověř, že běh skončí nenulovým kódem
- [ ] u souborové zálohy: **obnova jednoho souboru vyzkoušená** ze záložky Obnova
- [ ] manifest i kód commitnuté (instance a hesla ne — ty jsou v DB)

Související: [README → Jak napsat vlastní zálohovací skript](../README.md#jak-napsat-vlastní-zálohovací-skript) ·
[vývoj backendu](backend-development.md) · [nasazení produkce](production.md)
