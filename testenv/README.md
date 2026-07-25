# PR-labb: slskdarr + Lidarr + slskd + PostgreSQL

Det här är en lokal, destruktiv testmiljö för att köra en valfri slskdarr-checkout
mot riktiga Soulseek-sökningar utan att blanda in produktionsdata. Miljön bygger
alltid slskdarr från den aktuella worktreen och basladdar Lidarr med ett
reproducerbart urval av 150 saknade album.

## Första gången

```bash
cp testenv/.env.example testenv/.env
$EDITOR testenv/.env
./testenv/lab.sh reset
```

Fyll i **två separata Soulseek-testkonton**:

- `SLSKD_SOULSEEK_*` används av slskd.
- `SLSKDARR_SOULSEEK_*` används av slskdarrs native-klient.

Soulseek tillåter bara en samtidig inloggning per konto. Använd därför varken
samma konto i båda klienterna eller ditt vanliga konto. `.env` är gitignorerad och `lab.sh` sätter filrättigheten till `0600`.
Båda klienterna loggar in, men `SLSKDARR_BACKEND` avgör vilken som driver
pipeline-jobben. Standard är `soulseek`, alltså native-klienten — det är den
backenden labbet finns för att köra mot riktig trafik. Sätt värdet till `slskd`
för att jämföra mot daemonen. Observera att appens egen standard i
`config.example.toml` fortfarande är `slskd`; det är bara labbet som är omvänt.

## Delade filer

`SLSKDARR_SHARE_DIR` pekar ut katalogen slskdarr delar med Soulseek-nätverket.
Den monteras **read-only** på `/music/share` och är förkonfigurerad som
`[[soulseek.shared_folders]]`. Utan den delar klienten ingenting: `/api/shares`
rapporterar noll filer, ingen peer kan köa något från den, och varken
share-vyn eller upload-panelen går att testa på riktigt.

```
SLSKDARR_SHARE_DIR=/Users/dittnamn/Music/share
```

Använd en **absolut sökväg** — värden i `.env` expanderas inte av skalet, så
`~` och `$HOME` fungerar inte. Utelämnas raden används `testenv/share`, som
`lab.sh` skapar tom (och som är gitignorerad).

Katalogen monteras avsiktligt **inte** på `/music/library`: den sökvägen är
Lidarrs root-folder, dit labbet importerar testalbum. En riktig musikkatalog
där hade legat öppen för skrivningar från en miljö vars uttalade syfte är att
vara destruktiv. Monteringen sitter bara på slskdarr, som enbart behöver läsa.

Filer som läggs till medan labbet kör syns först efter en omindexering — klicka
**Rescan** i share-vyn, eller starta om containern.

`reset` tar bort all databas-, Lidarr-, slskd- och nedladdningsstate, bygger den
aktuella koden, startar stacken och kör seedningen. Det är kommandot att använda
när en PR verkligen ska testas från scratch.

## Dagligt flöde för en PR

```bash
git switch <pr-branch>
./testenv/lab.sh reset       # helt ren körning av aktuell checkout
./testenv/lab.sh logs slskdarr
```

För en ombyggnad som ska behålla state:

```bash
./testenv/lab.sh up
```

Övriga kommandon:

```bash
./testenv/lab.sh ps
./testenv/lab.sh info        # adresser, konton och lyssnarportar
./testenv/lab.sh seed        # återställ bara Lidarrs monitorerade testurval
./testenv/lab.sh down        # behåll volumes
./testenv/lab.sh destroy     # radera allt
./testenv/lab.sh config      # validera utan att starta containrar
```

`up` avslutas med samma utskrift som `info` ger. Den läser `testenv/.env`, så
egna portöverstyrningar syns korrekt. Lösenord och tokens redovisas med
variabelnamn i stället för värde — labboutput hamnar ofta i issues och PR:er.

## Tjänster

| Tjänst | Standardadress | Inloggning |
|---|---|---|
| slskdarr | <http://127.0.0.1:9090> | valfritt Basic-användarnamn, lösenordet i `SLSKDARR_OBSERV_TOKEN` |
| Lidarr | <http://127.0.0.1:8686> | avstängd i det lokala labbet |
| slskd | <http://127.0.0.1:5030> | `SLSKD_WEB_USERNAME` / `SLSKD_WEB_PASSWORD` |
| PostgreSQL | `127.0.0.1:15432` | `slskdarr` / `slskdarr-test`, databas `slskdarr` |

Webb- och databasportarna binds bara till loopback. Soulseek-portarna 50300
(slskd) och 50301 (native) publiceras däremot på hosten eftersom inkommande
peer-anslutningar behöver nå dem.

## Seedningen

`seed_lidarr.py` använder Lidarrs API och MusicBrainz-ID:n för ett fast artisturval.
Den skapar `/music/library`, lägger till artisterna utan automatisk sökning,
avmarkerar samtliga album i labbet och markerar sedan exakt `WANTED_ALBUMS`
redan utgivna album som fortfarande saknar filer. Kommandot misslyckas om
wanted/missing inte når exakt samma antal. Standard är 150; sätt ett värde
mellan 1 och 500 i `.env`.

Seedningen laddar **inte** ner album via Lidarr. De hamnar bara i wanted/missing så
att slskdarr kan plocka upp dem. Första körningen kan ta några minuter medan
Lidarr hämtar metadata.

## Volymer och import

Alla tre containrar ser samma download-volume som
`/music/slskd-downloads`. Lidarr ser dessutom biblioteket som `/music/library`.
Det gör att både slskd-backenden och native-backenden kan lämna över färdiga
filer till Lidarr utan remote path mappings. Den genererade slskdarr-konfigen
monteras read-only; ändra `.env` och kör `up` i stället för att redigera settings
i dashboarden.

## Begränsningar

Detta är ett integrationstest, inte ett hermetiskt CI-test:

- Soulseek-resultat, peer-tillgänglighet och överföringshastighet varierar.
- Inkommande anslutningar fungerar bäst om portarna 50300 och 50301 kan nås
  utifrån. Bakom NAT/VPN kan sökningar fungera men ge färre kandidater; ordna
  port forwarding när nätverksbeteende ska testas på riktigt.
- `latest` används som standard för Lidarr och slskd. Pinna `LIDARR_IMAGE` och
  `SLSKD_IMAGE` i `.env` när ett specifikt fel ska reproduceras.
- Kontona och de hämtade filerna måste behandlas som testdata. Lägg aldrig riktiga
  API-nycklar eller produktionscredentials i de spårade filerna.
