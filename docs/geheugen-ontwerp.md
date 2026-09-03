# Geheugen op een HopOS-node — wie krijgt wat, en wanneer komt het terug

> **Waarom dit dossier bestaat:** we hebben deze keten drie keer opnieuw
> uitgevonden en er drie keer op ijzer tegenaan gelopen (14-07 Altra, 17/18-08
> en 19-08 LicheeRV). De getallen staan in de code, de *redenen* stonden nergens
> bij elkaar. Dit is de bron; de code is de waarheid. Bij tegenspraak wint de
> code en is dít bestand de bug.

Eén ding vooraf, want het verklaart de helft van de verrassingen: **er is geen
OS om geheugen aan terug te geven.** Een app is geen proces met een `brk` en een
kernel die pagina's uitdeelt — hij krijgt één fysiek stuk RAM, dat stuk *is* zijn
wereld, en de enige die het ooit terugkrijgt is HOP, als de app stopt. Binnen die
grens bestaat er niets om aan te vragen en niets om aan af te staan:
`debug.FreeOSMemory()` geeft aan niemand iets terug, swappen kan niet, en een
app die te weinig heeft gaat niet trager maar dood.

## De keten in vijf stappen

```
DRAM  →  board-plan  →  pool  →  systeempot + partities  →  app-RAM
```

1. **DRAM** — wat er fysiek is. Gemeten via de DTB waar dat kan; faalt dat, dan
   valt de node terug op het statische layout en zegt dat luid
   (`HOPOS_RAM_CHECK_SKIPPED` / `HOPOS_POOL_FALLBACK`).
2. **Board-plan** (`board/*/plan.go`, `layout.UsePlan`) — welke adressen HOP
   zelf houdt en wat er overblijft. Dit is de enige plek waar een board zijn
   geometrie bepaalt.
3. **Pool** (`layout.Pool()`) — de regio's waaruit app-partities gesneden worden.
   Eén of meer, en dát aantal is bepalend (zie *Fragmentatie*).
4. **Partitie** (`kern/slots/partmem.go`) — één aaneengesloten stuk per slot,
   afgerond op 2MB. Dit is wat de kooi vrijgeeft en waarbuiten de app niets kan
   raken.
5. **Systeempot + app-RAM** — bij boot reserveert HOP standaard 50MB uit de
   pool; de kleine LicheeRV gebruikt standaard 4MB. `hopos.net.buffer` kan dit
   per node overrulen. Per mogelijk slot kosten alleen control en twee
   descriptorpagina's een vaste plak. Alle bytes daarachter zijn één HOP-brede
   pool van 2KiB-chunks die pas bij RX-backlog worden geleend en onmiddellijk
   terugkomen. Elke jobpartitie die daarna wordt uitgedeeld is volledig app-RAM;
   framepayload blijft daar tijdens normaal verkeer gewoon in app-buffers.

## Wat HOP voor zichzelf houdt, per board

| board | HOP's eigen venster | staart / structuren | pool |
|---|---|---|---|
| licheerv (riscv64) | 32MB op `0x80400000` | 2MB bovenaan het DRAM | **één regio, 218MB** |
| rk3566 (arm64) | 64MB op `0x02200000` | 1MB structuren + 1MB wektekens + 8MB NIC-DMA + 8MB fb | één span erboven, ~1,8GB |
| rpi4/rpi5 (arm64) | 128MB onderaan | ctrl/stage2/NIC-DMA/USB-DMA op `0x1000_0000`..`0x1500_0000` | 4 regio's (4GB-Pi: 126 + 30 + 24 + 3702MB) |
| qemuvirt (arm64) | 240MB op `0x40000000` | — | **2 regio's met opzet**: de fixture die de multi-regio-code eerlijk houdt |
| uefi (arm64) | uit `poolOff` | uit de firmware-map | veel regio's, gecoalesceerd |

Op de LicheeRV staan er nog twee dingen naast: de onderste 4MB is **vuil gebied**
(de FSBL decomprimeert U-Boot naar `0x80200020`, ~600KB, nádat hij ons image
geladen heeft), en `SlotBase` (`0x88000000`) ligt sinds 19-08 *midden in* de
pool-regio. Dat mag omdat de kooi verplaatst — zie *De kooi* hieronder.

