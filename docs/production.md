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

Celá instalace serveru jako jedna sekvence, k odklikání odshora dolů. Předpoklad je jen
tolik, že na cílovém stroji je `restic` a že v `/tmp` leží nakopírované binárky
a `scripts/` z build stroje (jak je tam dostat, je [krok 3b](#b-hotové-binárky-z-jiného-stroje)).
Proč který krok existuje a co se u něj dá zkazit, řeší kroky 1–14 — tohle je jen postup.

```sh
HOST=172.24.0.60                     # adresa, na kterou se runnery připojují
B=/central_backup/arcatum            # backup_dir

# --- adresáře (data/, runs/ a restic/ si server vytvoří sám) ---
install -d -m 0755 /etc/arcatum /opt/arcatum "$B/dist"
install -d -m 0700 "$B/pki"

# --- binárky na svá místa + definice skriptů na disk ---
install -m 0755 /tmp/arcatum-server /tmp/arcatum-ca /usr/local/bin/
install -m 0755 /tmp/arcatum-runner-linux-* "$B/dist/"
install -d -m 0755 /opt/arcatum/scripts && cp -a /tmp/scripts/. /opt/arcatum/scripts/

# --- PKI: jednou, ještě před prvním startem serveru ---
arcatum-ca init   -dir "$B/pki"                      # CA + podpisový + master klíč
arcatum-ca server -dir "$B/pki" -hosts "$HOST"       # doplň i DNS jméno, čárkou
arcatum-ca admin  -dir "$B/pki" -name petr           # tvůj certifikát do prohlížeče

# --- konfigurace ---
cat > /etc/arcatum/server.toml <<EOF
[server]
listen    = "0.0.0.0:8443"
scripts   = "/opt/arcatum/scripts"
data_dir  = "$B/data"
timezone  = "Europe/Prague"
log_level = "info"

[web]
listen      = "0.0.0.0:8080"
session_ttl = "12h"

[storage]
backup_dir = "$B"

[tls]
ca_cert = "$B/pki/ca.pem"
cert    = "$B/pki/server.pem"
key     = "$B/pki/server.key"

[signing]
key = "$B/pki/dispatch-signing.key"

[secrets]
master_key = "$B/pki/secrets-master.key"

[bootstrap]
listen   = "0.0.0.0:80"
dist_dir = "$B/dist"
api_url  = "https://$HOST:8443"
ca_key   = "$B/pki/ca.key"
EOF
chmod 640 /etc/arcatum/server.toml

# --- první start na popředí: hned je vidět, jestli config a PKI drží (Ctrl-C ukončí) ---
arcatum-server -config /etc/arcatum/server.toml -instances /dev/null
```

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
systemctl daemon-reload && systemctl enable --now arcatum-server
journalctl -u arcatum-server -n 30

# --- vydání runneru: bez VERSION se hostům nic nenabídne ---
printf '%s\n' 2026.07.26 > "$B/dist/VERSION"        # stejná verze, jakou mají binárky v dist/

# --- ověření z klienta (admin certifikát z pki/) ---
A=(--cacert "$B/pki/ca.pem" --cert "$B/pki/admin-petr.pem" --key "$B/pki/admin-petr.key")
curl "${A[@]}" "https://$HOST:8443/api/v1/whoami"    # {"role":"admin","secured":true,…}
curl "${A[@]}" "https://$HOST:8443/api/v1/scripts"   # co server načetl ze scripts/
curl -k "https://$HOST:8443/api/v1/runs"             # MUSÍ selhat na handshaku
curl -sS "http://$HOST/"                             # bootstrap: nápověda k instalaci runneru
```

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
| Go 1.26+ | **jen** když stavíš na serveru (krok 3a); s hotovými binárkami z jiného stroje (3b) ho tu mít nemusíš |

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
/opt/arcatum/                     git checkout (kvůli scripts/ a deploy/)     ty
  scripts/                        DEFINICE skriptů — server je čte za běhu    ty

/etc/arcatum/server.toml          konfigurace serveru                         ty
/usr/local/bin/arcatum-server     binárka serveru                             ty
/usr/local/bin/arcatum-ca         správa PKI                                  ty

/central_backup/arcatum/          backup_dir                                  ty
  pki/                            CA, certifikáty, podepisovací a master klíč ty
  dist/                           publikované binárky runneru + VERSION       ty
  data/arcatum.db                 SQLite (instance, běhy, evidence runnerů)   server
  runs/<run_id>/{stdout,stderr}.log   zachycený výstup běhů                   server
  restic/<instance>/              restic repozitář každé instance             server
```

Zakládáš tedy čtyři adresáře, dvěma příkazy:

```sh
install -d -m 0755 /etc/arcatum /opt/arcatum /central_backup/arcatum/dist
install -d -m 0700 /central_backup/arcatum/pki
```

`data/`, `runs/` a `restic/<instance>/` si server vytvoří sám (režimem 0750) — první dva při
startu, repozitáře až při prvním běhu instance. Naopak `dist/` a `pki/` zakládej ručně: do
nich zapisuješ ty, ne server. U `pki/` na tom záleží prakticky — když ho necháš vytvořit
`arcatum-ca`, dostane 0755, a privátní klíče leží v adresáři, do kterého smí kdokoli.

**Co musí být na disku vedle binárky.** Web UI i `install.sh` jsou zakompilované přímo
v binárce (`go:embed`), takže se neinstalují a nemůžou se rozejít s verzí serveru.
**Definice skriptů ale ne** — `scripts/` server čte z disku. Zapomenutý adresář navíc
start **nezastaví**: katalog jen zůstane prázdný a pozná se to až tím, že web nenabízí
žádný skript. (Chybný manifest je naopak fatální — viz krok 7.)

**Adresář `scripts` drž jako git checkout.** Definice skriptů jsou verzovaný kód; instance
a hesla v repozitáři nejsou (viz [README → Skript vs. instance](../README.md#skript-vs-instance)).

```sh
git clone <repo> /opt/arcatum
```

Checkout na serveru chtěj i kvůli `deploy/gen-certs.sh` (krok 4). Když ho tam mít nechceš,
jde celý návod projít jen s binárkami a nakopírovaným `scripts/` — obě odbočky jsou
popsané v krocích 3 a 4.

---

## 3. Build a instalace binárek

Vznikají tři binárky a každá má na produkci jiné místo:

| Binárka | Kam patří | K čemu |
|---|---|---|
| `arcatum-server` | `/usr/local/bin/` | služba (krok 6) |
| `arcatum-ca` | `/usr/local/bin/` | PKI, obnova certifikátů, rotace |
| `arcatum-runner` | `dist/` **pod jménem `arcatum-runner-linux-<arch>`** | odsud si ho stahují zálohované hosty (krok 8) |

Runner se na centrální server **neinstaluje** — ten ho jen rozdává. Proto nejde do
`/usr/local/bin`, ale do `dist_dir` a s příponou platformy v názvu; jiné jméno bootstrap
nenajde a instalace runneru skončí na 404.

Verzi vypaluj do binárky přes `-ldflags` — auto-update runnerů na ní stojí a nestampovaný
build se hlásí jako `dev`.

### a) Build přímo na serveru

Předpoklad je checkout v `/opt/arcatum` a Go (krok 1):

```sh
cd /opt/arcatum
V=$(date +%Y.%m.%d)

go build -ldflags "-X arcatum/pkg/version.Version=$V" -o /usr/local/bin/arcatum-server ./cmd/server
go build -ldflags "-X arcatum/pkg/version.Version=$V" -o /usr/local/bin/arcatum-ca     ./cmd/arcatum-ca
```

Je-li na serveru [`just`](../README.md#zkratky-přes-just), postaví `just release` všechny tři
binárky se stejnou verzí do `./bin`, odkud je nainstaluješ:

```sh
cd /opt/arcatum
just release                    # nebo V=2026.07.26 just release
install -m 0755 bin/arcatum-server bin/arcatum-ca /usr/local/bin/
```

Bez `V` se verze odvodí z dnešního data, tedy stejně jako v postupu výše. `just` na produkci
ničím povinným není — recepty jen skládají příkazy výše. Runner z `bin/` teď neinstaluj;
publikuje se v kroku 8.

### b) Hotové binárky z jiného stroje

Když na produkčním serveru Go být nemá co dělat, postav binárky u sebe a přenes je. Server běží bez
CGO (SQLite přes `modernc.org/sqlite`), takže je to jeden statický soubor bez runtime
závislostí — kromě resticu, který se volá jako externí program.

Na build stroji:

```sh
cd ~/src/backup_central
export V=$(date +%Y.%m.%d)

GOOS=linux GOARCH=amd64 go build -ldflags "-X arcatum/pkg/version.Version=$V" -o bin/arcatum-server ./cmd/server
GOOS=linux GOARCH=amd64 go build -ldflags "-X arcatum/pkg/version.Version=$V" -o bin/arcatum-ca     ./cmd/arcatum-ca
GOOS=linux GOARCH=amd64 go build -ldflags "-X arcatum/pkg/version.Version=$V" -o bin/arcatum-runner-linux-amd64 ./cmd/runner
GOOS=linux GOARCH=arm64 go build -ldflags "-X arcatum/pkg/version.Version=$V" -o bin/arcatum-runner-linux-arm64 ./cmd/runner

scp bin/arcatum-server bin/arcatum-ca bin/arcatum-runner-linux-* root@172.24.0.60:/tmp/
scp -r scripts root@172.24.0.60:/tmp/scripts
```

`GOOS`/`GOARCH` uveď i na Linuxu — jinak build mlčky vyrobí binárku pro tvou platformu.
Buildy runneru dělej pro **každou** architekturu, kterou máš mezi zálohovanými hosty;
cizí architekturu už na serveru bez Go nedoženeš.

Na produkčním serveru (adresáře z kroku 2 už existují):

```sh
install -m 0755 /tmp/arcatum-server /tmp/arcatum-ca /usr/local/bin/
install -m 0755 /tmp/arcatum-runner-linux-* /central_backup/arcatum/dist/

install -d -m 0755 /opt/arcatum/scripts                   # bez checkoutu: definice skriptů
cp -a /tmp/scripts/. /opt/arcatum/scripts/
rm -rf /tmp/arcatum-server /tmp/arcatum-ca /tmp/arcatum-runner-linux-* /tmp/scripts
```

Vědomě tu není `rsync` — na čistém serveru nemusí být nainstalovaný, `scp` a `cp` jsou vždy.
Při **aktualizaci** definic skriptů je ale `rsync -a --delete` lepší: `cp -a` smazaný
manifest v cíli nechá ležet a server ho bude dál nabízet.

Že binárka na tomhle stroji jde spustit, ověř přes `arcatum-ca` — vypíše nápovědu a skončí
s kódem 0:

```sh
arcatum-ca -h
```

> **Server takhle netestuj.** `arcatum-server` s chybnou cestou k configu **nespadne**:
> chybějící soubor se bere jako „žádná konfigurace", server nastartuje na vestavěných
> výchozích hodnotách — `0.0.0.0:8443`, `scripts` relativně k pracovnímu adresáři,
> `backup_dir = /central_backup/arcatum`, **bez TLS a bez šifrování hesel** — a založí si
> tam prázdnou databázi. Překlep v `-config` se tedy neprojeví chybou, ale tiše nezabezpečeným
> serverem nad špatnými cestami. Jediné, co ho odhalí, jsou `WARNING` řádky v logu (krok 7).
> Špatně zapsaný config je naopak fatální, protože se validuje.

Verze serveru se nikde nevypisuje samostatně; ověřuje se až přes běžící API v kroku 7,
u runnerů pak v záložce Runnery. `dist/VERSION` teprve **nezapisuj** — to je až krok 8,
který z nakopírovaných souborů udělá vydání.

---

## 4. PKI

Jeden příkaz vytvoří všechno. `-H` musí obsahovat **každou** adresu, na kterou se runnery
připojují (IP i DNS), jinak jim selže ověření TLS:

```sh
cd /opt/arcatum
deploy/gen-certs.sh -d /central_backup/arcatum/pki \
  -H 172.24.0.60,arcatum.xtuning.local -a petr
```

Skript potřebuje repozitář: přepne se do jeho kořene a `arcatum-ca` použije z `PATH`, jinak
si ho spustí přes `go run`. **Bez checkoutu** (cesta 3b) udělej totéž třemi příkazy samotné
binárky — dělají přesně to, co skript volá:

```sh
B=/central_backup/arcatum
arcatum-ca init   -dir "$B/pki"                                       # CA, podpisový klíč, master klíč
arcatum-ca server -dir "$B/pki" -hosts 172.24.0.60,arcatum.xtuning.local
arcatum-ca admin  -dir "$B/pki" -name petr
```

Ani jedna cesta ti nepřepíše existující PKI: `arcatum-ca init` nad hotovou CA skončí chybou
(*refusing to overwrite an existing CA*) a `gen-certs.sh` ji přeskočí a doplní jen to, co
chybí. `arcatum-ca server` a `admin` naopak certifikát vydají znovu — přesně proto se jimi
dělá obnova po expiraci.

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
  server certificate valid until 2028-…
  new certificates are issued under "Arcatum CA"
arcatum-server listening on 0.0.0.0:8443
  scripts=/opt/arcatum/scripts  db=…/data/arcatum.db  backup_dir=/central_backup/arcatum
  instance secrets are encrypted at rest
  mTLS enabled (CA …/ca.pem); job dispatches are signed
  bootstrap (plain HTTP) on 0.0.0.0:80 — install.sh and enrollment
  web UI (plain HTTP, password login) on 0.0.0.0:8080
```

První dva řádky se vypíšou **před** hlášením o naslouchání, protože vznikají při načítání
PKI; řádky o bootstrapu a webu se mohou objevit kdekoli za ním, běží ve vlastních gorutinách.

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
openssl x509 -in /central_backup/arcatum/pki/server.pem -noout -ext subjectAltName -dates
```

Vydáš-li certifikát jen na IP (`-hosts 172.24.0.60`), přes DNS jméno se nepřipojíš ani
s naimportovanou CA — a naopak. Doplnit adresu jde kdykoli, certifikát se prostě vydá znovu
a server restartuje:

```sh
arcatum-ca server -dir /central_backup/arcatum/pki -hosts 172.24.0.60,arcatum.xtuning.local
systemctl restart arcatum-server
```

Runnerům se tím nic nerozbije: ověřují CA, která zůstává stejná. Změnit ale musíš i
`[bootstrap] api_url`, míří-li na adresu, kterou jsi přidával — do `runner.toml` se zapisuje
z něj.

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

Se `just` je to jeden příkaz — recept postaví obě architektury i soubor `VERSION`:

```sh
cd /opt/arcatum
just dist-runner /central_backup/arcatum/dist
```

Cílový adresář je poziční argument (bez něj se staví do `local/dist`, což je vývojová
cesta — na produkci ho tedy uveď). Verzi přebíjíš `V=…` jako u `just release`.

**Bez Go na serveru** (cesta 3b) je publikování jen kopírování — binárky přenes z build
stroje a vedle nich napiš verzi, kterou mají vypálenou:

```sh
D=/central_backup/arcatum/dist
install -m 0755 /tmp/arcatum-runner-linux-* "$D/"
printf '%s\n' 2026.07.26 > "$D/VERSION"
```

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
arcatum-server -config /etc/arcatum/server.toml -passwd admin
# nebo konkrétní heslo, mimo historii shellu:
ARCATUM_PASSWORD='…' arcatum-server -config /etc/arcatum/server.toml -passwd admin
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

```sh
cd /opt/arcatum && git pull
V=$(date +%Y.%m.%d)
go build -ldflags "-X arcatum/pkg/version.Version=$V" -o /usr/local/bin/arcatum-server.new ./cmd/server
mv /usr/local/bin/arcatum-server /usr/local/bin/arcatum-server.old
mv /usr/local/bin/arcatum-server.new /usr/local/bin/arcatum-server
systemctl restart arcatum-server
journalctl -u arcatum-server -n 30
```

Se `just` nahradíš první dva řádky buildu `just release` a přesuneš binárku z `bin/`.
Přepisovat běžící binárku přímo nejde (`Text file busy`), takže dvojice `mv` platí tak
jako tak — proto tu není recept „nasadit".

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
- [ ] `scripts` je absolutní cesta, adresář je **fyzicky na serveru** a `/api/v1/scripts`
      vrací, co má (prázdná odpověď = zapomenutý `scripts/`, ne chyba v logu)
- [ ] služba běží s `-config /etc/arcatum/server.toml` a cesta v unitu skutečně existuje
      (překlep = tichý start na výchozích hodnotách bez TLS)
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
