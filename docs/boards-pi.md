# Raspberry Pi 4 en 5 op HopOS v3

De Pi 4 (BCM2711) en de Pi 5 (BCM2712) zijn geport van de Go-kern
(`OLD/metal/board/raspi`, `rpi4`, `rpi5`, de drivers `vcmail`, `gicv2`,
`nic/genet`, `nic/gem`, `brcmpcie`, en `OLD/image/rpi4-agent.sh`,
`rpi5-agent.sh`) naar Rust. Alles hieronder is op de host getest en bouwt
voor het target; de Pi 4-kern boot bovendien op QEMU `raspi4b` tot de
executor-tik. Niets ervan heeft in Rust op ijzer gedraaid. De Go-metingen
(07-07 tot 21-09) staan in het commentaar van de code; deze checklist zegt
per stap wat de console moet tonen en wat een afwijking betekent.

## Wat er is

| Deel | Waar | Getest |
| --- | --- | --- |
| Gedeeld Pi-board: de ingang op 0x80000 (EL2-init, cache-invalidate, event-stream), de DTB uit x0, de geheugenkaart uit de DTB (runtime bij de vaste tabel), de pool, de staging uit `/chosen/linux,initrd-*`, `hopos.*` uit de cmdline, de mailbox, de GIC-400 via `cpu::irq`, de CNTHP-lijn van de OS-core | `board/raspi` | 10 host-tests: kaart (1 en 8 GB Pi 4), pool, slot-plan, staging, cmdline, MAC, DTB-status |
| Pi 4: adressen, A72-nummering (aff0), vaste tabellen (GB0 en GB3), GENET-probe | `board/rpi4` | 2 host-tests, const-asserties op de tabellen; QEMU `raspi4b` |
| Pi 5: adressen, A76-nummering (aff1), vaste tabellen (GB0, 64, 65, 124), de RP1-keten en de NIC-interrupt via de MIP | `board/rpi5` | 2 host-tests, const-asserties op tabellen en adressen |
| VideoCore-mailbox: temperatuur, klokken, board-MAC, framebuffer | `driver/vcmail` | 7 host-tests (bericht-indeling, respons-codes, de "pending"-regel, een rondgang met een nep-firmware) |
| GIC-400 als `cpu::irq::Controller`, plus de kick van de OS-core: SGI 8 via GICD_SGIR, de GICC_HPPIR-peek, de EOI met de bron-core | `driver/gicv2`, `board/raspi` (`KICK_SGI`, `os_bell`, `kick_self`) | 5 host-tests op nep-GICD/GICC; QEMU `raspi4b`: `HOPOS_OS_SELFTEST ok` |
| GENET v5 (Pi 4) | `driver/nic/genet` | 9 host-tests: reset met DMA-stopbevestiging, ringen die de hardwaretellers volgen, QTAG, PROD-wrap, volle ring, RX-pad en foute frames, MDIO |
| Cadence GEM (Pi 5) | `driver/nic/gem` | 7 host-tests: ringen met de bus-offset, DBW uit DCFG1, AMP, TX-overdracht, RX en rearm, de expliciete ISR-ack |
| Broadcom-STB PCIe-RC (Pi 5, en de BCM2711-variant) | `driver/brcmpcie` | 6 host-tests: window-codering, setup (UBUS, PLL, burst, VDM), BCM2711-reset, link-fout, endpoint en BAR's, RESCAL |
| Linkscript: raw image op 0x80000, de ingang vooraan | `hopos/link-raspi.ld` | de builds, `_pi_start == 0x80000` als link-assertie |
| Kaart | `image/rpi4.sh`, `image/rpi5.sh` | leveren `kernel8.img` / `hop-agent5.img` en het kaart-image |
| QEMU-rook | `tools/qemu-rpi4-test.sh` | groen: P2, bunny, DTB, kaart, mailbox, GIC-400, kooi, staging, de zelftest van de OS-core (timer, yield, kick naar zichzelf), drie tikken met `kicks=1` |

## Het plan (beide Pi's gelijk)

| Bereik | Wat | Mapping |
| --- | --- | --- |
| 0x0000_0000 tot 0x0008_0000 | TF-A (BL31 of de armstub): niet van ons, ook geen cache-onderhoud | Normal (vaste tabel) |
| 0x0008_0000 tot 0x0800_0000 | kern-RAM: image, stack, heap | Normal WB |
| 0x0800_0000 tot 0x1000_0000 | laadvenster: DTB 0x0F00_0000, boot-scratch 0x0F10_0000, rolwoord +0x100, staging 0x0F20_0000 (14 MB) | Normal WB |
| 0x1000_0000 tot 0x1400_0000 | control-pages en kooi-regio | Device |
| 0x1400_0000 tot 0x1500_0000 | DMA: NIC 8 MB, mailbox-buffer 0x1480_0000 | Normal-NC |
| vanaf 0x1500_0000 | de pool: DTB `/memory` min `/memreserve/`, runtime gemapt | Normal WB |