## Uitdelen: hoe een partitie gekozen wordt

`partAlloc(slot, maat)` in `kern/slots/partmem.go`, en elke regel erin is een
meting:

- **2MB-korrel.** Alles wordt naar boven afgerond (`align2M`). Een app van 33MB
  kost 34MB. De korrel is de uitlijning die de kooi kan beschrijven.
- **Best-fit: de kleinste regio die het nog kan dragen.** Gemeten 31-07: een
  64MB-aanvraag pakte een stuk uit de 127MB-regio terwijl de losse 64MB-regio
  vrij lag, en daarna paste 124MB nergens meer. Best-fit laat een grote regio
  groot zolang een kleinere volstaat.
- **Hoog-eerst binnen de regio.** Houdt het lage DRAM vrij (daar wonen op
  sommige boards HOP's structuren en het onder-4GB-bereik voor DMA), en zorgt
  ervoor dat een kleine app van de bovenkant afsnijdt in plaats van midden in
  een groot gat te gaan zitten.
- **Een mislukte aanvraag laat alles staan.** Sinds 19-08: `partAlloc` maakt een
  momentopname van de vrije lijst en draait die terug als er geen gat past. Hier
  stond eerst een `releaseLocked(slot)` vooraan als "defensief bij een
  re-Start", en dat gaf bij een misser de partitie van een **draaiende** bewoner
  los in de pool — de volgende plaatsing kon die dan uitdelen. Vrijgeven blijft
  wél vóór de zoektocht, want een re-place van hetzelfde slot heeft juist zijn
  eigen regio nodig om weer te passen.

## Teruggeven: waar geheugen vandaan komt als een app stopt

`partRelease(slot)` → `releaseLocked` → **gesorteerd invoegen en met beide buren
samensmelten**. Dat coalescen is het enige dat fragmentatie ongedaan maakt.

De volgorde bij een stop is niet willekeurig (`releaseSlot`, `kern/slots/slots.go`):
eerst de servicer wegzetten, dan de outbox leegdrinken, dan het post-mortem
kopiëren — en **daarna** de partitie vrijgeven. De control page staat inmiddels
in de systeempot, maar de volgorde blijft de lifecyclegrens: wie ná een stop
vraagt "waarom viel hij" moet het laatste antwoord krijgen vóór dezelfde
slot-slice opnieuw wordt geïnitialiseerd.

Eén uitzondering, met opzet: faalde het **startschot zelf** (`errDispatch`), dan
is onbekend of de core tóch aangaat en gaat de partitie in **quarantaine** in
plaats van terug in de pool (`HOPOS_PART_QUARANTINE`). Geheugen kwijt is beter
dan geheugen dat twee eigenaren heeft.

## De kooi: begrenzen en verplaatsen zijn twee dingen

|  | begrenzen | verplaatsen |
|---|---|---|
| ARM | stage-2-tabel | dezelfde stage-2-tabel |
| RISC-V (C906) | PMP-whitelist (`kern/cage`) | een eigen tabel (`kern/cage/relocate.go`) |

Verplaatsen dient één doel: **één artifact per architectuur.** Elk app-image is
gelinkt op `SlotBase`, en de kooi vertaalt dat naar de partitie die het slot
werkelijk kreeg. Zonder dat zou elk slot zijn eigen build nodig hebben en zou er
in de praktijk één slot bestaan. Bewezen op ijzer: stulp liep op `0x81c00000`,
stulp-weather op `0x86a00000` — beide buiten hun linkbasis.

Dat onderscheid heeft één gevolg dat je moet kennen: op de C906 mag de app zijn
eigen map-register schrijven en dus zijn adresruimte hertekenen. Dat is veilig
omdat de hardware-walker zélf aan de PMP-whitelist onderworpen is: hertekenen
bereikt nooit iets buiten de kooi en schaadt alleen de app zelf.

## Plafonds die je gaat raken

- **768MB per partitie** (`maxLimitFor`). De kooi mapt de partitie binnen één
  1GB-blok vanaf de linkbasis, minus de afstand tot dat blok. Irrelevant op 256MB,
  beslissend op een Altra. De twee uitwegen staan in de code: de control-regio's
  omhoog schuiven, of een multi-GB-map (asm-/layout-werk per board).
- **2MB-korrel**, dus tot 2MB verlies per app.
- **`MaxSlots`** per board (LicheeRV: 16 kooien op één app-hart — kooien zijn
  goedkoop, 68KB elk; de échte grens is app-RAM bij plaatsing).
- **De systeempot**: standaard 50MB totaal (`hopos.net.buffer=50M`), behalve
  4MB op de LicheeRV met zijn 16 slots. Vast zijn alleen 128KB control plus twee
  queuepagina's per mogelijk slot; de rest is één dynamische framepool zonder
  per-slot payloadquotum. Een kleine toelatingsreserve beschermt andere
  aangesloten apps tegen een vastgelopen ontvanger. Als de pool echt vol is
  dropt Ethernet en verzorgt TCP de flow-control en retransmit.

## Fragmentatie: wanneer het bijt en wat je beschermt

De regel die alles samenvat:

> **Zolang er alleen apps bijkomen in één regio, is het vrije geheugen één stuk.
> "Er is X vrij" en "X is plaatsbaar" zijn dan hetzelfde antwoord.**

Dat is waarom het aantal pool-regio's zo bepalend is, en waarom HOP op de
LicheeRV 19-08 naar de onderkant van het DRAM verhuisde. Vóór die verhuizing was
de pool 126 + 64 + 32MB en gold het omgekeerde: 100MB vrij en geen 96 plaatsbaar
(vastgelegd in `TestLicheeRVOneRegionPlacesWhatThreeCouldNot`).

Wat het **niet** oplost: een app die middenin stopt splitst het vrije deel
alsnog. Op ARM zou de stage-2 die stukken aan elkaar kunnen plakken (scatter —
`L2part` mapt al per 2MB-blok, dus het is dezelfde tabel met andere entries). Op
de C906 kan dat niet: elke losse span kost twee PMP-entries en het budget is
acht, met de huidige app- en bufferslice plus een eventuele grant en deny-all
begrensd.

En de bescherming die er sinds 19-08 wél is: **de toelating vraagt het gat, niet
de som.** `slots.PoolLargest()` geeft het grootste plaatsbare stuk; HOP's agent
weigert daarop een hop-job die nergens past, zonder eerst te reserveren
(`hopos.PoolReporter`, optioneel — een driver die het niet weet valt terug op de
som). Uitzondering: bij een *replace* niet, want daar houdt de voorganger zijn
partitie nog vast.

## Binnen de app: het plafond en het tempo

`cpu/memlimit` leest bij de start het venster uit en zet twee dingen:

- **`SetMemoryLimit`** op het app-RAM minus wat het image en bss al kosten, met
  wat arena-slack. Dit is een absoluut plafond, geen advies.
- **`GOGC`**, en alleen in een smal venster. Go's default is "ruim op als de heap
  verdubbelt" — een relatieve regel tegen een absoluut en dichtbij plafond.
  Gemeten in HOP's LicheeRV-raam: GOGC 100 → piek 41,6MB en OOM na 151s;
  GOGC 50 → piek 36,3MB, bijna geen lucht; GOGC 25 → piek 30,6MB met 15MB over.
  Onder `tightWindow` (128MB) wordt het dus 25, daarboven blijft het 100 — want
  daarboven is verdubbelen efficiënt en zijn de extra rondes weggegooid.
  De grens leest het venster zelf uit: geen getal per board, geen getal per job.

Die extra GC-rondes kosten in de gemeten wereld vrijwel niets (22 GC's in 215s,
3,7ms stop-the-world totaal) omdat GC-werk met levende **pointers** schaalt en
niet met bytes — die heap is vooral `[]byte`. Draait er ooit een pointer-rijke
wereld, dan is dat getal opnieuw een meting waard. **Zo'n wereld bestaat al:** de
stulp-plugin-bundel is pointer-rijk en thrashte zich in een smal venster een
panic bij 18MB live tegen 47MB limiet (157 GC/s, 19-08). Die zet daarom zelf
`debug.SetGCPercent(100)` en krijgt een ruim venster.

## Hoe je het leest op een levende node

- **Bij de boot**, de vorm van de pool en niet alleen de som:
  `memory: pool is 1 region(s), largest placeable 218 MB — [0x82400000+218MB]`.
  Staat er meer dan één regio, dan kan "vrij maar niet plaatsbaar" bestaan.
- **Bij de boot**, HOP's eigen budget: `memory: HOP itself has 32 MB (...) — Go
  runtime holds 5 MB, heap 1 MB in use`. Elke MB die HOP niet gebruikt hoort bij
  de pool.
- **Per app**, één regel bij zijn start: `mem: Go memory limit 21MB, GOGC 25
  (window 30MB, image+bss 4MB)`.
- **Per task**, `mem_percent` in `/tasks` — het echte gebruik tegen de limiet.
  Dit is het getal waarmee je een `memory_limit` kiest in plaats van gokt.
- **Bij een plaatsing**: `slot N: partition X MB @ 0x… — streaming Y MB image`.

## De vallen waar we in gelopen zijn

Met datum, want het zijn allemaal metingen en geen meningen.

| wanneer | symptoom | oorzaak | wat het werd |
|---|---|---|---|
| 14-07 | "pool full or fragmented" met 300GB vrij; 12 van 127 taken geplaatst (Altra) | een UEFI-map van duizenden descriptors zag eruit als fragmentatie | rauwe spans eerst coalescen, dán 2MB-uitlijnen |
| 14-07 | OOM in de kern bij 127 plaatsingen | hele images door HOP's heap | streaming plaatsing: elke byte meteen op zijn eindadres |
| 31-07 | 124MB paste nergens meer na een 64MB-retry | first-fit sneed uit de énige grote regio | best-fit (kleinste passende regio) |
| 31-07 | "basis niet gealigneerd op maat" | NAPOT eiste een macht van twee | TOR + 2MB-korrel |
| 17/18-08 | node viel om, agent-poort verstikt | onplaatsbare job pingpongde in een hand-back-lus, watchdog-pets gemist | `memory_limit` afronden op de 2MB-korrel zodat de som klopt met de kosten |
| 19-08 | idem, maar nu bij 60MB vrij en geen 36MB aaneen | de afronding ziet **fragmentatie** niet | toelating vraagt `PoolLargest()`, weigert vóór er iets gereserveerd is |
| 19-08 | gerapporteerde capaciteit flapperde 162↔198MB en weigerde een job van 28MB die paste | de reservering van elke mislukte poging | zelfde fix; zonder reservering geen flapper |
| 19-08 | (latent) een mislukte re-place kon de partitie van een draaiende app vrijgeven | `releaseLocked` vóór de fit-check | momentopname + terugdraaien |
| 19-08 | 200MB paste niet op een node met 222MB vrij; een 32 maakte een 126 onmogelijk | HOP stond midden in het DRAM: pool in 3 stukken | HOP naar de onderkant, pool = één regio van 218MB |
| 19-08 | plugin-bundel panic'te bij 18MB live tegen 47MB limiet | pointer-rijke wereld + smal-venster-GOGC 25 | eigen `SetGCPercent(100)` + ruim venster |

## Waar de getallen wonen

| wat | bestand |
|---|---|
| HOP's venster en de vuile zone per board | `metal/board/*/…` (`HopBase`, `HopSize`) |
| pool-regio's en DMA-adressen | `metal/board/*/plan.go` |
| korrel, systeempot, best-fit, hoog-eerst, coalescen, `PoolLargest` | `metal/kern/slots/partmem.go` |
| vaste appvensters en de per-slot ceiling | `metal/abi/layout/layout.go`, `maxLimitFor` |
| begrenzen en verplaatsen | `metal/kern/cage/` (`cage.go`, `relocate.go`), `metal/kern/stage2/` |
| app-plafond en GC-tempo | `metal/cpu/memlimit/memlimit.go` |
| toelating op het gat | `hop/internal/agent/handlers.go`, `hop/pkg/hopos` (`PoolReporter`) |
| laadadres van het image (LicheeRV) | `image/licheerv-agent.sh` (`RUNADDR`) |

Verander je één van deze getallen, dan is de boot-regel hierboven het bewijs: één
regel zegt of de pool nog de vorm heeft die je bedoelde.
