# Mac mini M4 on HopOS v3

The Mac mini M4 (Apple t8132, J773g, Mac16,10: 6 E-cores "sawtooth" in
cluster 0, 4 P-cores "everest" in cluster 1, 24 GB from 1 TiB, a Broadcom
57762 behind an Apple PCIe root port, the SSD behind the ANS coprocessor).
Everything below is tested on the host and builds for the target
(`image/apple-m4.sh` produces the raw image). There is no QEMU model of the
M4; none of it has run on hardware in v3. The dated lessons (2026-08-28 to
2026-09-04) are in the comments of the code.

## What exists

| Part | Where | Tested |
| --- | --- | --- |
| ADT reader: nodes, properties, paths, `-`/`_` equal, the ranges translation, depth bounded | `fw/src/adt.rs` | 9 host tests (plus truncation at every byte and a tree that is too deep) |
| boot_args reader: RAM contract, framebuffer, the ADT address with the virt-to-phys conversion modulo 2^64 (2026-08-30) | `fw/src/xnuboot.rs` | 3 host tests with the numbers of 2026-08-29 and the two forms of virt_base |
| GPT: read the table, the largest gap BETWEEN or AFTER the partitions | `fw/src/gpt.rs` | 5 host tests (the real M4 table of 2026-08-30) |
| AIC as `cpu::irq::Controller` (claim = ack, complete = unmask, the 4-bit target), plus the fast IPI (kick, ack) | `driver/aic` | 6 host tests on a fake register block |
| tg3 (BCM57762): reset, MAC, MDIO/PHY, rings, `netdev::Device`, INTx with tagged status (ack = mailbox closed, reopen on the tag on an empty ring, HOSTCC_MODE_NOW) | `driver/nic/tg3` | 24 host tests, among them the three lessons of 2026-08-29 (INDIR_ACCESS, PCI_COMMAND, NIC_ADDR of the std ring), FCS, and ack/reopen on the tag |
| RTKit mailbox (ASC) and the SART v3 | `driver/rtkit` | 16 host tests against a fake coprocessor |
| SMC: keys, floats, sensors, the hottest | `driver/smc` | 7 host tests |
| ANS NVMe: the ANS transport under the NVMe core (linear SQ, TCBs, NVMMU, coprocessor), the GPT as capacity, the write window, clean shutdown | `driver/nvme/src/apple.rs` | 7 host tests on the core's fake controller: the four ANS lessons, secure bar, CID and TCB per slot with the invalidation, refusal outside or without a window, 512-to-4K, a fallen-over coprocessor; tickets and read-ahead in the core tests |
| Board: boot stub, VHE entry, 48-bit map with PXN, console, boot_args and ADT, cores with class, the CPU_ON hook, AIC, timer FIQ, fast IPI, PCIe bring-up, tg3 with its INTx line, ANS, SMC, watchdogs, p-states and their watcher, the config window, slot plan with preflight | `board/apple` | 23 host tests (map and PXN, plan, pool from the contract, tunables, OS core choice, config window, watchdog alarm, p-state ceiling, `hopos.cages`, `hopos.nicirq`) |
| OS core rotation for `AppleVhe`: the fast IPI as doorbell, the ack at EL2, the FIQ as `Back::Timer`/`Back::Ipi`; the Apple kick in the switcher; the CPU_ON hook and the SCTLR wipe in `cpu::smp`; the VHE-aware event stream | `cpu/src/el2`, `cpu/src/smp.rs`, `cpu/src/idle.rs` | host tests (the kick word, the vector index, the hook, the stream bits); the shared paths on QEMU (virt, UEFI, UEFI+VHE) |
| Link script: raw image at 0x101_0000_0000, stub at 0, kernel at 0x10000 | `hopos/link-apple.ld` | link assertions; `image/apple-m4.sh` checks the parameter block |
| Loader (m1n1 proxy), measurement cycle, console, installer | `image/apple/` | the test bench |

## The plan