## Bouwen en flashen

```sh
sh image/rpi4.sh            # Hop uit ../hop/hop; APP=appspike voor het ABI-bewijs
sh image/rpi5.sh
diskutil unmountDisk /dev/diskN
sudo dd if=target/hopos-rpi4.img of=/dev/rdiskN bs=4m    # of hopos-rpi5.img
```

Zonder kaart-image (geen firmware of geen `go` voor `OLD/image/mkcard`):
kopieer `target/sd-rpi4/*` (of `sd-rpi5/*`) op de FAT-partitie van een
bestaande Pi-kaart. De firmware komt uit `OLD/sd-rpi4` en `OLD/sd-rpi5`
(niet in git; herkomst in hun `LEESMIJ.txt`). Pi 4: `start4.elf`,
`fixup4.dat`, `bcm2711-rpi-4-b.dtb` en een zelfgebouwde TF-A `bl31.bin`
(VERPLICHT: de stock armstub8 heeft geen PSCI). Pi 5:
`bcm2712-rpi-5-b.dtb` en `overlays/bcm2712d0.dtbo`.

Nieuw ten opzichte van Go: `hopos.cfg` staat niet meer op de kaart. Het
`initramfs`-kanaal draagt nu het image van Hop (`hop.elf`), en de config is
`cmdline.txt`: `hopos.stage=hop|app` (standaard hop), `hopos.cores=N`,
`hopos.oscore=` (op de Pi altijd core 0).

UART: Pi 4 op de header (pin 8 TXD, 10 RXD, 6 GND, 3V3), `screen
/dev/tty.usbserial-* 115200`. Pi 5 op de 3-pins debug-connector tussen de
HDMI-poorten, `screen /dev/tty.usbmodem* 115200`.

## De checklist voor morgen

Per stap: wat er moet staan, en wat het betekent als het er niet staat.

1. **Bootloader-log** (`uart_2ndstage=1`). Niets: kabel, poort, of
   `BOOT_UART=1` in de EEPROM-config.
2. **`P2`**: de Pi-ingang draait op EL2. Wel bootloader-log maar geen P:
   het laden faalt (config.txt, DTB, armstub; Pi 5: `os_check=0`). `P1`:
   geen EL2 van de firmware; de kern meldt `HOPOS_BOOT_EL` en parkeert.
3. **De bunny en `runtime rustc`**: MMU aan met de vaste tabel, BSS, stack,
   `kmain`. P2 en dan niets: de MMU-stap (tabellen in `.data.pagetables`,
   TCR/MAIR van `cpu::boot`) of een fault vóór de console; een
   `exception: ... HOPOS_EXCEPTION`-regel noemt ESR, ELR en FAR.
4. **`fdt: N bytes at 0xf000000, model "Raspberry Pi 4 Model B", serial
   "1000000..."`**. Een ander adres: `device_tree_address` niet gehonoreerd
   (de kern accepteert alles tussen 0x80000 en 0x1000_0000). `WARNING
   HOPOS_RAM_CHECK_SKIPPED`: x0 was geen DTB.
5. **`stage: N KB at 0xf200000, role hop`**. `no initramfs`: de
   `initramfs`-regel in config.txt of `hop.elf` op de kaart ontbreekt.
6. **`mem: B banks, M MB mapped from the DTB, pool P MB`**. Verwacht: Pi 4
   8 GB rond 7,5 GB, Pi 5 8 GB rond 7,9 GB. Een fault direct na deze regel:
   de runtime-mapping (de TLB-invalidate in `board_raspi::arch`); noteer
   ESR/FAR.
7. **`vcmail: T C, ARM F MHz (max X MHz)`**. `HOPOS_VCMAIL_FAIL`: de
   mailbox-buffer (0x1480_0000, Normal-NC) of de basis; de rest boot door.
   Pi 5 met `arm_freq=1500`: max 1500.
8. **`boot: HopOS v3.0.0 on rpi4, EL2, 4 cores (big), N MB DRAM
   HOPOS_BOOT`**.
9. **`irq: GIC-400: GICD ... CTLR 0x1 ... cpu mask 0x1`**. CTLR 0 of mask
   0: TF-A liet de GIC anders achter; noteer de regel.
10. **Pi 4 NIC**: `net: genet rev 0x6000..., PHY at 1 (0x600d:...)`, dan
    `net: genet up, 1000 Mbps full duplex, polled`, `HOPOS_NIC_UP` en een
    DHCP-lease. `not a v5` of `no PHY`: noteer; `no link within 8 s`:
    kabel.
