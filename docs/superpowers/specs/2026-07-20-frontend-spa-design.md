# Frontend-SPA — fas 1: migrering till React

**Datum:** 2026-07-20
**Issue:** #87
**Status:** Godkänd design, redo för implementationsplan
**Omfattning:** Fas 1 — portering av befintlig dashboard till en React-SPA.
Fas 2 (nya vyer för #58–#61, #63) beskrivs som beroendekarta, inte som plan.

## Bakgrund

Dagens UI är `internal/observ/web/dashboard.html` (141 rader, inline `<style>`) och
`dashboard.js` (524 rader vanilla JS med `innerHTML`-rendering och hash-routing).
Det täcker fyra vyer: Översikt, Jobb, Händelser, Peers. Data hämtas klientsidigt
från REST-endpoints med `setInterval`-polling.

Ett färdigt designprojekt finns i Claude Design ("Slskdarr Dashboard Design",
`Slskdarr Dashboard.dc.html`). Det är en omdesign av samma funktionsyta med fyra
vyer — Översikt, Kö, Hälsa, Inställningar — men rikare: expanderbara jobbrader,
filterchips, sparklines, charts och toasts.

Fem öppna UI-issues (#58 manuell sökning, #59 manuell import, #60 enhetlig jobbvy,
#61 shares, #63 guidad setup) kräver vyer som varken finns i designen eller i
backend. UI:t kommer alltså växa väsentligt efter 1.0.

## Problem

`dashboard.js` har nått gränsen för vad ett enda `innerHTML`-renderat skript bär.
Varje ny vy lägger till hundratals rader i samma fil, och de nya vyerna är mer
tillståndsrika än de befintliga: strömmande sökresultat, wizard-steg med fyra
tillstånd per anslutningstest, formulär med validering.

Att bygga fas 2 på dagens grund innebär att skriva komponentlogik för hand i
strängkonkatenering. Migreringen måste ske före de nya vyerna, inte efter.

## Beslut

**React + TypeScript + Vite.** Vald över Preact och Svelte inte på tekniska
meriter — alla tre räcker — utan för att React är det konventionella valet som
ingen ny bidragsgivare behöver ifrågasätta, och kodbasen ska kunna överlämnas.
Bundle-storleken saknar betydelse för en självhostad app på LAN.

## Arkitektur

### Bygge och servering

Frontend-källkod bor i `web/` i repo-roten. Vite bygger till
`internal/observ/web/dist/`, som `go:embed` plockar upp — placeringen är ett tvång
från embed, som inte kan läsa filer utanför paketets katalog.

`dist/` är gitignorerat, med en incheckad placeholder-`index.html` som instruerar
att köra `make ui`. Det håller git-historiken fri från hashade bundles samtidigt
som `go build ./...` alltid kompilerar och ger ett begripligt fel om frontend inte
är byggd.

En `Makefile` binder ihop stegen:

| Mål | Gör |
| --- | --- |
| `make ui` | `npm ci && npm run build` i `web/` |
| `make build` | `make ui` följt av `go build ./cmd/slskdarr` |
| `make dev` | Vite dev-server med `/api`-proxy mot lokal Go-binär |
| `make test` | `go test ./...` och `npm test` i `web/` |

Dockerfilen blir tre steg: `node:22` bygger frontend, `golang:1.26` bygger binären
med `dist` embeddad, distroless kör den. Runtime-imagen är oförändrad — ingen Node
följer med.

### Servering

`web.go` ersätts. De två handlers som serverar en fil var byts mot en asset-handler
med tre regler, i denna ordning:

1. `/api/*`, `/metrics`, `/healthz`, `/readyz` matchas i muxen före SPA-fallbacken
   och når den aldrig. En felstavad API-sökväg måste ge 404, inte HTML.
2. `/assets/*` — Vites hashade filnamn — serveras med
   `Cache-Control: public, max-age=31536000, immutable`.
3. Allt annat returnerar `index.html` med `Cache-Control: no-cache`, så
   klientroutade sökvägar fungerar på direktlänk och omladdning.

`ProtectPrivateEndpoints` i `security.go` lämnas orörd, och kräver ingen ändring för
SSE. `TokenAuthenticator.Authenticate` provar HTTP Basic före Bearer, så webbläsaren
autentiserar sig med ambient credentials efter första 401-svaret. Ambient credentials
skickas med på alla same-origin-anrop inklusive `EventSource` — att `EventSource`
inte kan sätta `Authorization`-header saknar därför betydelse. Ingen
query-parameter-token behövs.

POST-mutationer kräver `Origin`-header när Basic används (`sameOriginMutation`);
webbläsare skickar den automatiskt på `fetch`. Verifieras med test.

### Applikationsstruktur

```
web/
  index.html
  package.json
  vite.config.ts
  src/
    main.tsx
    routes/          en fil per vy
    components/      återanvändbara: StatusPill, ProgressBar, DataTable, Toast…
    api/             typer, fetch-klient, query-hooks, SSE-prenumeration
    styles/          tokens.css, global.css
```

**Routing: React Router med riktiga sökvägar.** `/`, `/jobs`, `/jobs/:id`,
`/events`, `/peers`, `/health`, `/settings`.

`/events` och `/peers` finns inte i designen, som bara har fyra vyer. Men de finns i
dagens dashboard, har fungerande endpoints, och peers-vyn bär poängsättning,
sortering och artist-uppdelning som inte visas någon annanstans. Att portera
designen bokstavligt vore en funktionsregression, så de behålls och ritas i det nya
designspråket. Dagens `#/jobs/:id` slutar fungera; acceptabelt för en
självhostad app.

Sökvägar, komponentnamn och filnamn är på engelska. Fas 2 följer samma regel:
`/search`, `/shares`, `/setup`.

### Språk

**UI:t är på engelska.** Det är en ändring mot både dagens dashboard och
designprojektet, som båda är svenska. Strängarna översätts när komponenterna skrivs
— kostnaden är låg eftersom varje sträng ändå passerar genom porteringen.

**Alla användarvända strängar samlas i `src/strings.ts`.** Ingen sträng skrivs direkt
i en komponent. Mönstret finns redan i dagens `dashboard.js` som `STATUS_LABEL` och
`EVENT_LABEL`; det formaliseras och utvidgas till att gälla allt.

Detta är i18n-förberedelse, inte i18n. Att dra in `react-i18next` nu vore plumbing,
pluralformer och språkväljare för exakt ett språk — YAGNI. Kostnaden ligger inte i
biblioteket utan i att gräva fram hårdkodade strängar ur JSX i efterhand, och den
kostnaden undviks av katalogmodulen. Steget till ett i18n-bibliotek blir mekaniskt.
Spårat som #86.

**Datum och tal formateras genom samma modul**, inte med inline
`toLocaleString`-anrop. Datumformatet förblir `sv-SE` (`2026-07-20 14:32`) trots
engelskt UI: ISO-liknande datum är mer läsbara i ett tekniskt verktyg än `en-US`
format. Att formatteringen ligger bakom en funktion gör valet enkelt att ändra.

**Serverstate: TanStack Query.** Nästan all data i slskdarr är serverstate —
jobb, händelser, peers, hälsa — med minimal klientstate utöver filterval.
Biblioteket ger polling, cache, "behåll senast kända data vid fel", och framför allt
deklarativ invalidering efter mutationer. Med överlappande data mellan vyer (jobb
syns på både Översikt och Kö) är manuell omhämtning efter åtgärder en buggkälla som
växer med antalet vyer.

Ingen global state-container. Redux eller Zustand vore lager utan innehåll när
klientstate är begränsat till filterval och formulärfält.

**Styling: CSS-variabler + CSS Modules.** Designens tokens blir variabler i
`tokens.css`; varje komponent får en `.module.css`. Vite stödjer det utan
konfiguration. Ingen Tailwind — designen uttrycks redan i exakta värden, och en
översättning till utility-klasser är ett tolkningssteg som bara kan införa fel.

Tokens ur designen:

| Roll | Värde |
| --- | --- |
| Bakgrund | `#0b0d10` |
| Panel | `#14171c` |
| Kant | `#21252e` |
| Text | `#dfe2e8` |
| Dämpad text | `#8a919d` |
| `--accent` / `--done` | `#35c48f` |
| `--active` | `#4c8dff` |
| `--queued` | `#a78bfa` |
| `--stalled` | `#e0a740` |
| `--orphaned` | `#e5595d` |

Varje statusfärg har en `-bg`-variant på 13 % opacitet. Typsnitt: IBM Plex Sans för
text, IBM Plex Mono för siffror och tekniska värden, med
`font-feature-settings: 'tnum' 1` för sifferkolumner.

**API-typer skrivs för hand** i `api/types.ts`, speglande Go-strukturerna. Go-typerna
är små och stabila; kodgenerering skulle kräva en OpenAPI-spec som inte finns.
Risken är typ-drift, som mildras av att typerna samlas på ett ställe och att
befintliga Go-handlertester låser JSON-formen.

### SSE — uppskjuten till #60

Ursprungligen planerad för fas 1, men struken efter genomgång: **backend har ingen
push-källa.** Pipelinen skriver till store, inget strömmar. En `/api/stream` i fas 1
hade blivit en server-side timer som pushar samma ögonblicksbilder som klienten redan
kan polla — identisk användarupplevelse mot TanStack Querys polling, till priset av en
Go-endpoint, återanslutningslogik och tester.

Argumentet "bygg transporten medan datamodellen är enkel" håller inte: det är
transporten som är enkel, och den blir inte svårare senare.

Fas 1 använder därför TanStack Querys polling, med samma intervall som dagens
dashboard (jobb och händelser 3 s, status och peers 5 s). SSE byggs i #60 där det
finns riktig push-data — live transferdata med köposition hos peer och hastighet.
Transportvalet och auth-analysen är dokumenterade som kommentar på #60.

### Testning

**Vitest + React Testing Library** för komponenter och datalogik i `web/`.
Befintliga Go-handlertester täcker API-sidan och behålls; de utökas med tester för
den nya asset-handlern (SPA-fallback, cache-headers, att `/api/*` inte fångas) och
för SSE-endpointen.

Ingen E2E i fas 1. Playwright kan tillkomma när flödena stabiliserats — att skriva
E2E mot ett UI som fortfarande ritas om cementerar det som är minst färdigt.

## Omfattning

**Ingår i fas 1:** Vite-bygget, Makefile, Dockerfile-ändring, asset-handler,
tokens och komponentbibliotek, de fyra designade vyerna (Översikt, Kö, Hälsa,
Inställningar) plus de två bevarade (Händelser, Peers), TanStack Query mot
befintliga endpoints, read-only `/api/config`, Vitest-setup, borttagning av
`dashboard.html` och `dashboard.js`.

Översikt byggs **utan charts** — designens två chartytor utgår helt i stället för att
lämnas tomma, så layouten ser avsiktlig ut. Se #88.

**Ingår inte:** samtliga fem UI-issues. De kräver både utökad design och backend
som inte finns. SSE-transport (#60). Charts på Översikt (#88) — designens
reconcile-historik och 24-timmarskurva kräver tidsseriedata backend saknar.
Skrivbar konfiguration (#89).

### Beteendeändringar mot dagens dashboard

Beteendeinventeringen av `dashboard.js` hittade tre saker som ser ut som buggar.
Dessa rättas vid porteringen i stället för att bevaras:

| Idag | Efter |
| --- | --- |
| `failed`-statuskortet räknas men ritas aldrig | Kortet visas |
| `pct()` saknar clamp — baren kan spilla över vid `bytesDone > bytesTotal` | Klampas till 100 % |
| Cancel/retry kollar aldrig `res.ok` — misslyckat anrop ger ingen återkoppling | Felmeddelande visas |

Övriga beteenden bevaras exakt, inklusive de subtila: "behåll senast kända data vid
fel" i samtliga fetch-anrop, race-guarden som hindrar ett långsamt svar för ett
tidigare visat jobb från att skriva över det aktuella, tre-nivåers fallback i
jobbdetaljheadern när jobbet fallit ur `/api/jobs`, och att `STATE_LABEL` faller
tillbaka på `status` snarare än på rå `state`.

### Öppen konflikt: inställningsvyn

Designen visar Inställningar som ett **redigerbart formulär** — Lidarr-URL och
API-nyckel, reconcile-intervall, samtidiga nedladdningar, kvalitetspreferens,
stalled-timeout — med både `Spara` och `Testa anslutningar`.

Det förutsätter skrivbar konfiguration från UI:t. Idag är TOML-filen enda källa,
och #63 slår uttryckligen fast att 1.0 kan behålla filen som källa och låta UI:t
*validera* snarare än skriva.

Fas 1 implementerar därför Inställningar som **läsvy: fälten visas ifyllda från
konfigurationen men är inaktiverade, `Testa anslutningar` fungerar, `Spara` utgår.**
En rad förklarar att värdena ändras i konfigfilen.

Detta är ett medvetet avsteg från designen. Skrivbar konfiguration kräver beslut om
filskrivning, validering, omstart av moduler vid ändring och hantering av samtidiga
skrivningar — ett eget projekt, inte en detalj i en frontend-migrering.

**Beslutat: skrivbar inställningsvy är 1.1.** Vyn är inte svår att rita, den är svår
att göra säker. Fas 1 levererar läsvyn. Spårat som #89.

## Fas 2 — beroendekarta

| Issue | Kräver design | Kräver backend |
| --- | --- | --- |
| #58 manuell sökning | Sökvy, träffkort, kvalitetsbadges | #55 nedladdningar |
| #59 manuell import | Matchningsförslag, Lidarr-väljare | Källfält i `internal/store` |
| #60 enhetlig jobbvy | Käll-badge, utökade filter | Källfält (#59), köposition från native |
| #61 shares | Share-vy, upload-lista | #56 shares-indexering |
| #63 guidad setup | Wizard, anslutningstest-tillstånd | Login-test från protokollkärnan |

Prompten som beställer designutökningen ligger utanför repot och har levererats
separat. Varje rad i tabellen planeras för sig när båda beroenden är uppfyllda.

## Risker

**Typ-drift mellan Go och TypeScript.** Handskrivna API-typer kan glida isär från
Go-strukturerna utan att något går sönder vid kompilering. Mildras av att Go-testerna
låser JSON-formen; om driften blir ett verkligt problem är OpenAPI-generering nästa
steg.

**~~Auth för SSE~~ — avskriven.** Undersökt: `security.go` stödjer HTTP Basic, som
webbläsaren skickar ambient på same-origin-anrop inklusive `EventSource`. Ingen
åtgärd krävs utöver test som verifierar det.

**Funktionsregression vid portering.** Dagens dashboard har beteenden som inte syns i
designen: sortering på peers-tabellen, händelsefiltrering, "behåll senast kända data
vid fel". Planen måste inventera `dashboard.js` beteende för beteende innan filen tas
bort.

**Byggkedjan blir längre.** Node införs i CI och Docker-bygget. Ett glömt `make ui`
ger en binär med placeholder-UI. Mildras av att placeholdern säger vad som är fel.
