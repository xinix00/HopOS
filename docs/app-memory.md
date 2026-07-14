# App-geheugen — de adresruimte van een HopOS-app

Wat een app "ziet" is op elk slot identiek (het **canonieke IPA-beeld**); de
stage-2-kooi (hardware) vertaalt dat naar de fysieke per-slot regio's die HOP
uit de pool sneed. Een app kan niets anders zien — geen ander slot, niet de
HOP-kern, niet de control-structuren van een buur. Isolatie is de MMU, niet
een afspraak.

Bron: `metal/layout` (de IPA-constanten + het board-PA-plan), `metal/slots`
(laden/plaatsen), `metal/stage2` (de kooi). Een app is een **service of hij
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
 + RamSize                                     RamSize = job.MemoryLimit

0xB000_0000   control-page (4KB)   status/heartbeat/kill-flag/env  — eigen slot
0xB100_0000   hop-ABI-ringen (64KB) outbox (app→HOP: logs+RPC) / inbox (HOP→app)
0xB300_0000   net-ringen (2MB)     TX (app→switch) / RX (switch→app), rauwe frames
```

`RamStart` en `RamSize` worden door HOP bij het laden **in de image gepatcht**
(`slots.StartStream`): RamStart blijft het canonieke linkadres (0x50000000),
RamSize = de `job.MemoryLimit`. De app-runtime alloceert heap/stack dus binnen
precies zijn toegewezen partitie. De control-page/ringen op 0xB000_0000+ zijn
per slot canoniek en door stage-2 naar de fysieke per-slot pagina's gemapt —
dit is de hele hop-ABI (`metal/applib`); alles daarbuiten bestaat voor de app
niet.

## Fysiek (wat HOP beheert, per slot)

De partitie komt uit `partAlloc` (board-pool, `Plan.Pool`), grootte =
`align2M(memLimit)`, op fysiek adres `base`. De stage-2 mapt IPA `linkBase`
(0x50000000) → `base`, dus `delta = base − linkBase`; een segment op IPA
`p.Paddr` landt fysiek op `p.Paddr + delta`. Images zijn canoniek gelinkt op
het slot-1-bereik en draaien zo op elk slot — de MMU is de relocatie.

De control-page, hop-ABI-ringen, net-ringen en stage-2-tabellen liggen buiten
de partitie, op het board-PA-plan (`Plan.CtrlPA/RingPA/NetRingPA/Stage2PA`),
elk per slot op `base + slot*stride`. Ze liggen buiten élke RAM-declaratie →
device-gemapt → coherent zonder cache-onderhoud.

## Laadtijd: de image streamt de partitie in (sinds 14-07)

HOP buffert de app-image **nooit volledig in de kern-RAM** (dat was de
core-0-OOM onder een job-storm). In plaats daarvan streamt `slots.StartStream`
de download-body rechtstreeks een **staging-gebied bovenin de partitie** in
(`base + size − afgerond(imagegrootte)`), parseert de ELF uit dát
device-geheugen (`devReaderAt`) en plaatst de segmenten device→device
(`dev.Move`, 4KB stack-buffer). Core 0 houdt zo per fetch alleen ~64KB vast,
niet de hele image — 127 gelijktijdige fetches passen probleemloos, en een
te grote/kapotte image raakt hooguit zijn eigen partitie.

De staging leeft **alleen tijdens het laden**: hij ligt bovenin, waar straks
de stack/heap-top komt, en is al verlaten vóór de app z'n eerste instructie
draait (de segmenten staan dan onderin, de core wordt pas daarna gewekt). Een
segment dat tot in de staging zou reiken wordt geweigerd (partitie te klein).

## Grenzen

- **Per-app maximum** = één GB-blok vanaf het linkadres, geklemd onder
  0xB000_0000 (de control-IPA's) — zie `maxLimitFor` in `metal/slots`. Groter
  vergt een breder IPA-venster.
- **Aantal slots** = het aantal ontdekte app-cores (`layout.MaxSlots`, runtime
  gezet door het board: 127 op de Altra, 3 op de Pi, 11 op de O6N), begrensd
  door `SlotCap` (128) waarvoor de fysieke regio's zijn gereserveerd.
- **Nul gedeeld geheugen**: apps delen niets — communicatie loopt over de
  hop-ABI-ringen en het interne L2-net, nooit via gedeelde pagina's.