| Range | What | Mapping |
| --- | --- | --- |
| 0x100_0000_0000 to `top_of_kernel_data` | iBoot: boot_args, ADT, trust cache, m1n1 with its spin table | Device (2 MB) |
| `top_of_kernel_data` to 0x101_0000_0000 | pool, low part | Device (2 MB) |
| 0x101_0000_0000 + 256 MB | kernel RAM: stub, scratch 0xE000, param block 0xE100, config 0xF000, kernel 0x10000, BSS (with the tables), stack, heap | Normal WB |
| + 16 MB | DMA: tg3 8 MB, ANS 4 MB + RTKit buffers 3 MB + SMC 1 MB | Normal-NC |
| + 16 MB | control pages, cages, flip recorder, black box | Device |
| + 64 MB | boot scratch, handoff, staging (with magic) | Normal WB |
| 0x101_1600_0000 to `phys_base + mem_size` | pool, high part | Device (2 MB) |
| everything below 512 GB | MMIO (AIC, dockchannel, APCIe, ANS, PMGR) | Device (1 GB) |

Never a 1 GB block above 2^40: on this silicon that gives an "address size
fault, level 0" (2026-08-28). The pool comes from iBoot's contract
(2026-08-29: 23.7 GB in two regions); without boot_args 1 GB above the
window, loudly.

## Building and loading

```sh
sh image/apple-m4.sh                         # target/apple-m4/hopos-apple.img
CFG=hopos-m4.cfg sh image/apple-m4.sh        # with hopos.cfg in the window of the image
APP=appspike sh image/apple-m4.sh            # plus stage.elf for the staging
PYTHON=~/Git/m1n1/venv/bin/python3 CFG=hopos-m4.cfg \
  sh image/apple/boot-cycle.sh target/apple-m4/hopos-apple.img 90
STAGE=target/apple-m4/stage.elf ROLE=app ...  # an image in the staging
AT=0x10080000000 ...                          # let the stub relocate
image/apple/console.py /dev/cu.kis-100000-ch-0 60   # without m1n1
```

Without `CFG=` the script puts the three layers in the window
(`image/cfg/default.cfg`, `apple.cfg` and `headless.cfg`, see
[boards.md](boards.md), "The config"); `CFG="a.cfg b.cfg"` is a list of
layers, the last value wins; `CFG=` with no path leaves the window empty.
`EMBED=<ELF>` bakes a stripped app ELF (Hop) into the kernel image itself,
placed as Hop in slot 1. The release (`tools/release.sh`) does both:
`hopos-apple-headless.img.gz` is not a boot medium but a FAT stick (label
`HOPOS`, as in Go, tag v2.2.8) with `hopos-apple.img` (config and Hop baked
in), `install.sh` and a README; install from Recovery with `sh
/Volumes/HOPOS/install.sh go`. The M4 has no gui feature, so headless only.

The test bench (dossier, "Meetbank-recept"): `macvdmtool reboot serial`,
load over m1n1's USB gadget, `macvdmtool debugusb`, boot, and the console
over the dockchannel (`/dev/cu.kis-100000-ch-0`). Chrome and Spotify hold on
to the Debug USB device: close them before you start (2026-08-29). The
loader jumps with `P_VECTOR` to offset 0x800 with x0 = the real boot_args
block: the same handoff as after `kmutil configure-boot --raw
--entry-point 2048`.

`hopos.cfg` keys of this board: `hopos.oscore=small|mid|big|N`,
`hopos.pstate=off|E,P` (the ceiling per cluster, default 5,6),
`hopos.disk=off`, `hopos.smc=1`, `hopos.cages=on|off` (without a value:
cages if the preflight passes; `image/cfg/default.cfg` sets `on`, so with
the default layers the preflight is skipped), `hopos.wd=off`.

The config travels in the image, in the window of every kernel
([boards.md](boards.md), "The config window"): `CFG=hopos-m4.cfg sh
image/apple-m4.sh` or `hop image hopos-apple.img --config hopos-m4.cfg`.
Without a loader (after the installation) that is the only config. The
loader puts its `CFG=` in the same window and as text at 0xF000; `BARE=1`
leaves everything as it is (that is how the node boots after `kmutil`).
0xF000 is the fallback for an empty window: the loader's text, or the 4 KB
window that `image/apple-m4.sh` baked there until 2026-10-04. The boot line
says where the config came from: `cfg: hopos.cfg from the window in the
image`, `... from the 0xF000 window`, `... from the loader`, or `cfg: no
hopos.cfg ... HOPOS_CFG_NONE`. `APP=hop` with an empty `CFG=` is loud
(2026-09-25: an agent without config ran as `hopos-<random>` with an open
API).

