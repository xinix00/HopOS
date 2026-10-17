# Raspberry Pi 4 and 5 on HopOS v3

Everything below for the Pi 4 (BCM2711) and the Pi 5 (BCM2712) is tested on
the host and builds for the target; the Pi 4 kernel also boots on QEMU
`raspi4b` up to the executor tick. Both Pi's run v3 on hardware: boot, EL2,
cage, self-test, Hop as resident, warm kernel flip, watchdog, RNG200 and
clock policy (P1, 2026-10-03; as of 2026-10-04 the Pi 4 is at P40 and the
Pi 5 at Q40). The status per board is in [ALLES.md](../ALLES.md), the numbers
in [measurements.md](measurements.md). The measurements (2026-07-07 to
2026-09-21) are in the code comments; this checklist says per step what the
console must show and what a deviation means.

## What exists

| Part | Where | Tested |
| --- | --- | --- |
| Shared Pi board: the entry at 0x80000 (EL2 init, cache invalidate, event stream), the DTB from x0, the memory map from the DTB (runtime, on top of the fixed table), the pool, the staging from `/chosen/linux,initrd-*`, `hopos.*` from the cmdline, the mailbox, the GIC-400 via `cpu::irq`, the CNTHP line of the OS core | `board/raspi` | 10 host tests: map (1 and 8 GB Pi 4), pool, slot plan, staging, cmdline, MAC, DTB status |
| Pi 4: addresses, A72 numbering (aff0), fixed tables (GB0 and GB3), GENET probe | `board/rpi4` | 2 host tests, const assertions on the tables; QEMU `raspi4b` |
| Pi 5: addresses, A76 numbering (aff1), fixed tables (GB0, 64, 65, 124), the RP1 chain and the NIC interrupt via the MIP | `board/rpi5` | 2 host tests, const assertions on tables and addresses |
| VideoCore mailbox: temperature, clocks, board MAC, framebuffer | `driver/vcmail` | 7 host tests (message layout, response codes, the "pending" rule, a round trip with a fake firmware) |
| GIC-400 as `cpu::irq::Controller`, plus the kick of the OS core: SGI 8 via GICD_SGIR, the GICC_HPPIR peek, the EOI with the source core | `driver/gicv2`, `board/raspi` (`KICK_SGI`, `os_bell`, `kick_self`) | 5 host tests on fake GICD/GICC; QEMU `raspi4b`: `HOPOS_OS_SELFTEST ok` |
| GENET v5 (Pi 4) | `driver/nic/genet` | 9 host tests: reset with DMA stop confirmation, rings that follow the hardware counters, QTAG, PROD wrap, full ring, RX path and bad frames, MDIO |
| Cadence GEM (Pi 5) | `driver/nic/gem` | 7 host tests: rings with the bus offset, DBW from DCFG1, AMP, TX handoff, RX and rearm, the explicit ISR ack |
| Broadcom STB PCIe RC (Pi 5, and the BCM2711 variant) | `driver/brcmpcie` | 6 host tests: window encoding, setup (UBUS, PLL, burst, VDM), BCM2711 reset, link failure, endpoint and BARs, RESCAL |
| RNG200 (BCM2711 0xFE10_4000, BCM2712 0x10_7D20_8000): the warm-up of iproc-rng200, the continuous test, the NIST/lockout restart from Linux; seeds the kernel DRBG (`cpu::drbg`, `trng::Kind::Soc`) in `discover`, only if the DTB node `brcm,bcm2711-rng200` is enabled | `driver/rng200`, `board/raspi/src/rng.rs` | 6 host tests on a fake block with FIFO (start order, warm-up bound, absent block, stuck source, health, empty buffer); QEMU: the node is missing, the block is not touched |
| PM watchdog (BCM2711 0xFE10_0000, BCM2712 0x10_7D20_0000): 12 s in ticks of 1/65536 s, first FULL_RESET and then the check that the counter runs (on hardware it only counts after FULL_RESET, Pi 5 2026-09-30); `hopos.wd=off` turns it off like Linux | `board/raspi/src/watchdog.rs`, `hopos/src/watchdog.rs` | 4 host tests on a fake PM block (ticks, a counter that stands still after FULL_RESET is stopped again and not armed, running counter armed with RSTC bits preserved, inheritance from a flip without check); QEMU: `hopos.wd=off` in the test, because FULL_RESET resets there at once |
| Clock knob: the ARM clock via the mailbox (full = firmware max or `hopos.mhz`, quiet = 600 MHz or the firmware minimum) under `driver_dvfs::run`, with the same slot counters as the O6N | `board/raspi/src/clock.rs`, `hopos/src/telemetry.rs` | 3 host tests on the modes (floor, cap, one clock); QEMU: 700/700 MHz, one clock, no knob |
| Linker script: raw image at 0x80000, the entry at the front | `hopos/link-raspi.ld` | the builds, `_pi_start == 0x80000` as link assertion |
| Card | `image/rpi4.sh`, `image/rpi5.sh` (both via `image/raspi.sh`) | deliver `kernel8.img` / `hop-agent5.img` and the card image |
| QEMU smoke test | `tools/qemu-rpi4-test.sh` | green: P2, bunny, DTB, map, mailbox, GIC-400, cage, staging, the OS core self-test (timer, yield, kick to itself), three ticks with `kicks=1`, and the two refusals: RNG200 not touched (`HOPOS_RNG_INSECURE`), PM watchdog off (`HOPOS_WD_OFF` with `hopos.wd=off`, because FULL_RESET resets there at once); with `GUI=1` the gui flavor, where the VL805 chain does not touch the RC because QEMU removes it from the DTB (`no enabled brcm,bcm2711-pcie`, `HOPOS_USB_NONE`) |

