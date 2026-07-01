# slskdarr — designdokument

**Datum:** 2026-07-01
**Status:** Godkänd design, ej påbörjad implementation
**Ersätter:** soularr (github.com/mrusse08/soularr)

---

## 1. Syfte och bakgrund

slskdarr är en brygga mellan **Lidarr** och **slskd** (Soulseek): den bevakar Lidarrs
lista över efterfrågade/saknade album, söker efter dem på Soulseek via slskd:s REST-API,
väljer bästa träff, laddar ner den, och lämnar tillbaka de färdiga filerna till Lidarr
för import.

Det är en omskrivning av `soularr`, inte en portning. soularr fungerar men har sex
konkreta svagheter som den här designen ska lösa på riktigt:

1. **Ingen persistent state.** soularr är ett engångsskript som håller sina pågående
   nedladdningar i minnet och avslutas. Startar processen om medan nedladdningar pågår
   blir de permanent hängande i slskd ("orphans") — inget tar bort, avbryter eller gör
   om dem, för nästa körning saknar minne av dem.
2. **Cron-loop via sleep i stället för riktig daemon.** En bash-loop kör skriptet,
   sover N sekunder, upprepar — utan skydd mot överlapp utöver `ps aux | grep`.
3. **Tysta config-fallbacks som döljer stavfel.** `configparser` med `fallback=` överallt
   gör att en felstavad sektion/nyckel tyst faller tillbaka till ett default i stället för
   att fela.
4. **Svag retry/backoff och timeout-logik.** Grova, fasta timeouts som bara gäller medan
   processen lever.
5. **Ingen observability.** Bara en roterande loggfil.
6. **En enda hårdkodad matchningsstrategi.** Först-godtagbara-träff på en format-lista.

### Mål för v1
Lösa 1–6 på ett sätt som är underhållbart för en ensam maintainer och driftsäkert över lång
tid. Kärnan (persistens + avstämning + daemon-livscykel) måste vara rätt från början; UI och
plugin-arkitektur är påbyggnad som skjuts upp.

### Icke-mål
Ändra Lidarr eller slskd (de körs redan, orörda). HA/multi-instans. Web-auth utöver att binda
status-API:t till internt nät. Notiser.

---

## 2. Teknikval

**Språk: Go.** Valt på *operationell* passform, inte beräkningsbehov — tjänsten är I/O-bunden
glue-kod. Motiveringen:

- Ett statiskt länkat binärt → liten oprivilegierad Docker-image (`scratch`/distroless),
  körs som fast UID utan interpreter/venv-yta.
- Trivial cross-compile till arm64 för hemmaserver (`GOOS`/`GOARCH`), ingen QEMU.
- Daemon-formen (scheduler + samtidiga workers + graceful shutdown via `context.Context`)
  är idiomatisk Go.
- Mogna, tråkiga beroenden: `net/http` för klienter och status-API, officiell Prometheus-klient,
  och **ren-Go SQLite** (`modernc.org/sqlite`) → noll cgo, cross-compile förblir trivial.
- Pris jag äger: scorer/matcher-logiken blir ordrikare än i Python.

Alternativ som valdes bort: **Rust** (tokio + borrow-checker är dålig kostnad/nytta för
liten I/O-glue med en maintainer), **typad Python-omskrivning** (snabbast att skriva men
ingen äkta single-binary-image, och stannar i ekosystemet vars engångsskript-kultur vi lämnar).

---

## 3. Datamodell och tillståndsmaskin

### 3.1 Central insikt
soularrs orphan-bugg beror inte egentligen på "ingen databas" utan på att **timeouten lever i
processen, inte i datan.** Dör processen glöms timeouten. Lösningen är att göra varje deadline
till en **tidsstämpel i en kolumn**, så att vilken arbetare som helst, efter vilken omstart som
helst, kan hålla koll på den.

### 3.2 Tre nivåer av entiteter

