# RISC-V op HopOS v3: QEMU virt riscv64 en de LicheeRV Nano

De tweede architectuur, geport van de Go-kern (`OLD/metal/board/licheerv`,
`cpu/mmode`, `cpu/thead`, `kern/cage`, `kern/slots/cage_riscv64.go`,
`cpu/idle/idle_riscv64.go`, `driver/nic/dwmac`, `OLD/image/licheerv-agent.sh`).
QEMU virt riscv64 is de proefbank, de Sipeed LicheeRV Nano (SG2002, XuanTie
C906) het board. De Go-metingen (30-07 tot 19-08) staan in het commentaar van
de code; deze pagina zegt wat er staat, hoe je het bouwt, en per stap wat de
console op het ijzer moet tonen.

## Machine mode, en waarom

HopOS draait op RISC-V in machine mode, zoals de Go-generatie. De kooi is een
PMP-whitelist (begrenzen) plus een Sv39-tabel (verplaatsen), en PMP
programmeren kan alleen machine mode. Het image neemt daarom de plek van
OpenSBI in:

| | QEMU virt | LicheeRV Nano |
| --- | --- | --- |
| Wie laadt ons | QEMU `-bios none -kernel hopos` | de vendor-FSBL, MONITOR-slot van `fip.bin` |
| Waar | 0x8000_0000, elk hart | 0x8400_0000 (RUNADDR; lager boot niet, 19-08) |
| a0 / a1 | hart-id / DTB (bovenin het RAM) | hart-id / geen DTB |
| Timebase | 10 MHz | 25 MHz (40 ns per tik) |
| Cache-coherent | ja | nee: `dev::push`/`pull` doen `th.dcache.cpa`/`cipa` (feature `thead`) |

Onder OpenSBI (`-bios default`) landt de kern in S-mode en is de PMP van de
firmware: dan is er geen kooi, en `kmain` weigert net als "HopOS eist EL2" op
ARM (`board.privilege(3)`). SBI HSM voor de harts is daarmee ook geen weg: in
machine mode is er niemand onder ons om het te vragen. Een hart start via de
parkeerlus van de boot-stub (`cpu::riscv::boot::start_hart`: postvak plus
`msip`) of, op de LicheeRV, via het resetblok van de C906L.

De tabel ARM naar RISC-V (`cpu/src/riscv/mod.rs`):

```text
ARM                     RISC-V
EL2                     machine mode         (de kern)
EL1-app                 supervisor mode      (een slot)
HVC-yield               ecall-yield          (a0 = wektijd, a7 = 0/1)
stage-2 (VTTBR)         Sv39 (satp) + PMP    (verplaatsen + begrenzen)
ERET                    mret
PSCI CPU_ON             msip op een geparkeerd hart; resetblok op de C906L
GIC                     PLIC + CLINT
```

## Wat er is

| Deel | Waar | Getest |
| --- | --- | --- |
| Boot-stub: `_start` in M-mode, stack, BSS, `mtvec`, FPU, de parkeerlus van de andere harts, het T-Head-regime (`mxstatus`, `mcor`, `mhint`, `mhcr`) op elk hart | `cpu/src/riscv/boot.rs` | QEMU; host-test van de start |
| Trap-ingang van de kern: vlag, bron dicht in `mie`, dispatcher wekken; exceptions luid | `cpu/src/riscv/trap.rs` | QEMU; host-tests |
| CLINT: `mtimecmp` in 32-bit spec-volgorde, `msip`, de probe | `cpu/src/riscv/clint.rs` | QEMU; host-tests |
| Slaap en klok: `wfi` op de eigen `mtimecmp` met MIE dicht, TIME-CSR | `cpu/src/riscv/idle.rs` | QEMU; host-tests |
| PLIC als `cpu::irq::Controller` | `cpu/src/riscv/plic.rs` | QEMU (virtio-net-lijn); host-tests |
| TRNG: geen, luid (`HOPOS_RNG_INSECURE`) | `cpu/src/riscv/trng.rs` | |
| PMP-whitelist als TOR (Go `cage.Encode`) | `cpu/src/riscv/pmp.rs` | 5 host-tests |
| Sv39-relocatie in 2 MB-blokken (Go `cage.Relocate`), T-Head-attributen | `cpu/src/riscv/sv39.rs` | 4 host-tests |
| T-Head-cache-onderhoud in `dev::push`/`pull` | `dev/src/lib.rs` (feature `thead`) | bouwt |
| Board QEMU virt riscv64: ns16550, CLINT, PLIC, virtio-mmio net en blk | `board/qemuvirt-riscv` | `tools/qemu-riscv-test.sh` |
| Board LicheeRV: ns16550 (stride 4), dwmac plus ePHY-recept, CLINT, PLIC, de watchdog-probe, de C906L via het resetblok, het plan | `board/licheerv` | bouwt; host-tests |
| dwmac (DWMAC1000, 3.x) met elke descriptor en buffer op een eigen cacheline | `driver/nic/dwmac` | 26 host-tests |
| Linkscript | `hopos/link-riscv.ld` (build.rs zet de basis per board) | |
| FIP uit de donor, gehasht | `image/licheerv-agent.sh` | niet op ijzer |

