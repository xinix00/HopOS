# LAATSTE_PLAN — de boot onder het contract

**Stand: 02-09-2026.** Het generieke contract staat: één `board.Board`, `board.Cores`
met nil-poten, `cage.go` als naam van de kooi-naad, `idle.Sleeper`, `cpu/irq`,
arch-neutrale plan-namen. Alles wat *draait* gaat door dezelfde functies, voor
apps én voor HOP zelf. Wat er nog naast staat is hoe een core *gaat* draaien: de
boot. Dit plan legt die er ook onder. Daarna is het contract compleet, van
reset-vector tot Go.

## Waarom

De "EL2 → EL1"-drop — VBAR, HCR, CNTHCTL, CNTVOFF, SPSR, ELR, ERET — staat acht
keer in de boom. De EL1-landing — SCTLR schoon, stack uit RamStart/RamSize,
spring naar rt0 — zes keer.

| bestand | regels | drop | landing | wat er écht board-eigen is |
|---|---|---|---|---|
| `board/qemuvirt/cpuinit.s` | 124 | ja | ja | 3 adressen |
| `board/rk3566/cpuinit.s` | 116 | ja | ja | 3 adressen |
| `board/raspi/cpuinit_body.h` (Pi 4 + 5) | 419 | ja | ja | UART-banner, SMPEN op de A76 |
| `board/uefi/init.s` | 631 | ja | ja | ~480 regels UEFI-loaderwerk |
| `board/apple/cpuinit.s` | 479 | ja | ja | hop/park/adoptie, dockchannel, vectortabellen |
| `board/hopslot/cpuinit_arm64.s` (app) | 77 | ja | ja | niets |
| `cpu/el2/el2.s` (trampoline) | 143 | ja | nee | stage-2 |
| `cpu/el2/smp.s` | 259 | ja | nee | SMP-handoff |

Acht kopieën, en ze zijn al uit elkaar gegroeid op precies de punten waar het
pijn deed:

- **SCTLR_EL1 op een bekende waarde** vóór de ERET staat alleen in apple (de
  flip-fix van 02-09: EL1 erfde de MMU van de vórige kern) en in de trampoline
  (de Pi 5-fix van 10-07: een warme CPU_ON erfde EL1 van de vorige huurder). De
  andere zes erven wat er staat — dezelfde bug, in wachtstand.
- **CNTHCTL E2H-bewust** (beide lay-outs) staat alleen in apple.
- **CPTR_EL2 zonder FP-trap** staat alleen in de trampoline.

Elke fix is één plek gefixt en zeven niet. Dat is geen slordigheid, het is de
vorm: zolang de drop per board geschreven wordt, wordt hij per board gefixt.

## Het ontwerp

Het patroon staat al in de boom: de Pi's delen één body via `#include`, met de
board-defines ervoor en `#define RPI5` als haak. Dat generaliseren, twee lagen
diep. Alles op preprocessor-niveau: geen symbolen, geen indirectie, niets op
runtime.

### 1. `cpu/el2/drop.h` — de drop als macro

Eén `DROP_TO_EL1(entry)` met de hele reeks in de goede volgorde en met de drie
fixes erin:

```
SCTLR_EL1 = 0x30d00800     (RES1; M/C/I/A/WXN uit — nooit erven)
CPTR_EL2  = geen FP-trap
CNTHCTL   = EL1-toegang in beide lay-outs (E2H 0 én 1)
CNTVOFF   = 0
SPSR_EL2  = EL1h, DAIF gemaskeerd
ELR_EL2   = entry
ISB; ERET
```

Gebruikt door de trampoline, smp.s en elke boot — zoals `hygiene.h` nu al de
cache-veeg deelt. HCR blijft bij de aanroeper: de boot zet RW, de trampoline
RW|TSC|VM. Dat is het enige echte verschil tussen de twee, en het hoort
zichtbaar te blijven.

### 2. `cpu/el2/boot.h` — de boot als body

Eén `cpuinit`: EL bepalen, boot-EL en x0 naar de scratch, VBAR_EL2 op de
trap-vectoren, HCR RW, de drop, en de EL1-landing. Een board levert vóór de
include alleen zijn adressen en zijn haken:

```
#define BOOT_SCRATCH  ...   // boot-EL, x0 (DTB of param-blok)
#define TRAP_VEC      ...   // = layout.TrapVecPA (pariteit in het board-init)
#define BOARD_EARLY   ...   // vóór alles, MMU uit: Apple's hop en parkeren; de Pi-banner
#define BOARD_EL2     ...   // op EL2, ná HCR: Pi 5's SMPEN, Apple's HCR/CNTHCTL-teruglezing
#define BOARD_EL1     ...   // op EL1, vóór de stack: Apple's vroege faultdumper
#include "../../cpu/el2/boot.h"
```

