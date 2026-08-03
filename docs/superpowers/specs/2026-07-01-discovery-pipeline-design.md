# slusk — discovery-pipeline (designdokument)

**Datum:** 2026-07-01
**Status:** Godkänd design, ej påbörjad implementation
**Bygger på:** v1 (se `2026-07-01-slusk-design.md`), som är mergat till `main`
**Omfattning:** En egen spec→plan→implement-cykel för Lidarr discovery→search→select→download→verify→import.

---

## 1. Syfte

v1 byggde och härdade *reconciliation-kärnan* (persistent transfer-state, avstämning mot
slskd, anti-orphan). Jobb-lagret (`AlbumJob`-tillståndsmaskinen) lämnades **vilande** — inget
skapar eller driver album-jobb ännu. Denna pipeline aktiverar det: den upptäcker efterfrågade
album i Lidarr, söker dem på Soulseek via slskd, väljer bästa kandidat, laddar ner (via v1:s
write-ahead + reconciler), och lämnar över till Lidarr med korrekt hantering av avvisade
importer.

### Mål
- Driva `AlbumJob` från `DISCOVERED` till `COMPLETED`/`FAILED`, tillstånds-drivet och
  restart-säkert (samma filosofi som v1-kärnan).
- Bygga slskd:s **async search** (medvetet uppskjuten i v1).
- Hantera Lidarr-import via **manual-import-API med rejection-hantering** — inte blind scan —
  så kvalitetsavvisningar upptäcks och leder till nästa kandidat i stället för tyst miss.
- Proaktivt kvalitetsgolv vid kandidatval så avvisningar blir sällsynta.

### Icke-mål
- HTML-UI, plugin-scoring (kvarstår uppskjutna). Ingen ändring av v1:s reconciler-kontrakt.

---

## 2. Kontrollmodell: tillstånds-driven, ett steg per tick

Pipelinen är **inte** en lång procedur per album. Den är tillstånds-driven precis som
reconcilern: en discovery-loop tickar på `LidarrPoll` och tar varje jobb *ett steg* mot dess
nästa tillstånd. Allt framsteg lever i DB:n → en omstart mitt i pipelinen tappar ingenting.

**`engine.Run` får en andra ticker** (`LidarrPoll`) som kör `Discoverer.RunOnce(ctx)`;
reconcilern fortsätter på `StatusPoll`. Två loopar, en daemon. De rör olika tabellnivåer
(discovery = album/attempt, reconciler = transfer) och krockar inte.

Varje `RunOnce`-tick, i ordning:

1. **Sync från Lidarr:** `WantedMissing()` → `UpsertDiscoveredJob` per album (idempotent).
2. **Driv jobb ett steg**, per tillstånd (claima med limit för samtidighet):
   - **`DISCOVERED`** → bounded async `Search` → kvalitetsfiltra + `matcher.Rank` → välj
     toppkandidat ej redan fälld → **write-ahead enqueue** filerna → `DOWNLOADING`.
     (Sök-state persistas *inte*; omstart → jobbet kvar `DISCOVERED`, söker om. Billigt.)
   - **`DOWNLOADING`** → reconcilern övervakar transfers. Discovery-loopen läser den aktiva
     attemptens transfers: alla `COMPLETED` → `VERIFYING`; någon `ERRORED/CANCELLED/`förbi
     deadline → fälla kandidaten → `COOLDOWN`.
   - **`VERIFYING`** → alla enqueue:ade filer klara (bekräftat) → `IMPORTING`.
   - **`IMPORTING`** → kör manual-import (se §5). Rent → `COMPLETED`. Blockerande rejection →
     fälla kandidaten → `COOLDOWN` (eller `FAILED` om `candidates_tried >= max`).
   - **`COOLDOWN`** → `next_attempt_at` passerat → tillbaka till kandidatval (`DISCOVERED`-
     liknande steg men med fail-historik så redan provade kandidater hoppas över).
3. **`FAILED`** väntar på nästa Lidarr-poll (om albumet fortfarande är wanted försöker vi igen
   först när det lämnat och återkommit, eller efter en lång cooldown — se §6).