## Bouwen en draaien

```sh
cargo build --target riscv64gc-unknown-none-elf -p hopos --features board-qemuvirt-riscv
APP= sh tools/qemu-riscv-test.sh          # de boot-poort
sh tools/qemu-riscv-test.sh               # plus de kooi-keten met appspike

sh image/licheerv-agent.sh                # target/licheerv/fip-licheerv.bin
sh image/licheerv-agent.sh /dev/diskN     # plus fip.bin op de kaart
```

Twee builds, niet één: de feature `thead` (in `dev` en `cpu`) komt alleen mee
met `board-licheerv`. Bouw daarom per board met `-p hopos --features ...`,
nooit beide boards in één cargo-aanroep: dan unificeert cargo de feature en
draagt het QEMU-image T-Head-instructies die daar illegal zijn.

De QEMU-regel (`tools/qemu-riscv-test.sh`):

```sh
qemu-system-riscv64 -M virt -m 1G -smp 2 -bios none -nographic \
  -kernel target/riscv64gc-unknown-none-elf/release/hopos \
  -global virtio-mmio.force-legacy=false \
  -netdev user,id=n0 -device virtio-net-device,netdev=n0 \
  -drive file=disk.img,if=none,format=raw,id=d0 -device virtio-blk-device,drive=d0
```

Gemeten 30-09 (QEMU 11.0.2), de markers van de boot-poort:

```text
boot: HopOS v3.0.0 on qemuvirt-riscv, EL3, 2 cores (big), 1024 MB DRAM HOPOS_BOOT gen=1 stamp=dev
disk: up HOPOS_DISK_UP model=virtio-blk blocks=131072 block_size=512 max_transfer=1048576
hopfs: mounted 64 MiB (empty disk) HOPOS_FS_UP fresh=1 generation=0
net: nic up HOPOS_NIC_UP mac=52:54:00:12:34:56
net: 10.0.2.15 (mac 52:54:00:12:34:56, gw 10.0.2.2) HOPOS_NET_UP
HOPOS_TICK 3 sleeps=2334 polls=2669 irq(timer=0 nic=2 other=0) ...
```

"EL3" op de boot-regel is de privilege-modus: 3 = machine mode.

## De kooi: stand

Op QEMU bewezen (30-09) door de zelftest van het board
(`board/qemuvirt-riscv/src/cage.rs`, onderdeel van `tools/qemu-riscv-test.sh`):

```text
cage: riscv switcher on hart 1: yield resumed at 0x5000000c, exit state 4, escape state 4 with mcause 7 mtval 0x80000000 vec 1 HOPOS_RV_CAGE_UP
```