- **AlbumJob** — ett efterfrågat album från Lidarr. Den enhet användaren ser som "jobb".
- **CandidateAttempt** — en Soulseek-användare vi provar för albumet. Ett album betar av
  kandidater i rankad ordning tills en lyckas.
- **Transfer** — en enskild filnedladdning i slskd, med slskd:s eget `id`. Detta är **enda
  nivån slskd känner till**, så avstämningen jobbar på just denna nivå.

### 3.3 AlbumJob-tillstånd

```
DISCOVERED → SEARCHING → SELECTING → DOWNLOADING → VERIFYING → IMPORTING → COMPLETED
```

Avfarter:
- **COOLDOWN** — transient bakslag (nätverk, användare offline). Vänta med backoff, försök igen.
- **FAILED** — alla kandidater slut / permanent fel. Väntar på nästa Lidarr-poll.

`SEARCHING`/`SELECTING` är billiga och körs bara om vid omstart. `DOWNLOADING` är den farliga
som kräver avstämning.

### 3.4 Transfer-tillstånd

Speglar slskd: `QUEUED, IN_PROGRESS, COMPLETED, ERRORED, CANCELLED`, plus ett vi räknar ut
själva: **`STALLED`** (noll framsteg i byte under för lång tid). Varje transfer-rad har en
**`deadline`-tidsstämpel** — mekanismen som dödar orphans.

### 3.5 Skriv-först-ordning (kärnan i restart-säkerheten)

1. Spara *avsikten* att ladda ner (rader för `CandidateAttempt` + `Transfer`, tillstånd
   `QUEUED`, ännu inget slskd-id) — **innan** vi anropar slskd.
2. Anropa slskd; spara returnerat **`id`**.
3. Krasch *mellan* steg 1 och 2 → avstämningen hittar tillbaka via reservnyckeln
   **`(username, filename)`** och fyller i id:t.

**Stark nyckel = slskd:s `id`. Reservnyckel = `(username, filename)`.**

### 3.6 Avstämning (reconciliation)

Definition: lägg vår egen bild av vad som borde hända bredvid slskd:s faktiska aktuella
transfer-lista, se skillnaderna, rätta till dem. Körs **vid start OCH periodiskt — samma kod.**

| Situation | Åtgärd |
|---|---|
| Vår transfer finns kvar och gör framsteg i slskd | **Adoptera**, fortsätt bevaka |
| Finns men är terminal i slskd (klar/felad/avbruten) | För jobbet vidare (klar→VERIFYING; felad→fälla kandidat) |
| Finns i vår DB men saknas i slskd | Markera förlorad → köa om kandidaten eller fälla den |
| Adopterad men förbi sin `deadline` utan framsteg | **Avbryt i slskd, fälla kandidaten**, gå till nästa kandidat eller COOLDOWN |
| Finns i slskd men är inte vår | **Lämna i fred** (kan vara manuella nedladdningar), räkna som `unknown_transfers` |

### 3.7 Skisserat schema (vägledande, ej slutgiltigt)

```sql
CREATE TABLE album_jobs (
    id              INTEGER PRIMARY KEY,
    lidarr_album_id INTEGER NOT NULL,
    state           TEXT NOT NULL,          -- DISCOVERED..COMPLETED/COOLDOWN/FAILED
    candidates_tried INTEGER NOT NULL DEFAULT 0,
    next_attempt_at DATETIME,               -- för COOLDOWN-backoff
    created_at      DATETIME NOT NULL,
    updated_at      DATETIME NOT NULL,
    UNIQUE(lidarr_album_id)
);

CREATE TABLE candidate_attempts (
    id           INTEGER PRIMARY KEY,
    album_job_id INTEGER NOT NULL REFERENCES album_jobs(id),
    username     TEXT NOT NULL,
    score        REAL NOT NULL,
    state        TEXT NOT NULL,             -- PENDING/ACTIVE/SUCCEEDED/FAILED
    fail_reason  TEXT,                      -- timeout/errored/cancelled/incomplete
    backoff_until DATETIME,                 -- exponentiell backoff per användare
    created_at   DATETIME NOT NULL
);

CREATE TABLE transfers (
    id            INTEGER PRIMARY KEY,
    attempt_id    INTEGER NOT NULL REFERENCES candidate_attempts(id),
    slskd_id      TEXT,                      -- NULL tills slskd svarat (skriv-först)
    username      TEXT NOT NULL,             -- del av reservnyckeln
    filename      TEXT NOT NULL,             -- del av reservnyckeln
    state         TEXT NOT NULL,             -- QUEUED..CANCELLED/STALLED
    bytes_done    INTEGER NOT NULL DEFAULT 0,
    bytes_total   INTEGER,
    deadline      DATETIME NOT NULL,         -- anti-orphan
    last_progress_at DATETIME,               -- för STALLED-beräkning
    updated_at    DATETIME NOT NULL,
    UNIQUE(username, filename)
);
```