Elke haak heeft een lege default (`#ifndef`). Een board zonder
eigenaardigheden is dus drie defines en een include.

### 3. Wat per board blijft, en terecht

- **Apple**: de hop en het parkeerprotocol (param-blok, brievenbus), de
  dockchannel-printers, de EL2/EL1-faultdumpers. Dat is waar iBoot en m1n1 écht
  anders zijn.
- **Pi**: de UART-banner en SMPEN.
- **UEFI**: de loader (memory map, GOP, carve, verplaatsen) — dat is geen boot
  maar een loader, en hij eindigt in dezelfde `cpuinit`.
- **LicheeRV**: de boot-hart-loterij.

Alles daarna is dezelfde vijftien instructies.

### Uitkomst

| bestand | was | geschat | **is (02-09)** |
|---|---|---|---|
| qemuvirt/cpuinit.s | 124 | ~25 | 17 |
| rk3566/cpuinit.s | 116 | ~25 | 23 |
| hopslot/cpuinit_arm64.s | 77 | ~20 | 19 |
| raspi/cpuinit_body.h (+ rpi4/rpi5) | 419 (+41) | ~120 | 286 (+25) |
| uefi/init.s | 631 | ~550 | 595 |
| apple/cpuinit.s | 479 | ~250 | 429 |
| el2.s + smp.s | 402 | ~360 | 368 |
| drop.h + boot.h (nieuw) | — | — | 208 |

Netto 799 regels eruit en 261 erin (git diff, zonder de twee nieuwe headers),
en één plek voor de volgende fix. De board-bestanden zijn groter gebleven dan
geschat omdat de lessen erin staan: raspi houdt zijn faultdumpers en
cache-veeg, apple zijn hop/parkeerprotocol en dockchannel-printers, en de
commentaren zijn meeverhuisd naar de haken in plaats van geschrapt. Het contract
begint dan bij de reset-vector, niet bij Go: een board levert zijn PA-plan,
zijn `Cores`, zijn `Privilege`/`Firmware`, en zijn boot-defines. Niets in de
kern, niets in `cpu/`.

## Volgorde en bewijs

| stap | wat | bewijs |
|---|---|---|
| 1 | `drop.h`, in de trampoline en smp.s | **GEDAAN 02-09**: gate groen, QEMU-demo 21/21 (incl. SMP), M4 kern I (generatie 3): warme herdispatch op cpu 1 en 2 én een verse op cpu 5, alle drie 0% cpu en HTTP; disassembly H↔I van beide trampolines is dezelfde MSR-reeks. Open: een app met TWEE cores viel op de M4 om (EL1-exception in `runtime.lock`, core 4 parkeerde daarna niet) — SMP op Apple was sinds 7fef549 nooit op ijzer bewezen, dus niet aan de drop toe te schrijven en apart te jagen |
| 2 | `boot.h`; qemuvirt en hopslot erop | **GEDAAN 02-09**: qemuvirt = twee defines + include (124 → 16 regels), hopslot = alleen de include (77 → 20); demo 21/21, agent boot + appspike-dispatch op QEMU |
| 3 | apple erop, met zijn drie haken | **GEDAAN 02-09**: `BOARD_EARLY` = hop/parkeren + TLBI + EL2-tabel + banner, `BOARD_EL2` = HCR/CNTHCTL-teruglezing, `BOARD_EL1` = de EL1-dumper, elk een `BL` naar een eigen TEXT; VHE-bouwguard (zonder `-D=VHE` faalt de assembly met de reden als naam). M4 kern J via de flip: de geflipte kern liep zijn eigen `cpuinit`, welcome warm op cpu 1 en vitals op cpu 2, 0% cpu, HTTP ok |
| 4 | rk3566, raspi, uefi erop | **GEBOUWD 02-09**, gate groen. rk3566 = twee defines + include. raspi: `BOARD_EARLY` = 'P'+EL en de faultdump2-tabel, `BOARD_EL2` = de Linux-init_el2-registers, `BOARD_EL1` = faultdump-tabel + D-cache-veeg + 'R','p'; rpi4/rpi5 = alleen de UART-basis (SMPEN en `RPI5` waren al dood). uefi: de loader blijft, `BOOT_ENTRY = bootKernel`, boot-EL naar de Go-global in `BOARD_EL1`, VBAR_EL2 (RamStart+offset) in `BOARD_EL2`. **Bewezen op QEMU**: EDK2 → PE-loader → boot.h → agent up + app-dispatch. **Open**: Pi 4/5 en Radxa elk één kaart-boot (Derek) |
| 5 | riscv64: één board, zelfde vorm zodra er een tweede komt | **BEKEKEN 02-09, niets te vouwen**: de LicheeRV-boot (187 regels) blijft in M-mode en dropt dus niet — hart-loterij, vendor-CSR's, stack, rt0; de S-mode-landing van een app (`cpu/slotstart`, 55 regels) is SIE uit, FS aan, stack, rt0. Gedeeld zijn vijf regels stack. De echte drop (M→S, `sret`) zit al op één plek, in de kooi-plaatsing van `cpu/mmode`. Pas bij een tweede riscv64-board wordt dit een body |

