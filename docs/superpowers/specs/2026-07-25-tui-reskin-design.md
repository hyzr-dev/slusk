# TUI-omstilning av SPA:n — design

Issue: #198. Tangentbordslagret bröts ut till #199.
Visuell spec: `docs/design/slskdarr-tui.dc.html` (handoff-kopia av
`Slskdarr TUI.dc.html` ur designprojektet `9085f510-06a3-4d25-b01d-f992601dd938`,
hämtad 2026-07-25).

## Problem

Designprojektet har fått en ny mockup som ersätter `Slskdarr Dashboard.dc.html`.
Datamodellen är oförändrad; det är uttrycket som bytts. Den gamla mocken var ett
mörkt dashboard med kort, rundade hörn, sans-serif brödtext och en grön accent per
statuskategori. Den nya är en terminalyta: monospace genomgående, inga radier, en
fast `100vh`-ram med header, grupperad sidomeny och statusrad, och en palett där bara
OK och fel har egen kulör.

Det är alltså inte en tema-ändring utan ett byte av visuellt idiom, och det rör varje
vy och varje CSS-modul i `web/src`.

## Beslut om omfattning

Fattade tillsammans med Samuel 2026-07-25:

1. **En gren, ett PR.** Alternativet var grund-först följt av en issue per vy.
   Sammanhängande visuellt resultat vann över mindre deploysteg.
2. **Config förblir skrivbar.** Mocken ritar den read-only. #134 sköt skrivbar
   settings till prod; att följa mocken bokstavligt vore en regression.
3. **Search och Chat blir platshållarvyer.** Ingen av dem har backend. Att ta med
   dem i navet från dag ett håller mockens vyantal och gör att bindningarna i #199
   inte behöver flyttas när backend landar.
4. **Events och Peers behålls** som egna vyer trots att mocken tagit bort dem.
   Backend fungerar, och peer-data (score, per-artist-statistik) syns ingen
   annanstans.
5. **Inga backend-ändringar.** Datagap i mocken döljs hellre än fylls. Varje gap blir
   en egen issue.
6. **Tangentbordslagret bryts ut** till #199, och med det tangentglyferna i UI:t.

## Arkitektur

### Tokenlager

`styles/tokens.css` skrivs om. De sex statusfärgerna (`--active`, `--queued`,
`--stalled`, `--orphaned`, `--failed`, `--done`) och `--manual` pensioneras — mocken
renderar `queued`, `active` och `importing` alla i `--fg`/`--dim` och har bara två
kulörer, OK och fel. Det betyder att samtliga 18 CSS-moduler rörs; det är oundvikligt
i en total omstilning men bör inte komma som en överraskning i granskningen.

Tokens, hämtade ur mockens `rootStyle` (rad 987–991) plus de råa hex-värden mocken
använder utan variabel — de får namn i stället för att strös ut över modulerna:

```
--bg #08090a      --fg #e8eaec       --dim #7d868c      --faint #78828a
--line #191d20    --line2 #252b30    --text-dim #5f696f
--ok #4f9e80      --bad #b5595c      --bar #c8ccd0
--panel-hover #0e1012   --panel-inset #0b0c0e   --nav-active #111417
--btn #14181b     --tick-off #1d2125      --tick-queued #2a2f34
```

`--font-sans`, `--radius` och `--radius-sm` försvinner. IBM Plex Mono blir enda
typsnittet, och Plex Sans plockas ur `web/index.html`. `font-feature-settings: 'tnum'`
och `font-variant-numeric: tabular-nums` behålls — mocken sätter båda, och siffror i
kolumner måste stå still.

### Global chrome

`Layout.tsx` (idag 44 rader, enbart en sidomeny) blir ramen och delegerar:

- `components/chrome/TopBar.tsx` — märke och version, LIVE-puls med tiden sedan
  senaste lyckade hämtning, `RECONCILE T–Ns`, DOWN/UP-hastighet, beroendekvadrater
  för Lidarr / Soulseek / Shares.
- `components/chrome/SideNav.tsx` — grupperad navigation (MONITOR / SOULSEEK /
  SYSTEM) med badge-räknare per post och varningsfärg när något kräver
  uppmärksamhet.