---

## 4. Modulgränser (Go-paket)

Princip: **beroenden pekar inåt mot datamodellen, aldrig utåt mot omvärlden.** Klienterna vet
inget om databasen; databasen vet inget om HTTP; kärnlogiken beror bara på interfaces.

```
cmd/slskdarr/main.go        — wiring: läs config, öppna DB, starta daemon, hantera signaler
internal/
  core/                     — domäntyper: AlbumJob, CandidateAttempt, Transfer, tillstånd
  config/                   — strict parsing, en enda Config-struct
  lidarr/                   — Lidarr REST-klient: wanted-albums, trigga import
  slskd/                    — slskd REST-klient: search, enqueue, list transfers, cancel
  store/                    — SQLite: entiteter, migrations, queries, transaktioner
  matcher/                  — Scorer-interface + en viktad implementation
  engine/                   — scheduler, workers, reconciler, tillståndsövergångar (äger portarna)
  observ/                   — structured logging, Prometheus-metrics, status-API
```

### Ansvar och beroenden

- **`core`** — äger domäntyperna (substantiven). Importerar ingen. Alla får importera det.
- **`config`** — läser filen till en `Config`-struct, felar högljutt vid okänd nyckel/sektion
  eller typfel. Injiceras till övriga paket.
- **`lidarr`** / **`slskd`** — konkreta REST-klienter. Speglar sina externa API:er, returnerar
  egna typer. Vet inget om databas eller jobb-tillstånd. Definierar **inga** interfaces själva.
- **`store`** — äger SQLite. Enda stället med SQL och transaktioner. Metoder som
  `ClaimDueJobs`, `RecordEnqueueIntent`, `AttachTransferID`, `TransfersPastDeadline`,
  `AdvanceJob`. All atomär tillståndslogik bor här.
- **`matcher`** — `Scorer`-interface + viktad implementation. Ren funktion: sökträffar +
  config-vikter → rankad kandidatlista. Noll I/O, noll databas.
- **`engine`** — hjärtat. Scheduler-loop, worker-pool, reconciler. **Äger portarna** (Go-stil,
  se 4.1). Tar emot `lidarr`/`slskd`/`store`/`matcher` som uppfyller dem.
- **`observ`** — logger, `/metrics`, läs-API. Tar emot enkla räknare/tal; beror inte tillbaka.

### 4.1 Interface-stil: konsumenten äger porten

Go uppfyller interfaces **implicit** — en typ med rätt metoder uppfyller ett interface utan att
deklarera det. Därför bor portarna hos konsumenten (`engine`), inte hos producenten (`slskd`).
`engine` deklarerar det *smala* interface den faktiskt behöver:

```go
// internal/engine/ports.go
package engine

type PeerNetwork interface {
	Search(ctx context.Context, query string) ([]slskd.Result, error)
	Enqueue(ctx context.Context, user, file string) (string, error)
	ListDownloads(ctx context.Context) ([]slskd.Transfer, error)
	Cancel(ctx context.Context, id string) error
}
```

