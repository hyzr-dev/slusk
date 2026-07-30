# Overview: en nulägesbild av jobb

## Bakgrund

Overview visar idag TRANSFERS: en panel med `filter=transferring` (aktiva +
stalled, union), 8 rader, `sort=transfer` (statusgrupp → `created_at ASC`).
Ett jobb som blir klart lämnar unionen och försvinner spårlöst från panelen —
det finns ingen yta som svarar "vad hände precis" när ett jobb avslutas,
lyckat eller inte.

Målet: Overview ska vara en nulägesbild — "vad händer nu, vad hände precis
nyss" — inte bara "vad laddar ner just nu".

## Vad Overview ska visa

Två regioner, samma vänsterkolumn, TRANSFERS överst och en ny sektion under:

**Region 1 — TRANSFERS (befintlig panel, nytt urval).**
`state IN (DOWNLOADING, IMPORTING)` — exakt den mängd `MaxActive` begränsar.
Ersätter dagens `transferring`-filter (används bara av Overview, så det byts
ut snarare än kompletteras). Innehåller nu även:
- jobb som laddat ner något och väntar på mer (DOWNLOADING, inget i progress)
- jobb som laddat ner allt och väntar på import (DOWNLOADING, `live = 0`)
- jobb som importerar (IMPORTING)

SELECTING är medvetet exkluderat — det är pipelinens väntrum utan
`MaxActive`-tak, och skulle kunna svämma över panelen med kö istället för
arbete. Stat-radens `queued`-räknare täcker redan "hur mycket väntar".

**Region 2 — SENAST AVSLUTADE (ny sektion).**
`state IN (DONE, FAILED) AND updated_at > now() - 1h`. Fönstret är en
konstant i backend (inte config — se Beslut som avvägts), 1 timme.

