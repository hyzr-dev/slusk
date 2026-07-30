# SSE-ström för live-data — design

Issue: #161. UI-polish (mjuka övergångar, stabil radordning) bröts ut och
ligger utanför den här designen; se "Utanför scope".

## Problem

Dashboarden uppdateras ryckigt. Symptomen är tre, och de har tre olika
orsaker — bara en av dem är transport:

1. **Värden rör sig i ryck.** Progressbaren står still och skuttar sedan.
2. **Det känns gammalt.** Det tar sekunder innan något som händer syns.
3. **Rader byter plats.** TRANSFERS-listan omordnas mellan uppdateringar.

### Rotorsak till (1)

`toJobDTO` (`internal/observ/observ.go:132`) bygger ett jobbs DTO ur två
olika källor:

```go
BytesDone:  v.AlbumBytesDone,   // Postgres, via core.JobView
BytesTotal: v.AlbumBytesTotal,  // Postgres, via core.JobView
...
d.Speed = speed                 // backendens minne, via aggregateLiveAlbum
```

`Speed` och `QueuePosition` läses ur backendens in-memory-transfers och är
färska i samma ögonblick som requesten. `BytesDone`/`BytesTotal` läses ur
`album_jobs`/`transfers`, och de raderna skrivs bara av Downloading-modulens
reconcile-loop — var `downloading_interval`, **default 15 sekunder**.

UI:t pollar alltså var tredje sekund efter ett tal som ändras var femtonde.
Fyra av fem pollar returnerar identiska bytes, den femte hoppar, och
hastigheten bredvid tickar hela tiden. Det är rycket.

`jobdetail.go` har samma delning per fil, och där är felet synligt i klartext:

```go
if lt, ok := live.match(tr); ok {
    t.QueuePosition = lt.QueuePosition
    t.Speed = lt.Speed
}
```

`lt.BytesDone` och `lt.Size` ligger i samma redan matchade struct och används
inte.

Att bytes skrivs var 15:e sekund är inte ett fel i sig: `transfers` är
pipelinens återstartsjournal, inte en presentationskälla. Felet är att
presentationslagret läser journalen i stället för det levande tillståndet.

## Avgränsning: DB-data mot minnesdata

Regeln för vad som strömmas:

**Strömmen bär bara in-memory-data. Allt Postgres-backat pollas vidare
via REST precis som idag.**

| Data | Källa | Transport |
|---|---|---|
| Bytes, hastighet, köposition, throughput | backendens minne | SSE, ~1 s |
| Jobbstatus, tillstånd, händelser, peers, shares | Postgres | REST-poll, 10–15 s |
| Bytes för avslutade transfers | Postgres | följer REST-snapshoten |

Motiveringen är att DB-datan bara ändras när en pipelinemodul tickar
(`discovery_interval` 30 s, `selecting_interval` 10 s, `importing_interval`
30 s). Att polla den var tredje sekund är redan översampling; att pusha den
vore slöseri. Live-datan ändras däremot kontinuerligt.

Gränsen är dessutom lätt att hålla: ett nytt fält hör hemma i strömmen om och
endast om det kan besvaras utan att röra databasen.

## Del 1 — live-fält i REST-snapshoten

`toJobDTO` och `jobdetail.go` ska läsa `lt.BytesDone`/`lt.Size` när en
live-match finns, med det persisterade värdet som fallback för transfers som
inte längre är live (färdiga, avbrutna, felade — de försvinner ur backendens
`ListDownloads` och då är den sista persisterade siffran det enda som finns).

Det här hör hemma i samma PR som strömmen, inte i en egen: REST-snapshoten är
golvet som strömmens uppdateringar lägger sig på. Om snapshoten svarar med
DB-bytes medan strömmen skickar live-bytes hoppar siffran **bakåt** vid varje
sidladdning och varje återanslutning.

Båda backendarna fyller i live-bytes (`internal/slskd/client.go:138` för
slskd-adaptern, native-klienten likaså), så fixen gäller båda. Enbart `Speed`
och `QueuePosition` är native-only, vilket redan är dokumenterat på
`core.RemoteTransfer`.

## Del 2 — endpointen

```
GET /api/stream          → aggregat per jobb, throughput, total down
GET /api/stream?job=42   → samma, plus per fil för jobb 42
```

`Content-Type: text/event-stream`, `Cache-Control: no-store`,
`X-Accel-Buffering: no` (utan den buffrar en reverse proxy strömmen till
oanvändbarhet). Handlern flushar efter varje meddelande via `http.Flusher`.

