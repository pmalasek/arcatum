# Návod: nasazení produkční verze

Postup od čistého serveru k běžícímu Arcatum se zapnutým zabezpečením. Vývojový režim
(plain HTTP, bez ověřování) je popsaný v [README → Rychlý start](../README.md#rychlý-start-lokální-vyzkoušení)
a na produkci se nepoužívá.

Pořadí kroků není libovolné: PKI musí existovat před prvním startem serveru, server musí
běžet a publikovat binárky před instalací prvního runneru, a runner musí být schválený,
než mu půjde přidělit instance.

- [0. Instalace v kostce](#0-instalace-v-kostce)
- [1. Předpoklady](#1-předpoklady)
- [2. Rozvržení na disku](#2-rozvržení-na-disku)
- [3. Build a instalace binárek](#3-build-a-instalace-binárek)
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

## 0. Instalace v kostce

Adresáře, binárky, definice skriptů, PKI, config i systemd unit udělá
[`deploy/install-server.sh`](../deploy/install-server.sh). Na build stroji vyrobíš balík,
na serveru ho rozbalíš a spustíš instalátor. Předpoklad je jen `restic` na cílovém stroji.
Proč který krok existuje a co se u něj dá zkazit, řeší kroky 1–14 — tohle je jen postup.

```sh
# --- na build stroji ---
cd ~/src/backup_central
export V=$(date +%Y.%m.%d)
just bundle                                    # → bin/arcatum-$V.tar.gz
scp bin/arcatum-$V.tar.gz root@172.24.0.60:

# --- na serveru ---
tar xzf arcatum-$V.tar.gz
arcatum-$V/deploy/install-server.sh -H 172.24.0.60,arcatum.xtuning.local -a petr

# --- první start na popředí: hned je vidět, jestli config a PKI drží (Ctrl-C ukončí) ---
cd /root && arcatum-server -instances /dev/null
```

`-H` musí obsahovat **každou** adresu, na kterou se runnery připojují (IP i DNS) — jde do
certifikátu serveru a do `api_url`. `-a petr` je tvůj admin certifikát pro API.

Co instalátor udělá: založí `/opt/arcatum/{bin,pki,dist,scripts}`, `/etc/arcatum`
a `/central_backup/arcatum`; nainstaluje `arcatum-server` a `arcatum-ca` do
`/opt/arcatum/bin` a udělá na ně symlinky v `/usr/local/bin`; nakopíruje buildy runneru do
`dist/` a definice skriptů do `scripts/`; vygeneruje PKI; napíše `server.toml` a systemd
unit. **Co už existuje, nechá být** — takže tentýž příkaz je i postup pro aktualizaci
([krok 13](#13-aktualizace-serveru)). Suchý běh `-n` vypíše, co by udělal, a nesáhne na nic.

`-config` u ručního startu schválně není: server si config najde sám — nejdřív
`./server.toml`, pak `/etc/arcatum/server.toml`. `cd /root` je ujištění, že se spouští
odjinud než z adresáře s vlastním `server.toml`, takže se použije ten nainstalovaný.

V logu musí být `mTLS enabled` a `instance secrets are encrypted at rest`, a **žádné**
`WARNING`. Ve stejném výpisu je i **vygenerované heslo účtu `admin`** pro web — opiš si ho,
podruhé se nevypíše:

```
  ┌─ first start: created the web account ─────────────────────
  │   user:     admin
  │   password: k4m2ftq7hn3bwzla
```

Když to sedí, ukonči běh Ctrl-C a udělej z toho službu — unit je
[v kroku 6](#6-systemd-služba-serveru):

```sh
HOST=172.24.0.60
systemctl enable --now arcatum-server
journalctl -u arcatum-server -n 30

# --- ověření z klienta (admin certifikát z pki/) ---
A=(--cacert /opt/arcatum/pki/ca.pem --cert /opt/arcatum/pki/admin-petr.pem
   --key /opt/arcatum/pki/admin-petr.key)
curl "${A[@]}" "https://$HOST:8443/api/v1/whoami"    # {"role":"admin","secured":true,…}
curl "${A[@]}" "https://$HOST:8443/api/v1/scripts"   # co server načetl ze scripts/
curl -k "https://$HOST:8443/api/v1/runs"             # MUSÍ selhat na handshaku
curl -sS "http://$HOST/"                             # bootstrap: nápověda k instalaci runneru
```

Buildy runneru i soubor `VERSION` (bez něj se hostům nic nenabídne) nakopíroval instalátor
z balíku — [krok 8](#8-publikování-buildů-runneru) je potřeba, až budeš vydávat novou verzi.

Tím server běží a je zabezpečený. Zbývá to, co už se dělá z webu nebo z jiných strojů:
[přihlásit se do webu](#11-přístup-z-prohlížeče) na `http://$HOST:8080/` jako `admin`
a hned si změnit heslo, [runnery](#9-nasazení-runnerů)
(`curl -LsSf http://$HOST/arcatum_runner/install.sh | sudo sh` na každém zálohovaném hostu,
pak schválení v záložce Runnery), [instance](#10-instance) a
[záloha klíčů](#14-záloha-samotného-arcatum) — ta je jediná, kterou nesmíš odložit.

---

## 1. Předpoklady

**Na centrálním serveru:**

| Co | Proč |
|---|---|
| Linux se systemd | služba `arcatum-server` |
| `restic` (`apt install restic`) | **obnova dat a velikost repozitářů běží na serveru** — bez resticu tyhle části API vrátí chybu |
| dost místa v `backup_dir` | leží tam restic repozitáře i logy běhů |
| Go 1.26+ | **jen** když stavíš přímo na serveru ([krok 3b](#b-build-přímo-na-serveru)); s balíkem z build stroje (3a) ho tu mít nemusíš |

Go se v tomhle prostředí nenachází na `PATH`:

```sh
export PATH=/usr/local/go/bin:$PATH
```

**Na každém zálohovaném serveru:** systemd, `curl`, a nástroje, které tvoje skripty
používají (`restic` pro souborové zálohy, `mysqldump` pro MySQL, …). Chybějící `restic`
runner nahlásí jasnou chybou, ne záhadným selháním.

**Síť:** runnery potřebují **odchozí** spojení na port API (8443) a na bootstrap port (80).
Do zálohovaných serverů se nikdy nepřipojuje nic zvenčí — komunikace je pull. Navíc port
webu (8080) musí být dostupný ze stanic, ze kterých se na Arcatum kouká; web je plain HTTP
s přihlášením heslem, takže patří do vnitřní sítě, ne na internet (a když už, tak za HTTPS
reverse proxy — viz [krok 11](#11-přístup-z-prohlížeče)).

---

## 2. Rozvržení na disku

Doporučené rozvržení, na které odkazuje celý zbytek návodu. Sloupec vpravo říká, kdo
adresář zakládá — polovinu za tebe udělá server sám:

```
/opt/arcatum/                     instalace (git checkout kvůli scripts/, deploy/)  ty
  bin/                            arcatum-server, arcatum-ca                  ty
  pki/                            CA, certifikáty, podepisovací a master klíč ty
  dist/                           publikované binárky runneru + VERSION       ty
  scripts/                        DEFINICE skriptů — server je čte za běhu    ty

/etc/arcatum/server.toml          konfigurace serveru                         ty
/usr/local/bin/arcatum-server     symlink na /opt/arcatum/bin/                ty
/usr/local/bin/arcatum-ca         symlink na /opt/arcatum/bin/                ty

/central_backup/arcatum/          backup_dir — nic než data                   ty (jen kořen)
  data/arcatum.db                 SQLite (instance, běhy, evidence runnerů)   server
  runs/<run_id>/{stdout,stderr}.log   zachycený výstup běhů                   server
  restic/<instance>/              restic repozitář každé instance             server
```

Dělicí čára je jednoduchá: **do `backup_dir` po založení nesaháš ručně vůbec.** Všechno pod
ním si zakládá a píše server sám. Co tam musíš nakopírovat ty, tam nepatří — patří to do
`/opt/arcatum`.

**Proč PKI není v `backup_dir`.** `secrets-master.key` dešifruje hesla restic repozitářů
uložená v databázi. Kdyby ležel na stejném svazku jako `restic/`, znamenala by jedna
odnesená kopie toho svazku — ukradený disk, přimountovaný NFS export, off-site kopie
z [kroku 14](#14-záloha-samotného-arcatum) — zašifrovaná data **i** klíč k nim v jednom
balíku, a šifrování by ztratilo většinu smyslu. Táž úvaha platí pro `dist/`: publikované
binárky runneru jsou distribuční artefakt, ne zálohovaná data.

Zakládáš tedy pět adresářů, dvěma příkazy:

```sh
install -d -m 0755 /etc/arcatum /opt/arcatum /opt/arcatum/dist /central_backup/arcatum
install -d -m 0700 /opt/arcatum/pki
```

`data/`, `runs/` a `restic/<instance>/` si server vytvoří sám (režimem 0750) — první dva při
startu, repozitáře až při prvním běhu instance. U `pki/` na právech záleží prakticky — když
ho necháš vytvořit `arcatum-ca`, dostane 0755, a privátní klíče leží v adresáři, do kterého
smí kdokoli.

**Co musí být na disku vedle binárky.** Web UI i `install.sh` jsou zakompilované přímo
v binárce (`go:embed`), takže se neinstalují a nemůžou se rozejít s verzí serveru.
**Definice skriptů ale ne** — `scripts/` server čte z disku. Zapomenutý adresář navíc
start **nezastaví**: katalog jen zůstane prázdný a pozná se to až tím, že web nenabízí
žádný skript. (Chybný manifest je naopak fatální — viz krok 7.)

**Definice skriptů jsou verzovaný kód** — instance a hesla v repozitáři nejsou (viz
[README → Skript vs. instance](../README.md#skript-vs-instance)). Do `/opt/arcatum/scripts`
se tedy nikdy needituje ručně; obsah tam srovnává instalátor z repozitáře, ať už z balíku
z build stroje, nebo z checkoutu přímo na serveru ([krok 3](#3-build-a-instalace-binárek)).

Checkout na serveru dává smysl, když chceš aktualizovat přes `git pull` místo `scp`; pak
`/opt/arcatum` je rovnou ten checkout a `bin/`, `local/` v něm jsou build artefakty. Bez
checkoutu je `/opt/arcatum` jen instalace a repozitář žije na build stroji — obě varianty
jsou popsané v kroku 3.

---

## 3. Build a instalace binárek

Vznikají tři binárky a každá má na produkci jiné místo:

| Binárka | Kam patří | K čemu |
|---|---|---|
| `arcatum-server` | `/opt/arcatum/bin/` | služba (krok 6) |
| `arcatum-ca` | `/opt/arcatum/bin/` | PKI, obnova certifikátů, rotace |
| `arcatum-runner` | `dist/` **pod jménem `arcatum-runner-linux-<arch>`** | odsud si ho stahují zálohované hosty (krok 8) |

Celá instalace je tak jeden adresář: `rm -rf /opt/arcatum` a `/etc/arcatum` ji beze zbytku
odstraní a nikde nemůže zůstat ležet binárka z minulé verze. Systemd volá `arcatum-server`
absolutní cestou, takže na `PATH` nezáleží.

`/usr/local/bin` přesto dostane dva **symlinky** — na `arcatum-ca` a `arcatum-server`. To
jsou jediné dvě věci, které se píšou ručně: obnova certifikátů po expiraci, rotace klíčů
a `arcatum-server -passwd admin`, když se nikdo nedostane do webu. Bez `PATH` by navíc
`deploy/gen-certs.sh` `arcatum-ca` nenašel a tiše sáhl po `go run` — na serveru bez Go
tedy selhal přesně ve chvíli, kdy obnovuješ expirovaný certifikát. Symlink (ne kopie)
znamená, že aktualizace přepíše soubor, na který ukazuje, a verze se nemůžou rozejít.

Runner se na centrální server **neinstaluje** — ten ho jen rozdává. Proto nejde do `bin/`,
ale do `dist_dir` a s příponou platformy v názvu; jiné jméno bootstrap nenajde a instalace
runneru skončí na 404.

Verzi vypaluj do binárky přes `-ldflags` — auto-update runnerů na ní stojí a nestampovaný
build se hlásí jako `dev`.

### a) Balík z build stroje (doporučeno)

Server běží bez CGO (SQLite přes `modernc.org/sqlite`), takže binárky jsou statické soubory
bez runtime závislostí — kromě resticu, který se volá jako externí program. Na produkčním
serveru tedy Go být nemusí a nemá.

```sh
cd ~/src/backup_central
export V=$(date +%Y.%m.%d)
just bundle                                    # → bin/arcatum-$V.tar.gz
scp bin/arcatum-$V.tar.gz root@172.24.0.60:
```

`just bundle` postaví server, `arcatum-ca` a runnery pro amd64 i arm64 se stejnou verzí,
přibalí `scripts/`, `deploy/`, ukázkový config a soubor `VERSION`, a zabalí to do jednoho
archivu. Verzi vypaluje do binárek přes `-ldflags` — auto-update runnerů na ní stojí
a nestampovaný build se hlásí jako `dev`.

Na serveru rozbal a spusť instalátor. Ten už nic nestaví, jen rozmisťuje:

```sh
tar xzf arcatum-$V.tar.gz
arcatum-$V/deploy/install-server.sh -H 172.24.0.60,arcatum.xtuning.local -a petr
```

Instalátor nainstaluje binárky (přes dočasné jméno a `mv`, aby mu nevadila běžící služba),
nakopíruje buildy runneru do `dist/`, srovná `scripts/` (přes `rsync -a --delete`, je-li
k dispozici) a doplní, co chybí z PKI, configu a systemd unitu. Kroky 4–6 popisují, co
přesně napsal a co si v tom můžeš upravit.

Runnery stavěj pro **každou** architekturu, kterou máš mezi zálohovanými hosty — cizí
architekturu už na serveru bez Go nedoženeš. `just bundle` dělá amd64 a arm64; jinou
přidej ručně do `bin/` před zabalením.

### b) Build přímo na serveru

Když na serveru checkout a Go chceš (kvůli `git pull` místo `scp`), je to totéž bez
přenosu:

```sh
git clone <repo> /opt/arcatum        # jen poprvé
cd /opt/arcatum
just release && just dist-runner bin
deploy/install-server.sh -H 172.24.0.60,arcatum.xtuning.local -a petr
```

`just` povinné není — recepty jen skládají `go build` s `-ldflags`. Instalátor si vystačí
s tím, že v `bin/` leží `arcatum-server`, `arcatum-ca` a případně `arcatum-runner-linux-*`
se souborem `VERSION`.

Že binárky na tomhle stroji jdou spustit, ověř přes `arcatum-ca` — vypíše nápovědu a skončí
s kódem 0:

```sh
arcatum-ca -h
```

> **Server v tuhle chvíli spustit nejde** — config ještě neexistuje (krok 5) a bez něj
> server skončí chybou:
>
> ```
> config: no configuration found (looked for server.toml, /etc/arcatum/server.toml) —
> install one in /etc/arcatum or pass -config
> ```
>
> To je záměr: nenajít konfiguraci **není** důvod nastartovat na vestavěných výchozích
> hodnotách, protože ty znamenají plain HTTP a hesla instancí v databázi v plaintextu.
> Překlep v `-config` proto start zastaví, ne tiše zapne nezabezpečený server. Špatně
> zapsaný config je fatální taky, ten se validuje.

Verze serveru se nikde nevypisuje samostatně; ověřuje se až přes běžící API v kroku 7,
u runnerů pak v záložce Runnery. `dist/VERSION` teprve **nezapisuj** — to je až krok 8,
který z nakopírovaných souborů udělá vydání.

---

## 4. PKI

**Na první instalaci tenhle krok už proběhl** — PKI vytvořil instalátor z parametru `-H`
a existující nikdy nepřepíše. Zbytek sekce je pro ruční vydání a pro obnovu certifikátů.

`-H` musí obsahovat **každou** adresu, na kterou se runnery připojují (IP i DNS), jinak jim
selže ověření TLS. Jeden příkaz vytvoří všechno:

```sh
cd /opt/arcatum
deploy/gen-certs.sh -d /opt/arcatum/pki \
  -H 172.24.0.60,arcatum.xtuning.local -a petr
```

Skript potřebuje repozitář: přepne se do jeho kořene a `arcatum-ca` použije z `PATH`, jinak
si ho spustí přes `go run`. Bez checkoutu udělej totéž třemi příkazy samotné binárky —
dělají přesně to, co skript volá:

```sh
arcatum-ca init   -dir /opt/arcatum/pki                               # CA, podpisový klíč, master klíč
arcatum-ca server -dir /opt/arcatum/pki -hosts 172.24.0.60,arcatum.xtuning.local
arcatum-ca admin  -dir /opt/arcatum/pki -name petr
```

Výchozí `-dir` je relativní `pki`, takže z `cd /opt/arcatum` trefí totéž i bez přepínače.

Ani jedna cesta ti nepřepíše existující PKI: `arcatum-ca init` nad hotovou CA skončí chybou
(*refusing to overwrite an existing CA*) a `gen-certs.sh` ji přeskočí a doplní jen to, co
chybí. `arcatum-ca server` a `admin` naopak certifikát vydají znovu — přesně proto se jimi
dělá obnova po expiraci.

Runnerům certifikáty **předem negeneruj** — vydají se samy při enrollmentu (krok 9).
`-a petr` je tvůj admin certifikát pro web a API.

Práva a záloha:

```sh
chmod 600 /opt/arcatum/pki/*.key
chmod 644 /opt/arcatum/pki/*.pem /opt/arcatum/pki/*.pub
chown -R root:root /opt/arcatum/pki
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

Instalátor ho napsal, pokud tam ještě nebyl, a od té chvíle na něj nesahá — úpravy jsou
tvoje a přežijou každou další instalaci. Ručně se vyrábí kopií ukázky, ve které je popsaná
každá volba:

```sh
cp /opt/arcatum/config/server.example.toml /etc/arcatum/server.toml
chmod 640 /etc/arcatum/server.toml
```

Produkční obsah:

```toml
[server]
listen    = "0.0.0.0:8443"                  # API pro runnery (mTLS)
scripts   = "/opt/arcatum/scripts"          # absolutní cesta, ne relativní
data_dir  = "/central_backup/arcatum/data"
timezone  = "Europe/Prague"
log_level = "info"

[web]
listen      = "0.0.0.0:8080"                # web UI: plain HTTP, přihlášení jménem a heslem
session_ttl = "12h"                         # jak dlouho vydrží přihlášení bez aktivity
# secure_cookie = true                      # jen za HTTPS reverse proxy

[storage]
backup_dir = "/central_backup/arcatum"

[tls]
ca_cert = "/opt/arcatum/pki/ca.pem"
cert    = "/opt/arcatum/pki/server.pem"
key     = "/opt/arcatum/pki/server.key"

[signing]
key = "/opt/arcatum/pki/dispatch-signing.key"

[secrets]
master_key = "/opt/arcatum/pki/secrets-master.key"

[bootstrap]
listen   = "0.0.0.0:80"
dist_dir = "/opt/arcatum/dist"
api_url  = "https://172.24.0.60:8443"       # tuhle adresu dostane runner do runner.toml
ca_key   = "/opt/arcatum/pki/ca.key"
```

**Kde se config hledá.** Bez `-config` se bere první existující z:

| Pořadí | Cesta | K čemu |
|---|---|---|
| 1. | `./server.toml` | vývojový checkout spuštěný nad vlastním configem |
| 2. | `/etc/arcatum/server.toml` | produkční instalace |

Nenajde-li se ani jeden, server **skončí chybou**. To je celý smysl toho uspořádání: cokoli
spustíš na produkčním stroji odjinud než z adresáře s vlastním `server.toml` — služba,
binárka spuštěná ručně při ladění, `-passwd` — sáhne po téže konfiguraci, a tím po týchž
certifikátech. Nic se nedá odladit „skoro produkčně" s cizí PKI.

Do `/opt/arcatum` proto `server.toml` **nedávej**. Služba tam má `WorkingDirectory`, takže
by takový soubor přebil `/etc/arcatum/server.toml` a měl bys dvě konfigurace, mezi kterými
se rozhoduje podle toho, odkud se program spustil.

Načtenou cestu server vypíše hned prvním řádkem logu, ještě než na čemkoli z jejího obsahu
může spadnout:

```
configuration from /etc/arcatum/server.toml
```

Co config **odmítne** už při startu, místo aby to tiše obešel:

- `[tls]` vyplněné napůl — všechny tři cesty patří k sobě. Poloviční konfigurace by
  znamenala propadnutí na nezabezpečené HTTP.
- `[tls]` bez `[signing] key` — runnery by neměly co ověřovat.
- `[tls]` bez `[secrets] master_key` — hesla by ležela v `arcatum.db` v plaintextu.
- `[bootstrap]` bez `api_url` nebo `ca_key`, případně bez `[tls]` — vydávat certifikáty do
  systému, který je nekontroluje, nemá smysl.
- dva listenery na stejné adrese (`[web]`, `[server]`, `[bootstrap]`) — jinak by jeden
  z nich spadl na „address already in use" a nebylo by vidět který.
- nesmyslné `[web] session_ttl` — chybný údaj by tiše znamenal „nikdy nevyprší".

Dvě věci, které se snadno popletou:

- **`listen` vs. `api_url`.** `listen` je, kde server naslouchá; `api_url` je adresa, kterou
  server napíše do generovaného `runner.toml`. Server svou vlastní dosažitelnou adresu
  nezná — musíš mu ji říct.
- **`log_level`** se z configu načte, ale server dnes loguje jednou úrovní; nastavení
  `debug` tedy víc nevypíše.

---

## 6. Systemd služba serveru

Unit napsal instalátor (a stejně jako config ho pak nechává být). Tohle je jeho obsah —
k přečtení, k úpravě, nebo k ručnímu vytvoření:

```sh
cat > /etc/systemd/system/arcatum-server.service <<'EOF'
[Unit]
Description=Arcatum backup server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/arcatum/bin/arcatum-server -config /etc/arcatum/server.toml -instances /dev/null
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
ReadOnlyPaths=/opt/arcatum

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
- **`-config` je v `ExecStart` uvedený, i když by ho hledání našlo samo.** Služba má
  `WorkingDirectory=/opt/arcatum`, a explicitní cesta znamená, že soubor podstrčený do
  pracovního adresáře nemůže konfiguraci služby přebít. Při ladění z shellu naopak `-config`
  vynechávej — dostaneš tentýž config a nemáš co přepsat.
- **`PrivateTmp=yes`** je v pořádku i s resticem — obnova streamuje přímo do odpovědi,
  nestaguje se na disk.
- **`ProtectSystem=full`** ponechává `/usr` read-only, `ReadOnlyPaths=/opt/arcatum` totéž
  s instalací: server z `/opt/arcatum` jen čte (vlastní binárku, PKI, skripty, `dist/`)
  a zapisuje výhradně do `backup_dir`. Aktualizace tím omezená není — instalátor běží mimo
  službu. Když si `backup_dir` dáš jinam, uprav `ReadWritePaths`.
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
configuration from /etc/arcatum/server.toml
  server certificate valid until 2028-…
  new certificates are issued under "Arcatum CA"
arcatum-server listening on 0.0.0.0:8443
  scripts=/opt/arcatum/scripts  db=…/data/arcatum.db  backup_dir=/central_backup/arcatum
  instance secrets are encrypted at rest
  mTLS enabled (CA …/ca.pem); job dispatches are signed
  bootstrap (plain HTTP) on 0.0.0.0:80 — install.sh and enrollment
  web UI (plain HTTP, password login) on 0.0.0.0:8080
```

**První řádek zkontroluj vždycky** — jsou-li certifikáty jiné, než čekáš, obvykle je jiný
i config. Druhý a třetí se vypíšou ještě před hlášením o naslouchání, protože vznikají při
načítání PKI; řádky o bootstrapu a webu se mohou objevit kdekoli za ním, běží ve vlastních
gorutinách.

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

### Chyby handshaku v logu

Zapnuté mTLS znamená, že se v logu běžně objevují `TLS handshake error` — každý pokus
o připojení bez správného certifikátu vypadá jako chyba. **Server tím nic nehlásí o sobě,
hlásí, co se mu nepovedlo od klienta.** Podle textu se pozná, co dělá klient špatně:

| Text v logu | Příčina | Náprava |
|---|---|---|
| `client sent an HTTP request to an HTTPS server` | `http://` na port 8443 | používej `https://` |
| `remote error: tls: unknown certificate authority` | klient nezná `ca.pem` | přidej `ca.pem` mezi důvěryhodné autority (krok 11) |
| `tls: client didn't provide a certificate` | spojení došlo k mTLS, ale klient neposlal admin certifikát | naimportuj `.p12` (krok 11) nebo použij `--cert/--key` |
| `remote error: tls: unknown certificate` | **klient odmítl certifikát serveru** — typicky adresa není v jeho SAN | viz níž |
| `remote error: tls: bad certificate` | klient poslal certifikát, který tahle CA nevydala (např. po rotaci) | vydej nový: `arcatum-ca admin …` |

Slovo `remote error` znamená, že alert poslal **klient** — chyba je tedy v tom, čemu
nevěří on, ne v tom, co odmítl server.

**Adresa musí být v SAN certifikátu serveru.** Co v něm je, zjistíš takhle:

```sh
openssl x509 -in /opt/arcatum/pki/server.pem -noout -ext subjectAltName -dates
```

Vydáš-li certifikát jen na IP (`-hosts 172.24.0.60`), přes DNS jméno se nepřipojíš ani
s naimportovanou CA — a naopak. Doplnit adresu jde kdykoli, certifikát se prostě vydá znovu
a server restartuje:

```sh
arcatum-ca server -dir /opt/arcatum/pki -hosts 172.24.0.60,arcatum.xtuning.local
systemctl restart arcatum-server
```

Runnerům se tím nic nerozbije: ověřují CA, která zůstává stejná. Změnit ale musíš i
`[bootstrap] api_url`, míří-li na adresu, kterou jsi přidával — do `runner.toml` se zapisuje
z něj.

---

## 8. Publikování buildů runneru

Runnery se stahují z `dist_dir`. Publikovat znamená nakopírovat binárky a napsat vedle nich
verzi — což je přesně to, co dělá instalátor z balíku (`bin/arcatum-runner-linux-*`
a `bin/VERSION` jdou do `dist/`). **Nová verze runnerů se tedy vydává novým balíkem:**

```sh
# build stroj
V=$(date +%Y.%m.%d) just bundle && scp bin/arcatum-*.tar.gz root@172.24.0.60:
# server
tar xzf arcatum-*.tar.gz && arcatum-*/deploy/install-server.sh
```

Ručně, s Go na serveru, je to jeden recept — postaví obě architektury i soubor `VERSION`:

```sh
cd /opt/arcatum
just dist-runner /opt/arcatum/dist
```

Cílový adresář je poziční argument (bez něj se staví do `local/dist`, což je vývojová
cesta — na produkci ho tedy uveď). Verzi přebíjíš `V=…` jako u `just release`. Bez `just`
to jsou dva `go build` s `-ldflags` a `echo "$V" > dist/VERSION`.

Na jménech záleží: bootstrap i auto-update hledají přesně `arcatum-runner-linux-<arch>`.
Přejmenovaná nebo jinak pojmenovaná binárka se neprojeví chybou v logu — host si ji jen
nestáhne (HTTP 404 při instalaci).

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
cd /root && timeout 5 /opt/arcatum/bin/arcatum-server -instances /root/instances.json
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
v detailu běhu. Nečekej na noční rozvrh — až ráno zjistíš, že chybí `mysqldump`, je to
o jednu nezazálohovanou noc dražší.

---

## 11. Přístup z prohlížeče

Web běží na `[web] listen` (výchozí `0.0.0.0:8080`) jako **plain HTTP s přihlášením jménem
a heslem** — do prohlížeče se nic neinstaluje:

```
http://172.24.0.60:8080/
```

**První přihlášení:** účet `admin` a heslo, které server vypsal do logu při prvním startu
([krok 7](#7-první-start-a-ověření)). Když ho nemáš, vypsalo se jen jednou — nové vygeneruje:

```sh
arcatum-server -passwd admin
# nebo konkrétní heslo, mimo historii shellu:
ARCATUM_PASSWORD='…' arcatum-server -passwd admin
```

Hned po přihlášení si heslo změň (**změnit heslo** vpravo v hlavičce) a v záložce
**Uživatelé** přidej účty kolegům. Kdo má jen kontrolovat, jestli zálohy proběhly, dostane
roli `viewer` — vidí všechno, ale nic nespustí ani nezmění.

> **Web je plain HTTP, patří tedy do vnitřní sítě.** Když ho potřebuješ vystavit dál, postav
> před něj HTTPS reverse proxy a nastav `[web] secure_cookie = true`, aby cookie sezení
> chodila jen po HTTPS. Bez proxy tu volbu nezapínej — přihlášení by přestalo fungovat.

Admin **certifikát** je od téhle chvíle potřeba jen na volání API na portu 8443 ze shellu
(všechny `curl` příklady v tomhle návodu). Platí 1 rok, web na blížící se expiraci upozorní
nahoře a obnova je `arcatum-ca admin -dir pki -name petr`. Do prohlížeče ho importovat
nemusíš.

---

## 12. Provoz

**Denní kontrola** — web, záložka Běhy: cokoli jiného než `success` chce pohled do detailu
běhu. Ze shellu:

```sh
curl "${A[@]}" https://172.24.0.60:8443/api/v1/runs?limit=20
curl "${A[@]}" https://172.24.0.60:8443/api/v1/runs/run-42/output
curl "${A[@]}" "https://172.24.0.60:8443/api/v1/runs/run-42/output?stream=stderr"
```

ID běhu má tvar `run-42` — s holým číslem vrátí endpointy výstupu prázdné tělo, ne chybu.

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
| záložka Uživatelé | účty lidí, kteří už u firmy nejsou (vypnout nebo smazat), a `viewer` tam, kde admin není potřeba |

Runnery se aktualizují samy; verze každého hostu je vidět v záložce Runnery. Host, který
chceš držet na pevné verzi, dostane `auto_update = false` v `runner.toml`.

Rotace klíčů a životní cyklus certifikátů mají vlastní postupy v
[README → Rotace klíčů](../README.md#rotace-klíčů).

---

## 13. Aktualizace serveru

Aktualizace je tentýž příkaz jako instalace — instalátor nahradí binárky, buildy runneru
a `scripts/`, a config, PKI i unit nechá být. Běžící službu si restartuje sám:

```sh
# build stroj
cd ~/src/backup_central && git pull
V=$(date +%Y.%m.%d) just bundle
scp bin/arcatum-*.tar.gz root@172.24.0.60:

# server
tar xzf arcatum-*.tar.gz
arcatum-*/deploy/install-server.sh          # -H už není potřeba, PKI i config existují
journalctl -u arcatum-server -n 30
```

S checkoutem a Go na serveru totéž bez přenosu:

```sh
cd /opt/arcatum && git pull
just release && just dist-runner bin
deploy/install-server.sh
```

Zálohu předchozí binárky instalátor nedělá; když ji chceš, zkopíruj si
`/opt/arcatum/bin/arcatum-server` stranou před během. Přepisovat běžící binárku přímo nejde
(`Text file busy`) — instalátor ji proto ukládá vedle a přejmenovává na místo.

Co se u restartu děje:

- **Schéma DB se migruje samo.** Tabulky se zakládají idempotentně a chybějící sloupce se
  přidají; existující databáze se upgraduje na místě.
- **Katalog skriptů se načte znovu** — tady se projeví nové nebo změněné `scripts/*.toml`.
- **Rozvrhy se přepočítají od aktuálního času.** Scheduler je v paměti, takže běh, který měl
  padnout během restartu, se **přeskočí**. Restartuj tedy mimo okna záloh, nebo dotčené
  instance spusť ručně.
- **Probíhající běhy** restart přeruší; runner selhání nahlásí při dalším checkinu.

Rollback je rozbalit starší balík a spustit z něj instalátor (nebo `mv` odloženou binárku
zpět). Sloupce přidané novější verzí staré verzi nevadí — ignoruje je.

Runnery se aktualizují tímtéž balíkem: instalátor přepsal `dist/` i `VERSION`, takže hosty
si novou verzi stáhnou samy (krok 8). Když se změní protokol nebo formát podpisu, patří
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
- [ ] `scripts` je absolutní cesta, adresář je **fyzicky na serveru** a `/api/v1/scripts`
      vrací, co má (prázdná odpověď = zapomenutý `scripts/`, ne chyba v logu)
- [ ] první řádek logu je `configuration from /etc/arcatum/server.toml` — a v `/opt/arcatum`
      **neleží** žádný `server.toml`, který by ho při ručním spuštění přebil
- [ ] v `/central_backup/arcatum` není nic než `data/`, `runs/` a `restic/` — žádná PKI,
      žádné `dist/`
- [ ] `dist/VERSION` existuje a záložka Runnery hlásí u hostů reálnou verzi, ne `dev`
- [ ] všechny runnery schválené, `last_seen` svěží
- [ ] každá instance jednou spuštěná ručně a doběhla do `success`
- [ ] **obnova jednoho souboru z webu vyzkoušená**
- [ ] výchozí heslo účtu `admin` změněné, účty kolegů založené (viewer tam, kde stačí)
- [ ] port webu (8080) dosažitelný ze stanic operátorů a **ne** z internetu
- [ ] seed `instances.json` smazaný (`shred -u`), ne zapomenutý v `ExecStart`
- [ ] záloha `arcatum.db` a off-site kopie `restic/` naplánovaná

Související: [architektura](architecture.md) · [vývoj backendu](backend-development.md) ·
[vývoj skriptů](script-development.md)