## The plan (both Pi's the same)

| Range | What | Mapping |
| --- | --- | --- |
| 0x0000_0000 to 0x0008_0000 | TF-A (BL31 or the armstub): not ours, no cache maintenance either | Normal (fixed table) |
| 0x0008_0000 to 0x0800_0000 | kernel RAM: image, stack, heap | Normal WB |
| 0x0800_0000 to 0x1000_0000 | load window: DTB 0x0F00_0000, boot scratch 0x0F10_0000, role word +0x100, staging 0x0F20_0000 (14 MB) | Normal WB |
| 0x1000_0000 to 0x1400_0000 | control pages and cage region | Device |
| 0x1400_0000 to 0x1500_0000 | DMA: NIC 8 MB, mailbox buffer 0x1480_0000 | Normal-NC, except the NIC's frame buffers: Normal-WB and XN, the driver sweeps them (Pi 4 0x1400_0000 to 0x1420_0000, the GENET; Pi 5 0x1420_0000 to 0x1460_0000, the GEM; the GEM's descriptors below them stay NC) |
| from 0x1500_0000 | the pool: DTB `/memory` minus `/memreserve/`, mapped at runtime | Normal WB |

## Building and flashing

```sh
sh image/rpi4.sh            # Hop from ../hop/hop; APP=appspike for the ABI proof
sh image/rpi5.sh            # GUI=1 for the gui flavor, CFG= for your own config
diskutil unmountDisk /dev/diskN
sudo dd if=target/hopos-rpi4.img of=/dev/rdiskN bs=4m    # or hopos-rpi5.img
```

`tools/mkcard` builds the card. Without a card image (the firmware is
missing): copy `target/sd-rpi4/*` (or `sd-rpi5/*`) onto the FAT partition of
an existing Pi card. The firmware is in `image/firmware/rpi4` and
`image/firmware/rpi5` (origin and sha256 in their `LEESMIJ.txt`). Pi 4:
`start4.elf`, `fixup4.dat`, `bcm2711-rpi-4-b.dtb` and a self-built TF-A
`bl31.bin` (REQUIRED: the stock armstub8 has no PSCI). Pi 5:
`bcm2712-rpi-5-b.dtb` and `overlays/bcm2712d0.dtbo`.