Hart 1 komt via de parkeerlus in de switcher en slaapt op zijn CLINT; de
kick (`msip`) wekt hem; een bewoner van zes instructies, fysiek in de pool
op 0x9000_0000 en gelinkt op `LINK_BASE` (0x5000_0000), boot koud in S-mode
achter één TOR-venster plus de Sv39-tabel, yieldt (`ecall`, a7 = 0), wordt
bewaard en hervat op `mepc + 4` (0x5000_000c) en exit (a7 = 1, staat 4 =
Dead). Een tweede bewoner zonder vertaling schrijft naar 0x8000_0000 (de
kern): PMP weigert met mcause 7 (store access fault), de switcher schrijft
mcause, mtval en vec 1 op zijn control-page en zet hem dood.

Wat nog ontbreekt tot appspike in een slot draait staat onder "Niet
gedaan". Het ontwerp volgt Go één op één:

- **Begrenzen**: één TOR-venster over de partitie-claim (plus een grant),
  afgesloten met een deny-all, ONgelockt (de L-bit zou machine mode binden en
  de switcher buitensluiten). De switcher schrijft `pmpcfg0` en leest hem
  terug vóór hij een bewoner binnenlaat.
- **Verplaatsen**: `satp` met een Sv39-tabel in de ABI-staart van de partitie
  (`ABI_MAP_OFF`): het canonieke linkadres naar de echte partitie, de staart
  als device (zonder T-Head-cachebits). Geen G-bit, geen U-bit.
- **Wisselen**: de M-mode-switcher op een app-hart (`mtvec` = zijn ingang,
  `mscratch` = het sched-blok): `ecall` uit S-mode met a7 = 0 is een yield met
  de wektijd in a0, a7 = 1 een exit; al het andere een fault op de
  control-page. Parkeren is `wfi` op de eigen `mtimecmp`, gewekt door de kick
  (`msip`, `SCHED_MSIP_PA`).
- **Intrekken**: op de C906L het resetblok (wist ook de PMP, gemeten 30-07);
  op een hart zonder reset het woord `CTX_REVOKE`, gelezen bij elke yield en
  door de kill-tick (`SCHED_TICK_TICKS`).

## De checklist voor de LicheeRV, in bootvolgorde

Console: de UART0-pinnen van de LicheeRV (GND, TX, RX), 115200 8N1 (de FSBL
zet hem; HopOS doet geen init).

1. **De FSBL springt ons in.** Verwacht na de FSBL-regels meteen de bunny.
   Niets na de FSBL: RUNADDR klopt niet met KERN_BASE (het script weigert een
   ELF-entry die niet 0x8400_0000 is), of de donor zet LOADER_2ND nog op een
   runaddr (het script nult het param2-blok; "0 param2 blocks" is een andere
   donor).
2. **`boot: LicheeRV Nano (SG2002), machine mode monitor`.** Daarna
   `board: CLINT: mtimecmp writable`. `HOPOS_CLINT_FAIL` betekent dat de
   c900-CLINT de 32-bit-schrijf niet vasthoudt: de kern pollt dan (werkt,
   verbruikt meer). Nooit een lees op 0xbff8 (`mtime`): die bestaat niet
   (bus-fout, 30-07).
3. **`trng: WARNING ... HOPOS_RNG_INSECURE`.** Hoort er altijd te staan: er
   is geen hardware-TRNG. Geen geheimen van waarde op deze node.
4. **`watchdog: probing the DW-WDT at 0x3010000`**, dan `DW-WDT answers, NOT
   armed`. Stilte na de probe-regel is een bus-fout op het WDT-blok: de
   probe eruit en melden.
5. **`HOPOS_BOOT`** met `licheerv, EL3, 2 cores`. Een `exception: illegal
   instruction` vóór deze regel is het T-Head-regime (`mxstatus`
   THEADISAEE): vergelijk `mxstatus` met de gemeten FSBL-waarde 0xc0638000.
6. **`HOPOS_TICK 1..3`**: de slaap op de CLINT. Ticks die haperen of
   `sleeps` die niet oplopen: de `wfi` wekt niet op `mtimecmp` (de
   01-08-klasse); de vangrail van 2 ms houdt de node dan wel levend.
