# App-geheugen — de adresruimte van een HopOS-app

Wat een app "ziet" is op elk slot identiek (het **canonieke IPA-beeld**); de
stage-2-kooi (hardware) vertaalt dat naar de fysieke per-slot regio's die HOP
uit de pool sneed. Een app kan niets anders zien — geen ander slot, niet de
HOP-kern, niet de control-structuren van een buur. Isolatie is de MMU, niet
een afspraak.

Bron: `metal/abi/layout` (de IPA-constanten + het board-PA-plan), `metal/kern/slots`
(laden/plaatsen), `metal/kern/stage2` (de kooi). Een app is een **service of hij
bestaat niet** — een app die stopt telt als crash; batch-werk = één service
met meerdere threads.

## Wat de app ziet (IPA, canoniek — zelfde op slot 1 en op slot 127)

```
0x5000_0000  ┌─────────────────────────────┐  RamStart (app), = SlotsBase
             │ tamago low-area:            │
             │  L1 @ +0x4000, L2 @ +0x5000 │  MMU-tabellen van de app-runtime
             │  L3 @ +0x7000/+0x8000       │  (tamago InitMMU); +0..+0x1000 = null-trap
0x5001_0000  ├─────────────────────────────┤  TEXT_START (= load + 0x10000)
             │ .text  (code, RX)           │  ← ELF PT_LOAD-segmenten
             │ .rodata (RO)                │
             │ .data  (RW)                 │
             │ .bss   (RW, genuld)         │  t/m runtime.end
             ├─────────────────────────────┤
             │ Go-heap  ↑ (groeit omhoog)  │
             │            ⋮                │
             │ stack    ↓ (groeit omlaag)  │
0x5000_0000  └─────────────────────────────┘  RamStart + RamSize − RamStackOffset (0x100)
 + RamSize                                     RamSize = partitie − 2MB (zie net-ringen)

0xB000_0000   control-page (4KB)   status/heartbeat/kill-flag/env  — eigen slot
0xB100_0000   hop-ABI-ringen (64KB) outbox (app→HOP: logs+RPC) / inbox (HOP→app)
0xC000_0000   net-ringen (2MB)     TX (app→switch) / RX (switch→app), rauwe frames
                                   — fysiek: de bovenste 2MB van de eigen partitie
```