Eén verandering per boot-cyclus, zoals altijd. Stap 1 is de gevaarlijkste en
tegelijk de best te bewijzen: elke app-start loopt erdoorheen.

## Wat dit niet is

- **Geen nieuwe boot-route.** Wie ons aflevert (iBoot, TF-A, U-Boot, UEFI, de
  FSBL) blijft wie het is; alleen wat wij daarna doen wordt één ding.
- **Geen runtime-contract.** Dit is assembly vóór Go; de haken zijn macro's,
  geen functie-pointers. Een board dat een haak nodig heeft schrijft hem in
  zijn eigen `cpuinit.s`, naast zijn defines.
- **Niet in één keer.** Dit is de enige code die alleen op ijzer te bewijzen
  is. QEMU en de M4 kunnen zonder hulp; de Pi's, de Radxa en de Altra vragen
  elk één kaart-boot. Tot die gedaan is blijft stap 4 bouwbaar maar onbewezen,
  en landt hij per board, niet als golf.

## Wat hierna nog open staat

- **Kaart-boots voor stap 4**: Pi 4, Pi 5 en de Radxa Zero 3 draaien nu
  dezelfde boot als QEMU, de M4 en UEFI-op-QEMU, maar dat is op die drie
  alleen gebouwd, niet geboot. Eén kaart per board.
- **Twee cores op de M4 — de crash is OPGELOST (03-09)**: de RX-doorbell wekte
  de pomp-goroutine met tamago's `WakeG`, dat de timer-heap zonder lock
  herschrijft; op twee cores sloopt de ene core zo de heap van de andere.
  Nu `runtime.IdleMayReady` plus de sysmon-lite (`NextTimer`/`RunIdleTimers`) in de tamago-go-fork
  (`tools/tamago-go/0001-runtime-idlehook.patch`): onder de timer-lock,
  alleen een nog lopende slaap. Bewijs: 2 cores, 150 s load, 5440 requests,
  0 fouten.
- **Yield-idle voor SMP-apps — GEBOUWD EN OP DE M4 BEWEZEN (03-09)**: de
  Linux-vorm, een reschedule-IPI. De runtime wekt een slapende sibling via
  `goos.Wake` (semawakeup, preemptM, RunIdleTimers), `cpu/smp` maakt er HVC #4
  van, en de switcher wekt de sibling (`CtxKickTarget`, `CtxUnitSlot`,
  `CtxWakes`). De echte fout eronder: de switcher zocht de bewoner via de
  VMID, en een tweede core deelt die van zijn primaire — nu `SchedCurrent`.
  M4, kern Q, vitals met twee cores: cpu 0%, idle 100%, requests 14 ms
  mediaan, 120 s load met 882 requests en 77 GC-rondes zonder fout,
  verwijderen parkeert beide cores. QEMU met `hopos.idleyield=1` idem, op
  een enkele trage request na (de rotatie-peek; op de M4 doet HOP's wekker
  het). Observatie: de core met affiniteit 0 slaapt op EL2 niet in WFI
  (pendende interrupt van de boot-core, hoort bij de AIC-post hieronder).
- **Stap 5, riscv64**: één board (LicheeRV), dezelfde vorm zodra er een tweede
  komt.
- **Apple idling**: gebouwd op 02-09 als yield-naar-EL2 (`PrepYieldIdle`),
  WFI in de switcher en een fast IPI als `Cores.Kick`, met HOP's wekker —
  m1n1's park-recept op ditzelfde silicium. Gemeten: 0% cpu, ~47 wekken/s op
  E- én P-cores, en de `CtxSleeps`-meetlat bewijst dat de core op EL2 slaapt:
  ~85 slaapjes/s, gelijk aan het kick-tempo — geen spin. CYC_OVRD is op de M4 een dood spoor (EL1 én EL2
  undefined; Linux en m1n1 raken het op de M4 nooit aan).
- **Flip en geparkeerde cores**: geparkeerde cores blokkeren een flip niet
  (ze staan in de gegenereerde parkeerlus, identiek in elke kern); alleen
  bewoners binden de switch-code. `Cores.Reset` bestaat als poot, maar Apple
  heeft nog geen bewezen core-down (PMGR-stop stopt niets, gemeten 02-09).
- **IRQ op ijzer**: de NIC-interrupt is op QEMU bewezen; de M4 heeft zijn AIC
  en PCIe-MSI nog nodig, de LicheeRV een PLIC, de Pi's een GICv2.
