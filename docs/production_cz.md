# Rozjetí serveru v `/opt/arcatum`

Návod pro **testovací stroj**: jsi přihlášený na počítači, kde má Arcatum běžet, a leží tu
checkout repozitáře. Nic se nikam nepřenáší — postaví se to tady a rovnou se to odsud
nainstaluje do `/opt/arcatum`.

Zabezpečení je zapnuté i tady (mTLS, podepsané úlohy, šifrované secrets) — nedělá se to
kvůli přísnosti, ale proto, že vypnuté by to bylo něco jiného, než co pak pojede ostře.
Vývojový režim nad `local/` (plain HTTP, bez ověřování) je jiná věc a je v
[README → Rychlý start](../README_cz.md#rychlý-start-lokální-vyzkoušení).

> **Nasazení na cizí stroj (balík + `scp`) tady zatím není** — až tohle odladíme, přibude
> sem druhá kapitola. Zatím platí: `/opt/arcatum` a checkout jsou na jednom stroji.

- [Předpoklady](#předpoklady)
- [Celý postup](#celý-postup)
- [1. Postavit binárky](#1-postavit-binárky)
- [2. Nainstalovat do `/opt/arcatum`](#2-nainstalovat-do-optarcatum)
- [3. První start na popředí](#3-první-start-na-popředí)
- [4. Pustit to jako službu](#4-pustit-to-jako-službu)
- [5. Ověřit](#5-ověřit)
- [6. Web](#6-web)
- [7. Runner na tomtéž stroji](#7-runner-na-tomtéž-stroji)
- [8. Testovací instance a první běh](#8-testovací-instance-a-první-běh)
- [Ladicí smyčka](#ladicí-smyčka)
- [Začít znovu od nuly](#začít-znovu-od-nuly)
- [Kde co leží](#kde-co-leží)
- [`server.toml`](#servertoml)
- [Systemd unit](#systemd-unit)
- [Když to nejede](#když-to-nejede)

---

## Předpoklady

| Co | Proč | Ověření |
|---|---|---|
| Go 1.26+ | staví se tady | `go version` |
| `just` | recepty na build | `just --version` |
| systemd | služba `arcatum-server` | — |
| `restic` | **obnova dat a velikost repozitářů běží na serveru** | `restic version` |
| `rsync` (volitelný) | instalátor jím srovnává `scripts/` | `command -v rsync` |

Go v tomhle prostředí není na `PATH`:

```sh
export PATH=/usr/local/go/bin:$PATH
```

Bez `rsync` instalátor `scripts/` jen zkopíruje (`cp -a`) a upozorní na to. Rozdíl je jen
v tom, že smazaný manifest v `/opt/arcatum/scripts` zůstane ležet — při ladění skriptů na to
narazíš, tak buď `apt install rsync`, nebo ten soubor smaž ručně.

Adresa, na kterou se budou runnery připojovat, jde do certifikátu serveru a do `api_url`.
Na testovacím stroji je to jeho vlastní IP a hostname:

```sh
hostname -s        # backup-central
hostname -I        # 172.24.0.60
```

Dosaď je do `-H` v kroku 2. V celém návodu je to `172.24.0.60,backup-central`.

## Celý postup

Šest příkazů; zbytek dokumentu je rozepisuje.

```sh
export PATH=/usr/local/go/bin:$PATH
cd /root/src/backup_central

just release && just dist-runner bin                      # 1. binárky
deploy/install-server.sh -H 172.24.0.60,backup-central    # 2. do /opt/arcatum
cd /root && arcatum-server -instances /dev/null           # 3. zkusmý start, Ctrl-C ukončí
systemctl enable --now arcatum-server                     # 4. služba
journalctl -u arcatum-server -n 30
```

## 1. Postavit binárky

```sh
cd /root/src/backup_central
just release          # bin/arcatum-server, bin/arcatum-ca, bin/arcatum-runner
just dist-runner bin  # bin/arcatum-runner-linux-{amd64,arm64} + bin/VERSION
```

Instalátor nic nestaví — jen rozmisťuje to, co v `bin/` najde. Proto oba recepty; druhý
vyrábí binárky, které si stahují zálohované hosty, a soubor `VERSION`, bez kterého se
runnerům žádná aktualizace nenabídne.

Verze se vypaluje do binárek přes `-ldflags` a bere se z `V` (výchozí dnešní datum). Vlastní
si vynutíš `V=test1 just release`; nestampovaný build se hlásí jako `dev`.

## 2. Nainstalovat do `/opt/arcatum`

```sh
deploy/install-server.sh -H 172.24.0.60,backup-central -a petr
```

Spouští se **z checkoutu** a instaluje z něj (`bin/` a `scripts/` vedle sebe). Musí běžet
jako root; `-n` je suchý běh, který jen vypíše, co by udělal.

Co udělá:

- založí `/opt/arcatum/{bin,pki,dist,scripts}`, `/etc/arcatum` a `/central_backup/arcatum`,
- nainstaluje `arcatum-server` a `arcatum-ca` do `/opt/arcatum/bin` a udělá na ně symlinky
  v `/usr/local/bin` (proto se dál píše jen `arcatum-server`),
- nakopíruje `bin/arcatum-runner-linux-*` a `VERSION` do `dist/`,
- srovná `scripts/` — **definice skriptů jsou jediná věc, kterou server čte z disku**; web
  UI i `install.sh` jsou zakompilované v binárce,
- vygeneruje PKI, napíše `/etc/arcatum/server.toml` a systemd unit.

**Co už existuje, nechá být.** Config, PKI ani unit se po prvním zápisu nikdy nepřepisují —
tvoje úpravy tedy přežijou každý další běh a tentýž příkaz je zároveň postup pro
přeinstalaci po změně kódu ([ladicí smyčka](#ladicí-smyčka)).

Přepínače, které se hodí: `-b` jiný `backup_dir`, `-p` jiný prefix (třeba druhá instance
vedle), `-n` suchý běh.

> **`-a petr` platí jen při zakládání PKI.** Je to jméno v klientském certifikátu pro volání
> API ze shellu (`pki/admin-petr.pem` a `.key`) — do prohlížeče se nepoužívá, tam je jméno
> a heslo. Když PKI už existuje, instalátor ji nechá být a `-a` se **neprojeví**; na tomhle
> stroji je proto certifikát `admin-admin.*`. Další si vydáš kdykoli:
>
> ```sh
> arcatum-ca admin -dir /opt/arcatum/pki -name petr
> ```

Na konci vypíše instalátor cestu ke klientskému certifikátu a sekci `Check:` s tím, co
chybí. Přečti si ji.

## 3. První start na popředí

Instalátor službu **nespouští**. První start patří na popředí, kde je hned vidět, jestli
config a PKI drží:

```sh
cd /root && arcatum-server -instances /dev/null
```

`-config` tu schválně není: server si config najde sám — nejdřív `./server.toml`, pak
`/etc/arcatum/server.toml`. `cd /root` je ujištění, že se spouští odjinud než z adresáře
s vlastním `server.toml` (checkout jeden takový pro vývoj má v `local/`).

Chceš vidět tohle:

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

**První řádek kontroluj vždycky** — jsou-li certifikáty jiné, než čekáš, obvykle je jiný
i config. Žádné `WARNING` tam být nesmí; `no [tls]` nebo `no [secrets] master_key` znamená,
že server jede nezabezpečeně a config je špatně.

Když je databáze **prázdná** (první start vůbec), vypíše se tady jednou vygenerované heslo
webového účtu `admin` — opiš si ho:

```
  ┌─ first start: created the web account ─────────────────────
  │   user:     admin
  │   password: k4m2ftq7hn3bwzla
```

Když už databáze existuje (na tomhle stroji ano, běhy z minula v ní leží), účet tam je
a nic se nevypisuje. Heslo si přenastav:

```sh
arcatum-server -passwd admin                      # vygeneruje a vypíše nové
ARCATUM_PASSWORD='tajneheslo' arcatum-server -passwd admin   # nebo konkrétní
```

Ctrl-C ukončí.

> Server bez configu **nenastartuje vůbec** a je to záměr — vestavěné výchozí hodnoty
> znamenají plain HTTP a hesla instancí v plaintextu, takže překlep v cestě nemá tiše
> zapnout nezabezpečený server. Fatální je i vadný manifest ve `scripts/`. Naopak
> **zapomenutý celý adresář `scripts/` start nezastaví**: katalog jen zůstane prázdný
> a pozná se to tím, že web nenabízí žádný skript.

## 4. Pustit to jako službu

```sh
systemctl enable --now arcatum-server
journalctl -u arcatum-server -n 30
```

Unit už na disku je, napsal ho instalátor ([obsah a proč](#systemd-unit)). V logu hledej
totéž co v kroku 3.

## 5. Ověřit

```sh
A=(--cacert /opt/arcatum/pki/ca.pem
   --cert /opt/arcatum/pki/admin-admin.pem
   --key /opt/arcatum/pki/admin-admin.key)

curl "${A[@]}" https://172.24.0.60:8443/api/v1/whoami     # {"role":"admin","secured":true,…}
curl "${A[@]}" https://172.24.0.60:8443/status            # textový přehled + katalog skriptů
curl "${A[@]}" https://172.24.0.60:8443/api/v1/scripts    # co načetl ze scripts/
curl -k       https://172.24.0.60:8443/api/v1/runs        # MUSÍ selhat na handshaku
curl -sS      http://172.24.0.60/                         # bootstrap: nápověda k runneru
```

Že volání **bez** certifikátu selže, je stejně důležité jako že s ním projde — je to důkaz,
že mTLS opravdu platí. Prázdná odpověď z `/api/v1/scripts` znamená zapomenutý `scripts/`,
ne chybu v logu.

## 6. Web

```
http://172.24.0.60:8080/
```

Účet `admin` a heslo z kroku 3. Do prohlížeče se nic neinstaluje ani neimportuje — web je
plain HTTP s přihlášením jménem a heslem, admin certifikát je jen na `curl` na port 8443.

Na testovacím stroji to takhle stačí. Pro ostrý provoz platí, že web patří do vnitřní sítě,
a když má být vidět dál, staví se před něj HTTPS reverse proxy a zapíná `[web]
secure_cookie = true` (bez proxy tu volbu nezapínej, přihlášení by přestalo fungovat).

## 7. Runner na tomtéž stroji

Aby bylo co zkoušet, potřebuješ runner. Na testovacím stroji klidně tentýž počítač:

```sh
curl -LsSf http://172.24.0.60/arcatum_runner/install.sh | sh
```

Skript stáhne binárku pro tuhle platformu, `ca.pem` a podepisovací veřejný klíč, napíše
`/etc/arcatum-runner/runner.toml`, nainstaluje systemd službu a spustí ji. Adresu
bootstrapu si odvodí z URL, ze které se sám stáhl; adresu API bere z `[bootstrap] api_url`
v configu serveru — proto tam musí být adresa, která je zároveň v SAN certifikátu.

Runner si vygeneruje **vlastní** klíč (nikdy neopustí hostitele) a pošle jen žádost
o podpis. Pak čeká, a to je správný stav:

```sh
systemctl status arcatum-runner
journalctl -u arcatum-runner -f
```

Ve webu v záložce **Runners** žádost schval tlačítkem **approve**. Jeho `runner_id` je `hostname -s`, tedy
`backup-central` — tohle jméno pak patří do `runner_id` instancí.

```sh
curl "${A[@]}" https://172.24.0.60:8443/api/v1/runners   # stav, platforma, verze, last_seen
```

## 8. Testovací instance a první běh

Ve webu **Instances → new instance**, skript `hello`. Formulář se sestaví z parametrů
manifestu ([`scripts/example/hello.toml`](../scripts/example/hello.toml)), takže stačí
vyplnit `name`, `target` a `token` a jako runner vybrat `backup-central`. `hello` schválně
nepotřebuje žádný externí nástroj — projde celou cestou od odeslání po zachycený výstup,
takže když doběhne, funguje řetěz, ne skript.

Nová instance **nemá rozvrh** a spustí se, jen když ji spustíš — což je přesně to, co tady
chceš. Pak **run now**, což tě přepne do historie té úlohy; klikni na běh a sleduj živý tail.
Až řetěz funguje, dej mu časový plán v **Schedules → new schedule**. Totéž ze shellu:

```sh
curl "${A[@]}" -X POST https://172.24.0.60:8443/api/v1/instances/hello-demo/run
curl "${A[@]}" https://172.24.0.60:8443/api/v1/runs?limit=5
curl "${A[@]}" https://172.24.0.60:8443/api/v1/runs/run-1/output
```

ID běhu má tvar `run-1` — s holým číslem vrátí endpointy výstupu prázdné tělo, ne chybu.
Výstup leží i na disku v `/central_backup/arcatum/runs/<run_id>/{stdout,stderr}.log`.

Až tohle projde, zkus `files-backup` — ten už sahá na restic a je to první věc, která
opravdu něco zálohuje.

---

## Ladicí smyčka

Tohle budeš pouštět nejčastěji. Co se změnilo, určuje, kolik toho musíš udělat:

**Změna v Go kódu** — přestavět, přeinstalovat, restartovat. Instalátor si běžící službu
restartuje sám:

```sh
cd /root/src/backup_central
just release && just dist-runner bin
deploy/install-server.sh
journalctl -u arcatum-server -n 30
```

`-H` se už nepíše, PKI i config existují. Přes běžící binárku se přímo psát nedá
(`Text file busy`), proto ji instalátor ukládá vedle a přejmenovává na místo.

**Změna ve `scripts/*.toml` nebo ve skriptu** — katalog se čte při startu, takže je potřeba
`deploy/install-server.sh` (nakopíruje je do `/opt/arcatum/scripts`) **a restart**:

```sh
deploy/install-server.sh && systemctl restart arcatum-server
```

Nový nebo změněný skript se dřív neprojeví. Vadný manifest naopak start zastaví, tak se po
restartu vždycky podívej do logu.

**Změna instance nebo parametrů** — nic. Platí to hned, restart není potřeba.

**Změna rozvrhu** — taky nic. Přidání, úprava, pozastavení i smazání v záložce **Schedules**
(nebo `PUT /api/v1/schedules/{id}`) přepočítá příští běh okamžitě.

**Změna `server.toml`** — jen restart, instalátor na config nesahá:

```sh
systemctl restart arcatum-server
```

**Runner** se aktualizuje sám z `dist/`, když se změní `VERSION`; při ladění je rychlejší ho
restartovat rovnou:

```sh
systemctl restart arcatum-runner
journalctl -u arcatum-runner -f
```

Restart serveru mimochodem **přeskočí** běh, který měl v tu chvíli padnout — scheduler je
v paměti a rozvrhy se po startu počítají od aktuálního času. Na testu je to jedno, na ostrém
stroji ne.

## Začít znovu od nuly

Když chceš čistý stůl (nová PKI, prázdná databáze, žádné instance):

```sh
systemctl disable --now arcatum-server arcatum-runner

rm -rf /opt/arcatum /etc/arcatum /central_backup/arcatum
rm -f /etc/systemd/system/arcatum-server.service /usr/local/bin/arcatum-{server,ca}

# runner na tomhle stroji, pokud jsi ho instaloval
rm -rf /etc/arcatum-runner /var/lib/arcatum-runner /usr/local/bin/arcatum-runner
rm -f /etc/systemd/system/arcatum-runner.service

systemctl daemon-reload
```

Pak zpátky na [krok 1](#1-postavit-binárky). Instalace je jeden adresář právě proto, aby
tohle šlo — nikde nezůstane ležet binárka z minulé verze.

> Smazáním `/opt/arcatum/pki` zahodíš i `secrets-master.key`, kterým jsou zašifrovaná hesla
> instancí, a `ca.key`, kterému věří všechny už schválené runnery. Na testovacím stroji je
> to přesně to, co chceš. Na ostrém by to znamenalo, že se k datům v restic repozitářích už
> nedostaneš — proto se tyhle tři klíče (`secrets-master.key`, `ca.key`,
> `dispatch-signing.key`) při ostrém nasazení odnášejí zašifrovaně mimo stroj.

Když chceš nechat PKI a vyhodit jen data, stačí `rm -rf /central_backup/arcatum` — server si
databázi i adresáře založí znovu při startu.

## Kde co leží

```
/opt/arcatum/                     instalace                              instalátor
  bin/                            arcatum-server, arcatum-ca             instalátor
  pki/                            CA, certifikáty, podpisový a master klíč (0700)
  dist/                           binárky runneru + VERSION              instalátor
  scripts/                        DEFINICE skriptů — server je čte za běhu

/etc/arcatum/server.toml          konfigurace                            instalátor
/usr/local/bin/arcatum-server     symlink na /opt/arcatum/bin/
/usr/local/bin/arcatum-ca         symlink na /opt/arcatum/bin/

/central_backup/arcatum/          backup_dir — nic než data
  data/arcatum.db                 SQLite (instance, rozvrhy, běhy, runnery, účty)  server
  runs/<run_id>/{stdout,stderr}.log   zachycený výstup běhů               server
  restic/<instance>/              restic repozitář každé instance         server
  config-backups/                 konfigurace uložená před každým importem  server
```

Dělicí čára je jednoduchá: **do `backup_dir` ručně nesaháš.** Všechno pod ním si zakládá
a píše server sám. Co tam kopíruješ ty, tam nepatří — patří to do `/opt/arcatum`.

Runner do `bin/` nejde: centrální server ho **neinstaluje**, jen rozdává, a proto leží
v `dist/` pod jménem `arcatum-runner-linux-<arch>`. Jiné jméno bootstrap nenajde a instalace
runneru skončí na 404.

PKI schválně není v `backup_dir`: `secrets-master.key` dešifruje hesla restic repozitářů,
takže na stejném svazku jako `restic/` by jedna odnesená kopie znamenala zašifrovaná data
i klíč k nim v jednom balíku.

### Co zálohovat ze samotného Arcatum

Dvě věci, a každou jinak:

1. **`/opt/arcatum/pki/`** — CA klíč, podepisovací klíč a `secrets-master.key`. Ručně, mimo
   tenhle stroj, a s vědomím, že kdo tohle má, má přístup ke všem repozitářům. Bez
   `secrets-master.key` nejsou hesla instancí k ničemu, takže **tohle je ta část, jejíž
   ztráta je nevratná**.
2. **Konfigurace** — web, záložka **Administration** → *download configuration*, nebo
   `GET /api/v1/config/export`. Jeden zip s instancemi, jejich rozvrhy, účty a runnery, bez klíčů a bez dat
   záloh. Ten samý soubor se dá kdykoli nahrát zpátky a konfiguraci tím nahradit; podrobnosti
   v [README](../README_cz.md#záloha-konfigurace-a-reset-serveru).

Dělba je záměrná: klíče se mění jednou za rotaci a nepatří do žádného automatického exportu,
konfigurace se mění průběžně a stáhnout si ji smí být otázka jednoho kliknutí. Obnova na
novém stroji je tedy: nakopírovat `pki/`, nastavit `server.toml`, naimportovat zip.

Data záloh (`runs/`, `restic/`) jsou to největší. Buď se zálohují na úrovni svazku, nebo
— a to je doporučená cesta — se zapne **odlehlá replika** níž, která je průběžně odlévá
na druhý stroj i s klíči a snapshotem databáze. Bez ní je Arcatum pro ně poslední místo,
kde leží.

## Off-site replika

Druhý stroj, na který průběžně odtéká všechno, co server uloží. Návrh a chování jsou
v [README](../README_cz.md#odlehlá-kopie) a [architecture.md](architecture_cz.md#21-odlehlá-kopie);
tady je jen postup, jak to nachystat.

### Na replice

```sh
# vyhrazený účet, který nemá dělat nic jiného
useradd -r -m -d /var/lib/arcatum-replica -s /bin/sh arcatum
install -d -o arcatum -g arcatum -m 700 /data
apt-get install -y rsync                       # nebo dnf install rsync
```

Mód `0700` není kosmetika: při `include_keys = true` leží v `/data/keys/` master klíč
i klíč CA, takže kdo se tam dostane, otevře každý repozitář a vydá certifikát libovolnému
hostu. Ideálně na šifrovaném svazku.

### Klíč a jeho omezení

Na **Arcatum serveru** vyrob vyhrazený klíč jen pro tohle (bez passphrase — přenos běží
bez obsluhy):

```sh
ssh-keygen -t ed25519 -N '' -C arcatum-replica -f /opt/arcatum/pki/replica-ssh.key
chmod 600 /opt/arcatum/pki/replica-ssh.key
```

> **Klíč ani `known_hosts` nesmí ležet v `/root`.** Systemd unit má `ProtectHome=yes`, takže
> `/root` a `/home` jsou pro službu prázdné adresáře — `ssh_key = "/root/.ssh/id_ed25519"`
> ti z rootovského shellu ručně projde a službě selže, s `rsync exit 255` a ničím dalším,
> podle čeho se řídit. Správné místo je `/opt/arcatum/pki/`: služba na něj vidí a není to
> `backup_dir`.

Veřejnou část zapiš na replice do `~arcatum/.ssh/authorized_keys` **omezenou**:

```
from="172.26.0.1",restrict,command="rrsync /data" ssh-ed25519 AAAA… arcatum-replica
```

- `restrict` vypne port forwarding, agenta i pty — klíč umí jen spustit `command`.
- `command="rrsync /data"` udrží přenos uvnitř `/data`, i kdyby někdo ovládl Arcatum
  server. (`rrsync` bývá v `/usr/share/rsync/scripts/` nebo `/usr/share/doc/rsync/scripts/`.)
- `from=` omezí použití na adresu uvnitř WireGuard tunelu.

Pak si přišpendli hostitelský klíč repliky, ať přenos nikdy nezůstane viset na otázce:

```sh
ssh-keyscan -H 172.26.0.2 > /opt/arcatum/pki/replica-known_hosts
# fingerprint porovnej s tím, co hlásí replika: ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
ssh -i /opt/arcatum/pki/replica-ssh.key \
    -o UserKnownHostsFile=/opt/arcatum/pki/replica-known_hosts \
    arcatum@172.26.0.2 true && echo OK
```

### V `server.toml`

Sekci `[replica]` vezmi z [`config/server.example.toml`](../config/server.example.toml).
Minimum, které dává smysl u nás:

```toml
[replica]
enabled      = true
host         = "172.26.0.2"
user         = "arcatum"
path         = "/data"
ssh_key      = "/opt/arcatum/pki/replica-ssh.key"
known_hosts  = "/opt/arcatum/pki/replica-known_hosts"
mirror       = true
max_delete   = 100
include_keys = true
```

Server potřebuje mít nainstalovaný `rsync`; když chybí, replikace se vypne s hláškou
v logu a jinak běží všechno dál.

Po restartu se ve startovním logu objeví cíl a dvě varování, která nejsou chyby, jen
připomínka toho, co jsi zapnul (klíče na replice, případně nepřišpendlený hostitelský
klíč). Průběh je vidět v Administraci → **Off-site replica**.

### Na co si dát pozor

- **`max_delete`** musí být vyšší než kolik souborů odmaže běžná retence za hodinu,
  a nižší než kolik jich je v celém `backup_dir`. Sto je rozumný začátek. Průchod nad
  stropem se odmítne celý a je vidět jako chybná položka — je to zamýšlené chování, ne
  porucha, a znamená „zkontroluj, že `backup_dir` je namountovaný a plný".
- **Místo na replice.** Při `mirror = true` drží zhruba tolik, co `backup_dir`; při
  vypnutém zrcadlení roste bez omezení.
- **Obnova z repliky** je: nakopírovat `/data/keys/` do `pki/`, `/data/meta/arcatum.db`
  do `data_dir`, `/data/restic/` a `/data/runs/` do `backup_dir`, upravit `server.toml`
  a nastartovat. Vyzkoušej to nanečisto dřív, než to budeš potřebovat.

## `server.toml`

Napsal ho instalátor a od té chvíle na něj nesahá. Ukázka s popisem každé volby je
v [`config/server.example.toml`](../config/server.example.toml). Tohle je, co je na tomhle
stroji:

```toml
[server]
listen    = "0.0.0.0:8443"                  # API pro runnery (mTLS)
scripts   = "/opt/arcatum/scripts"          # absolutní cesta, ne relativní
data_dir  = "/central_backup/arcatum/data"
timezone  = "Europe/Prague"
log_level = "info"

[web]
listen      = "0.0.0.0:8080"                # web UI: plain HTTP, jméno a heslo
session_ttl = "12h"

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

**Kde se config hledá** bez `-config`: nejdřív `./server.toml`, pak
`/etc/arcatum/server.toml`. Nenajde-li se ani jeden, server skončí chybou. Do `/opt/arcatum`
proto `server.toml` **nedávej** — služba tam má `WorkingDirectory` a takový soubor by
konfiguraci služby přebil.

Co config **odmítne** už při startu, místo aby to tiše obešel:

- `[tls]` vyplněné napůl — všechny tři cesty patří k sobě, jinak by to spadlo na plain HTTP,
- `[tls]` bez `[signing] key` — runnery by neměly co ověřovat,
- `[tls]` bez `[secrets] master_key` — hesla by ležela v `arcatum.db` v plaintextu,
- `[bootstrap]` bez `api_url` nebo `ca_key`, případně bez `[tls]`,
- dva listenery na stejné adrese (`[web]`, `[server]`, `[bootstrap]`),
- nesmyslné `[web] session_ttl` — chybný údaj by tiše znamenal „nikdy nevyprší".

Dvě věci, které se snadno popletou:

- **`listen` vs. `api_url`.** `listen` je, kde server naslouchá; `api_url` je adresa, kterou
  server zapíše do generovaného `runner.toml`. Svou vlastní dosažitelnou adresu nezná —
  musíš mu ji říct.
- **`log_level`** se načte, ale server dnes loguje jednou úrovní; `debug` víc nevypíše.

## Systemd unit

```ini
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

AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
ProtectSystem=full
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=/central_backup/arcatum
ReadOnlyPaths=/opt/arcatum

[Install]
WantedBy=multi-user.target
```

- **`-instances /dev/null`** — instance se spravují z webu a žijí v DB. Prázdný i chybějící
  soubor server přejde bez chyby.
- **`-config` je uvedený explicitně**, i když by ho hledání našlo samo: unit má
  `WorkingDirectory=/opt/arcatum` a soubor podstrčený do pracovního adresáře nesmí
  konfiguraci služby přebít. Při ladění ze shellu naopak `-config` vynechávej — dostaneš
  tentýž config a nemáš co přepsat.
- **`ReadOnlyPaths=/opt/arcatum`** — server odsud jen čte a zapisuje výhradně do
  `backup_dir`. Aktualizaci to nebrání, instalátor běží mimo službu. Jiný `backup_dir` chce
  i úpravu `ReadWritePaths`.
- Bez `User=` běží služba jako root: čte privátní klíče a naváže port 80.
  `AmbientCapabilities` je připravené na to, až poběží pod vlastním uživatelem.

## Když to nejede

**`TLS handshake error` v logu je normální.** Zapnuté mTLS znamená, že každý pokus
o připojení bez správného certifikátu vypadá jako chyba. Server tím nehlásí nic o sobě —
hlásí, co se mu nepovedlo od klienta:

| Text v logu | Příčina | Náprava |
|---|---|---|
| `client sent an HTTP request to an HTTPS server` | `http://` na port 8443 | používej `https://` |
| `remote error: tls: unknown certificate authority` | klient nezná `ca.pem` | `--cacert /opt/arcatum/pki/ca.pem` |
| `tls: client didn't provide a certificate` | došlo to k mTLS, ale bez admin certifikátu | `--cert/--key` |
| `remote error: tls: unknown certificate` | **klient odmítl certifikát serveru** — adresa není v jeho SAN | viz níž |
| `remote error: tls: bad certificate` | certifikát vydala jiná CA (typicky po `rm -rf pki`) | vydej nový: `arcatum-ca admin …` |

`remote error` znamená, že alert poslal **klient** — chyba je v tom, čemu nevěří on.

**Adresa není v SAN certifikátu.** Co v něm je:

```sh
openssl x509 -in /opt/arcatum/pki/server.pem -noout -ext subjectAltName -dates
```

Vydáš-li certifikát jen na IP, přes DNS jméno se nepřipojíš ani s CA — a naopak. Doplnit jde
kdykoli, certifikát se prostě vydá znovu:

```sh
arcatum-ca server -dir /opt/arcatum/pki -hosts 172.24.0.60,backup-central
systemctl restart arcatum-server
```

Runnerům se tím nic nerozbije, ověřují CA, která zůstává stejná. Změň ale i `[bootstrap]
api_url`, míří-li na adresu, kterou jsi přidával.

**Web nenabízí žádný skript** — `/opt/arcatum/scripts` je prázdný nebo `[server] scripts`
míří jinam. Prázdný katalog start nezastaví; ověř `curl "${A[@]}" …/api/v1/scripts`.

**Smazaný skript pořád svítí** — na tomhle stroji chybí `rsync`, takže instalátor kopíruje
bez `--delete`. Smaž ho z `/opt/arcatum/scripts` ručně (nebo `apt install rsync`).

**Replikace padá na `rsync exit 255`** — ssh se nepřipojilo. Zkus tentýž příkaz ručně jako
root; když projde, jsou špatně cesty: s `ProtectHome=yes` v unitu služba na `/root/.ssh`
nevidí. Klíč i `known_hosts` přesuň do `/opt/arcatum/pki/` a uprav `[replica]`.

**Instalace runneru končí na 404** — v `dist/` není `arcatum-runner-linux-<arch>` pro danou
architekturu. `ls /opt/arcatum/dist`, případně znovu `just dist-runner bin` a instalátor.

**Runnerům se nenabízí aktualizace** — chybí `/opt/arcatum/dist/VERSION`. Samotné binárky
vydání neznamenají; je to schválně, aby šlo kopírovat a vydat zvlášť.

**`Text file busy` při instalaci** — přes běžící binárku se psát nedá. Instalátor to řeší
sám (ukládá vedle a přejmenovává); když to potkáš ručně, zastav službu.

Související: [architektura](architecture_cz.md) · [vývoj backendu](backend-development_cz.md) ·
[vývoj skriptů](script-development_cz.md)