A kernel flip lays the new kernel flat over the image, and a bundle
normally carries an empty window. So the running kernel hands its window to
the staged image (`board::cfgwin::carry`, `HOPOS_FLIP_CFG`), and 0xF000 as
well (`fwinfo::carry_config`); without that, flipped kernels boot without
`hopos.pstate=off` and do not adopt (2026-10-01). A kernel that cannot do
that yet (everything before 2026-10-01) gets the config inside the bundle:
`CFG=hopos-m4.cfg sh image/flip-bundle.sh apple` (or
`CFG=headless sh image/flip-bundle.sh apple` for the three layers).

The p-state tune runs in `start_interrupts` (not in `discover`: the lines
do not reach 5555 there) as a recipe in steps with an SError check after
each step; at the first SError it stops (`HOPOS_APPLE_PSTATE_SERROR`). On
the t8132 the write to P cluster +0x440f8 leaves an SError behind that stays
pending (and +0x48400 does not exist); E accepts the word. Without those
two: E 5/8 (2172 MHz), P 6/20 (2352 MHz), no SError (2026-10-01, stamp M7).

## What the kernel carries for Apple (2026-09-29)

Everything below is built and tested on the host, and the shared paths
(`cpu::el2::oscore`, `cpu::idle`, `cpu::smp`) are proven on QEMU against
regression (virt, UEFI, and UEFI under VHE with neoverse-n1:
`HOPOS_OS_SELFTEST ok`). It has not run on the M4 itself yet: that is the
checklist.

1. **The EL2 flavor in the glue.** `hopos/src/cage.rs` picks `AppleVhe` on
   this board and checks at build time that `board_apple::FLAVOR` is the
   same.
2. **`OsCore` for `AppleVhe`** (`cpu/src/el2/oscore.rs`): the kick is
   `Bell::apple(mpidr)`: the IPI_RR target `core | cluster << 16` with bit
   63 as "armed" (the target of E-core 0 is 0, and 0 means "do not kick").
   The switcher of an app core clears that bit and writes `msr
   s3_5_c15_c0_1` instead of ICC_SGI1R (on a yield and on HVC #6). The peek
   (`pending`) reads IPI_SR_EL1 bit 0 without an ack; the kernel acks
   itself, at EL2, when the turn comes back as `Back::Ipi` (2026-09-04). The
   timer and the kick come as FIQ: `fiq/lower-a64` (index 10) returns as
   `Back::Timer` (the CNTHP fired) or `Back::Ipi`, an IRQ (the AIC) as
   `Back::Irq`. FMO, IMO and AMO are set in the HCR of every resident.
3. **A CPU_ON from the board in `cpu::smp`.** `cpu::smp::set_cpu_on` (set
   in `discover`) routes `start_one` (the move to the OS core) and the cold
   start of a cage (`cage.rs`) through `board_apple::cores::cpu_on_mpidr`:
   m1n1's spin table, or PMGR plus the mailbox when we are the boot object.
   The entries such a core gets (`hopos_smp_entry`, and the trampolines of
   the Apple switcher) first close DAIF and turn SCTLR_EL2.M/C/I off,
   without set/way maintenance: m1n1 calls a released core as a function
   with its MMU still on (2026-08-29).
4. **The preflight.** The slot plan (`board_apple::slots::plan`) runs on its
   first call, on the OS core and before a single cage exists, the same
   three turns as the kernel's self-test (the CNTHP, a yield, the fast IPI
   to itself): `slots: OS-core preflight ... HOPOS_APPLE_PREFLIGHT ok`. Red
   (`HOPOS_APPLE_PREFLIGHT_FAIL`) = the plan refuses loudly
   (`HOPOS_SLOT_PLAN`) and the node runs on without cages, as before
   2026-09-29. `hopos.cages=on` skips the preflight, `hopos.cages=off`
   always refuses. The `hopos.cages=on` brake from before 2026-09-29 is
   gone.
5. **The choice of sleep per core** (unchanged): the board measures the
   timer FIQ (`HOPOS_APPLE_IDLE`) and picks WFI or WFE. An OS core that the
   kernel brought up itself (`hopos.oscore=small`) gets no timer FIQ on
   t8132 (2026-08-29): there the preflight fails on the timer turn, and
   that is intended.
6. **`cpu::idle::ArmSleeper::new` under E2H = 1** replaces only the event
   stream bits in CNTHCTL_EL2 (`merge_event_stream`): EL1PCTEN and EL1PTEN
   stay. The restore in the board is gone (board-uefi keeps its own, it is a
   no-op there now).
