# Boards: the config of every node, and the UEFI machines (O6N and Altra)

First the config every board shares (the shared files and the config
window in the kernel), then the UEFI boards. As of 2026-09-29, night
(interrupts over PCI, watchdog, clock and thermals added); the storage
default as of 2026-10-05. What has been built in v3 for the Radxa Orion O6N
and the Ampere Altra, what you should see on the console at first boot, what
does not exist yet, and what you measure. The bar is v2
([measurements.md](measurements.md)): a v3 board is only done when it does
at least what v2 did on every measured point.

## The config (all boards)

The config of a node is three layers from `image/cfg`, concatenated into
the kernel's config window in this order. The last value of a key wins
(`fw::bootcfg::get`); a repeated key (`hopos.init[]`) counts all layers:

| Layer | File | Contents |
| --- | --- | --- |
| 1, the default | `image/cfg/default.cfg` | for every node: `hopos.cluster=hopos`, `hopos.insecure=1` and `hopos.console=1` (open on your own LAN), `hopos.cages=on`, `hopos.replay=0`, `hopos.storage=stateless` (a device starts clean and forgets everything: a cold boot ignores the index file on the disk, and `HOPOS_FS_STATELESS` on the console says the boot started empty; `stateful` in a layer of your own loads the tree from the disk and lets Hop's state in `/hop/` survive a restart). A kernel flip landing (a handoff found, warm or cold: `flip::landed`) always keeps the tree of the previous kernel. A firmware boot follows the config, also one that finds a stale handoff (`HOPOS_FLIP_STALE`) |
| 2, the board | `image/cfg/<board>.cfg` (`rpi4`, `rpi5`, `radxa`, `o6n`, `altra`, `apple`, `licheerv`) | only what differs per board, the placement of Hop: `hopos.hop.sharegroup=system` on the Pis and the Radxa (Hop on the OS core next to the kernel; a storm of RPCs from apps can starve him there), `hopos.hop.sharegroup=hop` with `hopos.hop.core-class=small` on the O6N, the Altra and the M4 (an app core of his own, a frugal one where the board has classes: the A520 on the O6N, an E-core on the M4; the Altra has no classes), and `hop` without a class on the LicheeRV (the C906L) |
| 3, the flavor | `image/cfg/headless.cfg` or `image/cfg/headfull.cfg` | only init jobs: headless none, headfull welcome on port 80 from the release `apps`, and the display app as a commented-out line |

Every image script stacks the three itself (the flavor follows `GUI=1` or
`MEDIA=1`), `tools/release.sh` per board and flavor, and
`CFG=headless|headfull sh image/flip-bundle.sh <board>` in a bundle. A
custom `CFG=` is a list of files in that order, for example the three
layers plus a file of your own behind them (a node outside your own LAN:
with `hopos.apikey` and `hopos.insecure=0`, or a node that must remember:
`hopos.storage=stateful`). `hop image --config` takes one file:
`cat default.cfg <board>.cfg <flavor>.cfg own.cfg >node.cfg`.

No `hopos.node` in there: without it a node is named after its board and
the last two bytes of its uplink MAC (`rpi4-4c54`, `o6n-1a2b`; the line
`HOPOS_NODE_DEFAULT` on the console), so the same layers fit every node.
The media node has its own `jobs/hopos-media-o6n.cfg`.

**The config window** (`board/src/cfgwin.rs`, like v2 with `image/hopcfg`
and `mkcard -cfgwindow`): every kernel carries a 16 KiB window in
`.data.hopcfg`, on a 4 KiB boundary: the header line `#HOPCFG1 window=16384
len=0000000432`, the config, and `#` lines as padding. Every image script
puts `CFG=` (or the three layers of `image/cfg`) in there with
`image/hopcfg.py`: in `kernel8.img` of the Pis, `hopos.img` of the Radxa,
`BOOTAA64.EFI` of the UEFI boards, the boot object of the M4, `monitor.bin`
of the LicheeRV, and with `CFG=` of `image/flip-bundle.sh` in a flip bundle.
A filled window is the config file of the node: it replaces `hopos.cfg` on
the ESP, in the initrd of the Radxa and from the m1n1 loader at 0xF000,
which stay as the fallback for an empty window (`HOPOS_CFG_WINDOW` on the
console says the window counted). The bootargs (`cmdline.txt`, the APPEND)
are read first and the file wins key by key (`fw::bootcfg::layered`). A
flip hands the window of the running kernel to a bundle with an empty
window (`HOPOS_FLIP_CFG`); a bundle with its own config keeps it
(`HOPOS_FLIP_CFG_OWN`). A custom config in a release image, a bundle or on
a card is set by `hop image` (the hop repo): `hop image <img> --config
my.cfg --write /dev/rdiskN` writes the card in one go, `hop image
<img|bundle|/dev/rdiskN>` shows what is in it.

Note: the Radxa and the LicheeRV derive their MAC address from
`hopos.node` (otherwise a fixed MAC, `HOPOS_MAC_FIXED`). The Radxa
therefore sets `hopos.node` in the APPEND of extlinux (`NODE=`, default
`radxa-1`); on the LicheeRV a custom `CFG=` with `hopos.node` or
`hopos.mac` belongs as soon as there are two on one LAN.

## Building

```sh
BOARD=uefi  sh image/uefi-run.sh   # target/uefi-esp-uefi/: the generic image, straight onto QEMU/EDK2 (BUILD_ONLY=1: build only)
BOARD=o6n   sh image/uefi-run.sh   # target/uefi-esp-o6n/EFI/BOOT/BOOTAA64.EFI
BOARD=altra sh image/uefi-run.sh   # target/uefi-esp-altra/EFI/BOOT/BOOTAA64.EFI
GUI=1 BOARD=o6n sh image/uefi-run.sh   # the gui flavor (GOP); MEDIA=1 for the VPU (docs/media.md)
```

On a stick: a FAT32 partition with that tree on it (the config is in the
window of `BOOTAA64.EFI`: the layers of `image/cfg`, or `CFG=`), Secure Boot
off. `tools/release.sh` makes a dd-able stick (`hopos-o6n-headless.img.gz`
and so on). Both boards are the UEFI board (`board-uefi`; the kernel window
of the O6N is `window-8000` at 0x8800_0000, that of the Altra `window-a000`
at 0xA000_0000) plus a crate of their own: `board-o6n` and `board-altra`.
The binary chooses with `--features board-o6n` or `board-altra`.

**Build status:** all three UEFI images build (`tools/gate.sh`), and
`tools/qemu-uefi-test.sh` boots the generic image on QEMU/EDK2 with
interrupts over PCI. Test before you walk to the board: that test green,
and `BOARD=o6n` and `BOARD=altra` produce a `BOOTAA64.EFI`.

## Interrupts over PCI, watchdog, clock and thermals (all UEFI boards)

MSI-X. A PCIe device gets, in this order (`board_uefi::irq`,
`hopos.nicirq=auto`):

1. **MSI-X via the GICv3 ITS** (`driver_gicv3::its`): the ITS from the
   MADT, its tables and the LPI tables in the last MB of the NIC DMA half
   (`ITS_DMA`, Normal-NC), one collection on the redistributor of the OS
   core, per device `MAPD`/`MAPTI`, vector 0 in the MSI-X table. The
   DeviceID comes from the IORT (root complex, possibly through an SMMU, to
   the ITS group). If that group names an ITS other than the first from the
   MADT (the Altra: eight, one per root complex), that ITS is brought up as
   well (`HOPOS_ITS_MORE`; its own tables in `ITS_MORE_DMA`, the LPI
   configuration and the redistributor shared, its own range of LPI
   numbers) and the device gets its doorbell: an MSI to the doorbell of
   another ITS never arrives (A7g, 2026-10-03). Without an IORT route there
   is no MSI-X (a wrong DeviceID is silent: the ITS drops the write), unless
   `hopos.nicirq=msix` forces the guess DeviceID = requester ID.
2. **INTx from the `_PRT`** of the host bridge (`fw::aml`: a minimal reader
   that only reads static `_PRT` packages and loudly refuses a method or a
   link device), after the swizzle through the bridges. On the O6N the
   DSDT says exactly what the device tree says (host test against the real
   DSDT: 477 for bus 0x30, and the four other root ports).
3. **Polling** at 300 µs, with the reason on the line.

`hopos.nicirq=` in `hopos.cfg`: `auto` (default), `msix`, `intx`, `off`
(or `0`), or an INTID (board knowledge the DSDT does not have).

The expected lines (QEMU/EDK2, 2026-09-29):

```
irq: ITS IIDR 0x43b TYPER 0x1f0001efb1, 16 DeviceID bits, device table flat (4 KB pages), doorbell 0x8090040, LPIs enabled on the redistributor at 0x80a0000 HOPOS_ITS_UP
net: virtio-net-pci at 00:02.0, MSI-X via the ITS, LPI 8192 (DeviceID 0x10), queue 256 HOPOS_NIC_IRQ
net: pump on the nic (irq line, 10 ms guard), uplink queues 2x128 HOPOS_NET_PUMP
HOPOS_TICK 2 sleeps=1120 polls=2070 irq(timer=0 nic=17 other=0) os(...) temp=-
```

Without an ITS: `HOPOS_ITS_NONE`; LPIs that were already on with someone
else's table: `HOPOS_ITS_FAIL` (EnableLPIs, once set, often cannot be
cleared).