`*slskd.Client` har fler metoder än så (t.ex. `BrowseUser`, `ServerState`) men uppfyller
`PeerNetwork` automatiskt. `main.go` skickar in den konkreta klienten. Fördelar: tester behöver
bara en liten fejk med de metoder porten kräver; reconciler kan ta ännu smalare interfaces
(`TransferLister`, `TransferCanceller`); `slskd` beror aldrig på `engine`.

Regel: **interface när du behöver det** (för test eller andra implementationer), inte för allt.
Vi ritar `PeerNetwork`/`MusicSource` från början eftersom vi vet att vi vill fejka dem i test.

---

## 5. Daemon-livscykel

- **Scheduler + worker-pool.** Separata poll-intervall i config: Lidarr-poll (hitta nya album)
  vs. slskd-status-poll (bevaka transfers/avstämning).
- **Graceful shutdown** via `signal.NotifyContext`. Pågående nedladdningar checkpointas i DB —
  de fortsätter i slskd och adopteras vid nästa start via avstämningen. Ingen väntan på att
  nedladdningar ska bli klara krävs; state ligger redan persistent.
- **Överlappsskydd** kommer gratis: en daemon med en scheduler-loop, inte N cron-körningar.

---

## 6. Felhantering och retry

- **Transient vs. permanent** kodas in i tillståndsövergångarna, inte i loggen:
  - Transient (nätverk, slskd nere, användare offline) → `COOLDOWN` med **exponentiell backoff
    per kandidat-användare** (`candidate_attempts.backoff_until`).
  - Permanent (album saknas, alla kandidater slut) → `FAILED`, väntar på nästa Lidarr-poll.
- **`max_candidates_per_album`** i config sätter tak på hur många användare som provas.
- **Timeouts lever i `transfers.deadline`**, inte i processen (se 3.1).

---

## 7. Matcher / scorer

En enda viktad implementation bakom ett `Scorer`-interface. Rankar kandidater på
format, bitrate, användartillförlitlighet och filantal (fullständigt album). **Vikterna ligger
i config** så de kan trimmas utan omkompilering. Interfacet gör den utbytbar senare, men **inget
plugin-maskineri i v1** (YAGNI).

---

## 8. Observability

- **Strukturerad JSON-logg.**
- **Prometheus `/metrics`:** searches issued, matches found, downloads active/stalled/failed,
  time-to-completion, `unknown_transfers`.
- **Läs-API i JSON (`/status`):** vad som är köat, aktivt, stalled, orphaned — utan att grepa
  loggar eller anropa slskd för hand. Bunden till internt nät.
- **HTML-UI skjuts upp** — API:t ger datan; en sida ovanpå rör inte kärnan.

---

## 9. v1-gräns

### I v1 (byggs på riktigt)
Datamodell + tillståndsmaskin · avstämning (start + periodisk) · daemon-livscykel med graceful
shutdown · strict config · backoff + retry med deadline-i-kolumn · en viktad scorer bakom
interface med config-vikter · strukturerad logg + `/metrics` + läs-`/status`-API.

### Skjuts upp (medvetet)
HTML-UI · plugin-system för scoring · automatisk "avbryt okända transfers" (räknas men rörs ej)
· notiser (Discord/webhook) · multi-instans/HA · web-auth utöver intern nätbindning.

---

## 10. Testning

- **`matcher`** — enhetstest, ren funktion.
- **`store`** — enhetstest mot SQLite-fil i tempkatalog.
- **`engine`/reconciler** — testas mot **fejkade** `PeerNetwork`/`MusicSource`.
- **Nyckelscenario (existensberättigandet):** "krascha mitt i en enqueue → starta om →
  ingen orphan." Uttalat scenario-test i v1.

---

## 11. Drift

- Docker-image byggd `FROM scratch`/distroless, ett statiskt binärt.
- Körs som **oprivilegierad, fast UID** (ingen root). Skriver till NFS-monterat mediabibliotek
  → filrättigheter (UID/GID) måste stämma med Lidarr.
- SQLite-filen på en persistent volym.
- Config-fil monterad in; strict parsing felar vid start om något är fel.