`hopos.cfg` is not a file on the card. The `initramfs` channel carries Hop's
image (`hop.elf`), and the config is in the kernel's window
([boards.md](boards.md), "The config (all boards)"): the script puts `CFG=`
in it (by default the layers `image/cfg/default.cfg`, `rpi4.cfg` or
`rpi5.cfg`, and `headless.cfg` or `headfull.cfg`). Since 3.0.11
`default.cfg` sets `hopos.storage=stateless`; the Pi's have no disk, so this
changes nothing there. `cmdline.txt` carries `hopos.stage=hop|app` (default
hop) and `EXTRA=`; `hopos.cores=N` and `hopos.oscore=` (on the Pi always
core 0) may go there too, but a key in the window wins.

UART: Pi 4 on the header (pin 8 TXD, 10 RXD, 6 GND, 3V3), `screen
/dev/tty.usbserial-* 115200`. Pi 5 on the 3-pin debug connector between the
HDMI ports, `screen /dev/tty.usbmodem* 115200`.

## The checklist

Per step: what must be there, and what it means if it is not.

1. **Bootloader log** (`uart_2ndstage=1`). Nothing: cable, port, or
   `BOOT_UART=1` in the EEPROM config.
2. **`P2`**: the Pi entry runs at EL2. Bootloader log but no P: loading
   fails (config.txt, DTB, armstub; Pi 5: `os_check=0`). `P1`: no EL2 from
   the firmware; the kernel reports `HOPOS_BOOT_EL` and parks.
3. **The bunny and `runtime rustc`**: MMU on with the fixed table, BSS,
   stack, `kmain`. P2 and then nothing: the MMU step (tables in
   `.data.pagetables`, TCR/MAIR from `cpu::boot`) or a fault before the
   console; an `exception: ... HOPOS_EXCEPTION` line names ESR, ELR and FAR.
4. **`fdt: N bytes at 0xf000000, model "Raspberry Pi 4 Model B", serial
   "1000000..."`**. A different address: `device_tree_address` not honored
   (the kernel accepts anything between 0x80000 and 0x1000_0000). `WARNING
   HOPOS_RAM_CHECK_SKIPPED`: x0 was not a DTB.
5. **`stage: N KB at 0xf200000, role hop`**. `no initramfs`: the
   `initramfs` line in config.txt or `hop.elf` on the card is missing.
6. **`mem: B banks, M MB mapped from the DTB, pool P MB`**. Expected: Pi 4
   8 GB around 7.5 GB, Pi 5 8 GB around 7.9 GB. A fault right after this
   line: the runtime mapping (the TLB invalidate in `board_raspi::arch`);
   note ESR/FAR.
7. **`vcmail: T C, ARM F MHz (max X MHz)`**. `HOPOS_VCMAIL_FAIL`: the
   mailbox buffer (0x1480_0000, Normal-NC) or the base; the rest boots on.
   Pi 5 with `arm_freq=1500`: max 1500.
8. **`boot: HopOS v3.0.13 on rpi4, EL2, 4 cores (big), N MB DRAM
   HOPOS_BOOT`** (the version of the build).
9. **`irq: GIC-400: GICD ... CTLR 0x1 ... cpu mask 0x1`**. CTLR 0 or mask
   0: TF-A left the GIC differently; note the line.
10. **Pi 4 NIC**: `net: genet rev 0x6000..., PHY at 1 (0x600d:...)`, then
    `net: genet up, 1000 Mbps full duplex, polled`, `HOPOS_NIC_UP` and a
    DHCP lease. `not a v5` or `no PHY`: note it; `no link within 8 s`:
    cable.