**The watchdog** (`hopos::watchdog`, the policy in `kern::watchdog` with
its policy tests): the SBSA watchdog from the GTDT, 12 s requested, petted
every 2 s. Phase 1 blind until the sign of life, on a flip boot at most two
minutes on the raw counter; phase 2 only on proof. The sign of life is the
canary from Go: every round a new TCP connection over the node stack (3 s
patience, at most 15 s old), to Hop's agent at 10.100.0.2:8080 if Hop lives
on the node (through the switch and Hop's accept layer, plus his
heartbeat), otherwise to the node's own system port on the uplink address.
`hopos.wd=off` turns it off (also one the previous kernel armed). Lines:
`HOPOS_WD_ARMED`, `HOPOS_CANARY_LIVE`, `HOPOS_CANARY_MISS`,
`HOPOS_BOOT_GUARD`, `HOPOS_BOOT_GUARD_EXPIRED`, `HOPOS_RESET_REQUESTED`,
and per change of the dial `HOPOS_WD_CANARY_OK` or `HOPOS_WD_CANARY_FAIL`
(also without a block, so also on QEMU); QEMU has none: `watchdog: no SBSA
watchdog in the GTDT (QEMU?) - node liveness is UNGUARDED HOPOS_WD_NONE`.
Watch out for a 1 GHz counter (Armv8.6+, the O6N): WOR is 32 bits, so the
timeout becomes 8.6 s and the armed line says so.

