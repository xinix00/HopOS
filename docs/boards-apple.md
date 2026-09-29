# Mac mini M4 op HopOS v3

De Mac mini M4 (Apple t8132, J773g, Mac16,10: 6 E-cores "sawtooth" in
cluster 0, 4 P-cores "everest" in cluster 1, 24 GB vanaf 1 TiB, een Broadcom
57762 achter een Apple-PCIe-rootpoort, de SSD achter de ANS-coprocessor) is
geport van de Go-kern (`OLD/metal/board/apple` met `hop/`, de drivers `aic`,
`nic/tg3`, `rtkit`, `smc`, `nvme/apple.go`, de lezers `fw/adt`, `fw/xnuboot`,
`fw/gpt`, en `OLD/image/apple-m4.sh` met `OLD/image/apple/`) naar Rust.
Alles hieronder is op de host getest en bouwt voor het target
(`image/apple-m4.sh` levert het raw image). Er is geen QEMU-model van de M4;
niets ervan heeft in Rust op ijzer gedraaid. Het dossier met elke Go-meting
(28-08 tot 04-09) is `OLD/docs/v1/archief/apple-m4.md`; de gedateerde lessen
staan in het commentaar van de code.

## Wat er is

| Deel | Waar | Getest |
| --- | --- | --- |
| ADT-lezer: nodes, properties, paden, `-`/`_` gelijk, de ranges-vertaling, diepte begrensd | `fw/src/adt.rs` | 9 host-tests (de Go-tests geport, plus afkapping op elke byte en een te diepe boom) |
| boot_args-lezer: RAM-contract, framebuffer, het ADT-adres met de virt-naar-fys-omrekening modulo 2^64 (30-08) | `fw/src/xnuboot.rs` | 3 host-tests met de getallen van 29-08 en de twee vormen van virt_base |
| GPT: tabel lezen, het grootste gat TUSSEN of NA de partities | `fw/src/gpt.rs` | 5 host-tests (de echte M4-tabel van 30-08) |
| AIC als `cpu::irq::Controller` (claim = ack, complete = masker open, het 4-bit doel), plus de fast IPI (kick, ack) | `driver/aic` | 6 host-tests op een nep-registerblok |
| tg3 (BCM57762): reset, MAC, MDIO/PHY, ringen, `netdev::Device`, INTx-kant | `driver/nic/tg3` | 24 host-tests, o.a. de drie lessen van 29-08 (INDIR_ACCESS, PCI_COMMAND, NIC_ADDR van de std-ring) en FCS |
| RTKit-mailbox (ASC) en de SART v3 | `driver/rtkit` | 16 host-tests tegen een nep-coprocessor |
| SMC: sleutels, floats, sensoren, de warmste | `driver/smc` | 7 host-tests |
| ANS-NVMe: lezen én schrijven, flush, nette shutdown, het schrijfvenster | `driver/nvme/src/apple.rs` | 13 host-tests: de vier ANS-lessen, weigering buiten of zonder venster, 512-naar-4K |
| Board: bootstub, VHE-ingang, 48-bit-map, console, boot_args en ADT, cores met klasse, AIC, timer-FIQ, fast IPI, PCIe-bring-up, tg3, ANS, SMC, watchdogs, p-states, slot-plan | `board/apple` | 16 host-tests (map, plan, pool uit het contract, tunables, os-core-keuze) |
| Linkscript: raw image op 0x101_0000_0000, stub op 0, kern op 0x10000 | `hopos/link-apple.ld` | link-asserties; `image/apple-m4.sh` toetst het parameterblok |
| Loader (m1n1-proxy), meetcyclus, console, installer | `image/apple/` | overgenomen van de Go-meetbank |

## Het plan

| Bereik | Wat | Mapping |
| --- | --- | --- |
| 0x100_0000_0000 tot `top_of_kernel_data` | iBoot: boot_args, ADT, trust cache, m1n1 met zijn spin-table | Device (2 MB) |
| `top_of_kernel_data` tot 0x101_0000_0000 | pool, laag deel | Device (2 MB) |
| 0x101_0000_0000 + 256 MB | kern-RAM: stub, scratch 0xE000, param-blok 0xE100, config 0xF000, kern 0x10000, BSS (met de tabellen), stack, heap | Normal WB |
| + 16 MB | DMA: tg3 8 MB, ANS 4 MB + RTKit-buffers 3 MB + SMC 1 MB | Normal-NC |
| + 16 MB | control-pages, kooien, flip-recorder, zwarte doos | Device |
| + 64 MB | boot-scratch, handoff, staging (met magic) | Normal WB |
| 0x101_1600_0000 tot `phys_base + mem_size` | pool, hoog deel | Device (2 MB) |
| alles onder 512 GB | MMIO (AIC, dockchannel, APCIe, ANS, PMGR) | Device (1 GB) |

Nooit een 1 GB-blok boven 2^40: dat geeft op dit silicium een "address size
fault, level 0" (28-08). De pool komt uit iBoot's contract (29-08: 23,7 GB in
twee regio's); zonder boot_args 1 GB boven het venster, luid.