**Samtidighet:** konservativ default `max_concurrent_searches = 2` för att inte hamra
Soulseek/slskd. Config-ratt.

---

## 3. Async search (ny `slskd`-klientmetod)

`Search(ctx, query string, timeout time.Duration) ([]Result, error)`:
1. `POST /api/v0/searches` med söktexten → `id` + state.
2. Polla `GET /api/v0/searches/{id}` tills `state` = completed **eller** `timeout` nås
   (kort pollintervall).
3. `GET /api/v0/searches/{id}/responses` → platta ut till `[]Result` (typ finns från v1).
4. `DELETE` sökningen i slskd så de inte hopar sig.

Sök-state persistas inte (re-entrant vid omstart). Detta är metoden som medvetet sköts upp i
v1 (Task 5).

**Query:** `"<ArtistName> <AlbumTitle>"` från Lidarr-albumet. Tunbart senare.

---

## 4. Kandidatval med kvalitetsgolv

1. **Hårt kvalitetsfilter före scoring:** släng kandidatfiler under golvet. Golv (config):
   lossless (FLAC/etc.) alltid OK; lossy endast om `bitRate >= min_bitrate` (default 192).
   Släng användare vars kvarvarande filer inte räcker till ett album.
2. `matcher.Rank(results)` (finns) grupperar per användare och scorar på
   format/bitrate/filantal.
3. **Filantal-hint:** föredra kandidater vars filantal ≥ Lidarrs `TrackCount` (utökas på
   `WantedAlbum`). Hint, inte hårt krav — Lidarr är slutdomare.
4. Ta toppkandidaten som **inte** redan fällts för detta jobb (`AttemptsForJob`-historik).

**Write-ahead enqueue (återanvänder v1-store):** per fil `RecordEnqueueIntent` →
`slskd.Enqueue` → `AttachTransferID`; skapa `CandidateAttempt` (ACTIVE); sätt jobbet
`DOWNLOADING`. Reconcilern tar över övervakningen.

---

## 5. Import: manual-import-API med rejection-hantering

Ersätter soularrs blinda `DownloadedAlbumsScan` (som gav tysta kvalitetsavvisningar).

**Nya `lidarr`-klientmetoder:**
- `ManualImportCandidates(ctx, folder) ([]ManualImportItem, error)` —
  `GET /api/v1/manualimport?folder=<albumfolder>`; varje item har `id`, `rejections[]`,
  albummatch, kvalitet, spår.
- `ExecuteManualImport(ctx, items []ManualImportItem) error` —
  `POST /api/v1/command {name:"ManualImport", importMode:"move", files:[…]}` för de items som
  saknar blockerande rejection.

**Flöde i `IMPORTING`:**
1. Beräkna albumets mapp (se §7 path-översättning), hämta manual-import-kandidater.
2. Inga blockerande rejections → `ExecuteManualImport` (move) → `COMPLETED`.
3. Blockerande rejection (t.ex. "quality not in profile", "no matching album") → fälla
   kandidaten → `COOLDOWN`/nästa kandidat, eller `FAILED` vid slut på kandidater.

Vi väntar inte på Lidarrs asynkrona importresultat utöver detta; manual-import-anropet är
synkront nog för att veta om filerna accepterades.

---

## 6. Retry, backoff, fail

- **Per kandidat:** exponentiell backoff (`candidate_backoff`-bas) via
  `candidate_attempts.backoff_until` (kolumn finns). En fälld kandidat provas inte om.
- **Per album:** `candidates_tried` räknas upp; vid `>= max_candidates_per_album` → `FAILED`.
- **`COOLDOWN`:** `album_jobs.next_attempt_at` (kolumn finns) styr när albumet får nästa försök.
- **`FAILED`:** albumet lämnas; det återupptas naturligt om det fortfarande är wanted efter en
  lång album-cooldown (config `failed_retry_after`, t.ex. 24h) — så vi inte loopar hårt men
  inte heller ger upp för alltid.

---

## 7. Path-översättning (enda sköra integrationspunkten)