11. **Pi 5 NIC**: `net: pcie2 link up (status 0x..b0), RP1 1de4:0001`,
    `net: rp1 up, PHY at 1`, `net: gem up, 1000 Mbps full duplex, RX on
    INTID 166`. De lijn is een flank (een MSI via de MIP): het board
    registreert hem als `Trigger::Edge` en de dispatcher zet GICD_ICFGR
    vóór de enable (`cpu::irq::enable_as`). `PCIe bring-up failed` met de status in de regel ervoor:
    RESCAL, PLL of training (de reeks is Go's bewezen probe6-reeks).
    `RX polled: ...`: de NIC werkt, de MSI-X-weg niet; noteer de reden.
12. **`slots: cage up HOPOS_CAGE_UP`** en `core 1..3 mailbox Ok(Cold)`.
13. **`oscore: cpu 0 self-test timer=(Timer, ~1000 us) yield=(Yield, ..
    us) kick=(Ipi, .. us) HOPOS_OS_SELFTEST ok`**. Op QEMU `raspi4b`
    gemeten 29-09: timer 1344 us op een termijn van 1 ms, yield 22 us, kick
    8 us. `kick=(Timer, ~200000 us)`: de SGI kwam niet aan of de peek
    zag hem niet; noteer de `irq: GIC-400`-regel (CTLR, cpu mask). Sinds
    30-09 handelt elke proef eerst af wat al bij de GIC wacht, doet hij het
    tot drie keer opnieuw als een device-lijn hem onderbrak, en noemt een
    onderbroken proef de lijn: `(Irq, 0 us, vec 9, INTID 166, INTID 166
    pending before entry, try 3)`. Les van de eerste Pi 5-boot (30-09): drie
    keer `Irq` na 0 us was de NIC-lijn (of een firmware-lijn, `other=1`)
    die tussen `probe_nic` en de zelftest één keer vuurde; de vector zette
    de vlag en keerde gemaskeerd terug, maar de dispatch-taak draait pas
    als de executor loopt, dus de lijn stond pending bij elke proef. Staat
    er ná drie pogingen nog een INTID, dan is die lijn echt blijvend: een
    level-lijn zonder ack meldt de dispatch daarna als `HOPOS_IRQ_STUCK`
    en zet hem uit.
14. **De eerste plaatsing**: `cage: slot 1 core 1 cold: PSCI CPU_ON
    mpidr=0x1` (Pi 5: `0x100`) `-> Ok(())` en `HOPOS_SLOT_START`. Hangt
    het na `cage: slot 1 built`: geen PSCI (Pi 4: `bl31.bin` ontbreekt of
    `armstub=` staat er niet). `Err(AlreadyOn)` op de Pi 5: de armstub
    zette de cores al aan; dan een upstream-TF-A als armstub (Go-notitie).
15. **Hop**: `slot 1: applib: stage-1 on: RAM write-back, control page
    Normal-NC, rings write-back, 16 KB of tables HOPOS_APP_MMU` als eerste
    regel, dan zijn regels via de servicer, `HOPOS_NODE_IP`, en `curl
    http://<ip>:8080/health`. Een fault van Hop op EL1 (een alignment-fault,
    een ongedefinieerde instructie) staat sinds 30-09 als `slot 1: Hop
    faulted at EL1 vec=4 esr=0x96000021 (data abort, alignment fault)
    elr=0x5... far=0x... HOPOS_HOP_FAULT`: de échte ESR, ELR en FAR uit de
    vectortabel van applib. Staat er `Hop faulted vec=8 esr=0x82000005
    (instruction abort, translation fault) far=0x200`, dan sprong hij naar
    een lege VBAR_EL1: een Hop gebouwd tegen een applib van vóór 30-09
    (`image/rpi5.sh` bouwt hem via `tools/hop-build.sh` tegen deze
    werkboom; `HOP_PATCH=0` neemt de tag van de hop-repo). `HOPOS_APP_NO_MMU`:
    de stage-1 is geweigerd en elke ongealigneerde toegang faultt.
16. **`HOPOS_TICK`** elke seconde, `sleeps` loopt op: de WFE-slaap met de
    event-stream van EL2 werkt. Staat `sleeps` op 0 of loopt de tik achter:
    de event-stream (CNTHCTL_EL2 in `pi_entry!`).
17. **Minuten laten draaien met verkeer** (Pi 5): de C1-stepping kan stil
    bevriezen onder RX-DMA plus fabric-werk (`OLD/docs/v1/archief/
    bcm2712-c1-erratum.md`); noteer de stepping en de tijd tot de freeze.

## De eerste Pi 5-boot (30-09)

Na een gave boot tot `HOPOS_HOP_START` twee fouten, beide in deze boom
gefikst en op de host en QEMU getoetst, nog niet op ijzer:

- **De zelftest van de OS-core** gaf `timer`, `yield` én `kick` op 0 us
  terug, de eerste twee als `Irq`. Niet een lijn die permanent aanstond:
  `set_edge(166)` werd al vóór de enable gezet, de tik telde 58 NIC-
  interrupts per seconde (LAN-ruis, geen storm) en `os(irq=0)`. Wel een
  lijn die al pending stond toen de zelftest begon, in de boot, vóór de
  executor ooit de dispatch draaide (stap 13). De soort van een lijn is
  nu deel van de registratie (`cpu::irq::Trigger`), en een level-lijn
  zonder ack die binnen één ronde blijft terugkomen gaat uit
  (`HOPOS_IRQ_STUCK`).
- **Hop viel op EL1** (`esr=0x82000005 far=0x200`: de instructie-abort op
  `VBAR_EL1 + 0x200`, dus de tweede fault). Een app draaide met de MMU
  uit, dus op Device, en de A76 geeft op Device een alignment-fault bij
  elke ongealigneerde toegang; QEMU-TCG toetst dat niet onder stage 2.
  Sinds 30-09 zet `_start` van elke app een stage-1 aan (`applib::mmu`)
  en vangt een vectortabel elke fault op EL1 (`CTRL_APP_FAULT_*`,
  `tools/qemu-test-fault.sh`).

## Bekende gaten

- **De kick van de OS-core op ijzer** (29-09 gebouwd, alleen de kick naar
  zichzelf bewezen, op QEMU `raspi4b`): het board geeft de rotatie SGI 8 als
  32-bit GICD_SGIR-woord `(1 << (16 + cpu)) | 8` plus de PA van GICD_SGIR
  (Pi 4 0xff84_1f00, Pi 5 0x10_7fff_9f00), en de switcher van een app-core
  schrijft dat woord met de MMU uit. Waarom 8 en niet de 7 van Rockchip:
  zie `board_raspi::KICK_SGI`. Nog te bewijzen, in deze volgorde:
  1. de zelftest op ijzer (stap 13), Pi 4 en Pi 5;
  2. de kick van een app-core: een app die HVC #6 doet (applib na een
     TX-publicatie) of idle yieldt terwijl Hop op de OS-core draait. De
     `HOPOS_TICK`-regel moet `ipi=` en `kicks=` zien oplopen; blijft `ipi`
     0 en loopt `timer` op, dan bereikt de MMIO-schrijf van de app-core de
     distributor niet (Device-attribuut met de MMU uit is het verwachte;
     een ontbrekende `dsb` of een GICD die TF-A als Secure-only liet, niet);
  3. de Pi 5: de GIC-400 zit achter de 40-bit-adressering (0x10_7fff_9000);
     de switcher schrijft met een 64-bit adres, dus dat zou moeten werken,
     maar het is de eerste MMIO van een app-core buiten de onderste 4 GB;
  4. dat TF-A op beide Pi's de SGI's in Group 1 laat (de `plat/rpi`-BL31
     heeft geen beveiligde interrupts): een kick die stil verdwijnt terwijl
     GICD_CTLR 0x1 is, wijst hierop; dan een andere INTID proberen.
