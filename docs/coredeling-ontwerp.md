# Core-deling & de stille core — wektijd, doorbell, rotatie

> Het scheduling-model voor meerdere apps op één fysieke core, en hoe een
> idle app op zo'n core (bijna) niets meer kost zónder traag te worden.
> Aanleiding (Derek, 29-08): "kunnen we LEAN en KISS beter hoppen tussen apps
> die meer doen — kijk vooral af bij Linux, het wiel is vaak al uitgevonden."
> Dit dossier is het antwoord: wat er afgekeken is (virtio, niet CFS), wat er
> bewust is weggelaten, en de metingen die elke keuze droegen. Vervolg op het
> stroommodel in [archief/energie.md](archief/energie.md); de code-details
> staan bij de code (metal/cpu/idle/rxdoor.go is de leeswijzer).

## Het model: drie lagen die elk één ding doen

Een gedeelde core heeft géén timer en géén preemptie — het wisselmoment is
een vrijwillige yield van de idle-governor (metal/cpu/idle). Wie er dan mag
draaien bepalen drie mechanismen, van grof naar fijn:

| laag | zegt | woont in |
|---|---|---|
| **wektijd** (CtxWake) | "hervat me niet vóór T" — timers | de yield draagt hem mee; de rotatie slaat niet-due bewoners over |
| **doorbell** (CtrlRXDoor + CtxRingHeadPA) | "wek me wél als er verkeer ligt" — events | app wapent, rotatie peekt, governor wekt |
| **round-robin** (SchedCursor/Rotor) | "mogen er twee: om de beurt" | cpu/el2/switch.s · cpu/mmode/switch.s |

De volgorde is ook de belangrijkheidsvolgorde. Sinds de doorbell slaapt
vrijwel elke bewoner op een drempel of een verre timer, dus op een typisch
moment is er hooguit één due kandidaat — en dan is elke keuzestrategie
gelijk. De cursor hoeft alleen nog starvation onmogelijk te maken (de scan
begint ná de vorige gekozene), en dat is mechanisch gegarandeerd.

## De meting die het stuurde (schedbench, 29-08, QEMU)

Twee apps op één core, slot 1 echo, slot 2 klokt round-trips; daarna een stil
venster. Herhaalbaar met `image/qemu-run.sh bench`.

| stand | hete RTT p50 | koude RTT p50 | wekken/s idle |
|---|---|---|---|
| 300µs vaste RX-poll (de oude wereld) | 654µs | 0,9ms | **3.113** |
| + adaptieve backoff (cap 5ms) | 654µs | **8,2ms** | 218 |
| + backoff cap 100ms | 653µs | **157ms** | 30 |
| **+ doorbell** (cap mag 10s) | **415µs** | **0,8ms** | **20** |

Drie lessen in één tabel:

1. **De 300µs-poll wás de workload.** 3.113 wekken/s per idle app, en op een
   gedeelde core is elke wek een volledige context-wissel — dit is de op ijzer
   gemeten "twee lege apps lezen elk 36%" (31-07, C906L) in zijn ware gedaante.
2. **Backoff alleen ruilt wekken tegen latency**: het eerste pakket ná stilte
   wacht precies de cap. Die koppeling is de reden dat pollen nooit "af" is.
3. **De doorbell knipt de koppeling door**: 20 wekken/s (alleen nog heartbeat
   + GC) mét sub-milliseconde reactie. De hete RTT werd er zelfs 37% sneller
   van, want de bel wekt de pomp ook midden in zijn slaapje.

## De doorbell: virtio's EVENT_IDX in ~95 regels

Afgekeken van het juiste wiel: niet Linux' scheduler (CFS/EEVDF lost "verkeerd
kiezen" op, ons probleem was "te vaak wisselen") maar **virtio's
event-suppression**: de consument zegt waar hij gebleven is, en wordt alleen
gewekt als er iets nieuws is. Alle app-communicatie loopt door de framequeues
— netwerk, en dus ook SURF/gui — dus één bel-punt dekt alles.

De keten, per stuk met zijn maat:

1. **Wapenen** (cpu/idle/rxdoor.go, 34 regels): ligt er niets, dan schrijft de
   governor per idle-ronde `CtrlRXDoor = gezien-head | bit 63` op de eigen
   control-page en slaapt gewoon — de RX-pomp mag een cap van seconden hebben.
2. **Peeken** (el2/switch.s **10 instructies**, mmode/switch.s **19** incl.
   cache-onderhoud): de rotatie vergelijkt bij een niet-due bewoner de drempel
   met het live completion-headwoord van zijn RX-queue (CtxRingHeadPA, door HOP bij de
   start gezet). Gegroeid = er kwam verkeer = due, wektijd irrelevant.
