# HopOS v3: de omschrijving

Wat het systeem is, laag voor laag, uit de code gelezen en niet uit de
andere docs. Het doel is review: een omschrijving maakt zichtbaar wat twee
keer bestaat, wat op de verkeerde plek staat en wat geen eigenaar heeft. Het
eerste deel beschrijft; het laatste hoofdstuk, "Opgevallen bij het
schrijven", verzamelt wat dubbel of scheef lijkt, met paden.

Stand: main op 333d91e (04-10-2026), met het config-venster van a6ceae5.
Regelnummers verschuiven; de padnamen blijven.

## 1. In één alinea

HopOS is één statische binary die het hele OS is. Core 0 (of de core die
`hopos.oscore` aanwijst) draait de kern op EL2 (riscv: machine mode). Elke
app is een ELF in een eigen fysieke partitie achter een hardware-kooi
(stage-2 op arm64, PMP plus Sv39 op riscv), op een eigen core of op een core
die hij expliciet deelt. Er is geen shell, geen libc, geen proces: een app
praat met de kern via een gedeelde pagina, twee ringen en één TCP-verbinding.
De kern is alleen mechanisme. Hop, de orchestrator, is de eerste bewoner:
een app met het ene recht om andere apps te plaatsen, te stoppen en te
vervangen. De kern bezit de kooi, de switch, de opslag en de klok; Hop bezit
het beleid.

## 2. De lagen

```
                 apps/  appspike/  go/ (tamago)           ← apps, elk in een kooi
                 applib/  (runtime, leannet, system-API-client)
 ─────────────────────────── abi/ (het contract) ───────────────────────────
 hopos/          de binary: kiest één board, spawnt alle taken, de lijm
 kern/  net/     lifecycle, system-API, hopfs, flip  |  switch, NAT, pomp
 gui/  media/    fb-grant, USB-invoer, VOP2  |  VPU-driver, optisch
 board/          het Board-contract + 11 boards
 driver/         één crate per device (± 35)
 cpu/  fw/       boot, vectoren, kooi, switcher, OS-core  |  FDT, ACPI, ADT
 executor/ sync/ heap/ bounded/ dev/ netdev/ blkdev/      ← het fundament
```

Omvang in regels (Rust plus asm en scripts, incl. toetsen): driver 35k,
board 25k, kern 21k, cpu 16k, apps 13k, applib 13k, hopos 11k, tools 9k,
net 7k, abi 7k, gui 5k, media 5k, fw 5k, image 3k, de rest samen 6k.

De afhankelijkheden lopen van boven naar beneden. kern hangt bewust niet af
van net, cpu of board: al het ijzer komt binnen als trait (`kern/src/cage.rs`).
net kent geen kern. Alleen hopos kent alles en verbindt het.

## 3. Afspraken die overal gelden

- **Eén eigenaar per staat**, en die eigenaar is een taak (het handboek,
  `rustdoc/README.md` §1). Wie iets wil, stuurt een bericht. Een tabel die
  één taak schrijft en velen kort lezen is een `Local<RefCell<T>>`. De twee
  toegestane sloten: de heap (`heap::HeapLock`) en de console
  (`cpu/src/console.rs`).
- **Alleen `dev` raakt device-geheugen.** Geen `read_volatile`, geen barrière
  en geen cache-onderhoud buiten `dev/` (geverifieerd met grep). Een driver
  krijgt een `dev::Pa` van het board en leest via `Reg<T>`/`regs`.
- **Een driver vindt niets zelf.** Het board ontdekt het device (FDT, ACPI,
  ADT, PCI-walk), mapt het, geeft de driver een registerbasis en een
  DMA-regio die alleen van die driver is.
- **De interruptketen** is overal gelijk: de controller claimt, een
  `IrqAck` van de driver maskeert het device, een `sync::Signal` gaat aan,
  en de eigenaar-taak doet het werk. Nooit werk in exception-context. Het
  masker gaat pas weer open als de ring leeg is.
- **Faalbaar alloceren.** `BoundedVec` geeft `Full`, `try_reserve` in plaats
  van `push`, de executor boxt met `try_box`.
- **Een gebeurtenis, geen klok.** Taken slapen op een `Signal` of een
  deadline, niet in een poll-lus (`docs/apps.md`). Waar dat niet lukt is het
  een kandidaat in §8.

## 4. Van stroom aan tot een app op de draad

### 4.1 Boot