11. **Pi 5 NIC**: `net: pcie2 link up (status 0x..b0), RP1 1de4:0001`,
    `net: rp1 up, PHY at 1`, `net: gem up, 1000 Mbps full duplex, RX on
    INTID 166`. The line is an edge (an MSI via the MIP): the board
    registers it as `Trigger::Edge` and the dispatcher sets GICD_ICFGR
    before the enable (`cpu::irq::enable_as`). `PCIe bring-up failed` with
    the status in the line before it: RESCAL, PLL or training (the proven
    probe6 sequence). `RX polled: ...`: the NIC works, the MSI-X path does
    not; note the reason.
12. **`slots: cage up HOPOS_CAGE_UP`** and `core 1..3 mailbox Ok(Cold)`.
13. **`oscore: cpu 0 self-test timer=(Timer, ~1000 us) yield=(Yield, ..
    us) kick=(Ipi, .. us) HOPOS_OS_SELFTEST ok`**. Measured on QEMU
    `raspi4b` 2026-09-29: timer 1344 us on a 1 ms deadline, yield 22 us,
    kick 8 us. `kick=(Timer, ~200000 us)`: the SGI did not arrive or the
    peek did not see it; note the `irq: GIC-400` line (CTLR, cpu mask).
    Since 2026-09-30 each probe first handles what is already waiting at
    the GIC, retries up to three times if a device line interrupted it, and
    an interrupted probe names the line: `(Irq, 0 us, vec 9, INTID 166,
    INTID 166 pending before entry, try 3)`. Lesson of the first Pi 5 boot
    (2026-09-30): three times `Irq` after 0 us was the NIC line (or a
    firmware line, `other=1`) that fired once between `probe_nic` and the
    self-test; the vector set the flag and returned masked, but the dispatch
    task only runs once the executor runs, so the line was pending at every
    probe. If an INTID is still there after three tries, that line is really
    stuck: a level line without ack is then reported by the dispatch as
    `HOPOS_IRQ_STUCK` and turned off.
14. **The first placement**: `cage: slot 1 core 1 cold: PSCI CPU_ON
    mpidr=0x1` (Pi 5: `0x100`) `-> Ok(())` and `HOPOS_SLOT_START`. If it
    hangs after `cage: slot 1 built`: no PSCI (Pi 4: `bl31.bin` is missing
    or `armstub=` is not there). `Err(AlreadyOn)` on the Pi 5: the armstub
    already turned the cores on; then use an upstream TF-A as armstub.