- `components/chrome/StatusBar.tsx` — flash-meddelande och klocka. Tangenttipsen
  hör till #199.

Ramen är `100vh` flexkolumn: header, sedan en rad med nav och rullande `main`, sedan
statusraden. Body scrollar aldrig.

Sidomenyns fot i mocken (`uptime`, `mem`, `peers`) har ingen datakälla och utelämnas.

### Flash

`FlashContext` med en `flash(message)`-funktion; `StatusBar` renderar och rensar efter
~3 s. Mutationerna retry, cancel, delete och rescan triggar den. Kontexten finns i det
här PR:et eftersom knapparna behöver den — #199 återanvänder den för tangentvägen.

### Delade primitiver

Nya, under `components/tui/`:

- **`Ticks`** — tickfältet, ersätter `ProgressBar`. Props: fyllnadsgrad, antal ticks,
  färg, och om huvudticken ska blinka. Mockens matte ligger i `ticks()` rad 820–833:
  ticks under `floor(filled)` är tända, den precis över är halvtransparent, och den
  sista tända får en `tui-flare`-animation.
- **`Tag`** — tvåbokstavstaggen (DL/QU/ST/OR/FA/OK/IM), ersätter `StatusPill`.
  Mockens mappning ligger i `meta()` rad 854–858. Till skillnad från `StatusPill`
  tar den även kötillståndet, som härleds ur `queuePosition` och inte finns i
  `JobStatus` — det var precis den läckan #190 beskrev.
- **`SectionHeader`** — `hdr`-listen: versal etikett vänster, meta höger.
- **`Button`** — primary / ghost / danger.
- **`Chip`** — filter- och sorteringschips med räknare.
- **`EmptyState`** — `── NO MATCHING JOBS ──`-formen.

`ProgressBar` och `StatusPill` tas bort när alla anrop är flyttade.

### Prestanda

26 ticks per jobbrad × ~150 rader är omkring 3900 DOM-noder som annars renderas om vid
varje 3-sekunderspoll. `Ticks` memoiseras på **antal fyllda ticks som heltal**, inte på
procent, så den bara renderar om när stapeln faktiskt ändras — ett jobb som kryper från
41,2 % till 41,4 % rör inget. Jobbraden memoiseras på samma sätt.

Detta är en hypotes, inte ett konstaterande. Den mäts i `./testenv/lab.sh` med 150
seedade album innan PR:et påstås vara klart.

## Vyerna

| Vy | Datakälla | Utelämnat ur mocken |
|---|---|---|
| Overview | `/status`, `/api/jobs`, `charts.throughput`, `charts.passes` | Sparklines i fyra av fem stat-celler, delta-siffrorna `+2`/`+5` |
| Jobs | `/api/jobs`; fillistan via `/api/jobs/{id}/detail` | — |
| Health | `status.moduleDetails`, `charts.passes`, `charts.completedByHour` | 30-minuters uppetidsstaplar per beroende |
| Shares | `/api/shares`, `/api/uploads` | Scanprocent och filräknare under rescan |
| Events | `/api/events` | Vyn finns inte i mocken; byggs i TUI-idiom |
| Peers | `/api/peers` | Vyn finns inte i mocken; byggs i TUI-idiom |
| Search | — | Hela vyn; platshållare som pekar på #58 |
| Chat | — | Hela vyn; platshållare som pekar på #183 |
| Setup | `/api/config`, `/api/config/test/*`, `/api/shares` | Shares-steget saknar testendpoint |
| Config | Befintlig `Settings.tsx` | Enbart ny stil |

### Overview

Fem stat-celler överst (aktiva, köade, stallade, föräldralösa, klara) och därunder ett
tvåkolumnsraster: TRANSFERS till vänster med en rad per aktiv, importerande eller
stallad nedladdning, och till höger THROUGHPUT-arean över `charts.throughput` plus
RECONCILE-listan över `charts.passes`.

Mockens stat-celler har sparklines, men bara ACTIVE har en verklig serie att rita
(throughput). De övriga fyra ritar `hist()` — påhittad data. Deras sparklines
utelämnas hellre än fylls med brus. Samma sak med delta-siffrorna.

### Jobs