Region 1 och 2 är disjunkta per konstruktion: de partitionerar `j.state`,
inte `dashboardJobStatusSQL`s härledda status (se "Varför state, inte
status" nedan).

## Backend-kontrakt

Två nya filter i `DashboardJobsQuery.Filter`, ersätter `transferring`:

| Filter | WHERE |
|---|---|
| `inflight` | `j.state IN ('DOWNLOADING', 'IMPORTING')` |
| `finished` | `j.state IN ('DONE', 'FAILED') AND j.updated_at > $now - 1h` |

`$now` trådas in explicit (som `time.Time`-parameter på anropskedjan, samma
mönster som `MarkJobFailed(ctx, id, now)` m.fl.) — aldrig `now()` i SQL, för
att hålla tester oberoende av väggklockan.

**`sort=transfer` får en finare rankning** (idag: `active=1, stalled=2,
ELSE 3`, vilket skulle kollapsa IMPORTING och väntande DOWNLOADING-jobb till
samma grupp):

```
1  active      agg.in_progress > 0
2  stalled     agg.stalled > 0
3  queued      agg.live > 0, inget i progress/stalled  (täcker "väntar på mer"
                                                          och "väntar på import")
4  importing   j.state = 'IMPORTING'
```

Punkt 2 och 3 (väntar på mer / väntar på import) delar sorteringsgrupp —
avsiktligt, se Beslut som avvägts. Sen `created_at ASC, j.id ASC` inom
gruppen, som idag.

**`sort=recent` är ny:** `ORDER BY j.updated_at DESC, j.id DESC`. `j.id` som
tiebreaker av samma skäl som `sort=transfer`: utan en är Postgres ordning
för lika `updated_at` odefinierad. Validering avvisar `dir=asc` för detta
sort, hårdkodad riktning, samma mönster som `sort=transfer`.

**`DashboardJobsQuery` får ett `SkipFacets bool`-fält.** Se "Prestandafynd
under design" — region 2 sätter det för att slippa den dyra facett- och
count-beräkningen den inte läser. `Total` och `Facets` blir nollvärden när
satt; anropare som läser dem (Jobs-sidan) sätter aldrig fältet, så
`ListDashboardJobs` beter sig identiskt för alla befintliga anrop.

## Frontend

**Region 1** byter bara query-parametrar (`filter: 'inflight'`). Befintlig
radrendering hanterar redan de nya medlemmarna: IMPORTING-rader visar
`t.jobs.verifying` (Overview.tsx:139-140), väntande DOWNLOADING-rader får
`formatSpeed(0) → '—'` och ingen tick-flare (`live` kräver
`status === 'active'`, Overview.tsx:136).

Sektionsrubrikens meta byter från `status.active` till `result.total` —
den bredare mängden gör att `active`-räknaren annars systematiskt
underskattar vad panelen visar.

**Region 2** är en ny `useJobs`-query: `filter: 'finished', sort: 'recent',
pageSize: 5, skipFacets: true`. Egen `queryPhase`/`QueryNotice`-gate (samma
mönster som #201: en död poll för denna region blankar aldrig TRANSFERS).

Radanatomi, ingen tick-bar (allt är 100 % eller dött):

```
[OK]  Album Title                          12m
      Artist · peer

[FA]  Another Album                        41m
      Artist · —
```

`Tag` täcker redan OK/FA. Ålder är `formatAge` på `updatedAt` (redan
existerande formatter). Ingen `failReason` på raden — den finns i
jobbdetaljens accordion (klick navigerar till `/jobs/:id`, samma som
region 1); Overview förblir en yta man klickar sig vidare från, inte en
detaljvy.

**Tom-vy-copy är fönster-agnostisk**: "Inget avslutat nyligen" snarare än
"...senaste timmen". Fönstrets längd är en Go-konstant, copyn är en sträng
i `strings.ts` — inget test i någon svit kan upptäcka att de sagt emot
varandra om fönstret ändras. En agnostisk formulering kan inte hamna i
otakt.

**SSE-scope oförändrat i omfång**: `useJobScope` binder bara till region 1:s
id:n, som idag. Region 2:s jobb är terminala (DONE/FAILED producerar inga
deltan) — att ta in dem i scopet skulle kosta bokföring för uppdateringar
som aldrig kommer. Nya klara jobb kommer in via `finished`-pollen.

## Varför state, inte status

Både regionerna filtrerar på `j.state`, inte på `dashboardJobStatusSQL`s
härledda `status`. Skälet: `status = 'failed'` kan gälla ett jobb vars
`state` fortfarande är DOWNLOADING (alla dess transfers errorat, väntar på
retry — se `dashboard.go:142`, `WHEN agg.live = 0 AND agg.failed > 0 THEN
'failed'`). Hade regionerna filtrerat på `status` skulle ett sådant jobb
synas i **både** TRANSFERS och SENAST AVSLUTADE samtidigt — samma album,
två rader, olika berättelser.

`j.state` har ingen sådan tvetydighet: ett jobb har exakt ett `state`, så
`DOWNLOADING/IMPORTING` och `DONE/FAILED` är strukturellt disjunkta — inte
en konvention någon måste minnas, utan en egenskap hos datamodellen.

`dashboardJobStatusSQL` (single source of truth för statusfacetter/tagg/
sortering sedan #269) rörs inte av den här ändringen.

## Prestandafynd under design

Under brainstormingen mättes `ListDashboardJobs`s facettfråga (den som körs
vid varje `/api/jobs`-anrop oavsett filter, se `dashboard.go:428`) mot prod
(5183 `album_jobs`, 15716 `candidates`, 74174 `transfers`):
`EXPLAIN (ANALYZE, BUFFERS)`, varm cache → **~85 ms Execution Time**.

Det avgjorde två saker för den här designen:

1. **Inget nytt index behövs för `finished`-filtret.** Sidfrågan
   (`state IN ('DONE','FAILED') AND updated_at > ...`) tar ~1,5 ms mot
   samma tabell utan index — 1 % av facettfrågans kostnad. En migration för
   den vinsten vore inte försvarbar, och en migration är oåterkallelig när
   den väl mergats.
2. **Region 2 får inte vara en andra fullständig `useJobs`-query.** Att
   dubbla 85 ms per 15-sekunderspoll för fem rader som varken läser
   `total` eller facetter är inte rimligt. Därav `SkipFacets`.

Mätningen visade också två obesläktade prestandaproblem i samma
facettfråga (kandidatvalets `SubPlan` körs 6625× för 3568 rader; en JIT-
kompilering som kostar mer än den sparar) — de ligger utanför den här
designens omfång och är rapporterade separat: **#286** (ny issue) och en
kommentar på **#176** (befintlig issue om LATERAL-aggregatets saknade
index, nu med prod-mätning som stärker dess redan föreslagna fix).

`album_jobs` har ingen retention (jämför `job_events`: 30 dagar +
`PruneJobEvents`). 3504 av 5183 rader i mätningen var redan DONE/FAILED.
Ingen egen issue för det ännu — noterat i #286 så det inte glöms.

## Beslut som avvägts

- **Två regioner, inte en gemensam lista.** "Vad händer nu" och "vad hände
  nyss" har olika livslängd (en aktiv rad ligger kvar tills den ändras, en
  klar rad faller ur efter ett fönster) — att blanda dem tvingar in båda i
  samma sorteringsregel.
- **Tidsfönster (B), inte fast antal (A).** Fast antal skulle kunna visa en
  rad från igår som om den vore färsk. Konsekvensen accepteras: regionen är
  tom stora delar av dygnet när inget avslutats, vilket redan är avläsbart
  från stat-raden.
- **1 timme, inte konfigurerbart.** En config-nyckel kräver att produktionens
  `config.toml` uppdateras innan PR:en mergas (`internal/config` avvisar
  okända nycklar, ingen tyst default). Att betala den risken för ett tal vi
  inte vet är fel än är inte värt det — en konstant kan ändras i en
  uppföljande PR.
- **DONE + FAILED, inte bara DONE.** Ett jobb som just misslyckades är minst
  lika akut information som ett som just lyckades, och var annars lika
  osynligt som ett lyckat jobb. PARKED exkluderas: parkering kan ha skett
  för dagar sedan även om `updated_at` ser färsk ut — det är inte en
  händelse som "precis hände" på samma sätt.
- **SELECTING exkluderat ur region 1.** Se "Vad Overview ska visa" ovan.
- **Ingen `failReason` i Overview.** Jobbdetaljens accordion existerar
  redan (`JobExpansion.tsx`) med egen `useJobDetail`-query, `JobActions`,
  fillistor. Att duplicera det på Overview drar mot #274 (som nyss gick i
  motsatt riktning: sluta detalj-polla när strömmen bär det).