3. **Wekken** (rxdoor.go): de governor ziet bij de hervatting dat er iets ligt
   en wekt de pomp-goroutine met `runtime.Wake` — het primitief dat tamago
   voor bare-metal interrupt-handlers heeft (nosplit, allocatievrij, bestaat
   op arm64 én riscv64).

**Geen HOP-wijziging en geen IPI.** De core wordt sowieso elke ~1-2ms wakker
(ARM: de event-stream; RISC-V: de SleepCap van de park) — de peek doet alleen
de targeting. Een kick vanuit hopswitch (SEV/msip bij enqueue) zou de koude
tik van ~1-2ms naar ~een switchkost brengen; bewust niet gebouwd tot een
meting erom vraagt.

### Twee valstrikken die het ontwerp vormden

- **De peek alleen livelockt.** Een rotatie die op kaal head≠tail hervat,
  wekt een app wiens pomp op een Go-timer slaapt: de app yieldt meteen weer,
  de ring is nog vol, de rotatie hervat weer — een pingpong tot de timer
  afloopt. De wek moet de *goroutine* bereiken, niet alleen de core; daarom is
  `runtime.Wake` het onmisbare derde stuk.
- **Bit 63 is een grens, geen detail.** Een app zonder netstack leest zijn
  ring nooit leeg, en broadcasts (ARP!) landen in élke aangesloten ring — een
  peek zonder gewapend-teken maakt zo'n kooi permanent "due" en eet de core
  op. Alleen wie de ring draint mag de bel aanzetten, en dat dwingt het teken
  af: ongewapend = geen peek.

### De RX-pomp: backoff bleef, als bodem

De adaptieve slaap (300µs → verdubbelen → cap, terug naar 300µs bij verkeer;
applib/appnet) bleef staan onder de bel: hij dekt de drukke burst (pomp
recent actief = scherp) én is het vangnet als de bel ooit dooft. Default
`300us:1s:4`; de cap is bewust 1s en niet groter omdat de heartbeat elke app
toch ~1×/s wekt — een grotere cap levert nul minder wekken op (gemeten:
cap 1s = 21/s, cap 10s = 20/s) en 1s begrenst een bel-storing. `RXPOLL` in de
job-env is de ontsnappingsklep (`"300us"` = het oude gedrag).

## Bewust weggelaten

- **Preemptie.** Een bewoner die grofkorrelig yieldt laat zijn buren wachten,
  en geen rotatie-algoritme lost dat op — alleen een timer die hem
  onderbreekt. Dat is de ontwerpgrens ("compute hoort op een eigen core",
  app-isolatie-principe) met HOPOS_CORE_RECLAIM als noodgreep. Mocht het ooit
  knellen: op RISC-V trapt de kill-tick al elke 10ms binnen en hoeft alleen
  "buur due? → save + rotate" te leren (~10 regels). Knop voor als een meting
  erom vraagt.
- **Gewogen eerlijkheid (vruntime/EEVDF/shares).** Run-to-yield + cursor is
  vanzelf ~50/50 voor twee werkers (strikt alterneren), en tussen slapende
  apps valt niets eerlijker te verdelen. Linux heeft die machinerie omdat het
  duizenden onbekende processen moet knechten; HopOS kent zijn apps.
- **De HOP-kant van de bel** (SEV/msip bij enqueue): zie boven.

## ABI-stap (herbouw-regel!)

De control-page was vol: `CtrlRXDoor` kreeg 0x110 en **CtrlEnvData schoof
0x110 → 0x118**. Een oude app onder een nieuwe kern zou zijn env-blob één
woord verkeerd lezen (zelfde klasse als de UDP-klok-les van 11-08) — daarom
is dit géén herbouw-discipline maar een versie-stap: **ABIVersion 3 → 4**,
en HOP weigert bij plaatsing elk image van een andere versie. Stuk-op-een-
subtiele-manier is daarmee onmogelijk; het faalt luid, vóór de start.

## Status & open

- **29-08**: gebouwd op beide architecturen, QEMU-bewezen (bench + demo 13/13
  markers + volledige gate).
- **30-08 — IJZER-BEWEZEN op de LicheeRV**: de mmode-peek (incl. de cipa's op
  het niet-coherente T-Head-paar) doet zijn werk, en de apps die op dit hart
  altijd 36% "busy" lazen — de eerlijke meting van hun verspilde wekken —
  spinnen nu naar **0%**. De cpu-meting was al die tijd correct; wat er
  veranderde is de werkelijkheid die hij mat.
- **30-08 — uit**: gecommit (1b44df7) en released als **v1.999.0**, mét de
  ABI-hoging naar 4 — oude artifacts worden geweigerd, niet stil gebroken.