- **App-cores na CPU_ON**: de Pi-ingang zet EL2 (VTTBR, CPTR, MDCR, HSTR)
  alleen op core 0. Een app-core komt via PSCI direct in de trampoline van
  `cpu::el2`; als die registers daar garbage zijn (UNKNOWN na reset), zit
  de reparatie in `cpu::el2`, niet in het board.
- **`cpu::idle` zet de event-stream in CNTKCTL_EL1**; onder E2H=0 geldt
  voor EL2 CNTHCTL_EL2. De Pi-ingang zet die voor core 0 (1,2 ms op
  54 MHz); andere EL2-boards (O6N, Altra) hebben hetzelfde nodig.
- **GENET-interrupt** (Pi 4, SPI 157/158): niet bedraad, gepold zoals in
  Go.
- **Framebuffer-console**: niet geport; de kern meldt of de firmware er een
  gaf. `vcmail::alloc_fb` is er.
- **dvfs, watchdog, RNG200, NVMe (Pi 5 pcie1)**: niet geport; `arm_freq=1500`
  is de thermische cap op de Pi 5.
- **QEMU `raspi4b`** heeft geen GENET en geen PSCI; `tools/qemu-rpi4-test.sh`
  bewijst de boot tot de tik en de overgang van de OS-core met de kick naar
  zichzelf, niet het net, een app-core of een kick van een andere core.