- **Region 2 i vänsterkolumnen under TRANSFERS, inte i högerkolumnen och
  inte istället för RECONCILE.** Samma radbredd/-anatomi som TRANSFERS
  (albumtitel, artist, tid) — högerkolumnen är byggd för kompakt innehåll
  (11px-rader) och skulle trunkera långa titlar. RECONCILE är den enda ytan
  som visar att Discovery faktiskt söker; att ta bort den vore ett separat,
  medvetet beslut.
- **"Väntar på mer" och "väntar på import" delar sorteringsgrupp** (grupp 3
  i `sort=transfer`). De är särskiljbara i UI:t via `bytesRemaining = 0`,
  men att splittra dem i backend kräver en femte gren i
  `dashboardJobStatusSQL` — en enda källa till sanning för hela dashboarden.
  Släpps som det är för nu; utvärderas senare om det visar sig behövas.

## Vad som inte görs

| Inte detta | Varför |
|---|---|
| Delar upp `queued` i `dashboardJobStatusSQL` | Urvalet läggs på `j.state` istället — CASE:en förblir orörd. |
| Ny `done_at`-kolumn | `updated_at` är verifierat stabilt för DONE/FAILED (`MarkJobFailed` guardad mot om-failning, metadata-backfill rör den inte). |
| Config-nyckel för fönstret | Se Beslut som avvägts. |
| Index för `finished`-filtret | Se Prestandafynd: 1 % av den dominerande kostnaden. |
| Fixar facettfrågans `SubPlan`/JIT-kostnad i den här PR:en | Rör `currentCandidateSubquery`/`jobViewFrom`, sanningskällan för hela dashboarden — egen risk, egen PR. Se #286, kommentar på #176. |
| `album_jobs`-retention | Egen fråga, noterad i #286 för att inte glömmas. |
| `failReason` eller accordion i Overview | Se Beslut som avvägts. |

## Testplan

**Go** (`internal/store/dashboard_test.go`):
- `inflight`-medlemskap över aggregatmatrisen: DOWNLOADING med
  `in_progress > 0`, med bara STALLED, med allt PENDING, med allt
  COMPLETED; samt IMPORTING.
- `finished`-medlemskap och fönstergränsen explicit: ett jobb strax
  innanför 1h är med, strax utanför är det inte.
- **Disjunkthetstest**: ett DOWNLOADING-jobb vars transfers alla errorat
  (status `failed` via aggregatet) finns i `inflight`, aldrig i `finished`.
- `sort=recent`: ordning, `j.id`-tiebreaker vid identisk `updated_at`,
  `dir=asc` avvisas.
- `sort=transfer`s nya fyrgruppsrankning.
- `SkipFacets`: `Total`/`Facets` är nollvärden när satt; övriga anrop
  opåverkade.
- Facetter oförändrade av fönstret (`facets.status.done` räknar allt).

**Frontend** (`Overview.test.tsx`): region 2 renderar rader, tom-vy syns,
en död `finished`-poll blankar inte TRANSFERS, SSE-scope innehåller bara
region 1:s id:n.

**Browser (obligatoriskt, inte valfritt).** `web/`-sviten kör i jsdom, som
varken beräknar layout eller ritar — en ny sektion i vänsterkolumnen kan
aldrig fällas av ett test. Verifiera i riktig browser: `mainGrid`s
`1.6fr 1fr`-balans med en längre vänsterkolumn, samt brytpunkterna 1000px
och 640px (`Overview.module.css:134-147`) där gridden kollapsar till en
kolumn.

**Deploy.** Ingen ny config-nyckel, ingen migration. Verifiering i
`testenv/`-labbet före merge, enligt sedvanlig rutin — `feat:`-prefix
deployar direkt vid merge till `main`.