7. **The PXN hole.** `board/apple/src/mmu.rs` sets PXN (bit 53) on every
   block that is XN as well: under E2H = 1 bit 54 is only UXN, and the
   kernel was allowed to fetch instructions from Device (MMIO, the firmware,
   the cage window) and from the DMA region.
8. **The watchdog** (`hopos/src/watchdog.rs`, the `hw` module of Apple):
   `board_apple::wdt::{arm, pet, off}` on the primary of `/arm-io/wdt`,
   30 s at 24 MHz (the clock is not in the tree: the line says so).
9. **The clock and the temperature** (`hopos/src/telemetry.rs`): the
   watcher on the p-states (`board_apple::wdt::PStateWatch`) reports every
   jump (`HOPOS_APPLE_PSTATE`). With `hopos.smc=1` the temperature is in the
   boot log once (`HOPOS_APPLE_TEMP`); not by default, because on v2 the
   INITIALIZE answer never came (2026-08-31) and a half-started RTKit
   coprocessor that nobody polls fills up. It is not on the tick (every
   measurement talks the SMC awake and back to sleep).
10. **The waker of the app cores** (2026-10-02, `hopos/src/cage.rs`, Go's
    `kern/slots/waker.go`): an app core idles with a yield
    (`APP_IDLE_MODE = IDLE_YIELD`) and sleeps in the WFI of the switcher,
    because WFE at EL1 does not sleep on the M4 (2026-09-02). Every ms the
    kernel kicks every running app core with a yielded resident that is due
    (wake time, kick or RX); an RX write from the switch kicks the core of
    the slot itself (and the yielded secondaries), no longer mpidr 0.
    Marker `HOPOS_WAKER_UP`; with `hopos.tick=1` the counters are on the
    tick (`waker(rounds= seen= kicks= rx=)`). Go 2026-09-02: an app core
    from 74 % cpu and 1.3 M rounds/s to 0 % and 47 wakes/s.

## Checklist for the M4

Per step what the console (dockchannel) should show, and what a deviation
means. Under m1n1 first; only install after that. Build with a config:
`CFG=hopos-m4.cfg sh image/apple-m4.sh` (or the default layers without
`CFG=`).

1. **First light** before the MMU: `hopos x000001000xxxxxxx` (x0 =
   boot_args). Nothing: the stub did not run (wrong offset in the loader) or
   the dockchannel is elsewhere. x0 = 0: m1n1 gave no boot_args (P_VECTOR).
2. **The bunny and `runtime ...`**: the MMU, VBAR and the heap are up.
   Silent after line 1: the map (`mmu::build`) or the VHE regime; an
   `exception: ... HOPOS_EXCEPTION` with FAR in the DRAM points at the map.
   New on 2026-09-29: PXN on every XN block; an instruction abort (EC 0x21)
   with a PC outside the kernel RAM is then a jump that used to "work"
   silently.
3. `xnuboot: rev 3.x RAM 0x10001374000+24053 MB ... HOPOS_APPLE_BOOTARGS`
   and `adt: ... 10 cores (6 E + 4 P), this is core 6, serial ...`.
4. `watchdog: firmware watchdogs silenced: reg[0] ... (N window(s), ...)`.
   Without this line the node resets natively after 1:43 (2026-08-31).
5. `cores: via m1n1's spin-table (...)` under m1n1; `cores: ours` after the
   installation. `cfg: hopos.cfg from the window in the image, N bytes
   HOPOS_CFG` (or `from the loader`); `HOPOS_CFG_NONE` = an empty window.
   `cpufreq: cluster 0 (E): pstate 1 (900 MHz) -> 5/...`. With
   `hopos.smc=1`: `smc: die 41.5 C at boot HOPOS_APPLE_TEMP`;
   `HOPOS_APPLE_SMC_FAIL` or a `smc: ...` error is the measurement of
   2026-08-31 again (no shmem address), and then the boot should simply
   continue (at most ~8 s later).
6. **With `hopos.oscore=small`**: `oscore: moving the kern from core 6 to
   core 0 ... HOPOS_OSCORE_MOVE`, then `oscore: the kern runs on core 0
   HOPOS_OSCORE_UP`. Silent after that: the new core did not come out of
   m1n1's spin table or did not get through `hopos_smp_entry` (SCTLR wipe,
   then the regime); a `cores: cpu 0 ... not started` says why. An "address
   size fault, level 0" in the low DRAM on that core is the open puzzle of
   2026-08-29 (only under m1n1; retry after the installation). Without
   `hopos.oscore` the kernel stays on core 6 and there is no line.