7. **Het net**: `net: dwmac version 0x1037, PHY 0 id 0043:5649, link 100 Mbps
   full duplex`, dan `HOPOS_NIC_UP` en `HOPOS_NET_UP`. `version reads 0`: de
   klokgates (REG_CLK_EN_0 bit 25/26). `no PHY on the MDIO bus`: het
   ePHY-recept kwam niet aan (volgorde niet veranderen). Geen lease: kijk
   naar `rx_bad_len` en de diag-regel van de dwmac; een ring die na een paar
   frames stilvalt is de les van 30-07 (een invalidate die een CPU-schrijf
   weggooit), en de dwmac legt daarom alles op eigen cachelines.
8. **Geen opslag**: `disk: none` is juist; er is geen SD-driver.
9. **De C906L**: nog niet door de kern gestart (zie "Niet gedaan"). De eerste
   meting: `start_little(_start)`, de parkeerlus, en dan de zelftest van
   QEMU (`board/qemuvirt-riscv/src/cage.rs`) op het ijzer: dezelfde marker
   `HOPOS_RV_CAGE_UP`. Let daarbij op: de I-cache moet aan staan (het
   T-Head-regime in de boot-stub; anders ~77x trager, gemeten 18-08), en de
   eerste TOR-meting op dit silicium is nooit geldig gedaan (de 30-07-meting
   leunde op niet-gedrainde writes): mcause 7 op de escape is hier het
   eerste echte bewijs.

## Niet gedaan

- **appspike in een slot op riscv64.** Wat er staat: de switcher, PMP, Sv39,
  het parkeren en de kick (bewezen, hierboven) en de riscv-`arch` van
  applib (`applib/src/arch.rs`: rdtime, yield en exit als `ecall`). Wat
  ontbreekt, in volgorde:
  1. `hopos/src/cage_riscv.rs`: `kern::cage::Cage` en `Cores` over deze
     onderdelen (de bouw: de control-page en de ringen zoals `arm_tail`, het
     TOR-venster over de claim, de Sv39-tabel in `ABI_MAP_OFF` van de staart,
     het regime in `CTX_REGIME`; de dispatch: de slot in `SCHED_LIST`, staat
     BootPending, `msip`; `quiet`/`live`/`status` uit ctx-staat en
     control-page; `revoke` via `CTX_REVOKE`, gelezen bij elke yield).
     `board/qemuvirt-riscv/src/cage.rs` is er de kleinste vorm van.
  2. De aanhaak in `hopos/src/slots.rs` (niet van dit spoor): `mod cage`
     met `#[cfg_attr(target_arch = "riscv64", path = "cage_riscv.rs")]`, en
     de `el2`-aanroepen daar (`el2::kick`, `OsCore`, `core_state`) achter
     dezelfde splitsing. Tot dan meldt een riscv-boot `slots: cage init: this
     build carries no EL2 switch code HOPOS_CAGE_FAIL` en draait door zonder
     slots.
  3. `_start` en de `#[panic_handler]` van applib voor riscv64 (staan in
     `applib/src/rt.rs`, alleen voor aarch64): `cargo build -p appspike
     --target riscv64gc-unknown-none-elf` faalt daar nu op.
  4. De timebase voor apps: `applib` rekent met 10 MHz (QEMU); de LicheeRV
     telt 25 MHz, en dat hoort als woord op de control-page.
  5. De kill-tick (`SCHED_TICK_TICKS`): een bewoner die nooit yieldt, is nu
     alleen met het resetblok (C906L) of de node te stoppen.
- De OS-core-rotatie (Hop naast de kern op één hart) en de lottery van de
  Go-kern (de kern naar de kleine core): de kern blijft op hart 0.
- De kern-flip op riscv64: de `FLIP_*`-getallen staan in de boards zodat de
  lijm bouwt, de sprong (`cpu::el2::chain`) is arm64.
- `hopos.cfg`, `hopos.mac` en `hopos.node` op de LicheeRV: er is geen
  bootargs-bron (geen DTB van de FSBL); het MAC-adres is het ingebouwde,
  luid (`HOPOS_MAC_FIXED`).
- Het wapenen en aaien van de watchdog (Go: een canary die de node
  levensecht toetst); alleen de probe.
- De runtime-regel zegt nog `none/aarch64` (`hopos/src/main.rs`).
