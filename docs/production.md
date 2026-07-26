# Návod: nasazení produkční verze

Postup od čistého serveru k běžícímu Arcatum se zapnutým zabezpečením. Vývojový režim
(plain HTTP, bez ověřování) je popsaný v [README → Rychlý start](../README.md#rychlý-start-lokální-vyzkoušení)
a na produkci se nepoužívá.

Pořadí kroků není libovolné: PKI musí existovat před prvním startem serveru, server musí
běžet a publikovat binárky před instalací prvního runneru, a runner musí být schválený,
než mu půjde přidělit instance.

- [1. Předpoklady](#1-předpoklady)
- [2. Rozvržení na disku](#2-rozvržení-na-disku)
- [3. Build binárek](#3-build-binárek)
- [4. PKI](#4-pki)
- [5. `server.toml`](#5-servertoml)
- [6. Systemd služba serveru](#6-systemd-služba-serveru)
- [7. První start a ověření](#7-první-start-a-ověření)
- [8. Publikování buildů runneru](#8-publikování-buildů-runneru)
- [9. Nasazení runnerů](#9-nasazení-runnerů)
- [10. Instance](#10-instance)
- [11. Přístup z prohlížeče](#11-přístup-z-prohlížeče)
- [12. Provoz](#12-provoz)
- [13. Aktualizace serveru](#13-aktualizace-serveru)
- [14. Záloha samotného Arcatum](#14-záloha-samotného-arcatum)
- [Checklist](#checklist)

---

## 1. Předpoklady

**Na centrálním serveru:**

| Co | Proč |
|---|---|
| Linux se systemd | služba `arcatum-server` |
| `restic` (`apt install restic`) | **obnova dat a velikost repozitářů běží na serveru** — bez resticu tyhle části API vrátí chybu |
| dost místa v `backup_dir` | leží tam restic repozitáře i logy běhů |
| Go 1.26+ | jen pro build; výsledek je statický binár bez závislostí |

Go se v tomhle prostředí nenachází na `PATH`:

```sh
export PATH=/usr/local/go/bin:$PATH
```

**Na každém zálohovaném serveru:** systemd, `curl`, a nástroje, které tvoje skripty
používají (`restic` pro souborové zálohy, `mysqldump` pro MySQL, …). Chybějící `restic`
runner nahlásí jasnou chybou, ne záhadným selháním.

**Síť:** runnery potřebují **odchozí** spojení na port API (8443) a na bootstrap port (80).
Do zálohovaných serverů se nikdy nepřipojuje nic zvenčí — komunikace je pull.

---

## 2. Rozvržení na disku

Doporučené rozvržení, na které odkazuje celý zbytek návodu:

```
/opt/arcatum/                     git checkout (kvůli scripts/ a deploy/)
  scripts/                        DEFINICE skriptů — server je čte za běhu

/etc/arcatum/server.toml          konfigurace serveru
/usr/local/bin/arcatum-server     binárka serveru
/usr/local/bin/arcatum-ca         správa PKI

/central_backup/arcatum/          backup_dir
  data/arcatum.db                 SQLite (instance, běhy, evidence runnerů)
  runs/<run_id>/{stdout,stderr}.log   zachycený výstup běhů
  restic/<instance>/              restic repozitář každé instance
  dist/                           publikované binárky runneru + VERSION
  pki/                            CA, certifikáty, podepisovací a master klíč
```

**Adresář `scripts` drž jako git checkout.** Definice skriptů jsou verzovaný kód; instance
a hesla v repozitáři nejsou (viz [README → Skript vs. instance](../README.md#skript-vs-instance)).

```sh
git clone <repo> /opt/arcatum
install -d -m 0755 /central_backup/arcatum/{data,runs,restic,dist}
install -d -m 0700 /central_backup/arcatum/pki
install -d -m 0755 /etc/arcatum
```

---

## 3. Build binárek

Verzi vypaluj do binárky přes `-ldflags` — auto-update runnerů na ní stojí a nestampovaný
build se hlásí jako `dev`:

```sh
cd /opt/arcatum
V=$(date +%Y.%m.%d)

go build -ldflags "-X arcatum/pkg/version.Version=$V" -o /usr/local/bin/arcatum-server ./cmd/server
go build -ldflags "-X arcatum/pkg/version.Version=$V" -o /usr/local/bin/arcatum-ca     ./cmd/arcatum-ca
```

Server běží bez CGO (SQLite přes `modernc.org/sqlite`), takže výsledek je jeden statický
soubor — žádné runtime závislosti kromě resticu, který se volá jako externí program.

---

## 4. PKI

Jeden příkaz vytvoří všechno. `-H` musí obsahovat **každou** adresu, na kterou se runnery
připojují (IP i DNS), jinak jim selže ověření TLS:

```sh
cd /opt/arcatum
deploy/gen-certs.sh -d /central_backup/arcatum/pki \
  -H 172.24.0.60,arcatum.xtuning.local -a petr
```

Runnerům certifikáty **předem negeneruj** — vydají se samy při enrollmentu (krok 9).
`-a petr` je tvůj admin certifikát pro web a API.

Práva a záloha:

```sh
chmod 600 /central_backup/arcatum/pki/*.key
chmod 644 /central_backup/arcatum/pki/*.pem /central_backup/arcatum/pki/*.pub
chown -R root:root /central_backup/arcatum/pki
```

Tři soubory si **odnes mimo tenhle stroj** (šifrovaně, offline):

| Soubor | Co se stane při ztrátě |
|---|---|
| `secrets-master.key` | všechna uložená hesla instancí jsou nečitelná — **včetně hesel restic repozitářů, tedy i záloh** |
| `ca.key` | nelze vydat ani obnovit žádný certifikát; runnery postupně odpadnou, jak jim vypršej |
| `dispatch-signing.key` | nelze podepsat úlohu ani publikovat aktualizaci runnerů |

Ztráta master klíče je jediná chyba v tomhle návodu, která umí zlikvidovat zálohy samotné.
Odnes ho **před** vytvořením první instance.

---

## 5. `server.toml`

```sh
cp /opt/arcatum/config/server.example.toml /etc/arcatum/server.toml
chmod 640 /etc/arcatum/server.toml
```

Produkční obsah:

```toml
[server]
listen    = "0.0.0.0:8443"
scripts   = "/opt/arcatum/scripts"          # absolutní cesta, ne relativní
data_dir  = "/central_backup/arcatum/data"
timezone  = "Europe/Prague"
log_level = "info"

[storage]
backup_dir = "/central_backup/arcatum"

[tls]
ca_cert = "/central_backup/arcatum/pki/ca.pem"
cert    = "/central_backup/arcatum/pki/server.pem"
key     = "/central_backup/arcatum/pki/server.key"

[signing]
key = "/central_backup/arcatum/pki/dispatch-signing.key"

[secrets]
master_key = "/central_backup/arcatum/pki/secrets-master.key"

[bootstrap]
listen   = "0.0.0.0:80"
dist_dir = "/central_backup/arcatum/dist"
api_url  = "https://172.24.0.60:8443"       # tuhle adresu dostane runner do runner.toml
ca_key   = "/central_backup/arcatum/pki/ca.key"
```

Co config **odmítne** už při startu, místo aby to tiše obešel:

- `[tls]` vyplněné napůl — všechny tři cesty patří k sobě. Poloviční konfigurace by
  znamenala propadnutí na nezabezpečené HTTP.
- `[tls]` bez `[signing] key` — runnery by neměly co ověřovat.
- `[tls]` bez `[secrets] master_key` — hesla by ležela v `arcatum.db` v plaintextu.
- `[bootstrap]` bez `api_url` nebo `ca_key`, případně bez `[tls]` — vydávat certifikáty do
  systému, který je nekontroluje, nemá smysl.

Dvě věci, které se snadno popletou:

- **`listen` vs. `api_url`.** `listen` je, kde server naslouchá; `api_url` je adresa, kterou
  server napíše do generovaného `runner.toml`. Server svou vlastní dosažitelnou adresu
  nezná — musíš mu ji říct.
- **`log_level`** se z configu načte, ale server dnes loguje jednou úrovní; nastavení
  `debug` tedy víc nevypíše.

---

## 6. Systemd služba serveru

```sh
cat > /etc/systemd/system/arcatum-server.service <<'EOF'
[Unit]
Description=Arcatum backup server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/arcatum-server -config /etc/arcatum/server.toml -instances /dev/null
WorkingDirectory=/opt/arcatum
Restart=always
RestartSec=5

# Potřeba jen když službu přepneš na neprivilegovaného uživatele: bootstrap
# listener sedí na portu 80. Rootovi je to zbytečné, ale neškodí.
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
ProtectSystem=full
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=/central_backup/arcatum

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now arcatum-server
```

Poznámky k unitu:

- **`-instances /dev/null`** — instance se spravují z webu a v DB. Seed soubor se používá
  jen při prvním naplnění (krok 10); nechávat ho v `ExecStart` znamená držet hesla
  v plaintextu na disku navěky.
- **`PrivateTmp=yes`** je v pořádku i s resticem — obnova streamuje přímo do odpovědi,
  nestaguje se na disk.
- **`ProtectSystem=full`** ponechává `/usr` read-only. Když si `backup_dir` dáš jinam,
  uprav `ReadWritePaths`.
- Bez `User=` běží služba jako root — čte privátní klíče z `pki/` a naváže port 80.
  Přechod na vlastního uživatele je možný: přidej `User=arcatum`, dej mu vlastnictví
  `pki/` a `backup_dir`, a nech `AmbientCapabilities` kvůli portu 80 (nebo bootstrap
  přesuň na port > 1024 a předřaď reverzní proxy).

---

## 7. První start a ověření

```sh
journalctl -u arcatum-server -n 30
```

V logu musí být tohle:

```
arcatum-server listening on 0.0.0.0:8443
  scripts=/opt/arcatum/scripts  db=…/data/arcatum.db  backup_dir=/central_backup/arcatum
  instance secrets are encrypted at rest
  new certificates are issued under "Arcatum CA"
  server certificate valid until 2027-…
  bootstrap (plain HTTP) on 0.0.0.0:80 — install.sh and enrollment
  mTLS enabled (CA …/ca.pem); job dispatches are signed
```

Když se objeví některé z těchto varování, **nasazení není hotové**:

```
WARNING: no [tls] configured — plain HTTP, callers are not authenticated.
WARNING: no [secrets] master_key — credentials are stored in the database in plaintext.
```

Funkční test API (z tvého počítače, s admin certifikátem):

```sh
A=(--cacert pki/ca.pem --cert pki/admin-petr.pem --key pki/admin-petr.key)
curl "${A[@]}" https://172.24.0.60:8443/api/v1/whoami     # identita a expirace certifikátů
curl "${A[@]}" https://172.24.0.60:8443/status            # textový přehled + seznam skriptů
curl "${A[@]}" https://172.24.0.60:8443/api/v1/scripts    # co server načetl z scripts/
```

Bez certifikátu musí spojení skončit chybou — to je důkaz, že mTLS opravdu platí:

```sh
curl -k https://172.24.0.60:8443/api/v1/runs      # očekává se selhání handshaku
```

> **Vadný manifest zabrání startu serveru.** Katalog skriptů se načítá při startu a chybný
> `*.toml` (nebo chybějící entrypoint) je fatální chyba — schválně, ať se to pozná hned.
> Totéž platí opačně: **nový skript se objeví až po restartu serveru.**

---

## 8. Publikování buildů runneru

Runnery se stahují z `dist_dir`. Publikovat znamená nakopírovat binárky a napsat vedle nich
verzi:

```sh
cd /opt/arcatum
V=$(date +%Y.%m.%d)
D=/central_backup/arcatum/dist

for A in amd64 arm64; do
  GOOS=linux GOARCH=$A go build -ldflags "-X arcatum/pkg/version.Version=$V" \
    -o $D/arcatum-runner-linux-$A ./cmd/runner
done
echo "$V" > $D/VERSION
```

**Bez souboru `VERSION` se aktualizace nikomu nenabídne** — binárky samy o sobě neznamenají
vydání. To je záměrné: můžeš binárky nakopírovat a vydat je až zápisem verze.

Manifest aktualizací je podepsaný podepisovacím klíčem úloh a runner si před přepsáním sebe
ověří SHA‑256. Publikovat build tedy nejde jen z přístupu k `dist_dir` bez restartu serveru —
manifest podepisuje běžící server svým klíčem.

---

## 9. Nasazení runnerů

Na zálohovaném serveru jeden příkaz:

```sh
curl -LsSf http://172.24.0.60/arcatum_runner/install.sh | sudo sh
```

Instalátor stáhne binárku pro danou platformu, `ca.pem` a podepisovací veřejný klíč, napíše
`/etc/arcatum-runner/runner.toml`, nainstaluje systemd službu a spustí ji. Adresu serveru si
odvodí z URL, ze které se sám stáhl.

Runner si vygeneruje **vlastní** klíč (nikdy neopustí hostitele) a pošle jen žádost o podpis.
Pak čeká — a to je správný stav:

```sh
systemctl status arcatum-runner
journalctl -u arcatum-runner -f
```

Ve webu (záložka **Runnery**) žádost **schval**. Vidíš při tom IP adresu a fingerprint, takže
poznáš, že jde o pravý host. Runner_id je `hostname -s` daného stroje — tohle jméno pak patří
do `runner_id` instancí.

> **`curl … | sh` přes plain HTTP** znamená, že si zálohovaný server spustí jako root skript
> stažený ze sítě. Pro vnitřní síť je to běžný kompromis. Kdo chce víc, rozdá `ca.pem` předem
> (konfiguračním nástrojem) a stahuje přes plně ověřené HTTPS, nebo vydá certifikát ručně:
> `arcatum-ca runner -dir pki -id web-01` a soubory nakopíruje.

Ověření z centrální strany:

```sh
curl "${A[@]}" https://172.24.0.60:8443/api/v1/runners   # stav, platforma, verze, last_seen
```

---

## 10. Instance

Pro provoz je nejlepší cesta **web → Instance → nová instance**: formulář se sestaví
z parametrů skriptu, hodnoty se validují proti manifestu při uložení a hesla se šifrují už
při zápisu. Změny (včetně rozvrhu) platí bez restartu serveru.

Když zakládáš desítky instancí naráz, vyplatí se seed soubor. Ten obsahuje hesla
v plaintextu, proto:

```sh
# jednorázově, s dočasným souborem mimo repozitář
install -m 600 /dev/null /root/instances.json
$EDITOR /root/instances.json

systemctl stop arcatum-server
# jeden krátký běh jen kvůli seedu; v logu se objeví "seeded N new instance(s)"
timeout 5 /usr/local/bin/arcatum-server \
  -config /etc/arcatum/server.toml -instances /root/instances.json
shred -u /root/instances.json
systemctl start arcatum-server
```

Hesla se při importu zašifrují master klíčem, takže v `arcatum.db` už plaintext neleží.
`-instances /dev/null` v unitu je bezpečné — prázdný i chybějící soubor server přejde
bez chyby.

Seed vytvoří jen instance, které **ještě neexistují** — existující nepřepíše, jinak by
restart pokaždé vrátil změny udělané z webu. Vynutit přepsání jde `-import-force`, což je
přesně to, co při běžném provozu nechceš.

První ostrý test každé instance dělej ručně: **spustit teď** a sleduj živý tail výstupu
(viz [návod na ladění skriptů](script-development.md)). Nečekej na nočný rozvrh.

---

## 11. Přístup z prohlížeče

Web má stejnou ochranu jako API, takže prohlížeč musí poslat admin certifikát:

```sh
openssl pkcs12 -export \
  -inkey pki/admin-petr.key -in pki/admin-petr.pem -certfile pki/ca.pem \
  -out admin-petr.p12
```

`.p12` naimportuj do prohlížeče (Firefox: *Nastavení → Certifikáty → Vaše certifikáty*),
a `pki/ca.pem` přidej mezi důvěryhodné autority, ať web nehlásí neznámého vydavatele.
Bez certifikátu spojení neprojde — to je záměr.

Admin certifikát platí **1 rok** a vyprší nejdřív ze všech. Web na to upozorní nahoře;
obnova je `arcatum-ca admin -dir pki -name petr` a nový import do prohlížeče.

---

## 12. Provoz

**Denní kontrola** — web, záložka Běhy: cokoli jiného než `success` chce pohled do detailu
běhu. Ze shellu:

```sh
curl "${A[@]}" https://172.24.0.60:8443/api/v1/runs?limit=20
curl "${A[@]}" https://172.24.0.60:8443/api/v1/runs/42/output
curl "${A[@]}" "https://172.24.0.60:8443/api/v1/runs/42/output?stream=stderr"
```

Výstup leží i přímo na serveru v `backup_dir/runs/<run_id>/{stdout,stderr}.log`.

> **Notifikace při selhání zatím nejsou** (e-mail/Slack je v backlogu). Do té doby je
> kontrola záložky Běhy — nebo cron nad `/api/v1/runs` — jediné, co selhání odhalí.

**Co hlídat pravidelně:**

| Kde | Co |
|---|---|
| záložka Běhy | selhané a nespuštěné běhy |
| záložka Runnery | `last_seen` (mlčící runner nezálohuje), verze buildu, expirace certifikátů |
| záložka Klíče / `GET /api/v1/rotation` | rozjezd rotace, pokud nějaká běží |
| `GET /api/v1/instances/{id}/repo` | velikost repozitáře a počet snapshotů — nečekaný nárůst nebo zamrznutí |
| místo v `backup_dir` | restic dedupuje, ale neroste jen dolů |
| záložka Obnova | **občas zkus opravdu obnovit soubor** — nezkoušená záloha není záloha |

Runnery se aktualizují samy; verze každého hostu je vidět v záložce Runnery. Host, který
chceš držet na pevné verzi, dostane `auto_update = false` v `runner.toml`.

Rotace klíčů a životní cyklus certifikátů mají vlastní postupy v
[README → Rotace klíčů](../README.md#rotace-klíčů).

---

## 13. Aktualizace serveru

```sh
cd /opt/arcatum && git pull
V=$(date +%Y.%m.%d)
go build -ldflags "-X arcatum/pkg/version.Version=$V" -o /usr/local/bin/arcatum-server.new ./cmd/server
mv /usr/local/bin/arcatum-server /usr/local/bin/arcatum-server.old
mv /usr/local/bin/arcatum-server.new /usr/local/bin/arcatum-server
systemctl restart arcatum-server
journalctl -u arcatum-server -n 30
```

Co se u restartu děje:

- **Schéma DB se migruje samo.** Tabulky se zakládají idempotentně a chybějící sloupce se
  přidají; existující databáze se upgraduje na místě.
- **Katalog skriptů se načte znovu** — tady se projeví nové nebo změněné `scripts/*.toml`.
- **Rozvrhy se přepočítají od aktuálního času.** Scheduler je v paměti, takže běh, který měl
  padnout během restartu, se **přeskočí**. Restartuj tedy mimo okna záloh, nebo dotčené
  instance spusť ručně.
- **Probíhající běhy** restart přeruší; runner selhání nahlásí při dalším checkinu.

Rollback je `mv` staré binárky zpět. Sloupce přidané novější verzí staré verzi nevadí —
ignoruje je.

Runnery aktualizuj po serveru (krok 8). Když se změní protokol nebo formát podpisu, patří
nejdřív nový server, pak runnery — server rozumí i starším runnerům, opačně to neplatí.

---

## 14. Záloha samotného Arcatum

Arcatum zálohuje ostatní; sebe si nezazálohuje. Bez těchhle tří věcí je obnova dat nemožná
nebo bolestivá:

| Co | Kam | Jak často |
|---|---|---|
| `pki/secrets-master.key`, `pki/ca.key`, `pki/dispatch-signing.key` | offline, šifrovaně, mimo tenhle stroj | jednou, a po každé rotaci |
| `data/arcatum.db` | jiný stroj | denně |
| `restic/` | jiný stroj / off-site (3‑2‑1) | podle hodnoty dat |

**Databáze — konzistentní kopie.** SQLite jede v režimu WAL, takže `cp` běžící databáze umí
vytvořit soubor bez posledních transakcí. Buď službu na chvíli zastav:

```sh
systemctl stop arcatum-server
tar czf /tmp/arcatum-db-$(date +%F).tar.gz -C /central_backup/arcatum data
systemctl start arcatum-server
```

nebo, je-li k dispozici klient `sqlite3`, udělej online kopii:

```sh
sqlite3 /central_backup/arcatum/data/arcatum.db ".backup '/tmp/arcatum-$(date +%F).db'"
```

**Proč databáze i master klíč zvlášť:** hesla restic repozitářů leží šifrovaná v DB
a klíč k nim je `secrets-master.key`. Máš-li jen jedno z toho, k datům v repozitářích se
nedostaneš. Naopak s oběma zvládneš obnovu i bez Arcatum — restic repozitář jde otevřít
přímo (viz [README → Obnova dat](../README.md#obnova-dat)).

---

## Checklist

Před předáním do provozu:

- [ ] `restic` nainstalovaný **na serveru** (obnova) i na zálohovaných hostech
- [ ] `secrets-master.key`, `ca.key`, `dispatch-signing.key` zazálohované offline
- [ ] v logu serveru **žádné** `WARNING: no [tls]` / `no [secrets] master_key`
- [ ] volání API bez certifikátu selže; s admin certifikátem projde
- [ ] `scripts` je absolutní cesta a `/api/v1/scripts` vrací, co má
- [ ] `dist/VERSION` existuje a záložka Runnery hlásí u hostů reálnou verzi, ne `dev`
- [ ] všechny runnery schválené, `last_seen` svěží
- [ ] každá instance jednou spuštěná ručně a doběhla do `success`
- [ ] **obnova jednoho souboru z webu vyzkoušená**
- [ ] admin certifikát v prohlížeči, `ca.pem` mezi důvěryhodnými
- [ ] seed `instances.json` smazaný (`shred -u`), ne zapomenutý v `ExecStart`
- [ ] záloha `arcatum.db` a off-site kopie `restic/` naplánovaná

Související: [architektura](architecture.md) · [vývoj backendu](backend-development.md) ·
[vývoj skriptů](script-development.md)