7. `boot: HopOS v3.0.13 on apple-m4, EL2, 10 cores (big), 24576 MB DRAM
   HOPOS_BOOT`.
8. `disk: ans 0x481600000 nvmmu 0x485cc0000 nvme 0x4c5cc0000 sart
   0x485c50000 (v3) secure-bar true`, then the GPT lines
   (`iBootSystemContainer`, the APFS container, `RecoveryOSContainer`), and
   `HOPOS_ANS_UP` with the window, or `HOPOS_ANS_FULL` if macOS has not been
   shrunk yet (then no hopfs, and that is fine). If the ANS does not come
   ready: `resetting the ANS power domain and retrying` should appear once.
   With the default `hopos.storage=stateless` hopfs on the window starts
   empty at a cold boot (`HOPOS_FS_STATELESS`); `hopos.storage=stateful`
   keeps the tree.
9. `irq: AIC v... at 0x381000000: ... target N reaches this core, fast IPI
   and timer FIQ`. `no AIC target reaches this core`: the target
   measurement via ISR_EL1 does not work on this silicon; the node runs on
   the guard.
10. `watchdog: hardware reset armed (Apple WDT at 0x..., 30000 ms timeout
    at 24 MHz ...) HOPOS_WD_ARMED`. `no /arm-io/wdt` or `refuses to arm`:
    unguarded, loudly. If the node resets within 30 s after this line, the
    task does not pet (the executor is stuck) or the counter does not count
    at 24 MHz (then the timeout scales: measure the time until the reset).
    `dvfs: the APSC governs ... HOPOS_APPLE_PSTATE_WATCH`, and after that
    only lines when a cluster jumps (`cpufreq: cluster 1 (P) 6 -> 3
    HOPOS_APPLE_PSTATE`). Not a single jump in minutes of idle: the APSC
    does not clock down (the answer to the question of 2026-09-01).
11. `apcie: config 0x1cb0000000 ...`, a line per step (a deferred abort
    lands at the step that caused it), `apcie: brought up by HopOS, 6 power
    gate(s), 263 tunable(s), 2 of 3 port(s)` (2026-08-29), `apcie: link up,
    endpoint 02:00.0 14e4:1682`, `tg3: ASIC 0x57766 ...`, `tg3: LINK UP,
    1000 Mb/s full duplex`, `net: tg3 INTx on AIC irq 1253 (port 2 block
    1249 + INTA 4), first interrupt after N us ... HOPOS_NIC_IRQ`,
    `HOPOS_NIC_UP mac=1c:f6:4c:54:fa:90`. Under m1n1 the first line can be
    `apcie: already up`. `net: tg3 polled, AIC irq ...: the forced interrupt
    did not arrive within 50 ms, port INTSTAT ...`: the line did not arrive
    (bits 3:0 of INTSTAT are INTA to INTD, Linux `PORT_INT_INTx`, read
    before the NIC closes again; if bit 0 is set, the port saw INTA and the
    AIC number is wrong, and `hopos.nicirq=<n>` tries another one). The
    self-test is Go's "found by delivery" (2026-09-19). After that:
    `HOPOS_NET_PUMP` says `irq line, 10 ms guard`, and `hopos.nicirq=off` is
    the A/B against polling.
12. **DHCP** (`HOPOS_NET_UP`) and the tick: `HOPOS_TICK n sleeps=...`.
    Wakes per second around 954 (2026-08-29): `HOPOS_APPLE_IDLE` must then
    say `Wfi`. Many more: WFE on the event stream; check EVNTIS (ECV).