## Bouwen en laden

```sh
sh image/apple-m4.sh                         # target/apple-m4/hopos-apple.img
APP=appspike sh image/apple-m4.sh            # plus stage.elf voor de staging
PYTHON=~/Git/m1n1/venv/bin/python3 CFG=hopos-m4.cfg \
  sh image/apple/boot-cycle.sh target/apple-m4/hopos-apple.img 90
STAGE=target/apple-m4/stage.elf ROLE=app ...  # een image in de staging
AT=0x10080000000 ...                          # de stub laten verplaatsen
image/apple/console.py /dev/cu.kis-100000-ch-0 60   # zonder m1n1
```

De meetbank (dossier, "Meetbank-recept"): `macvdmtool reboot serial`, laden
over m1n1's USB-gadget, `macvdmtool debugusb`, booten en de console over de
dockchannel (`/dev/cu.kis-100000-ch-0`). Chrome en Spotify houden het
Debug-USB-device vast: dicht voor je begint (29-08). De loader springt met
`P_VECTOR` naar offset 0x800 met x0 = het échte boot_args-blok: dezelfde
overdracht als na `kmutil configure-boot --raw --entry-point 2048`.

`hopos.cfg`-sleutels van dit board: `hopos.oscore`, `hopos.pstate=off|E,P`,
`hopos.disk=off`, `hopos.smc=1`, `hopos.cages=on`.

## Wat de kern voor Apple nog mist

Het board levert de bouwstenen; de lijm en `cpu` zijn van andere sporen.
Tot die er zijn, WEIGERT `board_apple::slots::plan` luid
(`HOPOS_SLOT_PLAN`), zodat een ijzertest niet in de kooicode sterft; met
`hopos.cages=on` gaat de rem eraf.

1. **De EL2-smaak in de lijm.** `hopos/src/cage.rs` zet `FLAVOR = Flavor::Nvhe`
   vast. Op de M4 moet dat `board_apple::FLAVOR` (`AppleVhe`) zijn, bijvoorbeeld
   `pub(crate) const FLAVOR: Flavor = vboard::FLAVOR;` met die constante op elk
   board. Met Nvhe schrijft de zelftest van de OS-core via de EL1-encoderingen
   de EL2-registers van de kern zelf over (E2H is RES1).
2. **`OsCore` voor `AppleVhe`** (`cpu/src/el2/oscore.rs` weigert nu met
   `OsCoreFlavor`). Nodig:
   - een kick die geen GIC-SGI is: `Bell` met een Apple-vorm (het
     IPI_RR_GLOBAL-doel `core | cluster << 16` in plaats van `sgi1r`), en de
     switcher van een app-core die `SCHED_OS_KICK` via `msr s3_5_c15_c0_1`
     stuurt in plaats van `icc_sgi1r_el1`;
   - de peek (`pending`) als IPI_SR_EL1 bit 0 lezen zonder te acken, en de
     ack op EL2 bij terugkeer (W1C op IPI_SR_EL1): een ongeackte fast IPI
     blijft staan en de core komt nooit meer tot slapen (04-09);
   - in het HCR van een bewoner FMO (en IMO voor de AIC), en een
     `fiq/lower-a64`-ingang in de OS-core-vectoren die als `Back::Ipi` of
     `Back::Timer` terugkeert (de timer van de kern is de CNTHP, een FIQ);
   - de zelftest-kick via `board.kick_self()` bestaat al (fast IPI naar
     zichzelf); de WFI-slaap in `Resident` wekt op die FIQ, maar alleen op
     een core die de firmware zelf opzette (zie punt 4).
3. **Een CPU_ON van het board in `cpu::smp`.** Er is geen PSCI (een SMC
   zonder EL3). `board_apple::Apple::cpu_on` kiest m1n1's spin-table of
   PMGR plus de brievenbus van `stub_reset`; `cpu::smp::start_one` en het
   PSCI-pad van `cage.rs` moeten die kunnen roepen. Onder m1n1 komt een
   vrijgegeven core binnen als FUNCTIE met m1n1's MMU aan (29-08): de
   SMP-trampoline moet eerst SCTLR.M/C/I wissen en geen set/way-onderhoud
   doen. Tot dan blijft de kern op de boot-core en zegt `hopos.oscore=small`
   waarom niet (`HOPOS_OSCORE_FALLBACK`).
4. **De keuze van de slaap per core.** Het board meet de timer-FIQ
   (`HOPOS_APPLE_IDLE`) en kiest WFI of WFE; een OS-core die de kern zelf
   opbracht, krijgt op t8132 geen timer-FIQ (CYC_OVRD en
   VM_TMR_FIQ_ENA_EL2 vergrendeld, 29-08). De hop naar de zuinige core moet
   dus op de nieuwe core opnieuw meten.
5. **`cpu::idle::ArmSleeper::new` onder E2H = 1.** Vanaf EL2 schrijft
   `cntkctl_el1` dan CNTHCTL_EL2: de event-stream klopt, maar EL1PCTEN en
   EL1PTEN (bits 10, 11) gaan weg. Het board zet ze terug na `new`; de O6N
   (`Vhe`) heeft vermoedelijk hetzelfde.
