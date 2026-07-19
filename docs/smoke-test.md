# slskdarr — manuell smök-test-checklista (skarp körning)

Kör igenom denna första gången du släpper slskdarr mot din riktiga Lidarr + slskd.
Syftet är att verifiera hela flödet **innan** du litar på auto-import. Ta det uppifrån
och ner — varje fas bygger på den föregående.

Nyttiga kommandon (byt ut värden mot dina):
```bash
STATUS=http://192.168.86.33:9090        # observ.listen_addr
TOKEN='värdet-från-observ.auth_token'    # lägg aldrig token i URL/query
PGDSN='postgres://slskdarr:password@localhost:5432/slskdarr'  # store.dsn
docker logs -f slskdarr                 # strukturerad JSON-logg
curl -s $STATUS/healthz                 # publik, minimal liveness
curl -s -H "Authorization: Bearer $TOKEN" $STATUS/status | jq
curl -s -H "Authorization: Bearer $TOKEN" $STATUS/metrics | grep slskdarr
psql "$PGDSN" -c 'SELECT id,lidarr_album_id,state,candidates_tried FROM album_jobs'
```
(Distroless-imagen har ingen shell; kör `psql` från host mot Postgres-instansen,
eller `docker exec` in i Postgres-containern.)

---

## Fas 1 — Pre-flight (startar den ens?)

- [ ] `config.toml` monterad på `/config/config.toml`; **fel** i den ska ge en tydlig
      logg-rad och exit (testa: stava fel en nyckel → containern ska dö högljutt, inte
      tyst defaulta). En listener utanför loopback ska dessutom vägra starta utan
      `observ.auth_token`; loopback-only (`127.0.0.1`/`::1`/`localhost`) får köras utan
      token för lokal utveckling.
- [ ] Containern kör som **oprivilegierad UID** (`docker inspect slskdarr` → `User: nonroot`).
- [ ] Schemat skapas i Postgres-databasen vid start (`psql "$PGDSN" -c '\dt'` visar
      `album_jobs`, `candidates`, `transfers`, `job_events` m.fl.).
- [ ] Loggen visar `slskdarr started` med rätt `status_addr`.

## Fas 2 — Anslutning + observability

- [ ] `curl $STATUS/healthz` svarar utan autentisering och utan intern statusdata.
- [ ] Anrop utan token till `/`, `/status`, `/api/*` och `/metrics` svarar `401`.
- [ ] `curl -H "Authorization: Bearer $TOKEN" $STATUS/status` svarar `200` med JSON (`{"queued":..,"active":..,"stalled":..,"orphaned":..,"modules":{...}}`).
      `modules` listar varje pipeline-modul (`wanted_sync`, `discovery`, `selecting`,
      `downloading`, `importing`) med tidpunkten för dess senast avslutade tick — en
      modul som slutat synas där (eller vars tid slutat röra sig) har fastnat.
- [ ] `curl -H "Authorization: Bearer $TOKEN" $STATUS/metrics` innehåller `slskdarr_reconcile_total` (och ökar över tid).
- [ ] Loggen visar **inga** `reconcile failed`/`discovery failed`-rader → Lidarr och slskd
      nås. (Om de syns: fel URL/API-nyckel i config.)
- [ ] `slskdarr_unknown_transfers` speglar rimligt antal (dina ev. manuella slskd-nedladdningar
      räknas men rörs inte).

## Fas 3 — Första upptäckt → sökning → nedladdning

- [ ] Se till att Lidarr har minst **ett** wanted/missing album (lägg till ett artist/album
      du vet finns rikligt på Soulseek, t.ex. något populärt i FLAC).
- [ ] Inom en `WantedSync`-cykel (var `wanted_sync_interval`, default 15m): en rad i
      `album_jobs` med `state=WANTED` dyker upp.
- [ ] Nästa tick: jobbet går `WANTED → SELECTING` (kandidatsökning/matchning i
      `candidates`-tabellen). Så snart en kandidat väljs går jobbet vidare
      `SELECTING → DOWNLOADING`, och en/flera rader i `transfers` skapas.
      det autentiserade status-anropet ovan visar `active > 0`.
- [ ] I slskd:s eget UI syns nedladdningarna starta (samma filer slskdarr enqueue:ade).
- [ ] `slskdarr_downloads_active` > 0 i metrics.

## Fas 4 — Kvalitetsgolv (proaktivt filter)

- [ ] Välj ett album där Soulseek har både låg-bitrate MP3 (< `min_bitrate`) och FLAC.
      Verifiera i loggen/DB att slskdarr valde en **kandidat över golvet** (FLAC eller
      ≥192 kbps), inte skräpet. Justera `min_bitrate` i config om det inte matchar din profil.

## Fas 5 — ⚠️ Import-överlämningen (den overifierade biten)

Detta är det enda anropet jag **inte** kunde verifiera mot din live-Lidarr (muterande).
Här vill jag att du tittar noga.

- [ ] Låt en nedladdning bli **klar** i slskd (alla filer). Jobbet ska gå
      `DOWNLOADING → IMPORTING`.