`RamStart` en `RamSize` worden door HOP bij het plaatsen **in de image gepatcht**
(`slots.placeFromStaging`, het gedeelde plaats-pad van `Start`/`StartStaged`):
RamStart blijft het canonieke linkadres (0x50000000), RamSize = de partitie
mínus de bovenste 2MB — dat is de net-ring van het slot ("512MB → 510 Go +
2 netbuffer", `appRAMSize`). De app-runtime alloceert heap/stack dus binnen
precies zijn deel en declareert de ring-staart nooit als RAM: hij ziet die
uitsluitend device-gemapt op het canonieke 0xC000_0000 — coherentie zonder
cache-onderhoud, zonder statische ring-reservering in het board-plan. De
control-page/ringen op 0xB000_0000+ zijn per slot canoniek en door stage-2
naar de fysieke per-slot pagina's gemapt — dit is de hele hop-ABI
(`metal/app/applib`); alles daarbuiten bestaat voor de app niet.

## Fysiek (wat HOP beheert, per slot)

De partitie komt uit `partAlloc` (board-pool, `Plan.Pool`), grootte =
`align2M(memLimit)`, op fysiek adres `base`. De stage-2 mapt IPA `linkBase`
(0x50000000) → `base`, dus `delta = base − linkBase`; een segment op IPA
`p.Paddr` landt fysiek op `p.Paddr + delta`. Images zijn canoniek gelinkt op
het slot-1-bereik en draaien zo op elk slot — de MMU is de relocatie.

De control-page, hop-ABI-ringen en stage-2-tabellen liggen buiten de
partitie, op het board-PA-plan (`Plan.CtrlPA/RingPA/Stage2PA`), elk per slot
op `base + slot*stride`. De **net-ringen** liggen dáár niet: die zijn de
bovenste 2MB van de eigen partitie (de `netPA`-parameter die `kern/slots` per
lifecycle berekent en aan ring-init/`hopswitch.Attach`/`stage2.Build`
meegeeft) — ring-geheugen schaalt zo mee met wat er écht
draait i.p.v. een statische SlotCap-reservering. Alle vier liggen buiten
élke RAM-declaratie → device-gemapt → coherent zonder cache-onderhoud (de
partitie-staart: de app declareert hem niet als RAM, HOP raakt hem alleen
device-side, en de CleanInv over de hele partitie bij Start veegt de dirty
lines van de vórige huurder).

## Laadtijd: elke app haalt zijn eigen image op (twee-fase)

Elke core is onafhankelijk en heeft zijn **eigen opstart**: een app downloadt
zijn eigen image, op zíjn eigen core en netstack, rechtstreeks in zíjn eigen
partitie. Alles wat bij het starten kan gebeuren — de download, het parsen, het
in RAM zetten — speelt zich binnen die ene sectie af. Gaat er iets mis (trage of
kapotte download, te grote image, netwerkfout), dan raakt dat hooguit dat ene
slot; de buren en de kern merken er niets van. **Core 0 (de HOP-kern) blijft
daardoor altijd veilig**: hij draagt nooit een app-image of een download, laat
staan 127 tegelijk.

De opstart in drie stappen:

1. **HOP laadt de universele apploader** (`metal/app/apploader`) in de slot. Die zit
   ín de node-binary gebakken (`metal/kern/apploaderblob`, `//go:embed`, bouwtag
   `embedloader`) — een gedeelde blob, geen externe URL, geen fetch:
   `slots.StartLoader` kopieert 'm de partitie in en wekt de core (1 core,
   `HOP_IMAGE_URL` in de env).
2. **De apploader downloadt de echte image** over zíjn eigen netstack (`appnet`)
   naar een **staging-gebied bovenin zíjn eigen partitie**
   (`RamStart + RamSize − afgerond(imagegrootte)`), flusht de cache en seint HOP
   "staged" op de control-page (`StatusStaged` + `CtrlStagedSize`), waarna hij
   zijn core parkeert. Dít is de sectie waarin een startfout blijft.
3. **HOP plaatst de echte app** (`slots.StartStaged` → `placeFromStaging`):
   parseert de ELF uit die staging (`devReaderAt`), plaatst de segmenten
   device→device (`dev.Move`, 4KB stack-buffer), patcht RamStart/RamSize/slotHint,
   bouwt de stage-2-kooi en **her-dispatcht de geparkeerde core** op de echte app.

Het downloaden verdeelt zich zo over evenveel netstacks als er cores zijn — nooit
één trechter. Het geprivilegieerde deel (ELF plaatsen, stage-2 bouwen, core
dispatchen) blijft bij HOP: een app op EL1 kan zijn eigen kooi niet bouwen, dat
is de isolatie-invariant.

De staging leeft **alleen tijdens het laden**: hij ligt bovenin, waar straks de
stack/heap-top komt, en is verlaten vóór de echte app z'n eerste instructie
draait. Een segment dat tot in de staging zou reiken wordt geweigerd (partitie
te klein).

## Grenzen

- **Per-app maximum** = één GB-blok vanaf het linkadres, geklemd onder
  0xB000_0000 (de control-IPA's) — zie `maxLimitFor` in `metal/kern/slots`. Groter
  vergt een breder IPA-venster.
- **Aantal slots** = het aantal ontdekte app-cores (`layout.MaxSlots`, runtime
  gezet door het board: 127 op de Altra, 3 op de Pi, 11 op de O6N), begrensd
  door `SlotCap` (128) waarvoor de fysieke regio's zijn gereserveerd.
- **Nul gedeeld geheugen**: apps delen niets — communicatie loopt over de
  hop-ABI-ringen en het interne L2-net, nooit via gedeelde pagina's.
