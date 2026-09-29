# Meetinstrumenten

Wat er in HopOS zit om een node te ondervragen die niet meer praat, en hoe je
elk instrument terugbouwt of aanzet. Geschreven na de flip-jacht van 06-09,
waarin de helft van het werk bestond uit het opnieuw uitvinden van gereedschap
dat er al was — en uit het ontdekken dat één instrument de verkeerde vraag
beantwoordde.

De volgorde is die van de boot: hoe vroeger iets stuk gaat, hoe minder van deze
lijst er nog werkt.


## 1. De vluchtrecorder (kern/kernflip/stage.go)

Twee woorden in DRAM, op een board-adres buiten élke RAM-declaratie
(`layout.FlipStagePA`, op de M4 `StructBase+0xF8000`). Het eerste woord zegt hoe
ver de lopende flip is; het tweede is het ARCHIEF: de laatste poging die niet
landde.

- **Waarom twee woorden.** Een gevallen flip laat de node terugvallen op de
  geïnstalleerde kern, en de flip waarmee je hem daarna weer optilt schrijft
  zijn eigen stappen over het spoor heen dat je wilde lezen. Elke nieuwe poging
  archiveert daarom eerst wat er stond (`archiveStage`).
- **De sprong draagt zijn doelgeneratie** (`stageJump`), zodat een landende kern
  "mijn eigen sprong" kan onderscheiden van "een sprong die nooit landde". Het
  archief negeert alles van de eigen generatie — zonder dat archiveert elke
  geslaagde boot zijn eigen laatste stand en meldt de volgende boot een
  mislukking die er niet was.
- **Aflezen.** Elke boot drukt het archief af (`ReportArchivedStage`), en een
  koude boot ook de lopende stand (`ReportLastFlip`). Zoek op
  `HOPOS_FLIP_STALLED`.
- **Nieuwe stappen horen ACHTERAAN** in de `const`-lijst. Een stap ertussen
  schuiven hernummert de rest, en dan meldt een oudere kern zijn eigen stand
  verkeerd. Dat is één keer gebeurd en het kostte een ronde.

### De les die dit instrument zelf opleverde

Tijdens de jacht is de recorder vier keer fijner gemaakt: eerst één marker op de
eerste regel van `main`, toen één per regel van de vroege boot, toen één per
stap in de overdracht. **Bij élke val meldde hij precies de laatste marker die
erbij was gezet.** Dat is niet het patroon van "hier crasht hij" maar van "de
boot-core loopt gewoon door en het dodelijke gebeuren zit elders" — de bewoners
draaien tijdens een flip immers door op hun eigen cores. Die tien markers zijn
daarom weer weg; alleen `stEarlyMain` (haalde de nieuwe kern zijn main?) en
`stAdoptBlobBad` (het ene faalpad dat bewust blijft staan) zijn gebleven.

Wie ooit weer markers op een boot-pad wil zetten: toets eerst of de gemelde
stand meebeweegt met de laatst toegevoegde marker. Doet hij dat, dan meet je de
snelheid van de boot-core en niet de plek van de fout.


## 2. De zwarte doos (driver/conlog/blackbox.go)

Elke console-byte wordt ook naar DRAM buiten elke RAM-declaratie geschreven (op
de M4 `StructBase+0xF8100`, 28KB, naast de recorder). De volgende boot drukt de
staart af als er iets misging, of altijd met `hopos.blackbox=1`.

- **Waarom niet opslag.** Opslag is een prima logkanaal, maar de NVMe komt véél
  later op dan waar een mislukte flip sterft. Een kern die in zijn bring-up
  omvalt heeft geen netwerk, geen console op tcp/5555 en geen volume — alleen
  DRAM en een UART.
- **De teller loopt DOOR over boots heen.** Zou elke boot de ring legen, dan
  wist een kern die meteen omvalt precies het spoor dat je zocht. De staart is
  dus het interessante deel.
- **Elke boot meldt hoeveel bytes hij overnam** (`black box: N bytes carried
  over`). Nul betekent: leeg, of iemand heeft de regio onderweg gewist. Dat
  onderscheid is essentieel — zie de beperking hieronder.
- **De regio is krap.** Tussen de recorder en `CagePA` zit op de M4 32512 byte.
  Er staat een compile-time grens omheen (`const _ = uint(CagePA - (BlackBox +
  BlackBoxSize))`), want 32KB liep er 256 overheen — precies over het begin van
  de kooi-regio, waar de instructies staan die de app-cores UITVOEREN. Met
  levende bewoners schrijf je dan console-tekst over hun code. Elke vaste regio
  naast een andere hoort zo'n grens te krijgen; de rekensom in een comment is
  niet genoeg.

**Beperking (06-09, nog open):** valt de node terug op een kern van vóór
metal/v2, dan overleeft dit spoor die tussenboot niet. De post-mortem van
diezelfde val is dan ook weg (zie 3). Zolang de geïnstalleerde kern oud is, is
elke crash-analyse van een flip geblokkeerd.


## 3. Wat er NIET is: een kooi-post-mortem

De verleiding is groot en hij is één keer gebouwd: de ctx-blokken liggen op
vaste adressen die een reset overleven, elk blok draagt de control-page van zijn
bewoner, en dáár schrijft de EL2-switcher zijn fault-rapport (vec, ESR, FAR —
`cpu/el2/switch.s`, label `fault:`). Na een reboot zou je dus kunnen zien of een
APP-core omviel en waarop.