- [ ] Nästa tick: loggen visar antingen import eller `import rejected`. Kolla:
  - [ ] **Lyckad import:** jobbet blir `state=DONE`, filerna **flyttade** in i ditt
        Lidarr-bibliotek (inte kvar i `slskd_complete_dir`), och albumet försvinner ur
        Lidarrs wanted-lista.
  - [ ] Om filerna **inte** flyttades men jobbet blev DONE → `ManualImport`-POST-kroppen
        (per-fil-fälten `path/folderName/artistId/albumId`) accepterades inte som väntat av
        din Lidarr-version. Fånga `docker logs` + Lidarrs egen logg och hör av dig — det är
        `internal/lidarr/client.go:ExecuteManualImport` som då behöver justeras.
- [ ] **Path-kollen:** bekräfta att `AlbumFolder` pekade rätt — leta i loggen efter
      `import rejected`/`empty folder` med `folder=...` och se att sökvägen matchar var slskd
      faktiskt la filerna (under `paths.slskd_complete_dir`).
- [ ] Om ett import-svar dröjer förbi `import_confirm_timeout` (Lidarrs `ManualImport`-kommando
      är asynkront) behandlas jobbet som misslyckat och roteras till nästa kandidat, precis som
      ett avvisat svar (se Fas 6).

## Fas 6 — Felvägar (att de inte loopar tyst)

- [ ] **Avvisad import:** om Lidarr avvisar (t.ex. fel kvalitet) → loggen visar
      `import rejected`, jobbet går tillbaka till `SELECTING`, och nästa kandidat provas
      (ny rad i `candidates`). Det ska **inte** loopa på samma kandidat.
- [ ] **Kandidater slut:** ett svårt album där alla kandidater fälls → efter `max_retries`
      försök (default 10, räknas i `candidates_tried`) blir jobbet `state=FAILED` (inte evig
      loop). Mellan försöken backar jobbet av med `backoff_base`/`backoff_cap` innan nästa
      kandidat provas, snarare än att direkt gå i loop.
- [ ] **Retry efter fönster:** ett FAILED-album som fortfarande är wanted ska återupptas
      (`state` tillbaka till WANTED) efter `failed_revive_after`. (Sätt tillfälligt
      `failed_revive_after = "2m"` för att testa snabbt, återställ sen.) Samma sak går att
      trigga manuellt via dashboardens Retry-knapp (`POST /api/jobs/{id}/retry`), som ger
      `409` om jobbet redan hunnit lämna FAILED.

## Fas 7 — Restart-säkerhet (existensberättigandet)

- [ ] Starta en nedladdning, vänta tills den är **mitt i** (`active`, byte-progress i slskd).
- [ ] `docker restart slskdarr` (eller `kill` + starta om).
- [ ] Efter omstart: loggen/`$STATUS/status` visar att downloading-modulen **adopterade** den
      pågående transfern (ingen ny enqueue, ingen orphan i slskd). Nedladdningen fortsätter.
- [ ] Låt en transfer bli hängande förbi `transfer_deadline` → verifiera att downloading-modulens
      reconcile **avbryter** den i slskd och att jobbet går vidare (tillbaka till SELECTING/nästa
      kandidat), och att inget lämnas orphanat i slskd.

---

## Rekommenderade test-config-värden (snabbare cykler)

För smök-test, korta ner intervallen så du slipper vänta:
```toml
[pipeline]
transfer_deadline = "5m"
backoff_base = "1m"
failed_revive_after = "2m"
```
Återställ till produktionsvärden (t.ex. `failed_revive_after = "720h"`)
när allt gått igenom grönt.

## Fas 8 — Dashboard-verifikation

Webb-gränsnittet är nu serverat från samma `observ`-server som `/status` och `/metrics`.

- [ ] Öppna `http://<observ.listen_addr>/` i en webbläsare (t.ex. `http://192.168.86.33:9090/`). Webbläsarens HTTP Basic-dialog accepterar valfritt användarnamn och `observ.auth_token` som lösenord. Använd TLS via reverse proxy utanför ett betrott privat nät. Reverse proxyn måste bevara den ursprungliga `Host`-headern, ta bort eventuell klientskickad `X-Forwarded-Proto` och sätta exakt ett betrott värde (`http` eller `https`); skicka aldrig vidare en lista eller en opålitlig header.
- [ ] Sidan laddar med **mörktemat** och visar:
  - [ ] Sidofält med nav-knapparna `Översikt` och `Kö`.
  - [ ] **Översikt-vyn:** stat-kort för Köad, Aktiv, Stannad, Klar (även om de visar 0).
  - [ ] **Kö-vyn:** tabell över jobb (tom om ingen är aktiv just nu).
- [ ] Starta en eller flera nedladdningar (eller vänta tills existerande jobb dyker upp).
- [ ] Kö-tabellen visar jobben med status, album-namn, artist, och antal försökta kandidater.
- [ ] **Expandera en rad:** klicka på en jobbrad i tabellen → den expanderar och visar detaljinfo (peer, bytes, etc.).
- [ ] **Avbryt-åtgärd:** expanderad rad visar en `Avbryt`-knapp. Klicka på den → efter nästa status-poll (~3s senare) uppdateras radens status till avbruten.
  - [ ] Ingen särskild logg-rad förväntas vid en lyckad avbrytning — endast om det underliggande slskd-anropet misslyckas loggas en varning.

## Om något fastnar
Kolla i ordning: `docker logs slskdarr` (JSON, sök `err`) → autentiserat anrop till `$STATUS/status` →
`album_jobs.state` i DB → slskd:s eget transfer-UI → Lidarrs egen aktivitetslogg.
Tillstånd lever i DB:n, så du kan alltid se exakt var ett jobb står.