6. **De watchdog en de NIC-interrupt.** `hopos/src/watchdog.rs` kent alleen
   de SBSA-watchdog; het board levert `wdt::arm`/`wdt::pet` (24 MHz, 30 s).
   De INTx-bedrading van de tg3 (Go 19-09: het AIC-nummer uit de ADT plus
   een scan over negen lijnen) is niet geport: de pomp pollt.

## Checklist voor de M4

Per stap wat de console (dockchannel) hoort te tonen, en wat een afwijking
betekent. Onder m1n1 eerst; pas daarna installeren.

1. **Eerste licht** vóór de MMU: `hopos x000001000xxxxxxx` (x0 = boot_args).
   Niets: de stub draaide niet (verkeerde offset in de loader) of de
   dockchannel staat elders. x0 = 0: m1n1 gaf geen boot_args (P_VECTOR).
2. **De bunny en `runtime ...`**: de MMU, VBAR en de heap staan. Stil na
   regel 1: de map (`mmu::build`) of het VHE-regime; een
   `exception: ... HOPOS_EXCEPTION` met FAR in het DRAM wijst naar de map.
3. `xnuboot: rev 3.x RAM 0x10001374000+24053 MB ... HOPOS_APPLE_BOOTARGS`
   en `adt: ... 10 cores (6 E + 4 P), this is core 6, serial ...`.
4. `watchdog: firmware watchdogs silenced: reg[0] ... (N window(s), ...)`.
   Zonder deze regel reset de node natief na 1:43 (31-08).
5. `cores: via m1n1's spin-table (...)` onder m1n1; `cores: ours` na de
   installatie. `cpufreq: cluster 0 (E): pstate 1 (900 MHz) -> 5/...`.
6. `boot: HopOS v3.0.0 on apple-m4, EL2, 10 cores (big), 24576 MB DRAM
   HOPOS_BOOT`.
7. `disk: ans 0x481600000 nvmmu 0x485cc0000 nvme 0x4c5cc0000 sart
   0x485c50000 (v3) secure-bar true`, dan de GPT-regels
   (`iBootSystemContainer`, de APFS-container, `RecoveryOSContainer`), en
   `HOPOS_ANS_UP` met het venster, of `HOPOS_ANS_FULL` als macOS nog niet
   gekrompen is (dan geen hopfs, en dat is goed). Komt de ANS niet ready:
   `resetting the ANS power domain and retrying` hoort er één keer te staan.
8. `irq: AIC v... at 0x381000000: ... target N reaches this core, fast IPI
   and timer FIQ`. `no AIC target reaches this core`: de doel-meting via
   ISR_EL1 werkt niet op dit silicium; de node draait door op de vangrail.
9. `apcie: config 0x1cb0000000 ...`, per stap een regel (een uitgestelde
   abort landt bij de stap die hem veroorzaakte), `apcie: brought up by
   HopOS, 6 power gate(s), 263 tunable(s), 2 of 3 port(s)` (29-08), `apcie:
   link up, endpoint 02:00.0 14e4:1682`, `tg3: ASIC 0x57766 ...`, `tg3:
   LINK UP, 1000 Mb/s full duplex`, `HOPOS_NIC_UP mac=1c:f6:4c:54:fa:90`.
   Onder m1n1 kan de eerste regel `apcie: already up` zijn.
10. **DHCP** (`HOPOS_NET_UP`) en de tik: `HOPOS_TICK n sleeps=...`.
    Wekken per seconde rond 954 (29-08): `HOPOS_APPLE_IDLE` moet dan `Wfi`
    zeggen. Veel meer: WFE op de event-stream; controleer EVNTIS (ECV).
11. `slots: plan refused: ... HOPOS_SLOT_PLAN` en `oscore: ...
    HOPOS_OS_CORE_FAIL`: verwacht, tot de punten hierboven er zijn.
12. **Met `hopos.smc=1`**: één temperatuur via de SMC (nog niet in de
    bootlog van de kern; `Apple::temp_milli_c`). Onder Go kwam het
    shmem-adres nooit (31-08).
13. **Installeren** (1TR, fysiek; `image/apple/install.sh`): `kmutil
    configure-boot -c hopos-apple.img --raw --entry-point 2048
    --lowest-virtual-address 0 -v "/Volumes/Macintosh HD"`. Daarna moet
    stap 5 `cores: ours` zeggen: dat is de meting dat RVBAR naar ons image
    wijst. Terug: dezelfde regel met `m1n1.bin`.

## Niet gedaan

- Een mini-proxy in het image (elke iteratie na de installatie kost een
  1TR-bezoek); de config ingebakken in het image (`cfgblob`): zonder loader
  is er nu geen `hopos.cfg`.
- De NIC-interrupt (INTx over de AIC), de watchdog-bedrading in de kern, de
  PStateWatch-taak, `pmgr_reset` als herstelpad na een crash buiten de
  retry in `probe_disk` (die er wel is).
- De framebuffer: iBoot laat er een achter, maar niemand scant hem uit (Go
  30-08); bewust uit.