Het werkt alleen niet zolang de node bij een val terugvalt op een kern die de
kooi-regio bij zijn koude boot vers neerzet: die wist de blokken vóórdat iemand
ze kan lezen. De code is daarom weer weggehaald — hij leverde nul regels bewijs
op. Zodra er een actueel image op de NVMe staat, is dit het eerste dat de moeite
waard is om terug te bouwen; het is ~40 regels en het staat hierboven
beschreven. Toets bij het terugbouwen élke pointer die je uit oud geheugen
leest tegen de pool, anders is de post-mortem zelf een crash.

## 4. De boot-guard (cmd/hopos/watchdog.go, armBootGuard)

`SetupPlan` zet als allereerste élke watchdog stil (iBoot laat er meerdere
gewapend achter), en het beleid wapent er pas één ná de agent. Op een koude boot
is dat gat ongevaarlijk. Na een flip is het fataal: de gewapende watchdog van de
vertrekkende kern gaat binnen milliseconden uit, en sterft de nieuwe kern dan,
dan waakt niemand — gemeten 06-09: zeven minuten volledig donker, geen ping,
geen console. `armBootGuard` wapent nu meteen als er een overdracht klaarligt
(`kernflip.FlipPending`), vóór de overdracht zelf, en aait tot het beleid begint.


## 5. De idle-meetlat (hopos.idlestat=1)

Eén regel per tien seconden op de console: wekken per seconde, idle-percentage,
waker-rondes, hoeveel bewoners er slapend gezien zijn, fallback-kicks versus
directe RX-kicks, switch-wekken, en `idle: cores` met per core de ctx-staat,
slaapteller, wektijd, kick-doel, unit en control-page. Dit is het instrument
waarmee "de app slaapt en wordt nooit gewekt" te onderscheiden is van "de app
spint" — beide zijn anders 100%.

Zet hem aan in de platform-config; hij kost een print per tien seconden.


## 6. De console-ring en tcp/5555

`driver/conlog` houdt de laatste 32KB console vast, ook als de UART-poll hangt,
en de node serveert hem op tcp/5555 (`hopos.console=5555`). Lezen met
`nc -d <node> 5555`. Let op: de ring is HOP's eigen geheugen en gaat mee met de
kern — na een crash is hij weg. Daarvoor is de zwarte doos.

Een `nc` die tijdens een flip openstaat, breekt bij de sprong af; de nieuwe kern
opent de poort pas ná zijn netwerk. De console is dus juist blind in het venster
waarin een flip sterft.


## 7. De QEMU-flipregressie (image/qemu-run.sh flip)

Kern A start twee bewoners, haalt de bundel van een webserver en springt erin;
de geflipte kern B adopteert ze en flipt door, drie generaties lang. Bewijst per
generatie: doorlopende heartbeat (geen herstart), overlevende NAT-flows,
overlevende mounts en gepubliceerde poorten, en dat het venster van de vorige
kern weer vrije pool is. Daarna draait de volledige demo-suite nog.

De opstelling bootst sinds 06-09 de M4-vorm na: één bewoner die de kooi van een
net gestopte SMP-app hergebruikt zonder op de afbraak te wachten, plus een
tweede bewoner ernaast.

Twee valkuilen die elk een run kostten:

- `mkkernel` bakt standaard flip-ABI 2 in de staart terwijl de kern 1 spreekt.
  Het script geeft nu `-flipabi 1` mee; zonder dat weigert élke flip met
  "bundel spreekt flip-ABI 2, deze kern 1".
- De gast kan de loopback van de host niet bereiken via slirp (QEMU 11). Serveer
  de bundel op het LAN-adres van de host, niet op 127.0.0.1.


## 8. vitals (apps/vitals)

De board-dokter, als job te plaatsen en op `:8090` te bevragen
(`/api/run?test=<naam>`, resultaat op `/api/state`). Relevante tests:

| test | meet | zegt iets over |
|---|---|---|
| `disk` | schrijven/lezen in chunks via het system-callpad, plus 4KiB-writes en de kale Stat-vloer | waar de rem zit: alles boven de Stat-vloer is hopfs + NVMe, alles eronder het LAN-pad |
| `syscall` | stat-latency op de systeemverbinding naast een keep-alive GET naar de agent | het wek-pad van een stille verbinding |
| `sqlite` | SQLite op het volume in vier inrichtingen naast elkaar, `?trap=1` zet de valkuil ernaast | hoe een database ingericht moet op dit OS |
| `push` | deze app uploadt naar een ándere app op dezelfde node (`?url=`, `?token=` voor Spins chunk-API) | het netwerkpad zonder meetclient ertussen |

Die laatste is 06-09 gebouwd omdat een laptop op Wi-Fi alles boven ~94 MB/s
verbergt: app naar app haalt 1,9 GB/s over één stroom en 3,2 GB/s samen.


## Recept: een flip-val op de M4 reproduceren

1. Bouw twee bundels met identieke code maar een andere ingebakken config (één
   extra commentregel is genoeg). De flip-wacht vergelijkt de CHECKSUM van de
   bundel, dus dezelfde bundel twee keer wordt geweigerd met "already running
   the kernel".
2. Zet cloudflared op slot 1 en een tweede bewoner op slot 2, en flip heen en
   weer tussen de twee bundels.
3. De POST gaat naar de AGENT (`:8080/flip`), niet naar de leader (`:9080`, die
   geeft "not found"). Op een bank met `hopos.insecure=1` zonder handtekening;
   anders `X-Hop-Auth` = HMAC-SHA256 over `POST\n/flip\n<sha256 van de body>`.

Valt hij om, dan komt de node terug op de geïnstalleerde kern. Bewaar dus vóóraf
de jobspecs (`GET /v1/jobs`): de leader draait op de node zelf en zijn
jobdefinities leven alleen in RAM.

Stand 06-09: zonder bewoners acht flips op rij foutloos (generatie 2 t/m 9), met
bewoners ongeveer één op drie eruit.