13. **The preflight and the cages**: `slots: OS-core preflight on mpidr
    0x80010100: timer=Some(Timer) yield=Some(Yield) kick=Some(Ipi)
    HOPOS_APPLE_PREFLIGHT ok` (not with `hopos.cages=on`, the default
    layer), then `slots: cage up HOPOS_CAGE_UP ...` and `oscore: cpu 6
    self-test timer=... yield=... kick=... HOPOS_OS_SELFTEST ok`. What a red
    turn means:
    - `timer=Some(Yield)`: the CNTHP FIQ did not reach this core (the
      spinner gave up by itself after two periods). On a core the kernel
      brought up itself that is expected (2026-08-29); on core 6 under m1n1
      it is new: check that FMO is set in HCR_EL2 and that
      `HOPOS_APPLE_IDLE` said `fiq-at-core=true`.
    - `kick=Some(Timer)` or `Some(Yield)`: the fast IPI to itself did not
      come in as FIQ; `kick=Some(Irq)`: it came, but IPI_SR_EL1 bit 0 was
      not set (then the peek is wrong, not the kick).
    - `yield=...` not `Yield`: the transition itself (the `_EL12` regime,
      VBAR of the os vectors); a `HOPOS_EXCEPTION` with ELR in
      `hopos_os_vhe_enter` is that path.
    - A hang without a line: the turn never came back. Then a FIQ did not
      go to EL2 (FMO) and the spinner did not give up either (CNTVCT from
      EL1 traps: CNTHCTL_EL2).
    Then the cold start of an app core: `cage: slot 1 core 1 cold: CPU_ON
    mpidr=0x80000000 entry=... -> Ok(())`. `Err(Other(-9))` or
    `Err(InvalidParams)` with a `cores: cpu N ... not started` line: no
    spin table address for that core (loader) or RVBAR is not ours. An app
    that does not start after `Ok`: the trampoline (SCTLR wipe, then VBAR
    and stage 2); m1n1's exception handler catches that on the console
    under the loader.
14. **Installing** (1TR, physically; `image/apple/install.sh`): `kmutil
    configure-boot -c hopos-apple.img --raw --entry-point 2048
    --lowest-virtual-address 0 -v "/Volumes/Macintosh HD"`. After that step
    5 must say `cores: ours` and `cfg: hopos.cfg from the window in the
    image`: those are the measurements that RVBAR points to our image and
    that the config came along without a loader. Step 13 then goes through
    PMGR and the mailbox. Back: the same line with `m1n1.bin`.

## Seen on hardware at M1 (2026-10-03)

- **The scrub of the Device-mapped pool**: the same Go app (tamago,
  `cloudflared-arm64-tamago.elf` from the rolling release `apps`, 64 MiB,
  with a deliberately invalid `TUNNEL_TOKEN`) placed three times in a row
  with a `DELETE` in between, each time in the same partition
  (`part=0x10068000000+0x4000000`, slot 4, core 4; the first time `cold:
  CPU_ON`, after that `dispatched to parked core 4`). All three the same
  lines (`mem: Go memory limit 27MB`, `appnet: RX doorbell served as
  interrupt`, the banner, `62 MB RAM, arm64 go1.26.4`) and the same end:
  exit code 4294967295, the -1 of cloudflared's `UsageError` for an invalid
  token. No `HOPOS_SLOT_FAULT`, no panic. The last lines of the app (the
  reason) do not reach the console and the task log: the kernel closes the
  slot (`HOPOS_SYSTEM_EVICTED`) before the servicer drains them.
- **The cold flip without PSCI**: not requested on hardware. The kernel
  refuses early in `flip::prepare` (`HOPOS_FLIP_COLD_NO_PSCI`, before the
  staging and the freeze), but on `"cold":true` Hop first stops all his
  tasks on the node (`HOP_FLIP_COLD_STOP`, `agentd-hopos/src/node.rs`) and
  only asks the kernel after that; a refusal puts them back
  (`HOP_FLIP_COLD_BACK`). With a live workload on the node that is not a
  test you can run without stopping residents.

## Not done

- A mini proxy in the image (every iteration after the installation costs a
  1TR visit).
- Hop on the OS core: `hopos/src/slots.rs` (`arch::SHARES_OS_CORE`) still
  excludes `AppleVhe` from the shared OS core (a different track), so Hop
  gets an app core on the M4: with `image/cfg/apple.cfg` in the group `hop`
  on an E-core (`hopos.hop.core-class=small`); `hopos.hop.sharegroup=system`
  is refused loudly. Remove that once step 13 is green on hardware.
- `pmgr::reset_ans` as a recovery path after a crash outside the retry in
  `probe_disk` (which does exist).
- The temperature on the tick: needs an SMC that stays awake and is polled
  (its syslog), not an open-measure-sleep per second.
- A core that waits in `cpu::el2::hold` while the cages refuse (a move with
  a red preflight) spins there: WFE does not sleep on the M4.
- A timer of a resident itself (CNTV or CNTP at EL1 with the interrupt on)
  is a FIQ on Apple that the kernel sees as a device; the residents of
  HopOS sleep via the HVC yield and do not turn it on.
- The framebuffer: iBoot leaves one behind, but nobody scans it out
  (2026-08-30); deliberately off.