Griden ST / ALBUM / PEER / FMT / PROGRESS / SPEED / ETA / TRY är fullt täckt av
`Job` (`format`, `queuePosition`, `speed`, `etaSeconds`, `retries` finns alla sedan
#98, #157 och #174). Filterchips med räknare, `/`-fältet som textfilter, och
expandera-på-plats.

Expansionen visar metaträdet (peer, källa, köposition, tid i tillstånd, kvalitet,
överfört, jobb-id) och en FILES-lista. Filerna kommer från `useJobDetail(id)` —
`AttemptDetail.transfers` bär filnamn och bytes — och hämtas först när raden
expanderas.

Befintlig `jobFilter.ts` behålls oförändrad.

### Health

Tre beroendekort ur `status.moduleDetails` med prick, tillstånd och detaljtext.
Mockens uppetidsstaplar bygger på 30 minuters historik per beroende som ingen lagrar;
de utelämnas.

RECONCILE RATE-staplarna kommer ur `charts.passes`, COMPLETED-arean ur
`charts.completedByHour`. Båda finns redan som komponenter i `components/charts/` och
behöver ny stil, inte ny matte.

METRICS-sektionen behålls med verkliga värden: `reconcile_total`, aktiva, föräldralösa
och stallade jobb ur `/status`, aktiva uppladdningar ur `/api/uploads`, delade filer ur
`/api/shares`.

### Shares

Mappraster med sökväg, filer, storlek och indexeringstid; RESCAN-knapp med befintlig
202/409-hantering. Under en pågående scan visar mocken procent, en tickstapel och
antal hashade filer — `SharesReport` bär bara `scanning: bool`, så indikatorn blir
spinner och ordet "indexing" utan siffra.

Uploads-panelen kommer ur `/api/uploads` och behåller `UploadsPanel.tsx`s beteende.

### Setup

Tre steg — Soulseek-inloggning, Lidarr-anslutning, delade mappar — med fältvärden ur
`/api/config` och TEST-knappar mot `/api/config/test/{lidarr,soulseek}`.

Shares-steget har ingen testendpoint; dess tillstånd härleds ur `/api/shares` (OK om
minst en mapp är indexerad med filer, annars otestat).

Mockens inledande text — "slskdarr validates the config file you already wrote — it
never writes it" — är falsk sedan #134 och skrivs om till att peka på Config-vyn.

### Config

`Settings.tsx` (1248 rader) rör bara stil. Sektionskort blir `SectionHeader`-lister,
fältrader blir etikett/värde-raster, inputs får `--panel-inset`, och danger zone
behåller sin tvåkliksbekräftelse. Ingen ändring i validering, sparlogik eller
omstartsflöde.

## Copy

All text går genom `strings.ts` (i18n-förberedelsen från #86). Mocken är på engelska
den här gången, till skillnad från den förra, men copyn ska ändå in i katalogen och
inte klistras in i komponenterna.

## Test

De 167 befintliga frontend-testerna frågar mestadels på text och roll och bör överleva
omstilningen. De som brister är signal om att beteende ändrats, inte brus som ska
tystas.

Nytt:

- `Ticks` — fyllnadsmatte vid 0 %, 100 %, delvis fylld tick, och att huvudticken
  bara markeras när fältet är live.
- `Tag` — mappningen från status plus kötillstånd.
- Platshållarvyerna renderar och pekar rätt.
- `StatusBar` visar och rensar flash.
- `SideNav` badge-räknare och varningsfärg.

## Risk

Merge till `main` är deploy. Inga nya confignycklar tillkommer, så den vanliga fällan —
att prods `config.toml` saknar en nyckel och containern inte startar — är inte i spel.

Den kvarvarande risken är rent visuell och funktionell regression i en stor diff.
Motmedlet är att köra `./testenv/lab.sh reset` mot grenen och gå igenom alla tio vyer
innan PR:et mergas.

## Berörda issues

- **#181** (förstasida och globalt ramverk enligt den gamla mocken) blir helt ersatt
  av det här arbetet och bör stängas när PR:et mergas.
- **#190** (StatusPill och ProgressBar räcker inte för jobbvyn) absorberas — båda
  komponenterna försvinner till förmån för `Tag` och `Ticks`, och `Tag` tar
  kötillståndet som en riktig prop i stället för att Jobs når förbi den publika ytan.