| Pad | Ingang | Wie |
| --- | --- | --- |
| arm64 nVHE (qemuvirt, Pi's, Radxa) | `cpu/src/boot.rs` `_start` (op de Pi eerst `_pi_start`) | core 0, of een core met `FLIP_ENTRY` in x3 |
| UEFI (generiek, O6N, Altra; VHE of nVHE) | `board/uefi/src/entry.rs` `_start_efi`: zelf-relocatie, EFI-memmap, `ExitBootServices`, `uefi_enter_kernel` | de BSP |
| Mac mini M4 | `board/apple/src/head.rs` (eigen relocatie, VHE-only) | iBoot of m1n1 |
| riscv64 machine mode | `cpu/src/riscv/boot.rs` `_start`; andere harts in een postvak-lus | hart 0 |

Alle vier zetten stack, BSS, vectoren (`__hopos_vectors`), de identity map
en de MMU, en roepen `kmain(dtb, el)` in `hopos/src/main.rs` aan. Een
flip-boot komt via dezelfde ingangen binnen (x3 = `FLIP_ENTRY`, op UEFI een
feitenpagina).

`kmain` doet console, privilegetoets (EL2 of M-mode, anders parkeren), heap,
`discover`, en `move_to_os_core`: wijst `hopos.oscore` een andere core aan,
dan start `cpu::smp::start_one` die core en wordt de boot-core een app-core
(`cpu::el2::hold`). Daarna `setup` en `exec.run`.

`setup` (`hopos/src/main.rs`) zet in deze volgorde op: flip-landing,
klok, conport, wandklok, het zaad (`seed`), de rol van de staging plus het
`Privilege`-token voor slot 1, opslag (probe, hopfs-actor, committer),
codec, system-API, interrupts met een IRQ-taak, de taken tick, watchdog,
telemetrie en load, de node-config, de NIC (pomp, switch, poort 0, DHCP,
listeners), USB-invoer, flip en slots. Ten slotte Hop in slot 1 (64 MiB,
LicheeRV 10 MiB) of de adoptie van de bewoners na een flip. Hop draait in de
idle van de kern op de OS-core (`sleeper.host`).

### 4.2 De OS-core en de app-cores

De OS-core is gastheer: als de executor van de kern niets te doen heeft,
geeft `ArmSleeper::sleep` (`cpu/src/idle.rs`) de core in modus Resident aan
bewoners op EL1 onder stage-2 (`cpu/src/el2/oscore.rs`, `OsCore::run`). De
kern neemt hem terug via IRQ, kick, CNTHP-timer of een yield. riscv volgt
dezelfde vorm (`cpu/src/riscv/oscore.rs`) en deelt de rotatieregel
(`round`, `next`, `due`) en de bewonerslijst (`cpu/src/el2/roster.rs`).

Een app-core draait de **switcher** (`cpu/src/el2/switch.rs`), assembly in
drie smaken (nvhe, vhe, apple) die positie-onafhankelijk naar de plan-regio
gekopieerd wordt (descriptor `HOPSWTC1` met FNV). Een app komt binnen via
`_tramp` (VTTBR, VTCR, HCR, ERET naar EL1) en verlaat de core alleen met een
HVC: #0 exit, #1 yield met wektijd, #4 een sibling wekken, #6 de OS-core
kicken. De wissel is coöperatief: round-robin over de bewoners die due zijn.
Op riscv heeft de switcher een kill-tick van 10 ms, de enige preemptie in
HopOS.

De **kooi** op arm64: stage-2 met 4 KB-granule, 39-bit IPA, 2 MB-blokken,
tabellen in het kooiblok (`cpu/src/el2/stage2.rs`). Intrekken is nullen,
vegen, `tlbi alle1is`. Op riscv: een TOR-whitelist in PMP plus een
Sv39-relocatie (`cpu/src/riscv/{pmp,sv39}.rs`), geschreven door
`hopos/src/cage_riscv.rs`; de switcher leest `pmpcfg0` terug en weigert de
bewoner als het niet klopt.

### 4.3 Een slot van plaatsing tot stop

Wie wat bezit:

| Eigenaar | Staat | Waar |
| --- | --- | --- |
| `Lifecycle` (actor) | partities, cores, bewoners, kooi, grants | `kern/src/slots.rs` |
| `Servicers` (leesbare tabel) | per slot generatie, mounts, job, partitie | `kern/src/slots.rs` |
| servicer-taak per slot | de outbox-lezer van één levensduur | `kern/src/slots.rs` |
| `System` | lopende image-stromen, het Privilege-token | `kern/src/system.rs` |
| `FsActor` | de hopfs-boom | `kern/src/rpc.rs` |
| `StoreQueue` | 8 openstaande store-ops | `kern/src/store.rs` |
| `Switch` (actor) | poorten, ringhelften, NAT | `net/src/switch.rs` |
| `Pump` | de NIC | `net/src/pump.rs` |

1. **Start.** Hop stuurt `START_SLOT` over de system-API. `System::start`
   toetst env, poorten en mounts en vraagt de lifecycle om een claim.
   `Lifecycle::claim` kiest cores (`kern/src/pool.rs`: dedicated, een
   sharegroep, of de OS-core als groep `system`), alloceert een partitie
   (`kern/src/partmem.rs`, best-fit op 2 MiB, levensloop als type
   `Partition<Free|Owned|Quarantined>`) en scrubt die in brokken.
2. **Stromen.** Hop stroomt de ELF met `STREAM_IMAGE`. `Placer`
   (`kern/src/system.rs`) schrijft elk segment rechtstreeks in de partitie,
   leest daarna de symbolen, en `abi::place::build` zet de vier woorden die
   de kern patcht (RamStart, RamSize, slotHint, ABI-stempel). Een Go-image
   (stempel 10) gaat door dezelfde weg.
3. **Wapenen.** `Lifecycle::arm`: poorten publiceren bij de switch, de kooi
   bouwen (en de ringen aan de switch hangen), grants, de servicer
   registreren, dispatch, en wachten tot de boot niet meer pending is. Een
   fout vóór de dispatch is een rollback; een onzekere dispatch zet het slot
   in quarantaine.
4. **Draaien.** De servicer leest de outbox (logregels naar de console en
   `NEXT_LOG`, SMP-verzoeken naar de lifecycle).
5. **Stop.** `STOP_SLOT`: evict uit de tabel, de ringen van de switch,
   kill-vlag, poorten weg, wachten op stil, anders revoke met 1 s gratie.
   Stil: partitie en cores vrij. Niet stil: quarantaine. De committer ziet de
   generatie verdwijnen en legt hopfs vast.

### 4.4 De app-kant: applib

applib (`applib/`) is de runtime waar elke Rust-app tegen linkt (op
0x50010000, het canonieke adres dat ook Go gebruikt). `_start` zet vectoren,
de stage-1-identity map en de MMU; `main!` → `rt::run` maakt de `App`
(staart, outbox, control-page, env), de heap (dezelfde crate als de kern),
meldt READY, en draait de executor met twee taken: de hartslag (`watch`,
elke 50 ms) en `main`. De slaper (`applib/src/sleep.rs`) wapent eerst de
RX-deurbel en yieldt dan (HVC #1) of wacht op WFE.

Wat een app verder krijgt: een leannet-stack per app (`appnet.rs`: IP en MAC
uit het slotnummer, de kern als gateway, DNS-stub, TCP/UDP/UDP6-handvatten),
de system-API-client (`sys.rs`), de store (`store.rs`), SMP met één executor
per core (`smp.rs`), `log!` naar de outbox, een ChaCha20-DRBG (`rand.rs`),
het glas en de invoer (`fb.rs`), de codec-client (`codec.rs`) en `stacktask`
voor synchrone C-code. Alle getallen van het contract komen uit `abi` via
`applib/src/contract.rs`.

### 4.5 Het contract: abi

`abi/` is het enige dat kern en app delen, ABI-versie 1, byte voor byte
gelijk aan Go-ABI 10.

- **De staart**: de bovenste 2 MiB van elke partitie, met de control-page
  (0x0), de outbox-ring (0x1000), de stub, de RISC-V-map en vanaf 0x20000 de
  net-ringen (`abi/src/layout/`). `Tail` rekent elk adres uit.
- **De ring** (`abi/src/ring.rs`): SPSC over gedeeld geheugen, monotone
  indexen, records die nooit wrappen, een `Coherence`-belofte per kant, en
  een lezer die de schrijver wantrouwt (een onmogelijke kop is `Corrupt`).
  `write` meldt de overgang van leeg naar niet-leeg: het deurbelcontract.
- **De system-API** (`abi/src/systemapi.rs`, `hopabi.rs`): een frame
  "HOPS" van 12 bytes over TCP naar 10.100.0.1:10100, met daarin een
  verzoek of antwoord met een kop van 24 bytes. Bestands-, store-, codec-,
  device- en bevoegde ops (`PrivOp`, alleen Hop).
- **De plaatsing** (`place.rs`), het PA-plan (`layout/plan.rs`), en
  `sha256`, `checksum` (FNV), `FlowState` (NAT-flows voor de flip).

### 4.6 De system-API, van app naar kern en terug

De app opent met zijn eigen stack een TCP-verbinding naar 10.100.0.1:10100.
De switch toetst bron-MAC en -IP en geeft het frame aan poort 0, de
node-stack van de kern (leannet zonder ipv6, `hopos/src/net.rs`). De
listener laat de verbinding toe als het bron-IP bij een levende generatie
hoort (hoogstens 2 verbindingen per slot, Hop 3). Een werker draait
`System::serve`: kop lezen, dispatchen naar log, `FsActor`, store, codec,
device of `PrivOp`, antwoord terug door dezelfde stack. Een bewaker sluit de
verbinding als de generatie verdwijnt of na 300 s stilte.

### 4.7 Het netwerk: switch, NAT, pomp

net/ is geen TCP/IP-stack maar een L2-switch met NAT. Er is in Rust één
TCP/IP-implementatie, leannet, twee keer geïnstantieerd: als node-stack in
de kern en per app in applib. DHCP draait alleen in de kern, DNS alleen in
applib.

- **In:** NIC → `Pump::rx_pass` → ingress-rij → `Switch::switch_pass` →
  `uplink_in`: multicast naar alle slots, IPv6 op een slot-MAC naar dat
  slot, `Nat::inbound` voor het node-IP (antwoord op een masquerade-flow, of
  DNAT naar een gepubliceerde poort), de rest naar poort 0 (DHCP,
  listeners).
- **Uit:** de app schrijft zijn TX-ring en kickt; de switch kiest relay
  (naar een ander slot), forward (broadcast, ARP voor de gateway beantwoordt
  hij zelf), of de gateway: naar poort 0 voor 10.100.0.1, SNAT voor een
  antwoord van een gepubliceerde poort, hairpin voor het eigen node-IP, of
  masquerade naar de uplink.
- **Poorten:** jobspec-poorten via `Cage::publish` naar `Nat::publish`
  (tcp en udp), hoogstens 512.

### 4.8 hopfs

`kern/src/hopfs.rs`: de boom in RAM, de data in blokken van 4 KiB op de
schijf, twee metaplekken met SHA-256. Een commit schrijft de boom naar de
andere plek en dan de kop; vrijgaven wachten tot de commit erna, zodat de
oude boom nooit naar hergebruikte blokken wijst. De actor
(`kern/src/rpc.rs`) plant synchroon en doet de I/O als futures in een pool
van 16, met de regels: per slot één call tegelijk (lezingen mogen naast
elkaar), één schrijver per bestand, en destructieve ops alleen als niets
anders loopt. `OP_READ_MANY` (21) is een bundel van tot 16 lezingen uit één
bestand in één call: alles in één batch op de wachtrij van het device, één
antwoord met per lees de uitkomst ([apps.md](apps.md), "Veel kleine
lezingen").

Paden: een volume uit de jobspec (`/data` → `/volumes/demo`) wint op
langste prefix, anders `/.tasks/slot<N>`, dat per levensduur gewist wordt.
`OP_SYNC` is een bevestigde barrière (`docs/storage-sync.md`). De committer
legt elke 10 s vast, of zodra een slot stopt.

Onder hopfs: `blkdev::Queue` (blk-mq in het klein, één pacer pollt het
device en wekt de anderen) over nvme of virtioblk.

### 4.9 De kern-flip

Het beleid staat in `hopos/src/flip.rs`, het mechanisme in
`kern/src/kernflip.rs` en `cpu/src/el2/chain.rs` (`docs/flip.md` voor de
procedure).

1. Hop stroomt een bundel in een rauw slot en stuurt `PrivOp::Flip` met de
   sha. De kern toetst de som en de switch-code, maakt de bundel plat achter
   Hop en relokeert (HOPRELO1).
2. Warm: hopfs bevriezen en committen, de NAT-flows vangen
   (`SnapshotNat`), USB stil, `Lifecycle::snapshot`, het handoff-blob op
   `FLIP_HANDOFF_PA`, en de sprong. Koud: bewoners stoppen, app-cores uit,
   springen zonder adoptie.
3. De nieuwe kern leest het blob (`kernflip::adopted`), houdt de poorten
   vast vóór de eerste switch-ronde, adopteert de bewoners, publiceert de
   poorten opnieuw, zet de NAT terug, en draait 120 s een boot-guard.

### 4.10 Rondom: watchdog, telemetrie, console, config

- **Watchdog** (`hopos/src/watchdog.rs`, beleid in `kern/src/watchdog.rs`):
  elke 2 s een verse TCP-verbinding naar Hop; beweegt de hartslag niet, dan
  stopt het aaien en reset de hardware-watchdog de node. De hardware zit per
  board.
- **Telemetrie** (`hopos/src/telemetry.rs`): thermiek en klokbeleid per
  board, met `driver/dvfs` als governor.
- **Console**: de tee gaat naar zwarte doos, UART, een ring van 256 KiB en
  het glas. `nc NODE 5555` speelt de ring af en leest mee (hoogstens 4
  lezers; `kern/src/conport.rs`, `hopos/src/conport.rs`).
- **Config**: een venster van 16 KiB (`#HOPCFG1`) in elke kern
  (`board/src/cfgwin.rs`), gevuld door `image/hopcfg.py` of `hop image`.
  Een gevuld venster wint van de oude bronnen (ESP-`hopos.cfg`, initrd,
  0xF000 op de M4, bootargs). `kern/src/nodecfg.rs` maakt er de env van Hop
  van.

## 5. De lagen één voor één

### 5.1 Het fundament

| Crate | Wat | Gebruikt door |
| --- | --- | --- |
| `dev` (0,9k) | MMIO-lezen en -schrijven, `Reg<T>`, kopieën, barrières, `push`/`pull` (cache), `poll_until`, `delay`, de memcpy-lussen. Op de host no-ops. | ± 50 crates |
| `bounded` (0,3k) | `BoundedVec<T, N>` en `Full<T>`. Geen map, set of ringbuffer. | 35 bestanden |
| `sync` (1,5k) | `Signal`, `Stop`, `AtomicWaker`, `WakerSet`, `spsc::Channel`, `mpsc::Mailbox`, `Local`/`LocalCell`, `select`, `Pool` (futures zonder heap), `yield_now`. Geen mutex, geen oneshot. | overal |
| `heap` (1,2k) | De allocator van kern en apps: vrije lijsten met grenslabels, klassen per 16 bytes tot 1 KiB, `HeapLock` (spinslot met core-id). | `board/src/heap.rs`, `applib/src/heap.rs` |
| `executor` (0,8k) | Eén per core: taken in een box, timerwiel, ready-bits, `Sleeper`-trait, uitstelbare timers. | hopos, applib, net |
| `abi` (7k) | Zie §4.5. `forbid(unsafe)`. | kern, net, cpu, boards, applib, hopos, gui |
| `netdev` (0,1k) | Het NIC-contract: `transmit`, `receive`, `flush`, `mac`, `irq`. | 7 NIC-drivers, net, applib |
| `blkdev` (1,3k) | Het blokcontract: `AsyncBlockDevice` (één opdracht of tickets), `Pace`, `Paced`, `Queue`, `BlockIo` voor hopfs. | nvme, virtioblk, hopos |

### 5.2 cpu en fw

`cpu/` (16k) is de architectuur, in twee sporen: boot (`boot`, `vectors`,
`console`) en kooi (`el2/`, `psci`, `smp`, `irq`, `gicv3`, `idle`, `trng`,
`drbg`, `memattr`). `el2/` bevat stage-2, de switcher, dispatch, de
bewonerslijst, de OS-core en de flip-sprong; `riscv/` is de machine-mode-helft
(csr, boot, trap, clint, plic, idle, pmp, sv39, switch, oscore). Op de host
bouwt elk arm64-module als stub, omdat hopos ze bij naam noemt.

`fw/` (5k, `forbid(unsafe)`) leest firmware op `&[u8]`: FDT, ACPI
(MADT, MCFG, SPCR, GTDT, IORT, PCCT, FADT), AML (alleen `_PRT`), de
Apple-ADT, xnuboot, GPT en `bootcfg`. Onvertrouwde input geeft `None`,
nooit een panic.

### 5.3 driver

| Groep | Crates |
| --- | --- |
| Interrupts | `gicv2` (Pi), `gicv3` met ITS (QEMU, Radxa, Altra, O6N), `aic` (M4); de PLIC staat in cpu |
| Console | `pl011`, `ns16550`, `fb` (tekst op een framebuffer) |
| PCIe | `pcie` (ECAM, BAR's, capabilities, MSI-X), `brcmpcie` (Broadcom-root-complex van de Pi's) |
| NIC | `nic/mdio` (gedeelde clause 22), `genet` (Pi 4), `gem` (Pi 5, RP1), `igb` (Altra, QEMU), `rtl8126` (O6N), `stmmac` (Radxa, LicheeRV), `tg3` (M4), `virtionet` |
| Opslag | `virtiopci` (transport mmio en pci), `virtioblk`, `nvme` (pci en Apple-ANS) |
| Firmware | `rtkit` (Apple-coprocessoren), `smc` (Apple), `scmi` (O6N, Radxa), `smpro` (Altra), `vcmail` (Pi) |
| RNG | `rkrng` (Radxa), `rng200` (Pi) |
| USB | `usb/xhci`, `usb/dwc3`, `usb/hid` |
| Geen driver | `dvfs` (klokbeleid), `codec` (het videocodec-contract) |

Een NIC is van de RX-pomp, een blokdriver van de hopfs-actor via
`blkdev::Queue`, xhci van de usbin-taak, de firmwarekanalen van de
thermiek-taak.

### 5.4 board

`board/src/lib.rs` definieert `trait Board`: `Nic` en `Sleeper` als types,
console, privilege, firmware, heap, `discover`, klok, geheugen, cores en
hun klasse, het PA-plan, `start_interrupts`/`dispatch_interrupts`,
`probe_nic`, en met `gui` de framebuffer en USB-hosts. Daarnaast `cfgwin`,
`stage` en `heap`.

hopos kiest precies één board met een feature (`board-qemuvirt`, `-rpi4`,
`-rpi5`, `-rk3566`, `-uefi`, `-o6n`, `-altra`, `-qemuvirt-riscv`,
`-licheerv`, `-apple`), linkt het als `vboard` en kiest het linkscript in
`hopos/build.rs`.

| Board | Ijzer en boot | Eigen |
| --- | --- | --- |
| qemuvirt | QEMU virt, `-kernel`, DTB | virtio-mmio, ramfb |
| qemuvirt-riscv | QEMU virt riscv64, `-bios none` | proefbank van de LicheeRV, kooi-zelftest |
| uefi | PE-stub op EDK2 of vendor-UEFI, ACPI | GICv3+ITS, SBSA-watchdog, GOP, vaste kernvensters |
| o6n | Orion O6N (Cix P1), bovenop uefi | rtl8126, NVMe, SCMI-thermiek, `_CPC`-klok, tien xHCI's, VPU, VHE |
| altra | Ampere Altra, bovenop uefi | igb met MSI-X, NVMe, SMpro-thermiek |
| raspi | de gedeelde Pi-laag, `Raspi<S: Soc>` | VideoCore-mailbox, GIC-400, PM-watchdog, RNG200 |
| rpi4 / rpi5 | `Soc` voor BCM2711 / BCM2712 | GENET en VL805 / de RP1-keten met GEM |
| rk3566 | Radxa Zero 3E, U-Boot `booti` | DWMAC4, SCMI-klok, TSADC, DW-WDT, VOP2 |
| licheerv | SG2002, FSBL, M-mode zonder SBI | dwmac1000 met ePHY, C906L als app-hart, Hop ingebakken |
| apple | Mac mini M4, iBoot of m1n1, ADT | AIC, DART, tg3, ANS-NVMe via RTKit, eigen MMU |

### 5.5 kern en net

Zie §4.3 tot §4.9 voor de stromen. Overige modules in `kern/`: `store`
(object-store-ops in de rij, Hop doet de S3-kant), `deviceabi` (de optische
drive via `/devices/discN`), `codecabi` (codec-sessies, feature `media`),
`grants` (het framebuffer-venster in één kooi), `nodecfg`, `watchdog` en
`load` (alleen rekenwerk), `conport` (de bouwstenen van poort 5555).

In `net/` verder: `flows` (conntrack, 4096 flows, 512 per slot), `host`
(poort 0 en de node-stack achter een trait), `gw` (de 1:1-herschrijving
10.100.0.1 ↔ node-IP), `plan` (adresplan), `ring`, `wire`, `nodemac`.

### 5.6 hopos

De binary (11k): `main.rs` (kmain, setup, tick), en de lijm die de traits
van kern invult en de taken spawnt: `slots`, `kooi`, `cage`/`cage_riscv`,
`glue`, `net`, `storage`, `flip`, `codec`, `optical`, `conport`,
`watchdog`, `telemetry`, `seed`, `clock`, `load`, `bench`, `mem`.
Features buiten het board: `gui`, `media`, `vhe`, en de meetkernen
`nvmebench`, `wdtest` en `hopcost`.

### 5.7 gui en media

- `gui/fbgrant`: wie het glas krijgt (`GUI=display` of `FB=1`), de
  `FB_*`-env, en de kernconsole van het scherm af en terug.
- `gui/usbin`: bezit de xHCI-hosts, zet HID om naar JSON-regels voor de
  houder van het glas (poort 7879), en bedient bulk-opslag voor optical.
- `gui/rkscan`: VOP2 en DW-HDMI van de RK3566 (nog niet op ijzer).
- `media/mve`: de Linlon V8-VPU van de O6N als `driver_codec::Engine`.
- `media/optical`: BOT en MMC READ(10) over usbin.

### 5.8 Apps

| App | Wat |
| --- | --- |
| `appspike` | De ABI-toets met rollen (SMP, SHARE, FLIPCONN, STORE, VOLUME, FAULT); markers voor de QEMU-tests |
| `welcome` | De nodepagina en `/health`; het referentievoorbeeld |
| `bench` | De serverkant van `tools/netmeter`, plus lastrollen (BURN, MCAST, …) |
| `vitals` | De board-dokter: HTTP-pagina met cpu-, mem-, net- en disktests |
| `syncprobe` | De duurzaamheidsprobe voor `OP_SYNC` |
| `display` | Houdt de fb-grant, tekent klok, cursor en invoer |
| `decode` | Een stream door de VPU (feature `media`) |
| `cloudflared-lean` | De Cloudflare-tunnel in Rust (TLS 1.3, HTTP/2, Cap'n Proto) |

Go-apps (`go/`, `docs/go-apps.md`): gebouwd met de gepatchte
tamago-toolchain tegen de Go-SDK `metal/v2` (buiten deze repo), met een
eigen netstack. De kern plaatst ze via dezelfde weg (ABI-stempel 10).

### 5.9 Bouwen, testen, release

- **Images**: per board een script in `image/` (build, objcopy, het
  config-venster vullen, Hop als staging, firmware, `tools/mkcard`). Hop
  komt uit de hop-repo via `tools/hop-build.sh`, gepatcht tegen applib, abi
  en sync van deze werkboom. `image/flip-bundle.sh` maakt de flipbundels.
- **Gate** (`tools/gate.sh`): host-tests, clippy, fmt, target-builds van
  alle boards en smaken. Geen QEMU.
- **QEMU-tests**: 18 shellscripts plus `qemu-test-sync.py`, met als enige
  gedeelde laag `tools/lib.sh` (38 regels). Elk wacht op markers in de
  console. `tools/soak.sh` (ijzer) en `tools/qemu-soak-hop.sh` (QEMU) voor
  uren.
- **Release** (`tools/release.sh`): elk board headless en headfull, de
  bundels, de apps, `SHA256SUMS`; publiceert zelf niets.
- **Host-tools**: `mkcard` (MBR + FAT16 + raw blobs), `netmeter`, `loc`.

## 6. Waar de documentatie staat

| Doc | Over |
| --- | --- |
| `README.md` | Pitch, mapindeling, de belangrijkste commando's |
| `docs/README.md` | Wat bewezen is op QEMU, de vlakken, images per board |
| `docs/boards*.md` | Per board de checklist; `boards.md` is UEFI én de config voor alle boards |
| `docs/flip.md`, `gui.md`, `media.md`, `apps.md`, `go-apps.md` | De vlakken |
| `docs/heap.md`, `stacktask.md`, `storage-sync.md`, `ipv6.md` | Losse ontwerpen |
| `docs/measurements.md` | Metingen per board naast v2 |
| `docs/review-*.md` | Momentopnames van reviews |
| `ALLES.md` | De lijst van wat er nog moet |
| `jobs/README.md` | Jobspecs en het releaseschema |

## 7. Wat nergens in deze repo staat

Hop zelf (`agentd-hopos` in de hop-repo), de `hop`-CLI met `hop image`,
leannet, leanhttp en de andere lean-crates, de Go-SDK `metal/v2`, en de
kopie van applib in Stulp (`stulp/vendor/applib`).

## 8. Opgevallen bij het schrijven

Wat hieronder staat kwam boven toen de lagen naast elkaar lagen. Het is per
thema gegroepeerd, niet per crate, omdat de meeste dubbelingen over een
laaggrens lopen. **H** = beide kanten in de code gelezen, **M** =
waarschijnlijk, **V** = vermoeden. Regelaantallen zijn schattingen.

### 8.1 Mogelijke fouten (eerst kijken)

- **O6N: `hopos.oscore=small` valt terug.** `BOARD.os_core()` komt via
  `Deref` bij `Uefi::os_core` (`board/uefi/src/lib.rs:484`), en die gebruikt
  de MADT-klassen, die op de Cix allemaal 0 zijn. `O6n::core_class` (de
  MPIDR-tabel) kent wel small en big. Elke methode van `Uefi` die
  `self.core_class` aanroept heeft dit risico. H op de code, niet op ijzer.
- **De kern na `move_to_os_core` draait op 64 KB zonder wachtpagina.**
  `cpu::smp::NODE_STACK` is 64 KiB (`cpu/src/smp.rs:39`); de boot-stack is
  256 KiB en de diepste boot 156 KB (ALLES.md). Alleen als `hopos.oscore` een
  andere core aanwijst. H voor de getallen, V dat het overloopt.
- **Hop op twee manieren gebouwd.** `image/qemu-run.sh`,
  `image/radxa-zero3.sh` en `image/apple-m4.sh` bouwen Hop ongepatcht in
  `HOP_DIR`; de Pi-scripts, de flipbundels, de release en de tests gebruiken
  `tools/hop-build.sh`. Een handmatige Radxa- of M4-image draagt dus een
  andere Hop dan de release. H.
- **`gem::transmit` spint tot 1 ms per frame** op de TSTART-quirk
  (`driver/nic/gem/src/lib.rs:556`), op de executor van de OS-core. H voor de
  lus, V of het zo lang duurt.
- **stmmac heeft een eigen `poll_until`** (`driver/nic/stmmac/src/lib.rs:91`)
  zonder de laatste blik na de deadline die `dev::poll_until` wel doet. H.
- **`claim` toetst de dedicated cores met `filter_map(Core::new)`**
  (`kern/src/slots.rs:901`), de vorm die de OS-core overslaat. V dat het
  ooit iets raakt.
- **De grenzen van de flip-handoff kloppen niet met elkaar**:
  `kernflip::decode` neemt 64 mounts en 4096 bytes per pad, `slots` 32 en
  256. H.

### 8.2 Hetzelfde begrip, meer definities

- `Region` vier keer: `abi` (`lib.rs:72`), `kern` (`lib.rs:166`, veld voor
  veld gelijk), `board` (`base: Pa`), `fw::fdt` (`addr`). `Slot` twee keer
  (`abi::layout`, `kern`). `Core` drie keer met drie betekenissen (fysiek in
  abi, logisch in kern, een trait in heap). H.
- `Full<T>` identiek in `sync` en `bounded`; aanroepers matchen op beide. H.
- `Pool` drie betekenissen (`sync::Pool`, `abi::layout::Pool`,
  `kern::pool::CorePool`). H, naamgeving.
- **Het adresplan** (slot ↔ IP/MAC) drie keer: `abi::layout`,
  `net::plan`, `applib::contract` en `applib::net`; de inverse eigen in
  `kern::system::slot_from_remote` en `net::gw::from_host`. H.
- **De NAT-staat voor de flip** drie keer (`kernflip::NatState`,
  `net::nat::NatState`, `net::switch::NatSnapshot`) plus twee losse
  `MAX_FLOWS`; hopos zet ze om. Hoort bij `abi::FlowState`. H.
- **`Timer`** drie keer (`applib::sys`, `kern::cage`, `driver_xhci`),
  `ExecTimer` twee keer, `Conn` twee keer, en daarnaast `blkdev::Pace` en
  `dvfs::Host` als klok-en-slaap. De klokparameter heet `clock` in elf
  drivers en `now` in vijf. H.
- **Codec-types** drie keer (`driver/codec`, `abi::hopabi`,
  `applib::codec`). H.
- **Het glas** twee keer beschreven (`applib::fb::Glass` en
  `driver_fb::Desc`, dezelfde `encode`), en het invoercontract (JSON-regels,
  poort 7879, `FB_*`-sleutels) staat buiten abi, in usbin en applib. H.
- Kleine botsingen: `MAX_CHUNK` is 8 KiB in abi en 1 MiB in applib;
  `KIND_LOG` is 1 als ringsoort en 3 als framesoort; `Coherence` is een enum
  en een trait; `Glass` een struct en een trait; de temperatuur heet
  `temp_milli_c`, `temp_millic`, `temp`, `reading` met vier returntypes. H.

### 8.3 Het contract buiten abi

- **De draadvorm van de system-API wordt drie keer gecodeerd**: in abi
  (`systemapi::encode_header`, `hopabi::encode_req`/`decode_resp`), in de
  kern (`kern/src/system.rs`: `read_header`, `write_frame`, `Call::decode`,
  `put_resp_head`) en in applib (`applib/src/sys.rs:194-296`). Binnen de kern
  gebruiken `store.rs` en `codecabi.rs` wél de abi-encoder. Stond in de
  review van 01-10 als "abi als enige waarheid". H.
- **`abi` draagt ook kern-interne dingen**: `sha256`, `checksum` en
  `FlowState` hebben geen app-gebruiker. Ontwerpkeuze. H.

### 8.4 Het Board-contract is grotendeels impliciet

- Wat hopos van een board nodig heeft staat voor een groot deel níet in het
  trait: `this_core`, `os_bell`, `kick_self`, `probe_disk`/`Disk`,
  `config`/`boot_param`, `temp_*`, `clock_knob`, de modules `watchdog` en
  `slots`, constanten als `KERN_RAM` en `FLAVOR`. Het wordt bij naam
  aangeroepen via `vboard::`. H.
- Gevolg: een **`cfg`-bos in hopos**. Drie `mod hw` in `watchdog.rs`
  (de Apple-module is een kopie van de UEFI-module), acht in
  `telemetry.rs`, drie `mod src` in `bench.rs`, acht in `flip.rs`, en
  board-eigenschappen als `cfg!` in `net.rs`, `cage.rs`, `kooi.rs`,
  `slots.rs`. Kandidaat: traits `Watchdog`, `Thermal`, `ClockKnob` in board,
  en constanten van het board in plaats van `cfg!`. ± 700 regels. H.
- **O6N en Altra** delegeren 10 tot 12 methodes één-op-één naar
  `self.uefi`; `probe_disk` en het NIC-skelet zijn gelijk. ± 200 regels. H.
- **Watchdog-drivers staan in board/** in plaats van driver/; de DW-WDT
  staat twee keer (rk3566, licheerv). H.
- De O6N-governor in telemetry is `policy::governor` zonder de boot-flank. H.

### 8.5 Interrupts

- **gicv3 implementeert `cpu::irq::Controller` niet**, gicv2, aic en de
  PLIC wel. Daardoor hebben qemuvirt, uefi en rk3566 elk een eigen
  claim/eoi-lus met dezelfde takken, en licheerv en qemuvirt-riscv ook
  (zonder `STRAY_LIMIT`), terwijl raspi en apple de `Dispatcher` gebruiken.
  uefi heeft daarbovenop een eigen lijntabel. ± 250 regels. H.
- gicv2 en gicv3 delen de distributor (`Gicd` tot `icfgr` identiek, de
  SPI-enable en -disable dezelfde code). ± 80 regels. H.
- `riscv::trap::IRQ_FLAG`/`take_irq` dubbelt `irq::IRQ_PENDING` en wordt
  weggegooid; `irq::run`/`report`/`wait` hebben geen aanroeper. H, dood.

### 8.6 Drivers: hetzelfde skelet met de hand

- **Het NIC-skelet zeven keer**: dezelfde DMA-indeling (`N_RX`, `N_TX`,
  `BUF_SIZE`, `BUF_OFF`, `DMA_NEED`), dezelfde IRQ-vorm (`set_irq`,
  `IrqAck`, `rearm`) en dezelfde RX-kopie. In drie boards een
  `static NIC_ACK`. Kandidaat: een `netdev::IrqAck`-trait en gedeelde
  helpers. ± 150-250 regels. H voor de duplicatie, M voor de waarde.
- **Link en PHY**: tg3 en rtl8126 doen clause 22 zonder `Mdio`-impl en een
  eigen link-lus van dezelfde vorm als `mdio::autoneg`; vier boards doen
  scan → autoneg → init zelf, drie drivers hebben `link_up` in de driver.
  ± 70 regels. M (quirks per chip).
- **virtionet en virtioblk** delen de status-handdruk, `new()`, `IrqAck`,
  de publish en zes foutvarianten buiten virtiopci. ± 90 regels. H.
- **brcmpcie implementeert `driver_pcie::Config` niet**; rpi5 bouwt de
  capability-walk en MSI-X met de hand na. ± 80 regels. H.
- **rkrng en rng200** hebben dezelfde `Regs`/`Mmio`, en de board-lijm
  erboven (`board/raspi/src/rng.rs`, `board/rk3566/src/rng.rs`) is bijna
  regel voor regel gelijk. ± 85 regels. H.
- **Wachten op tellingen** in plaats van op de klok: genet, gem,
  virtiopci, gicv3, its, brcmpcie. De duur hangt dan af van de CPU. H.
- **Tellers die niemand leest**: `rx_bad`, `doorbells`, `tx_full` in igb,
  rtl8126, tg3, virtionet; `requests`/`slowest_ns` in virtioblk. Alleen
  stmmac print zijn `Stats`. `netdev` heeft geen `stats`. H.
- **Vier bump-allocators** (virtionet, xhci, rtkit, `pcie::MmioWindow`). H.
- **Drie UART-pollers** met verschillend beleid (pl011 en ns16550 permanent
  dood, apple herstelt). H.
- **Het rearm-moment** verschilt (in `receive` of in `flush`) en `netdev`
  legt het niet vast. M.
- `smpro` parseert de PCCT zelf (hoort in `fw::acpi`); `board/o6n/src/cpc.rs`
  heeft eigen AML-primitieven naast `fw::aml`. H.
- Niet alles onder `driver/` is een driver: `codec` is een contract (zoals
  `netdev`, `blkdev` in de root), `dvfs` beleid. Andersom zijn `gui/rkscan`
  en `media/mve` wel drivers. Smaak.

### 8.7 arm64 en riscv naast elkaar

- `ArmSleeper::resident` en `RvSleeper::resident` zijn bijna kopieën, het
  `sleep`-skelet ook. Het fault-rapport van de OS-core (`settle`) ook, met
  kleine semantische verschillen (kick in `settle` alleen op arm). H.
- De uit-stub voor de koude flip twee keer met een andere signatuur. H.
- riscv heeft een eigen FNV (`hopos/src/cage_riscv.rs:443`) naast
  `abi::checksum::Fnv64`. H.
- De kooi bouwen: op arm in `cpu::el2::stage2`, op riscv in
  `hopos/src/cage_riscv.rs` en nog een keer in de zelftest van
  qemuvirt-riscv. H.
- `cpu::el2` bevat arch-neutrale delen die riscv gebruikt (roster, de
  rotatieregel, `OS_STATS`, `chain`). "el2" is daar een misnomer. H.

### 8.8 arm64 assembly

- Het EL2-regime voor bewoners staat in de switcher én in
  `oscore::arch::prepare`, met dezelfde letterlijke waarden; de
  EL1-restore (19 woorden) ook. H.
- "MMU en caches uit" op vijf plekken; drie boot-stubs (cpu, uefi, apple)
  met elk eigen MAIR/TCR/SCTLR, en de nVHE-tak van uefi met letterlijke
  waarden die ook in `el2.rs` en `boot.rs` staan. H. Deels moet het blob
  blijven.
- Twee flip-ingangen (x3 = `FLIP_ENTRY` en de UEFI-feitenpagina). V dat één
  protocol kan.

### 8.9 Config en firmware-feiten

- "Eerst het bestand, dan de cmdline" drie keer: `cfgwin::first`,
  `board_rk3566::boot_param`, `kern::nodecfg::text`. Hoort in
  `fw::bootcfg`. H.
- Config-toegang per board met andere namen (`config`, `boot_param`,
  `cfg_text`, `bootargs`), en in hopos kiest elk module zelf. `NodeCfg::parse`
  drie keer. Config-logica staat in `bench.rs`. Twee `boot_param`'s zonder
  aanroeper. H. a6ceae5 heeft een deel opgeruimd.
- De DTB lezen of kopiëren vijf keer; `bench::bootparam` kopieert de DTB bij
  elke aanroep opnieuw (tot 1 MiB). H.
- Het config-venster drie keer: `board/src/cfgwin.rs`, `image/hopcfg.py` en
  `hop image` in de hop-repo. V dat hopcfg.py kan vervallen.
- Geen gedeeld feitentype: `fdt::Fb` en `xnuboot::Fb`, de GIC als
  `fdt::GicV3` of als tuples uit de MADT; elk board bouwt zijn eigen. V.

### 8.10 Verzoek en antwoord zonder bouwsteen

`sync` heeft geen oneshot. Daardoor is het patroon "zend, wacht op een bel,
neem het antwoord" zes keer met de hand gemaakt: `slots::Reply`,
`switch::Ack`, `NatReply`, de `done[i]` van store, `Door` in hopos, en de
helpers `slots::call`, `rpc::call`, `rpc::freeze`, `rpc::kern_read`,
`deviceabi::call`. M-H dat het één bouwsteen kan zijn.

### 8.11 Twee paden waar één bedoeld is

- **blkdev**: `Paced` en `Queue` hebben dezelfde chunk-lus en dezelfde
  wachtmachine; in productie draait alleen `Queue`. Het contract heeft
  `start`/`poll_done` naast `start_tag`/`poll_tag`/`reap`. nvme heeft
  daarbij nog een eigen synchroon leespad voor de GPT. H.
- **hopfs**: `Fs::read_at`/`write_at`/`commit` naast de `*_shared`-functies,
  alleen voor tests en bench. H.
- **Logregels van een app**: via de outbox (standaard), via `KIND_LOG` over
  de system-API (alleen appspike), en via `LOGNET` (230 regels, niemand zet
  het aan). H.
- **Hop's poorten** 8080/9080 via `net::publish` (alleen TCP, twee
  gekopieerde lussen), buiten `Resident.ports` en `republish`; jobspec-poorten
  via `Cage::publish`. H.
- **Switch-beheer in de kooi-lijm**: `Attach`/`Detach` gebeuren in
  `hopos/src/kooi.rs`, de lifecycle weet niet dat de ringen van de switch
  af moeten (een FIXME). H.
- **Twee DRBG's** (Hash-DRBG in de kern, ChaCha20 in applib) en twee
  jitter-oogsters. Bewust gescheiden; wel twee keer review. Laag.
- `memcpy`/`memcmp`/`bcmp`-shims regel voor regel gelijk in `applib/src/mem.rs`
  en `hopos/src/mem.rs`. H.

### 8.12 Pollen waar een gebeurtenis hoort

- **De acceptor van welcome, bench en vitals** pollt elke 2 tot 5 ms als
  alle werkers bezet zijn, precies wat `docs/apps.md` verbiedt; de kern
  heeft een vierde pool (`DOORS`). Eén pool in applib met een `Signal`.
  ± 140 regels. H, open sinds de review van 01-10.
- **decode pollt de codec elke 1 ms** met een system-call; de ABI heeft
  geen wachtende poll. H.
- De committer kijkt elke seconde naar `Servicers` in plaats van een bericht
  van de lifecycle te krijgen; `flip::restore_nat` pollt elke 20 ms;
  `free_slot` doet tot `max_slots` rondreizen. M.
- In applib wekt een stille secundaire core 100 keer per seconde om vlaggen
  te zien. V of een kick dit kan vervangen.

### 8.13 Apps

- `fmt::Write` op een vaste buffer vijf keer (bench, welcome, vitals,
  display, usbin); `bounded::Text<N>` bestaat nog niet. H.
- `port_of` drie keer, `read_exact` drie keer (`TcpStream` heeft wel
  `write_all`), "blijf staan" op vier manieren, JSON-escape twee keer. H.
- De burn-werklast en het idle-percentage in bench en vitals. H.
- syncprobe (57 regels) kan een rol in appspike worden; replica gebruikt
  hem, dus dat kost afstemming. Keuze.
- cloudflared-lean (Rust, 5,4k) naast go/cloudflared (upstream-CLI, de Go-SDK,
  en ABI 1 die vast moet blijven). Keuze.
- Geen gebruiker in deze repo, hop of Stulp: `sys::Client::device_command`
  (dus het optische pad heeft geen end-to-end-toets vanuit een app),
  `Net::resolve_via`. V (grep).
- Stulp heeft een eigen kopie van applib die 1693 diff-regels afwijkt. H.

### 8.14 Scripts

- **De QEMU-tests**: 1340 regels komen woordelijk in vier of meer scripts
  voor (cleanup, objcopy, artifact-server, hop-build, wachtlus, POST,
  rapport). Functies in `lib.sh` schelen 600-800 regels. `probe`/`refusals`
  identiek in drie scripts; riscv-hop deelt 204 van 283 regels met
  `qemu-test-hop.sh`; de QEMU-regel van virt, EDK2 en riscv staat
  meermaals. H.
- **De image-scripts**: het `APP`-keuzeblok zes keer; `rpi4.sh` en `rpi5.sh`
  delen 92 van ± 143 regels; ELF-werk in inline Python verspreid (de
  RELATIVE-toets twee keer); de terugleesproef alleen voor de Radxa (een
  `mkcard --verify` zou elke kaart dekken). H.
- Twee manieren om een bewoner in te bakken (`HOPOS_EMBED` op Apple,
  `HOPOS_LRV_STAGE` op de LicheeRV). H.
- **Lijsten die synchroon moeten blijven**: de apps op vier plekken (gate,
  release, release-notes, jobs/README), `STAGE_MAX` vijf keer, de koude
  linkadressen vier keer. Drie node-configs die bijna gelijk zijn. H.
- Geen runner voor de hele QEMU-suite. M.

### 8.15 Docs

- De bouwcommando's per board op vijf plekken; elke test drie keer
  beschreven (scriptkop, docs/README, vlak-doc). De root-README mist de
  riscv-boards en Apple in de mapindeling en noemt 6 van de ± 20 tests. H.
- `docs/boards.md` heet UEFI maar draagt de config voor alle boards;
  ALLES.md verwijst naar `boards-*.md`, waar het niet onder valt. M.
- De testtabel in `docs/README.md` mist `qemu-riscv-test-flip.sh` en
  `-share.sh`; de kop zegt nog 30-09. H.
- Verouderd: `jobs/hopos-media-o6n.cfg` (v2-stijl, `hopos.flip.enable` bestaat
  niet, `hopos.console=5555` werkt bij toeval);
  `jobs/job-cloudflared-lean.json` wijst naar `-tamago.elf` die de release
  niet bouwt; `go/cloudflared/README.md` noemt gVisor;
  `docs/measurements.md:357` zegt "Zero 3W"; `image/apple/console.py` noemt
  `load-probe.py` en `console.go`; het commentaar in `hopos/src/main.rs:122`
  zegt nog "bump-allocator". H.
- `heap.md`, `stacktask.md` en de twee review-docs zijn niet gelinkt vanuit
  `docs/README.md`; de reviews hebben regelnummers die niet meer kloppen. H.
- De handboekregel "statische taken" klopt niet met de executor, die elke
  taak in een box zet en stil dropt als de tabel vol is. H.

### 8.16 Dood of zonder gebruiker

`Handoff.agent` (altijd leeg), `CommitWhy::Flip`, `FsActor::handle` en
verwanten (alleen tests), `codecabi::serve` (alleen tests), `sync::Stop` in
productie (gaat nooit af), `riscv::trap::jitter`, `irq::run`/`report`/`wait`,
`board/licheerv/src/lottery.rs` (0 bytes), de feature `window-b000`. H.