**The thermals** (`hopos::telemetry`): every second on the tick
(`temp=41.5C`, `-` without a sensor) and on Hop's control page (`CTRL_TEMP`,
millidegrees; `applib::Ctrl::temp_milli_c`), for his heartbeat.

**The randomness** (`cpu::drbg`, `hopos::seed`, `applib::rand`): the board
seeds the kernel's DRBG in `discover` from the CPU's standard sources
(`cpu::drbg::seed_from_cpu`): RNDR on the O6N (Cortex-A720, FEAT_RNG), the
SMCCC TRNG of TF-A on the Altra, and otherwise jitter (EDK2 on QEMU). Until
2026-09-30 this board seeded nothing: the O6N booted without a single
`trng:` line and the DRBG stayed unseeded. The expected lines:

A UEFI board without FEAT_RNG and without an SMCCC TRNG (the O6N: the Cix
has neither, 2026-09-30) first says what the SMCCC TRNG probe saw
(`HOPOS_SMCCC_TRNG` or `HOPOS_SMCCC_NO_TRNG` with the reason: no EL3
monitor, SMCCC 1.0, or TRNG_VERSION NOT_SUPPORTED) and can, with the
feature `efi-rng` (on for the O6N), use the TRNG behind the firmware: before
ExitBootServices the stub asks the EFI_RNG_PROTOCOL for 64 bytes (what
Linux' `efi_get_random_bytes` does too) and the kernel seeds its DRBG with
them as source `efi-rng` (`HOPOS_RNG_EFI_UP`). Only on request: on
2026-07-13 a firmware hung in that protocol. After a flip the firmware is
gone: the departing kernel then puts 64 fresh bytes from its DRBG behind the
facts page (`HOPOS_FLIP_SEED`; Linux puts an `rng-seed` in the DTB of the
new kernel on kexec the same way), and the new kernel seeds from that, with
the same source `efi-rng` (`HOPOS_RNG_EFI_CARRIED`). Until 2026-10-03 a
flipped O6N seeded from jitter here.

```
trng: rndr online, the kernel DRBG is seeded from rndr (FEAT_RNG) HOPOS_RNG_RNDR_UP                  (O6N)
trng: smccc-trng online, the kernel DRBG is seeded from the SMCCC TRNG (DEN 0098) HOPOS_RNG_SMCCC_UP (Altra)
trng: WARNING the kernel DRBG is seeded from timer jitter, not hardware entropy: <reason> (EL3 monitor: yes|no); ... HOPOS_RNG_INSECURE
rng: every slot gets 32 bytes from the kernel DRBG (rndr) on its control page at build and every second HOPOS_RNG_SLOTS source=hardware
slot 1: applib: rng seeded from the kernel (rndr, gen 2) and 256 jitter samples HOPOS_APP_RNG source=hardware kind=rndr
slot 1: hop: TLS randomness from the kernel seed (rndr, hardware) mixed with timer jitter (512 samples) HOP_TLS_ENTROPY_HW source=rndr
```

Every slot gets 32 bytes from that DRBG on its control page: when the cage
is built, after a flip adoption, and fresh every second from the telemetry
tick. The block sits below the EL1 fault report, growing downward
(`abi::hopabi`, 2026-09-30):

| Offset | Word | Meaning |
| --- | --- | --- |
| 0xFA8 to 0xFC8 | `CTRL_RNG_SEED` | 32 bytes of seed, distinct bytes per slot |
| 0xFA0 | `CTRL_RNG_GEN` | seqlock: 0 no seed, odd the kernel is writing, even and not 0 valid; monotonic per page, also across a flip |
| 0xF98 | `CTRL_RNG_SOURCE` | `0xC0DE5EED` in the upper 32 bits, the source in the low byte: 1 jitter, 2 rndr, 3 smccc-trng, 4 SoC block |

The env space (`CTRL_ENV_MAX`) went from 0xEA8 to 0xE78 bytes for this.
Additive: an old app does not read the block; a new app on an old kernel
sees generation 0 (or a source word without the magic) and reports
`HOPOS_APP_RNG_NONE`, and still reads an env from an old kernel up to 0xEA8
bytes (`CTRL_ENV_LEGACY_MAX`). The app never uses the seed raw:
`applib::rand::Rng` mixes it with its own jitter in a ChaCha20 DRBG, and
reseeds by itself when a new generation is there. Why not a system op: the
seed is needed before there is a connection (the ISS of the netstack, Hop's
first TLS handshake), and a word on the page is the form of the temperature
and the wall clock.

**The clock** (`driver_dvfs::run` on the OS core): the load of the node is
the kernel (the sleep of its executor) plus every slot (`CTRL_IDLE`, all
cores). Sample every 10 ms, judge over 50 ms: if a resident misses more
than 30% of one core in idle (the kernel: 70%, its UART console is polled),
then full; down after 30 s of quiet. Every 10 s a measurement line `dvfs:
clock <MHz> (full|quiet), temp <C>, busy <source> HOPOS_CLOCK` (`busy slot 3
(0 permille idle)`, or `none (last slot 3, 14 s ago)`), and a line per edge
(`HOPOS_CLOCK_EDGE`, with the source: `(full, busy: slot 3 (...))`).
`hopos.clock=dvfs|max|quiet|firmware` pins, `hopos.mhz=` clamps the
ceiling. The knobs: the Pis the ARM clock via the mailbox, the O6N the
`_CPC` words, the Radxa `SCMI_CLK_CPU` of the TF-A with vdd_cpu over I2C
(`board_rk3566::clock`, 816 to 1800 MHz); the rest keeps the firmware's
clock.

## Radxa Orion O6N

### What exists

| Part | Crate | How tested |
| --- | --- | --- |
| NIC: RTL8126A (5G, O6) and RTL8125B/D/CP/BP (2.5G, O6N), the chip XID picks the variant | `driver-rtl8126` | 12 host tests against a fake chip: reset up to `hw_start`, rings, MAC fallback, EPHY table and PHY steps of the 8125B, link via PHYSR, RX bounds, TX padding and ownership, IRQ mask/rearm |
| NVMe: one core (admin and I/O queue, identify, sixteen tickets with their own pages and PRP list, a ticket above the MDTS as several commands, read-ahead, flush, 512-byte sectors on top of 4K namespaces) with the PCI transport | `driver-nvme` (`lib.rs`, `pci.rs`) | 17 host tests on the core with a simulated transport (ring and linear) and 2 for PCI (CAP, CC, stride), against a fake controller that executes real SQs/CQs and PRPs; a timeout, a foreign CID and a dead transport kill the driver |
| Thermometer: SCP via SCMI at 0x065d0000 (the channel of the DSDT `_TMP`) | `driver-scmi`, `board-o6n::thermal` | 6 + 2 host tests; picks CPU sensors, otherwise all Celsius |
| Clock: `_CPC` scanner (AML without an interpreter), domains, `CpcKnob` | `board-o6n::{cpc, clock}`, `driver-dvfs` | 3 + 3 host tests; the policy 8 host tests |
| Core classes: MADT, otherwise `_CPC` (25% clustering), otherwise fixed per MPIDR (aff1 0-3 small) | `board-o6n::class` | 4 host tests, including the measurement of 2026-09-17 (2232 ×4, 8192/7876/7246/6931 ×2 = small ×4, big ×8) |
| PCIe search: first Realtek, first NVMe, INTx per root port | `board-o6n::probe` | 3 host tests on a fake config space, and the INTx table against the `_PRT` of the real DSDT |
| NIC line: MSI-X via the ITS if the IORT knows the DeviceID, otherwise INTx 477 from the `_PRT` | `board-uefi::irq`, `driver-gicv3::its` | ITS: 5 host tests (commands, flat and two levels, a route); `_PRT`: 5 host tests; proven on QEMU/EDK2 with virtio-net |
| Clock: `CpcKnob` from the DSDT and SSDTs, after the OEM check `CIXTEK` | `board-o6n::machine`, `driver-dvfs::run` | the task on the host: dropping after 30 s, clocking up under load, the measurement line |

### EL2: the whole kernel under VHE (E2H = 1)

On the Cortex-A720 an EL1 under nVHE died within half a second
(2026-09-17), so the O6N runs VHE, and not only in the switcher of the app
cores: the whole kernel runs under E2H = 1 (`board-o6n` always turns on
`board-uefi/vhe`, `board/uefi/src/el2.rs`). The entry sets E2H with the MMU
still off and writes TCR_EL2 and SCTLR_EL2 in the form of TCR_EL1 and
SCTLR_EL1, CPTR_EL2 in the CPACR form and CNTHCTL_EL2 in the VHE layout
(EL1PCTEN/EL1PTEN at 10/11); the map sets PXN next to XN. The OS core
rotation (Hop on the kernel core) and the switcher of the app cores
(`hopos_el2_vhe`) use the `_EL12` encodings for the EL1 regime of their
residents, and those only exist under E2H = 1: a VHE switcher on an nVHE
kernel gave on 2026-09-29 an `exception: sync/el2 ESR=0x2000000 (EC 0x0)`
in `hopos_os_vhe_enter` at the self-test of the OS core. The build now
refuses that combination (`hopos/src/cage.rs`). Under E2H = 1 the kernel's
timer is the CNTHP (PPI 26: `cntp_*_el0` from EL2), the same timer the OS
core sets to the deadline; they alternate on the same core.

**Proven on QEMU, not on the A720.** `FEATURES=vhe CPU=neoverse-n1 sh
tools/qemu-uefi-test.sh` (EDK2, 4 cores, the whole appspike chain in two
slots and the tests from outside) is green three times, also with the
kernel moved to core 2 (`hopos.oscore=2`), and `FEATURES=vhe
CPU=neoverse-n1 BOARD=uefi sh tools/qemu-test-flip.sh` (Hop as a resident of
the OS core, the flip with the checksum over the vhe blobs) is green. The
plain nVHE path (`sh tools/qemu-uefi-test.sh`, cortex-a57) is unchanged. The
kernel window of the O6N has really been at 0x8800_0000 since 2026-09-29:
until then nobody turned on `window-8000`.

### What you should see (in this order)

- `uefi: booted at EL2 ... window 0x88000000+0x14000000 ... OEM "CIXTEK"`,
  immediately followed by `uefi: kern under E2H=1 (VHE), HCR_EL2
  0x480000038 HOPOS_UEFI_VHE` (HCR read back: RW, IMO/FMO/AMO and E2H). If
  it says `HOPOS_UEFI_VHE_FAIL`, the core did not accept E2H. Then `pcie:
  ...` per function (from `board-uefi`). Check that both Realteks and the
  NVMe are in there, with their BARs.
- `slots: cage up HOPOS_CAGE_UP ... hash=0x...` and `oscore: cpu 0 self-test
  timer=Some((Timer, ~1000+)) yield=Some((Yield, ..)) kick=Some((Ipi, ..))
  (back, us) HOPOS_OS_SELFTEST ok`: the rotation under VHE on the kernel
  core (on QEMU timer ~1300, yield ~20, kick ~10 µs).
- `o6n: 11 app cores, classes from Mpidr - small 3, mid 0, big 8` (or
  `Madt`). The kernel core is core 0; if that is an A520, it says small 3.
- `boot: HopOS v3.0.13 on o6n, EL2, 12 cores (...) HOPOS_BOOT`.
- `irq: ITS IIDR 0x... ... HOPOS_ITS_UP` (the GIC-700 has an ITS; the line
  gives the DeviceID bits and flat or two levels).
- `net: RTL8125? 10ec:8125 at 31:00.0 xid 0x... link 2500 Mbps full duplex, MSI-X via the ITS, LPI 8192 (DeviceID 0x...) (the DT table said INTID 477) HOPOS_NIC_IRQ`,
  or without an IORT route `..., INTx on INTID 477 (SPI 445) ...`, and then
  `HOPOS_NIC_UP mac=...` and after DHCP `HOPOS_NET_UP`. In the tick `nic=`
  must rise with the traffic.
- `dvfs: 5 _CPC domains, policy Auto, ... HOPOS_CLOCK_UP` and `dvfs: -> 2600
  MHz (fastest domain) (full, boot) HOPOS_CLOCK_EDGE`; after 30 s of quiet
  the edge to quiet, and every 10 s `HOPOS_CLOCK` with the temperature.
- `watchdog: hardware reset armed (SBSA watchdog, 8.5 s ...) ...
  HOPOS_WD_ARMED`, after DHCP (and Hop) `HOPOS_CANARY_LIVE`.
- `disk: nvme <model> at ... HOPOS_NVME_UP`, then `HOPOS_DISK_UP` and
  `HOPOS_FS_UP` (hopfs on the NVMe; the whole device belongs to HopOS). With
  the default `hopos.storage=stateless` a cold boot prints
  `HOPOS_FS_STATELESS` first and the tree starts empty; a flip landing
  mounts the generation of the previous kernel. After the interrupts the
  line: `disk: nvme at ... MSI-X via the ITS, LPI N (DeviceID ...), vector 0
  of N, first interrupt after N us, ... HOPOS_NVME_IRQ`, or `polled
  (<reason>) HOPOS_NVME_IRQ` preceded by `HOPOS_NVME_IRQ_DIAG`. The disk
  counts in the tick under `other=`, not under `nic=`; `queue_polls=` in
  `HOPOS_DISK_STATS` says how often the waiter looked at the CQ.
- Hop with the board layer `image/cfg/o6n.cfg`: `slots: Hop in the
  sharegroup hop (hopos.hop.sharegroup) HOPOS_HOP_GROUP`, `slots: Hop
  prefers a small core for his group (hopos.hop.core-class) HOPOS_HOP_CLASS`
  and `HOPOS_HOP_START slot=1 core=N cpu=N ... class=small` (an A520). A warm
  flip takes Hop along on his old core (`HOPOS_HOP_RESUMED`); the new
  placement applies from the next start of Hop (cold boot or cold flip).
- At the first temperature request: `hwmon: N SCMI sensors of M (first
  ...)`.

If it goes wrong, the line says which step: `rtl8126: <step> timed out (reg
0x..=0x..)`, `rtl8126: unsupported chip XID 0x...`, `nvme: CSTS.RDY never
became 1`, `HOPOS_NIC_FAIL`, `HOPOS_NVME_FAIL`.

The kick of the OS core is SGI 1 (`board_uefi::KICK_SGI`), not 8: TF-A
keeps SGI 8 through 15 as secure, and a non-secure kick there vanishes
silently (O6N 2026-09-30: `kick=(Timer, 100000 us, try 2)`). If the boot
says `irq: kick SGI 1 is not ours (GICR_IGROUPR0 ...) HOPOS_KICK_SECURE`,
the redistributor reads the group or the enable as 0; if the self-test says
`irq: kick SGI 1 did not arrive: ICC_SGI1R 0x... to MPIDR 0x...,
GICR_IGROUPR0 ... ISENABLER0 ... ISPENDR0 ..., ICC_HPPIR1 ...
HOPOS_KICK_LOST`, the word went out but the SGI was not pending after 1 ms.

### What does not exist yet

- **VHE on the A720 is unproven.** QEMU's neoverse-n1 is VHE without the
  quirks of the A720; what the O6N thinks of E2H = 1 is told by the first
  boot (`HOPOS_UEFI_VHE`, the self-test of the OS core, and whether a
  resident lives longer than half a second: Hop's `HOP_UP` and the tick).
- **MSI-X on hardware is unproven.** Whether the Cix IORT maps the root
  complexes to the ITS, and whether the RTL8125 gives its IntrStatus edge
  on MSI-X the way it does on INTx, is told by the first boot. Without an
  IORT route it falls back to INTx 477 (L80: one interrupt per frame, rtt
  p50 156-201 µs). With `hopos.nicirq=intx` you measure the fallback on its
  own.
- **The NVMe line on hardware is unproven.** Vector 0 (the admin CQ and the
  I/O CQ, IEN) via the ITS of its root complex, `board_uefi::pcie::wire_nvme`
  after `start_interrupts` (the bench and the mount poll before that), with
  a self-test (a Get Features on the admin CQ): what does not arrive polls
  with the reason. The waiter of `blkdev::Queue` then sleeps on the
  doorbell, with a 10 ms guard, instead of polling 100 us per round after
  every submit.
- **The `_CPC` class source**: now MADT, otherwise the MPIDR table.
- **The header UART** (0x040d0000) as a mirror: the console is the one from
  the SPCR.
- **The kernel core in the clock policy**: its idle time is in the sleeper
  of the executor and that reads no task; Hop (who shares the core) does
  count, as a slot.
- Two ports at once (the first supported one wins), VPU and USB (the
  media/gui flavors).

### What you measure (bar: L74-L83 of v2)

1. Boot up to `HOPOS_NET_UP` and `HOPOS_FS_UP`; note the variant (XID) and
   the link speed.
2. Network polled: rtt p50 (v2 polled 160-168 µs) and throughput M4 ↔ O6N
   (v2 with IRQ 112-117 MB/s). Pump rounds per second idle (v2 polled
   3333/s).
3. NVMe: write and read back 1 GiB with position-dependent content; MB/s
   both ways; `slowest_ns`. Restart with `hopos.storage=stateful` in a layer
   of your own: hopfs restores (`restored`).
4. Core classes: does the split match the hardware (4× A520, 8× A720)? Is
   core 0 an A520 or an A720?
5. Temperature: `temp=` on the tick and `HOPOS_CLOCK`, and whether it rises
   under load.
6. Interrupts: `nic=` in the tick against the packets; rtt p50 with MSI-X
   against INTx (`hopos.nicirq=intx`) against polled (`off`); no `stray`
   line.
7. Clock: `HOPOS_CLOCK_EDGE` to quiet after 30 s idle, and back to full
   within ~20 ms under load (v2 L83: 867 against 267 Msteps/s).
8. Watchdog: armed line (the real timeout), `HOPOS_CANARY_LIVE`; then pull
   the cable until `HOPOS_CANARY_MISS` and the reset (the O6N refresh: WRR
   does not work there, rewriting WOR does).

## Ampere Altra

### What exists

| Part | Crate | How tested |
| --- | --- | --- |
| NIC: Intel igb (I210 8086:1533 and family; QEMU's 82576 8086:10c9), advanced descriptors, RX 256 / TX 64, doorbell per burst | `driver-igb` | 11 host tests against a fake igb: reset (with Go's 10 ms pause), MAC from RAL0/RAH0, rings, RDT only after RXDCTL.ENABLE, link via MDIC and STATUS, RX bounds, self-flush per 32, TX ownership and the TDT≠TDH brake |
| NVMe | `driver-nvme` | see the O6N |
| Thermometer: SMpro via PCC channel 14 (xgene-hwmon), PCCT parser | `driver-smpro` | 4 host tests, including "an unfinished command leaves the firmware's buffer alone" |
| Classes: homogeneous big | `board-altra` | host test |
| PCIe search over up to eight segments, high BARs via `map_device` | `board-altra` | host test on two fake segments |

### What you should see

- Before the exit, on the firmware console: `uefi: window
  0xa0000000+0x14000000, ... MB DRAM, N map entries, M page tables`. N is
  the number of descriptors (Go: thousands); the buffer is 256 KB, and if
  the firmware asks for more, first `... HOPOS_UEFI_MAP_BIG`. If the
  identity map does not fit in 1024 tables: `HOPOS_UEFI_TABLES` and back to
  the firmware.
- If 0xA000_0000 is taken: `kernel window ... is taken`, the free regions,
  Go's six candidates each with `free` or `taken`, and
  `HOPOS_UEFI_WINDOW_FREE` with the first free one (rebuild with that
  window). One window per build: the layout, `slots` and the kernel flip
  compute with constants, although the kernel is a PIE.
- `uefi: booted at EL2 ...`, `pcie: segment ...` for each segment.
- `slots: pool ... MB in N regions, largest ... MB`. If something falls
  outside the pool (a full list, the ceiling of 64 regions):
  `HOPOS_UEFI_MAP_DROPPED` with the number of pieces and the MBs.
- `boot: HopOS v3.0.13 on altra, EL2, 128 cores (big) ... HOPOS_BOOT` (or
  80, depending on the SKU).
- `irq: ITS IIDR ... doorbell 0x100100130040, for 01:00.0 (the IORT's ITS for its root complex), collection on the redistributor at ... HOPOS_ITS_MORE`
  (the igb hangs off ITS 7 of the eight),
- `net: igb 8086:1533 at ... link 1000 Mbps full duplex, MSI-X via the ITS, LPI 8224 (DeviceID 0x70100), first interrupt after N us, pump on the line with a 10 ms guard HOPOS_NIC_IRQ`,
  then `HOPOS_NIC_UP`, `HOPOS_NET_PUMP` with `irq line, 10 ms guard` and
  `HOPOS_NET_UP`. If the fired vector does not arrive: `polled (the forced
  interrupt (EICS) did not arrive within 50 ms)`. Before that, one line
  `net: igb MSI-X diag: ... HOPOS_NIC_IRQ_DIAG`: the DeviceID and the IORT
  route (SMMUv3 with CR0 and GBPA, the ITS the group names against ours
  from the MADT), whether MAPD/MAPTI/INV/SYNC ran in this kernel life, the
  MSI-X control, the command register, entry 0 and the PBA as the function
  returns them, GPIE/IVAR0/EIMS/EICR of the igb, and an `INT` from the ITS
  itself: if that one arrives, the ITS side works and the igb's write does
  not reach the ITS; if it stays silent, the problem is in the ITS, the
  redistributor or the collection.
- `hwmon: SoC 45.2C (SMpro, PCC channel 14)` and then `temp=` on the tick.
- `watchdog: hardware reset armed (SBSA watchdog, 12.0 s ...) ... HOPOS_WD_ARMED`
  (servers are well-behaved SBSA; the first real trial of this path).
- `dvfs: server clocks are firmware domain on this board HOPOS_CLOCK_NONE`.
- `disk: nvme ... HOPOS_NVME_UP`, `HOPOS_DISK_UP`, `HOPOS_FS_UP` (stateless
  by default, as on the O6N), and after the interrupts `disk: nvme at ...
  MSI-X via the ITS, LPI N ... HOPOS_NVME_IRQ` (the NVMe has a root complex
  of its own and so perhaps an ITS of its own: `HOPOS_ITS_MORE` for it,
  before the one of the igb).

### What does not exist yet

- **The igb on MSI-X has not been seen on hardware yet.** Default
  `hopos.nicirq=msix`: vector 0 via the ITS, the driver in the MSI-X mode of
  Linux `igb_configure_msix` with one vector (GPIE, IVAR0 for RX queue 0,
  EIAC/EIAM/EIMS; ack EIMC, the pump reopens on an empty ring), and a
  self-test with EICS: what does not arrive polls with the reason.
  `hopos.nicirq=off` in `hopos.cfg` is the way back without a rebuild.
  Never INTx: the `_PRT` INTx kills the SoC (L83, 2026-09-19, UART proof);
  the board refuses `intx` and an INTID (`board_altra::nic_irq_mode`).
- **The NVMe line**: as on the O6N, never INTx (the function has INTx off
  from the probe on).
- The memory plan is that of `board-uefi`: whether the pool reaches the
  ~300 GB above 512 GB (v2 2026-07-15: 1.62 GB without the high map) is told
  by the boot line of `board-uefi`.

### What you measure

1. Boot up to `HOPOS_NET_UP`/`HOPOS_FS_UP`; the igb variant and the link.
2. Throughput and rtt p50/p99 on MSI-X against `hopos.nicirq=off` (netmeter
   and bench pull).
3. NVMe as on the O6N; the BAR is high: is `map_device` right?
4. The number of cores and the pool size against the firmware memory map.

## QEMU `-device igb`

The igb driver is testable on QEMU (82576), MSI-X included. By hand
(2026-10-03): the ESP of `BOARD=altra image/uefi-run.sh` on that script's
QEMU line, with `-cpu max` and `-device igb,netdev=n0,romfile=` instead of
virtio-net. Then `MSI-X via the ITS, LPI 8192 (DeviceID 0x10), first
interrupt after ~120 us`, DHCP and `HOPOS_NET_UP` over the line, and the NIC
interrupts rise with the traffic; with `hopos.nicirq=off` polled. No script
yet (a `NIC=igb` variant of `tools/qemu-uefi-test.sh`).