Meddelandena är namngivna SSE-events på samma anslutning — `event: live` på
varje anslutning, plus `event: throughput` (issue #265) för en prenumerant
som ber om den via `?throughput=1` — så att #129:s sökresultatström kan bli
`event: search` på samma endpoint utan att röra något här. Det är #161:s krav
på ett öppet händelseschema.

### Nyttolast

**UPPDATERAT (#265):** throughput-serierna (`throughput`/`uploadThroughput`)
flyttades ut ur `event: live`-kroppen (`livePayload`) till en egen
`throughputPayload` på `event: throughput`, fältnamn `download`/`upload`.
Skälet: en prenumerant utan sparkline på skärmen (Jobs, JobDetail, Settings,
...) ska aldrig behöva bygga eller ta emot serien alls. `down`/`up` (globala
skalärer) ligger kvar på `event: live` oavsett scope. Se
`internal/observ/stream.go`s paketkommentar för den aktuella kontraktet;
nedanstående beskriver fortfarande formen som gällde före #265:

Bara live-fält:

- **per jobb**: `id`, `bytesDone`, `bytesTotal`, `speed`, `queuePosition`,
  `etaSeconds`
- **per fil** (endast med `?job=`): `filename`, `state`, `bytesDone`,
  `bytesTotal`, `speed`, `queuePosition`. "Vilken fil som processas just nu"
  faller ut som den med `state: in_progress` — inget eget fält behövs.
- **throughput**: de sampel som är nyare än förra ticket (numera på
  `event: throughput`, inte `event: live` — se ovan)
- **down**: summan av hastigheterna, så headern slipper räkna ur hela
  jobblistan

Ingen `status`, inget `state`, ingen händelselogg, inga peers.

### Serverloopen

En delad goroutine tickar på throughput-samplingens befintliga intervall,
läser `deps.LiveTransfers` och throughput-funktionen — **inga DB-queries** —
och sänder resultatet. Loopen startar med första prenumeranten och stoppar med
den sista.

Varje tick sänder **hela den aktuella live-mängden**, inte deltan mot förra
ticket. Mängden är naturligt liten (den begränsas av antalet samtidiga
nedladdningar), och alternativet kräver tombstones: när en transfer blir klar
försvinner den ur backendens `ListDownloads`, och klienten måste sluta visa
dess hastighet. Med hela mängden betyder frånvaro helt enkelt borttagen, och
klienten behöver ingen egen bokföring. Undantaget är throughput, som är en
växande tidsserie — där skickas bara sampel nyare än förra ticket.

Är mängden oförändrad sedan förra ticket skickas inget alls, så en idle
installation ger bara heartbeats.

Inga nya portar behövs: båda funktionerna finns redan i `ServerDeps` och
används av `/api/jobs` respektive `/api/charts`.

**Ingen ny konfignyckel.** Tickern återanvänder throughput-intervallet. Det är
medvetet: `internal/config` avvisar okända nycklar, och en ny obligatorisk
nyckel måste finnas i produktionens `config.toml` innan PR:en mergas, annars
startar inte containern vid nästa deploy.

### Prenumeranter

Registret ligger bakom en mutex. Varje klient får en kanal med **kapacitet 1
som håller senaste nyttolasten** — är den full ersätts innehållet i stället
för att blockera. En långsam klient kan därmed varken hålla upp broadcastern
eller få en kö av inaktuella tillstånd serverad; den hoppar över mellanlägena.
Det är säkert just för att nyttolasten är "så här ser det ut nu", inte "det
här hände".

Serverns nedstängningskontext stänger alla öppna strömmar.

### Heartbeat

En kommentarsrad var 15:e sekund. Utan den flödar ingenting när inga
nedladdningar pågår, och en död anslutning ser identisk ut med en tyst.
Den hindrar samtidigt proxies från att döda idle-anslutningar.

### Autentisering

Ärvs från `ProtectPrivateEndpoints`, som wrappar hela muxen. `EventSource`
skickar webbläsarens ambienta Basic-credentials på en GET, och GET omfattas
inte av same-origin-kravet (det gäller POST och DELETE). Ingen ny
säkerhetsyta.

### Reconnect

**Inget `Last-Event-ID`.** #161 föreslog replay, men det kräver en
ringbuffert, monotona event-id:n och glappdetektering — och betalar sig bara
om en återupptagen ström är dyrare än att ta om snapshoten. Det är den inte:
REST är sanningskällan, och en re-snapshot kostar lika mycket som en enda av
dagens pollar. En ringbuffert kan dessutom ge *fel* tillstånd om ett
mellanläge tappas, vilket är den enda vägen till en klient som tyst visar
gammal data.

`EventSource` återansluter av sig själv, och servern styr fördröjningen med
`retry:`. Klienten invaliderar REST-queryna i `onopen` — då tar varje
återanslutning en färsk snapshot, precis som en sidladdning. Ingen egen
reconnect-logik.

### Fallback

Ingen egen kodväg. Eftersom REST-snapshoten efter del 1 bär samma live-fält
som strömmen är strömmen aldrig en egen datakälla, bara en snabbare leverans
av något REST redan kan svara på. Dör strömmen slutar klienten skriva till
cachen och pollintervallen går tillbaka till dagens värden. Degraderingen blir
exakt dagens UI: inga tomma fält, ingen specialrendering, bara lägre kadens.

## Del 3 — frontend

En `EventSource` för hela appen, ägd av en provider i `web/src/api/`. På en
jobbroute öppnas den som `?job=<id>`, så JobDetail får per fil-data på samma
anslutning som headern får sin `down` — en anslutning per vy, inte per panel.
Byte av visat jobb återöppnar anslutningen.

Inkommande data skrivs in i TanStack Querys cache med `setQueryData`, där
live-fälten mergas in i de cachade jobben.

Alternativet — en `useLive()`-hook som varje komponent läser vid sidan av
`useJobs()` — tvingar fram ändringar i `Overview`, `TopBar`, `JobDetail` och
`JobExpansion`, och varje komponent måste själv veta hur två källor slås ihop
och vilken som vinner. Med cache-skrivning fortsätter alla fyra läsa
`useJobs()` oförändrat och vet ingenting om att datan kommer via en ström.
Strömmen blir en transportdetalj i `api/`-lagret, inte ett nytt begrepp i
vylagret. Det är också varför fallback blir gratis.

### Pollintervall efteråt

| Query | Före | Efter | Motivering |
|---|---|---|---|
| `jobs` | 3 s | 15 s | bär nu bara DB-data |
| `charts` | 15 s | 15 s | snapshot-golv för throughput |
| `status` | 5 s | 5 s | billiga räknare, oförändrat |
| `events`, `peers`, `uploads`, `shares` | — | oförändrat | rörs inte |

## Test

- Diff-logiken i broadcastern skrivs som en ren funktion → tabelltester.
- Handlern testas med `httptest` mot en flush-kapabel recorder: headers,
  första nyttolasten, heartbeat, att två klienter delar en loop, att sista
  frånkopplingen stoppar tickern, och att nyttolasten **inte** innehåller
  DB-fält.
- `toJobDTO`/`jobdetail.go`: live-match vinner över persisterat värde;
  utan live-match används det persisterade.
- Frontend: `EventSource` finns inte i jsdom och behöver en mock. Tester för
  cache-mergen, intervallväxlingen vid strömdöd och invalidering vid `onopen`.
- `go test ./... -race` krävs — endpointen är genuint samtidig.
- Verifiering i `testenv/`-labbet före merge, eftersom merge till `main` är
  en produktionsdeploy.

## Regel för framtida händelsetyper: värden mot hintar

Avgränsningen "in-memory strömmas, Postgres pollas" är en proxy, inte
kriteriet. Det verkliga kriteriet är **hur ofta datan ändras jämfört med
pollintervallet**. Proxyn håller för all data vi har idag, eftersom allt
Postgres-backat skrivs av en pipelinetick — men den går sönder så fort något
persisteras av en inkommande nätverkshändelse i stället.

Privata meddelanden (#183) är det första sådana fallet: de skrivs till
Postgres vid godtycklig tidpunkt, inte på en tick. De hör alltså hemma på
strömmen trots att de är DB-backade.

De kan däremot **inte** skickas som nyttolast. Prenumerantkanalen har
kapacitet 1 och ersätter innehållet när den är full — säkert för `live`, som
är ett tillståndsavtryck där ett överhoppat mellanläge korrigeras av nästa
tick. Ett meddelande är motsatsen: en diskret händelse, som samma mekanism
skulle tappa tyst hos varje klient som halkar efter. Det är exakt den felmod
#183 beskriver i protokollet redan (offline-PM som aldrig ackas och tappas
tyst vid varje inloggning).

Därför gäller två former på samma endpoint:

| Form | Innehåll | När | Varför säker |
|---|---|---|---|
| **Värde** | fältet självt (`live`) | in-memory, tickbunden | tillståndsavtryck — nästa tick korrigerar ett tapp |
| **Hint** | bara "något ändrades" (`chat`) | DB-backad, godtycklig tidpunkt | idempotent — två hintar som slås ihop är harmlöst |

En hint får klienten att invalidera motsvarande REST-query. Meddelandetexter
går aldrig genom strömmen och kan därför aldrig tappas där. REST förblir
sanningskällan i båda formerna.

Chatthändelsen byggs i #183, inte här.

## Utanför scope

Radordningen i TRANSFERS — **#233**. Orsaken visade sig inte vara avsaknad av
stabil sortering utan en aktivt destabiliserande: `dashboard.go:160` sorterar
på `updated_at DESC`, så ett jobb hoppar till toppen varje gång det skrivs,
och `.slice(0, 8)` kan trycka ut en rad på grund av ett annat jobbs skrivning.

Mjuka övergångar på progressbaren utgår helt. `Ticks` renderar 104 diskreta
streck, inte en `width`-baserad bar, så det finns ingen bredd att
transitionera. Vid 1 s-kadens rör sig baren ett streck (~1 %) i taget.

Sökresultatströmmen (#129) — senare händelsetyp på samma endpoint.

## Avvikelser från #161

1. **`Last-Event-ID` utgår** till förmån för re-snapshot vid återanslutning.
   Motivering ovan under Reconnect.
2. **Live-bytes i REST-snapshoten ingår**, trots att det låter som en separat
   bugfix. De måste dela källa med strömmen, annars hoppar siffran bakåt vid
   varje sidladdning.
3. **Räknarna (`/status`) strömmas inte.** De är DB-backade och faller under
   avgränsningen ovan.

Hela den här designen är postad som kommentar på #161, så att den går att
implementera utan tillgång till den här filen.