Import kräver mappen där slskd lade de färdiga filerna, **som Lidarr ser den**.
- Config `[paths] slskd_complete_dir` = den roten (Lidarr-synlig).
- slskd:s `filename`-fält bär den relativa katalogen (t.ex.
  `music\Sia\1000 Forms of Fear (2014)\...`). Albumets mapp beräknas som
  `slskd_complete_dir` + gemensam katalog för attemptens filer, med `\`→`/`-översättning och
  sanering.
- **Fallback:** kan albummappen inte beräknas entydigt → peka manual-import på
  `slskd_complete_dir` (Lidarr matchar ändå per album).

Verifieras mot den faktiska slskd-mapp-layouten under implementation (som API-formerna i v1).

---

## 8. Nya store-metoder (store äger fortsatt all SQL)

- `JobsInState(ctx, state core.AlbumJobState, limit int) ([]core.AlbumJob, error)`
- `DueCooldownJobs(ctx, now time.Time, limit int) ([]core.AlbumJob, error)`
- `AttemptsForJob(ctx, jobID int64) ([]core.CandidateAttempt, error)`
- `TransfersForAttempt(ctx, attemptID int64) ([]core.Transfer, error)`
- `FailAttempt(ctx, attemptID int64, reason string, backoffUntil time.Time, now time.Time) error`
- `SucceedAttempt(ctx, attemptID int64, now time.Time) error`
- `SetJobCooldown(ctx, jobID int64, nextAttemptAt time.Time, now time.Time) error`
- `IncrementCandidatesTried(ctx, jobID int64, now time.Time) error`

Alla context-aware; ingen annan modul rör SQL.

---

## 9. Moduländringar

- **`internal/slskd`:** lägg till `Search` (async, §3).
- **`internal/lidarr`:** lägg till `ManualImportCandidates`, `ExecuteManualImport`; utöka
  `WantedAlbum` med `TrackCount`.
- **`internal/config`:** `[engine]` får `search_timeout`, `min_bitrate`,
  `max_concurrent_searches`, `candidate_backoff`, `failed_retry_after`; nytt `[paths]` med
  `slskd_complete_dir`. `Validate()` utökas.
- **`internal/matcher`:** kvalitetsfilter-hjälp (eller filtrering i discovery före `Rank`).
- **`internal/engine`:** nytt `discovery.go` (`Discoverer.RunOnce`), nya konsument-portar
  `MusicSource` (Lidarr) + utökad slskd-port (`Search`/`Enqueue`); `JobStore` växer med §8.
  `Run` får en andra ticker på `LidarrPoll`. Nya metrics: searches issued, matches found,
  imports ok/rejected, candidates exhausted.
- **`cmd/slusk/main.go`:** wire in `Discoverer` (Lidarr-klient + store + slskd + matcher) och
  starta discovery-loopen.

---

## 10. Testning

- **`slskd.Search`:** `httptest` som simulerar start→poll→responses→delete.
- **`lidarr` manual-import:** `httptest` med och utan `rejections`.
- **Kvalitetsfilter:** enhetstest (lossy under golv slängs, lossless behålls).
- **`Discoverer.RunOnce`:** mot fejkade portar — de kritiska scenariona:
  - `DISCOVERED` → search → enqueue → `DOWNLOADING` (write-ahead-rader skapade).
  - `DOWNLOADING` med alla transfers klara → `VERIFYING`.
  - `IMPORTING` med blockerande rejection → kandidat fälld, `COOLDOWN`, nästa kandidat provas.
  - `candidates_tried >= max` → `FAILED`.
- **Path-översättning:** enhetstest av `\`→`/`-mappning + fallback.

---

## 11. Öppna/verifieras-under-implementation

- Exakt JSON-form för slskd `searches/{id}` + `/responses` och Lidarrs `manualimport` —
  verifieras mot de riktiga instanserna (Lidarr 3.1.0, slskd 0.25.1), som i v1.
- slskd:s faktiska completed-downloads-mapplayout för path-översättningen (§7).
- `transfers.UNIQUE(username, filename)` är global (v1-not): en re-enqueue av samma user+fil
  över attempts kan krocka → `RecordEnqueueIntent` behöver hanteras med `ON CONFLICT`/per-attempt
  åtgärd i denna cykel (bärs in från v1:s downstream-not).