15. **Hop**: `slot 1: applib: stage-1 on: RAM write-back, control page
    Normal-NC, rings write-back, 16 KB of tables HOPOS_APP_MMU` as the first
    line, then its lines via the servicer, `HOPOS_NODE_IP`, and `curl
    http://<ip>:8080/health`. A fault of Hop at EL1 (an alignment fault, an
    undefined instruction) shows since 2026-09-30 as `slot 1: Hop
    faulted at EL1 vec=4 esr=0x96000021 (data abort, alignment fault)
    elr=0x5... far=0x... HOPOS_HOP_FAULT`: the real ESR, ELR and FAR from
    applib's vector table. If it says `Hop faulted vec=8 esr=0x82000005
    (instruction abort, translation fault) far=0x200`, it jumped to an empty
    VBAR_EL1: a Hop built against an applib from before 2026-09-30
    (`image/rpi5.sh` builds it via `tools/hop-build.sh` against this work
    tree; `HOP_PATCH=0` takes the tag of the hop repo). `HOPOS_APP_NO_MMU`:
    the stage-1 was refused and every unaligned access faults.
16. **`HOPOS_TICK`** every second, `sleeps` goes up: the WFE sleep with the
    EL2 event stream works. If `sleeps` stays at 0 or the tick lags behind:
    the event stream (CNTHCTL_EL2 in `pi_entry!`).
17. **Let it run for minutes with traffic** (Pi 5): the C1 stepping can
    freeze silently under RX DMA plus fabric work; note the stepping and the
    time to the freeze.
18. **RNG200** (in the log right after step 7): `trng: RNG200 at
    0x107d208000 (BCM2712) online, the kernel DRBG is seeded from rng200
    HOPOS_RNG200_UP` (Pi 4: `0xfe104000 (BCM2711)`). The Pi 4 may sit here
    for up to two seconds: the first FIFO words after the warm-up come
    slowly on the BCM2711 (2026-07-11). `trng: WARNING RNG200 ...
    HOPOS_RNG_INSECURE` names the reason: `no such node in the DTB` (the DTB
    lacks the node, or an overlay removed it), `RNG_CTRL reads 0x... after
    enabling RBGEN` (the block does not answer), `FIFO stayed empty for 2000
    ms after the warm-up` (note: Pi 4 or Pi 5), `two equal words in a row`
    or `RNG_INT_STATUS ...` (the source is broken). A `HOPOS_EXCEPTION` with
    FAR 0x107d208000 or 0xfe104000: the address does not match the DTB.
19. **Clock** (after `irq: GIC-400`): `dvfs: ARM via the mailbox, full 1500
    MHz, quiet 800 MHz (firmware min/max 800/1500), policy Auto, cap 0,
    ... HOPOS_CLOCK_UP`, then `dvfs: -> 1500 MHz (full, boot)
    HOPOS_CLOCK_EDGE`, after 30 s without load `dvfs: -> 800 MHz (quiet,
    idle 30s) HOPOS_CLOCK_EDGE`, and every 10 s `dvfs: clock ... temp ...
    HOPOS_CLOCK`. The numbers follow config.txt (`image/rpi5.sh` sets
    `arm_freq=1500` and since 2026-09-30 `arm_freq_min=800`). `the firmware
    leaves one ARM clock, 1500 MHz ... HOPOS_CLOCK_NONE`: the card carries a
    config.txt without `arm_freq_min`; add the line or flash again. If the
    firmware reports a different number than requested on an edge, that
    number is in the edge line (the firmware clamps, the knob does not lie).
    `hopos.clock=max|quiet|firmware` and `hopos.mhz=N` in cmdline.txt work
    as on the O6N.
20. **Watchdog** (after the self-test): `watchdog: hardware reset armed (BCM PM
    watchdog at 0x107d200000, 12.0 s (786432 ticks of 1/65536 s)) - boot
    guard: blind pets until the node proves liveness HOPOS_WD_ARMED`, after
    DHCP and Hop's heartbeat `HOPOS_CANARY_LIVE`. If it says `watchdog: PM_WDOG
    reads 0x... 2 ms after FULL_RESET with 0xc0000 loaded: the counter
    does not run` and `HOPOS_WD_NONE`, this PM block does not count even
    when armed (WRCFG is clear again then); note the value read. On the Pi 5
    (2026-09-30) it turned out the counter only counts after FULL_RESET,
    hence the order arm-then-check. The real test: do not set `hopos.wd`,
    stop Hop or pull the cable, and within ~12 s after the first
    `HOPOS_CANARY_MISS` the Pi should restart by itself.
21. **VL805, the USB of the Pi 4** (gui flavor, after the NIC and before
    the self-test; `board/rpi4/src/usb.rs`). The test is a COLD boot: on
    2026-09-30 it gave `firmware loaded by the VideoCore, version now 0x0`,
    an SError in the self-test (`vec 11`) and `timeout on HCRST clear`, while
    the warm flips of that same day worked. Since then the chain is Linux's:
    the RC up with SCB0_SIZE to the inbound window, the link, the BARs with
    the endpoint still closed, `NOTIFY_XHCI_RESET`, 1 ms quiet, then every
    10 ms config 0x50 until a version is there (at most 1 s), and only then
    memory decode on and the host to the USB task. Expect, cold and warm
    (cold proven on hardware, P1g):

    ```
    usb: vl805 firmware 0x...... loaded by the VideoCore after N us (attempt 1, reply 0x0)
    usb: vl805 on PCIe (status 0xb0), xHCI window at 0x600000000 (bus 0xf8000000)
    ...
    oscore: cpu 0 self-test ... HOPOS_OS_SELFTEST ok
    usb: vl805 xHCI 1.0, 32 slots, 5 ports, 32-byte contexts
    ```

    Note N: that is the measurement the wait time must carry (Linux counts
    on less than 1 ms). A warm flip that inherits a running VL805 says
    `usb: vl805 firmware 0x... already loaded (attempt 1)`. What it means if
    it goes differently:

    - `usb: vl805 firmware still 0x0 N us after NOTIFY_XHCI_RESET
      (attempt 1, reply 0x..., MISC_CTRL 0x..., status 0x...)
      HOPOS_USB_VL805` and then a line with `(attempt 2)`: the first chain
      got no firmware, the second starts again at PERST#. If the second
      says `loaded`, USB works, but note both lines: only the second
      attempt works, just as on the warm flips.
    - `usb: vl805: no firmware after 2 attempts, the xHCI registers stay
      untouched HOPOS_USB_VL805` and `usb: no host controllers on this board
      HOPOS_USB_NONE`: no USB, but no SError and no HCRST timeout any more
      either. MISC_CTRL [31:27] should be 0x11 (17, 4 GB); the `reply` is
      what the firmware wrote back into the tag.
    - `usb: vl805: SError pending after <step>; the first EL1 entry will
      take it HOPOS_USB_SERROR`: that step caused the asynchronous abort that
      the self-test then sees as `vec 11` (`the PCIe bring-up`,
      `NOTIFY_XHCI_RESET` or `a config read of the VL805 version`). If it
      says `the boot, before any PCIe access`, it was not the VL805.
    - `usb: vl805 firmware handshake: <reason> (attempt N) HOPOS_USB_VL805`:
      the mailbox did not take the notify (`NOTIFY_XHCI_RESET refused by the
      firmware`: a start4.elf without the tag).
    - `usb: vl805: no enabled brcm,bcm2711-pcie in the DTB, PCIe not
      touched`: the DTB lacks the RC (QEMU, or an overlay).

## The first Pi 5 boot (2026-09-30)

After a clean boot up to `HOPOS_HOP_START`, two faults, both fixed in this
tree and tested on the host and QEMU at the time, since then green on
hardware (P1, 2026-10-03):

- **The OS core self-test** returned `timer`, `yield` and `kick` at 0 us,
  the first two as `Irq`. Not a line that was permanently on:
  `set_edge(166)` was already set before the enable, the tick counted 58 NIC
  interrupts per second (LAN noise, no storm) and `os(irq=0)`. It was a
  line that was already pending when the self-test started, in the boot,
  before the executor had ever run the dispatch (step 13). The kind of a
  line is now part of the registration (`cpu::irq::Trigger`), and a level
  line without ack that keeps coming back within one round is turned off
  (`HOPOS_IRQ_STUCK`).
- **Hop faulted at EL1** (`esr=0x82000005 far=0x200`: the instruction abort
  at `VBAR_EL1 + 0x200`, so the second fault). An app ran with the MMU off,
  so on Device memory, and the A76 gives an alignment fault on Device for
  every unaligned access; QEMU TCG does not check that under stage 2. Since
  2026-09-30 `_start` of every app turns on a stage-1 (`applib::mmu`) and a
  vector table catches every fault at EL1 (`CTRL_APP_FAULT_*`,
  `tools/qemu-test-fault.sh`).

## Known gaps

- **The OS core kick on hardware** (built 2026-09-29, first proven only as
  the kick to itself, on QEMU `raspi4b`; hardware IRQs including the kick
  are green on both Pi's since P1, 2026-10-03): the board gives the rotation
  SGI 8 as a 32-bit GICD_SGIR word `(1 << (16 + cpu)) | 8` plus the PA of
  GICD_SGIR (Pi 4 0xff84_1f00, Pi 5 0x10_7fff_9f00), and the switcher of an
  app core writes that word with the MMU off. Why 8 and not Rockchip's 7:
  see `board_raspi::KICK_SGI`. What had to be proven, in this order:
  1. the self-test on hardware (step 13), Pi 4 and Pi 5;
  2. the kick from an app core: an app that does HVC #6 (applib after a
     TX publication) or idles with a yield while Hop runs on the OS core.
     The `HOPOS_TICK` line must show `ipi=` and `kicks=` going up; if `ipi`
     stays 0 and `timer` goes up, the MMIO write from the app core does not
     reach the distributor (the Device attribute with the MMU off is the
     expected one; a missing `dsb` or a GICD that TF-A left Secure-only is
     not);
  3. the Pi 5: the GIC-400 sits behind the 40-bit addressing
     (0x10_7fff_9000); the switcher writes with a 64-bit address, so that
     should work, but it is the first MMIO from an app core outside the
     lowest 4 GB;
  4. that TF-A leaves the SGIs in Group 1 on both Pi's (the `plat/rpi` BL31
     has no secure interrupts): a kick that silently disappears while
     GICD_CTLR is 0x1 points to this; then try a different INTID.

  Open (Pi 4, 2026-10-04): on the WFE path of the OS core, 4 of 25 runs
  with a vitals against the applib of HEAD had one dial of 0.75 to 1.3 ms:
  the kernel picked up the SYN one event-stream period after the app's
  kick (see ALLES.md).
- **App cores after CPU_ON**: the Pi entry sets EL2 (VTTBR, CPTR, MDCR, HSTR)
  only on core 0. An app core comes via PSCI directly into the trampoline of
  `cpu::el2`; if those registers are garbage there (UNKNOWN after reset),
  the fix belongs in `cpu::el2`, not in the board.
- **`cpu::idle` sets the event stream in CNTKCTL_EL1**; under E2H=0,
  CNTHCTL_EL2 applies to EL2. The Pi entry sets that for core 0 (1.2 ms at
  54 MHz); other EL2 boards (O6N, Altra) need the same.
- **GENET interrupt** (Pi 4, SPI 157/158): not wired, polled (as in Go).
- **Framebuffer console**: in the gui flavor (`GUI=1 sh image/rpi5.sh`, or
  `GUI=1 sh image/flip-bundle.sh rpi5` on a running node), via `FB_ALLOC`
  on the mailbox (board/raspi/src/vcfb.rs). Measured 2026-09-30 on the Pi 5:
  1920x1080, 16 bpp, `HOPOS_FB_CONSOLE`, and both RP1 xHCIs `HOPOS_USB_UP`.
  Since a reflash the Pi 5 firmware refuses every framebuffer (0x80000001);
  on the Pi 4 the screen works, and its VL805 xHCI too (P2g, 2026-10-03).
  Without the gui flavor the kernel only reports whether the firmware gave
  one.
- **RNG200, PM watchdog, clock knob** (ported 2026-09-30, steps 18 to 20):
  proven on the host and on QEMU up to their refusal, and on hardware on both
  Pi's since P1 (2026-10-03: hardware RNG for the kernel and the slots,
  watchdog with canary, clock policy). Still to see: that a node that no
  longer gets a pet really resets.
- **NVMe (Pi 5 pcie1)**: deliberately not ported; the Pi's have no disk.
  `arm_freq=1500` is the thermal cap on the Pi 5; the clock policy follows
  it.
- **QEMU `raspi4b`** has no GENET and no PSCI; `tools/qemu-rpi4-test.sh`
  proves the boot up to the tick and the OS core transition with the kick to
  itself, not the network, an app core or a kick from another core.
