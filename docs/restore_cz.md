# Návod: obnova databáze z dumpu

Jak z Arcatum dostat dump zpět do běžící databáze — MySQL i PostgreSQL. Čte se to nejhůř
ve chvíli, kdy je to potřeba, takže si celý postup jednou projdi nanečisto dřív, než ho
budeš potřebovat doopravdy.

- [1. Co Arcatum garantuje a co ne](#1-co-arcatum-garantuje-a-co-ne)
- [2. Jak se dostat k dumpu](#2-jak-se-dostat-k-dumpu)
- [3. MySQL / MariaDB](#3-mysql--mariadb)
- [4. PostgreSQL](#4-postgresql)
- [5. Co v dumpu není](#5-co-v-dumpu-není)
- [6. Katalog chyb](#6-katalog-chyb)
- [7. Zkušební obnova](#7-zkušební-obnova)

---

## 1. Co Arcatum garantuje a co ne

**Garantuje:** dump ke stažení existuje jen tehdy, když zálohovací skript skončil
návratovým kódem 0. Server přijatá data píše do `data.part` a přejmenuje je na `data.bin`
teprve při úspěšném dokončení běhu, takže useknutý přenos ani spadlý `mysqldump` po sobě
nenechá soubor, který by vypadal jako hotová záloha ([architektura §17](architecture_cz.md)).

**Negarantuje nic o obsahu.** Server do dumpu nekouká: nekontroluje formát, příponu,
hlavičku ani velikost — přijme jakýkoli proud bajtů. Skript, který skončí nulou a vypíše
nesmysl, vyrobí zálohu, která je od té správné k nerozeznání až do chvíle, kdy ji budeš
obnovovat.

> Jediný důkaz, že záloha je obnovitelná, je **provedená obnova**. Ne velikost souboru,
> ne zelený řádek v přehledu běhů. Viz [§7](#7-zkušební-obnova).

Dumpy se navíc **rotují** (`keep_last`, `keep_days` na instanci) — starší už nemusí být
k dispozici, i když řádek běhu v historii zůstává ([architektura §19](architecture_cz.md)).

---

## 2. Jak se dostat k dumpu

**Z webu** — záložka **Restore**, vybereš instanci; u streamované instance se místo
procházení stromu vypíšou uložené dumpy ke stažení. Soubor se jmenuje
`<instance>-<run>.dump` bez ohledu na to, co je uvnitř.

Totéž přes API:

```sh
A=(--cacert pki/ca.pem --cert pki/admin-petr.pem --key pki/admin-petr.key)
API=https://172.24.0.60:8443/api/v1

curl "${A[@]}" $API/instances/mysql-web01/dumps        # co je k dispozici, od nejnovějšího
curl "${A[@]}" -O -J $API/runs/run-42/data             # stažení konkrétního běhu
```

Stažení jde i **rovnou do klienta databáze**, bez mezikroku na disku — u
několikagigabajtového dumpu to je rozdíl:

```sh
curl "${A[@]}" $API/runs/run-42/data | mysql --host=… --user=… shop
```

| Odpověď | Znamená |
|---|---|
| `200` | dump je tam |
| `404 this run has no backup data` | běh žádná data nevyrobil (nebo neskončil úspěšně) |
| `410 Gone` | dump **byl odrotován** retencí — běh proběhl v pořádku, soubor už neexistuje |

Stahování vyžaduje roli se čtením (klientský certifikát nebo přihlášení do webu) a umí
`Range`, takže přerušené stahování velkého dumpu jde navázat, ne opakovat od nuly.

---

## 3. MySQL / MariaDB

Dump ze [scripts/example/mysql_backup.sh](../scripts/example/mysql_backup.sh) je **plain
SQL jedné databáze**. Neobsahuje `CREATE DATABASE` ani `USE`, takže cílovou databázi si
volíš při obnově — a můžeš ji obnovit i pod jiným jménem.

```sh
# 1) cílová databáze (musí existovat; kódování podle originálu)
mysql --host=db01 --user=root -e \
  "CREATE DATABASE shop_obnova CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"

# 2) obnova; heslo přes MYSQL_PWD, ať není v process listu
export MYSQL_PWD='…'
mysql --host=db01 --user=root --default-character-set=utf8mb4 \
      shop_obnova < mysql-web01-run-42.dump
echo "exit=$?"
```

Dump má ve výchozím stavu `DROP TABLE IF EXISTS` u každé tabulky, takže obnova **přes
existující databázi projde** a tabulky přepíše. Co v cílové databázi je navíc (tabulka,
která ve dumpu není), ale zůstane — pro čistou obnovu vždycky prázdná databáze.

`mysql` končí nenulovým kódem na první chybě, takže `exit=0` tady opravdu znamená, že
prošel celý soubor.

**Pozor na GTID.** Pokud má zdrojový server zapnuté GTID, `mysqldump` vloží do dumpu
`SET @@GLOBAL.GTID_PURGED=…`. Obnova do serveru, který už nějaké transakce zná, na tom
skončí chybou. Řešení je dump s `--set-gtid-purged=OFF`, nebo tenhle příkaz z dumpu před
obnovou odstranit.

---

## 4. PostgreSQL

Postgres se zálohuje **dvěma instancemi**: `postgres-backup` dumpuje jednu databázi,
`postgres-globals-backup` role a tablespaces celého clusteru. Při obnově na prázdný
cluster potřebuješ oba dumpy a **v tomhle pořadí** — role musí existovat dřív, než na ně
dump databáze narazí v `ALTER … OWNER TO`.

Dump ze [scripts/example/postgres_backup.sh](../scripts/example/postgres_backup.sh) je
**plain SQL jedné databáze** bez `CREATE DATABASE` — obnovuje se přes `psql`
(ne `pg_restore`, ten umí jen formáty `custom`/`directory`/`tar`).

```sh
export PGPASSWORD='…'

# 1) role a tablespaces (jen na cluster, kde ještě nejsou)
psql --host=db01 --username=postgres --dbname=postgres \
     --set=ON_ERROR_STOP=1 \
     --file=pg-globals-web01-run-41.dump

# 2) cílová databáze; -T template0 kvůli kódování a collation z dumpu
createdb --host=db01 --username=postgres --template=template0 --encoding=UTF8 shop_obnova

# 3) obnova dat
psql --host=db01 --username=postgres --dbname=shop_obnova \
     --set=ON_ERROR_STOP=1 --single-transaction \
     --file=postgres-web01-run-42.dump
echo "exit=$?"
```

Krok 1 je psaný pro **prázdný cluster** — tam je `ON_ERROR_STOP=1` na místě, protože
každá chyba je skutečná. Do clusteru, kde už nějaké role jsou, ho naopak vynech: dump má
`CREATE ROLE` i pro role, které tam existují (`postgres` prakticky vždycky), a první
kolize by zbytek zahodila. Pak ale **projdi stderr ručně** — `role "x" already exists`
je v pořádku, cokoli jiného ne.

> Globals dump obsahuje **hashe hesel všech rolí** (`ALTER ROLE … PASSWORD 'SCRAM-…'`).
> Arcatum payloady nešifruje, takže leží v otevřené podobě na serveru jako každý jiný
> dump — zachází se s ním jako s citlivým souborem.

**`ON_ERROR_STOP=1` tam není pro parádu.** `psql` ve výchozím nastavení po chybě
pokračuje dál a skončí kódem 0 — obnova, které chybí půlka tabulek, tak vypadá jako
úspěšná. `--single-transaction` k tomu přidá, že se při chybě nic nepřipojí a cílová
databáze zůstane prázdná místo napůl naplněné.

**Vlastníci a práva jsou v dumpu.** `pg_dump` do plain formátu zapisuje
`ALTER … OWNER TO <role>` a `GRANT`. Do **jiného** clusteru, kde ty role neexistují, se
takový dump neobnoví — musíš buď role předem vytvořit:

```sh
psql --host=db01 --username=postgres -c "CREATE ROLE shop_app LOGIN"
```

…nebo pořídit dump s `--no-owner --no-privileges`. To je volba **při dumpu**, ne při
obnově; z hotového plain dumpu se ownership pohodlně vyndat nedá.

Do stejného clusteru, odkud záloha pochází, tenhle problém nenastane.

**Verze.** `pg_dump` musí být stejné nebo vyšší verze než zálohovaný server; obnovovat
dump do **starší** major verze Postgresu spolehlivě nejde.

---

## 5. Co v dumpu není

Dump jedné databáze zachytí databázi, ne server. Tohle v něm nehledej:

| | MySQL | PostgreSQL |
|---|---|---|
| funkce, procedury, triggery, pohledy | ano | ano |
| sekvence / AUTO_INCREMENT | ano | ano |
| naplánované úlohy | ano (`--events`) | n/a |
| rozšíření | n/a | ano (`CREATE EXTENSION`, musí být na cíli nainstalované) |
| large objects | n/a | ano |
| uživatelé, hesla, granty | **ne** (`mysql.user`) | ano, ale **jinou instancí** — `postgres-globals-backup` |
| ostatní databáze na serveru | ne | ne |
| `CREATE DATABASE` | ne | ne (chybí `--create`) |
| konfigurace serveru | ne (`my.cnf`) | ne (`postgresql.conf`, `pg_hba.conf`) |
| point-in-time recovery | ne (binlogy) | ne (WAL archiv) |

Co z toho plyne prakticky:

- **MySQL uživatelé a granty nejsou zálohovaní vůbec.** Po obnově na jiný server je musíš
  vytvořit ručně, jinak se do obnovené databáze nikdo nepřipojí. Arcatum pro to zatím
  nemá protějšek toho, co u Postgresu dělá `postgres-globals-backup`.
- **U Postgresu potřebuješ obě instance.** Jedna `postgres-globals-backup` na server,
  jedna `postgres-backup` na každou databázi. Sama o sobě neobnoví ani jedna.
- `--single-transaction` u `mysqldump` dává konzistentní snímek jen pro **InnoDB**;
  MyISAM tabulky se dumpnou tak, jak se zrovna trefí.

---

## 6. Katalog chyb

| Zpráva | Kde | Co s tím |
|---|---|---|
| `410 this run's dump has been rotated away by retention` | stažení | dump smazala retence — vyber novější běh, nebo zvedni `keep_last`/`keep_days` |
| `404 this run has no backup data` | stažení | běh neskončil úspěšně, nebo skript nemá `capture = "stream"` |
| `ERROR 1840 … @@GLOBAL.GTID_PURGED can only be set when @@GLOBAL.GTID_EXECUTED is empty` | MySQL obnova | GTID — viz [§3](#3-mysql--mariadb) |
| `ERROR 1049 Unknown database` | MySQL obnova | dump neobsahuje `CREATE DATABASE`, založ ji ručně |
| `role "x" does not exist` | PG obnova | globals dump obnov **před** dumpem databáze, nebo role vytvoř ručně |
| `role "x" already exists` | PG obnova globals | cluster už role má — vynech `ON_ERROR_STOP` a projdi stderr ručně |
| `permission denied for table pg_authid` | běh `postgres-globals-backup` | instance neběží pod superuživatelem; hesla rolí jinak přečíst nelze |
| `you need (at least one of) the EVENT privilege(s)` | běh `mysql-backup` | zálohovací účet nemá `EVENT` — přidej ho, nebo z skriptu vyhoď `--events` |
| `relation "x" already exists` | PG obnova | obnovuješ do neprázdné databáze; `pg_dump` bez `--clean` nic nemaže |
| `unsupported version … in file header` | `pg_restore` | plain dump se obnovuje `psql`, ne `pg_restore` |
| `server version mismatch` | PG obnova | dump z novějšího Postgresu do staršího nejde |
| obnova skončí kódem 0, ale data chybí | PG obnova | zapomenuté `ON_ERROR_STOP=1` — `psql` chyby jen vypsal a šel dál |
| dump je podezřele malý | — | skript možná selhal bez `pipefail`; porovnej `data_bytes` s předchozími běhy |

---

## 7. Zkušební obnova

Záloha, ze které se nikdy nic neobnovilo, je nepotvrzená domněnka. Postup, který se vejde
do půl hodiny a dá skutečnou odpověď:

1. Stáhni **nejnovější** dump instance (`/api/v1/instances/<id>/dumps` → `/runs/<run>/data`).
2. Obnov ho do **nové prázdné databáze** na testovacím serveru, ne přes produkci.
3. Zkontroluj, že obnova skončila kódem 0 — u Postgresu **s `ON_ERROR_STOP=1`**, jinak
   ten kód nic neznamená.
4. Porovnej obsah proti originálu. Pusť **tentýž příkaz** proti zdrojové i obnovené
   databázi a výstupy prožeň `diff`em:

   ```sh
   # MySQL — kontrolní součty obsahu, ne jen počty řádků
   mysql -N -B shop -e "SHOW TABLES" |
     xargs -I% mysql -N -B shop -e 'CHECKSUM TABLE `%`' | sed 's/^[^.]*\.//'

   # PostgreSQL — přesné počty řádků; n_live_tup ze statistik je jen odhad
   psql -At -d shop -c "
     SELECT relname, (xpath('/row/c/text()',
              query_to_xml(format('SELECT count(*) AS c FROM %I.%I', schemaname, relname),
                           false, true, '')))[1]::text::bigint
     FROM pg_stat_user_tables ORDER BY relname"
   ```

   (`sed` u MySQL utíná jméno databáze, aby diff nehlásil rozdíl jen proto, že obnovená
   kopie se jmenuje jinak.)

5. Ověř, co tabulky neukážou: existují funkce, triggery a EVENTy, sedí sekvence /
   `AUTO_INCREMENT`, aplikace se do obnovené databáze připojí.

Checklist pro novou databázovou instanci:

- [ ] instance má aspoň jeden povolený **rozvrh** — bez něj se spustí, jen když někdo zmáčkne
      „run now"
- [ ] zkušební obnova proběhla, `exit = 0`, kontrolní součty / počty řádků sedí
- [ ] cílová databáze šla založit se **stejným kódováním a collation**
- [ ] u Postgresu: server má i instanci `postgres-globals-backup` a její dump jsi obnovil
      **jako první**
- [ ] u Postgresu: instance globals běží pod superuživatelem (jinak běh spadne na `pg_authid`)
- [ ] u MySQL: zálohovací účet má právo `EVENT` — skript ho po přidání `--events` vyžaduje
- [ ] u MySQL: víš, kde vezmeš uživatele a granty — v dumpu nejsou a Arcatum je nezálohuje
- [ ] `keep_last` / `keep_days` na instanci pokrývají dobu, za kterou se pozná tiché
      poškození dat, ne jen včerejšek

---

Související: [vývoj a ladění skriptů](script-development_cz.md) ·
[architektura §17 (payload vs. log)](architecture_cz.md) ·
[architektura §19 (retence dumpů)](architecture_cz.md) ·
[README → Obnova dat](../README_cz.md#obnova-dat) (souborové zálohy přes restic)
