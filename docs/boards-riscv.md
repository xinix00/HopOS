# RISC-V on HopOS v3: QEMU virt riscv64 and the LicheeRV Nano

The second architecture. QEMU virt riscv64 is the test bench, the Sipeed
LicheeRV Nano (SG2002, XuanTie C906) the board. The measurements on this
board (2026-07-30 to 2026-08-19) are in the code comments; this page says
what exists, how to build it, and per step what the console on the hardware
must show.

## Machine mode, and why

HopOS runs on RISC-V in machine mode. The cage is a PMP whitelist (bounding)
plus an Sv39 table (relocation), and only machine mode can program PMP. The
image therefore takes the place of OpenSBI:

| | QEMU virt | LicheeRV Nano |
| --- | --- | --- |
| Who loads us | QEMU `-bios none -kernel hopos` | the vendor FSBL, MONITOR slot of `fip.bin` |
| Where | 0x8000_0000, every hart | 0x8400_0000 (RUNADDR; lower does not boot, 2026-08-19) |
| a0 / a1 | hart id / DTB (top of RAM) | hart id / no DTB |
| Timebase | 10 MHz | 25 MHz (40 ns per tick) |
| Cache coherent | yes | no: `dev::push`/`pull` do `th.dcache.cpa`/`cipa` (feature `thead`) |

Under OpenSBI (`-bios default`) the kernel lands in S-mode and the PMP belongs
to the firmware: then there is no cage, and `kmain` refuses, just like
"HopOS requires EL2" on ARM (`board.privilege(3)`). SBI HSM for the harts is
therefore not an option either: in machine mode there is nobody below us to
ask. A hart starts via the park loop of the boot stub
(`cpu::riscv::boot::start_hart`: mailbox plus `msip`) or, on the LicheeRV, via
the reset block of the C906L; the C906B of the LicheeRV has no doorbell and no
reset block and polls its mailbox.

The ARM to RISC-V table (`cpu/src/riscv/mod.rs`):

```text
ARM                     RISC-V
EL2                     machine mode         (the kernel)
EL1-app                 supervisor mode      (a slot)
HVC-yield               ecall-yield          (a0 = wake time, a7 = 0/1)
stage-2 (VTTBR)         Sv39 (satp) + PMP    (relocate + bound)
ERET                    mret
PSCI CPU_ON             msip on a parked hart; reset block on the C906L;
                        the C906B polls its mailbox
GIC                     PLIC + CLINT
```

## What exists

| Part | Where | Tested |
| --- | --- | --- |
| Boot stub: `_start` in M-mode, stack, BSS, `mtvec`, FPU, the park loop of the other harts (check first, then `wfi`), the reset entry for a hart the kernel takes out of reset (`reset_pc`: the hart id from `set_reset_hart`, not from `mhartid`), the T-Head regime (`mxstatus`, `mcor`, `mhint`, `mhcr`) on every hart | `cpu/src/riscv/boot.rs` | QEMU; host test of the start |
| Kernel trap entry: flag, source masked in `mie`, wake the dispatcher; exceptions loud | `cpu/src/riscv/trap.rs` | QEMU; host tests |
| CLINT: `mtimecmp` in 32-bit spec order, `msip`, the probe | `cpu/src/riscv/clint.rs` | QEMU; host tests |
| Sleep and clock: `wfi` on its own `mtimecmp` with MIE masked, TIME CSR | `cpu/src/riscv/idle.rs` | QEMU; host tests |
| PLIC as `cpu::irq::Controller` | `cpu/src/riscv/plic.rs` | QEMU (virtio-net line); host tests |
| TRNG: none, loud (`HOPOS_RNG_INSECURE`) | `cpu/src/riscv/trng.rs` | |
| PMP whitelist as TOR | `cpu/src/riscv/pmp.rs` | 5 host tests |
| Sv39 relocation in 2 MB blocks, T-Head attributes | `cpu/src/riscv/sv39.rs` | 4 host tests |
| The M-mode switcher: yield with wake time, exit, fault, park on the CLINT, the kick as wake, the doorbell (RX), the kill tick, the revoke of a sleeper, the recheck of the list on a cold boot | `cpu/src/riscv/switch.rs` | QEMU: self-test and appspike |
| The cage: `kern::cage::{Cage, Cores}` as policy for both architectures (`Kooi`, `KooiCores`), with the riscv side over PMP, Sv39 and the switcher (`Rv`) | `hopos/src/kooi.rs`, `hopos/src/cage_riscv.rs` | QEMU: appspike twice through the lifecycle |
| The OS core: residents on the kernel's hart, in its idle (the transition M to S and back, the rotation over sched block 0, the alarm on the deadline) | `cpu/src/riscv/oscore.rs`, `idle.rs` (`RvSleeper::host`) | QEMU: self-test on every boot, Hop as resident |
| The cage self-test (yield, exit, escape, kill tick, sleeper) | `board/qemuvirt-riscv/src/cage.rs` | QEMU, every boot |
| applib on riscv64: `_start` in S-mode, the panic, the timebase from the control page (`CTRL_TIMEBASE_HZ`) | `applib/src/rt.rs`, `arch.rs`, `clock.rs` | QEMU: appspike 9 of 9 |
| T-Head cache maintenance in `dev::push`/`pull` | `dev/src/lib.rs` (feature `thead`) | builds |
| Board QEMU virt riscv64: ns16550, CLINT, PLIC, virtio-mmio net and blk | `board/qemuvirt-riscv` | `tools/qemu-riscv-test.sh` |
| Board LicheeRV: ns16550 (stride 4), dwmac plus ePHY recipe, CLINT, PLIC, the DW watchdog (probe, arm, pet), `hopos.cfg` from a window in the image, the C906L via the reset block, appspike in the image | `board/licheerv` | builds; host tests |
| dwmac (DWMAC1000, 3.x) with every descriptor and buffer on its own cache line, as an ops table on the shared stmmac core | `driver/nic/stmmac` (feature `dwmac1000`) | 12 host tests for the DWMAC1000, 12 for the core |
| Linker script | `hopos/link-riscv.ld` (build.rs sets the base per board) | |
| FIP from the donor, hashed, with `hopos.cfg` and appspike in it | `image/licheerv-agent.sh` | not on hardware |
| The cold kernel flip: the app hart out of the image (reset block or the off stub of the switcher), the M-mode trampoline, the bundle (`virt-riscv`, `licheerv`) | `hopos/src/flip.rs`, `cpu/src/el2/chain.rs`, `cpu/src/riscv/switch.rs` (`off_stub`), `image/flip-bundle.sh` | QEMU: `tools/qemu-riscv-test-flip.sh`; not on hardware |

## Building and running

```sh
cargo build --target riscv64gc-unknown-none-elf -p hopos --features board-qemuvirt-riscv
cargo build --target riscv64gc-unknown-none-elf -p appspike
sh tools/qemu-riscv-test.sh               # boot, cage self-test, appspike in slot 1 and 2, the external test
APP= sh tools/qemu-riscv-test.sh          # only the boot gate
sh tools/qemu-riscv-test-hop.sh           # the loop: Hop on hart 0, a jobspec, appspike on hart 1, the restart
sh tools/qemu-riscv-test-flip.sh          # the cold kernel flip with `hop flip --cold` (docs/flip.md, "riscv64")

sh image/licheerv-agent.sh                                  # target/licheerv/fip-licheerv.bin
CFG=~/lrv.cfg APP=appspike sh image/licheerv-agent.sh /dev/diskN   # with config and appspike, onto the card
```

Two builds, not one: the feature `thead` (in `dev` and `cpu`) only comes
with `board-licheerv`. So build per board with `-p hopos --features ...`,
never both boards in one cargo invocation: cargo then unifies the feature and
the QEMU image carries T-Head instructions that are illegal there.

The QEMU line (`qemu_rv` in `tools/lib.sh`, used by every riscv ring):

```sh
qemu-system-riscv64 -M virt -m 1G -smp 2 -bios none -nographic \
  -kernel target/riscv64gc-unknown-none-elf/release/hopos \
  -global virtio-mmio.force-legacy=false \
  -netdev user,id=n0,hostfwd=tcp:127.0.0.1:10100-:10100 -device virtio-net-device,netdev=n0 \
  -drive file=disk.img,if=none,format=raw,id=d0 -device virtio-blk-device,drive=d0 \
  -device loader,file=appspike.stripped,addr=0xa8200000,force-raw=on \
  -device loader,addr=0xa8100000,data=<size>,data-len=8 \
  -device loader,addr=0xa8100008,data=0,data-len=8
```

The last loader is the role (0 = app, 1 = Hop). `BOOTARGS=...` adds
`-append "$BOOTARGS"`: boot parameters in `/chosen/bootargs` of the DTB,
which the board reads as a config layer, like `image/qemu-run.sh` on arm64.

### Storage on riscv64

Since 3.0.11 a node is stateless by default (`hopos.storage=stateless` in
`image/cfg/default.cfg`): on a cold boot hopfs ignores its index file and
starts empty (`HOPOS_FS_STATELESS`). Only a real flip landing
(`flip::landed`, a found handoff, warm or cold) mounts the tree the old
kernel committed. Until 05aee54 this used `flip::jumped`, which on riscv64 is
true on every boot (the trampoline passes no marker), so every riscv boot
counted as a flip landing and stayed stateful. A firmware boot that still
finds a handoff pair wipes it and boots cold and stateless
(`HOPOS_FLIP_STALE`). The rings that test the cold restart
(`tools/qemu-riscv-test-hop.sh` among them) boot with
`BOOTARGS=hopos.storage=stateful`. The LicheeRV has no disk (`disk: none`),
so there it only means that a node starts empty after a cold boot; a flip
stays stateful.

Measured 2026-09-29 (QEMU 11.0.2), the markers of the gate, green twice:

```text
cage: riscv switcher on hart 1: yield resumed at 0x5000000c, exit state 4, escape state 4 with mcause 7 mtval 0x80000000 vec 1, spinner state 4 after the kill tick (1210 us), sleeper state 2 -> 4 HOPOS_RV_CAGE_UP
boot: HopOS v3.0.0 on qemuvirt-riscv, EL3, 2 cores (big), 1024 MB DRAM HOPOS_BOOT gen=1 stamp=dev
cage: hart 1 (core 1) in the switcher: wake 0x2004008, bell 0x2000004, sleep cap 100000 ticks, kill tick 100000 ticks, reset false HOPOS_RV_HART_UP
cage: slot 1 built: part 0xa6000000+0x2000000 -> va 0x50000000, satp 0x80000000000a7e12, pmp 4 entries cfg 0x8000f00, ctrl 0xa7e00000, core 1 (hart 1)
cage: slot 1 dispatched to hart 1 (BootPending + msip) HOPOS_RV_DISPATCH
HOPOS_SLOT_START slot=1 core=1 cpu=1 entry=0x50010000 part=0xa6000000+0x2000000
slot 1: HOPOS_APPSPIKE_TIMER ok slept_ns=10521000
slot 1: HOPOS_APPSPIKE_NET ok ip=10.100.0.2 dial_us=5441 tx_kicks=4 log_us=540 flush_us=1679
slot 1: HOPOS_APPSPIKE_FS ok file=hallo.txt size=62 list=1 us=12855
slot 1: HOPOS_APPSPIKE_DONE pass=9 fail=0
HOPOS_SLOT_DONE slot=1 exit=0
slot 1: stopped, partition and core released HOPOS_SLOT_STOPPED
HOPOS_SLOT_START slot=2 core=1 cpu=1 entry=0x50010000 part=0xa6000000+0x2000000
slot 2: HOPOS_APPSPIKE_DONE pass=9 fail=0
HOPOS_SLOT_DONE slot=2 exit=0
slot 2: stopped, partition and core released HOPOS_SLOT_STOPPED
```

The `HOPOS_RV_HART_UP` line now also prints the kick address
(`..., bell 0x..., kick 0x..., sleep cap ...`).

"EL3" on the boot line is the privilege mode: 3 = machine mode. The
`dial_us` (5.4 ms) is higher than on arm64 (2.9 ms): an app cannot ring the
kernel hart yet (no HVC #6 counterpart), so a TX waits for the failsafe of
the switch.

### The LicheeRV donor

`image/licheerv-agent.sh` needs two vendor files (not our own work):
`image/firmware/licheerv/donor-fip.bin` and
`image/firmware/licheerv/fiptool.py`. Both come from Sipeed, and both were
recovered and hashed on 2026-09-30:

| File | Origin | sha256 |
| --- | --- | --- |
| `donor-fip.bin` (440832 bytes) | Release `20260114` of [sipeed/LicheeRV-Nano-Build](https://github.com/sipeed/LicheeRV-Nano-Build/releases/tag/20260114), asset `2026-01-14-16-03-d4003f.tar.xz`; inside it `2026-01-14-16-03-d4003f.img`, partition 1 (FAT16, type 0x0C, LBA 1 to 32768, 16 MiB), the file `fip.bin` in the root. Next to it are `boot.sd`, `ver` (`2026-01-22-14-17-d4003f.img`) and the flag files. | `d85e68836f57a9fcb1bbfba3c1ccf93d1b062ac168305d7f0cc83a72a796c6b9` |
| `fiptool.py` (25165 bytes) | The same repo, branch `main`: `fsbl/plat/cv181x/fiptool.py` (not the `cv180x` version next to it, which has a different hash). | `cc1d37d0d8fbcb3e6180c0403bcf4fa5c7038306be13779f3b8b948915084bf5` |

This is the donor; since 2026-10-02 it is in the repo
(`image/firmware/licheerv/`), and the script finds it there by itself:

```sh
LICHEERV_DONOR_SHA256=d85e68836f57a9fcb1bbfba3c1ccf93d1b062ac168305d7f0cc83a72a796c6b9 sh image/licheerv-agent.sh
```

To recreate it: unpack the tar (1.7 GB image), and take `fip.bin` from
partition 1, for example on the Mac with
`hdiutil attach -readonly -imagekey diskimage-class=CRawDiskImage 2026-01-14-16-03-d4003f.img`
and a `cp` from the mounted `boot` volume; or without mounting with
`dd if=2026-01-14-16-03-d4003f.img of=p1.img bs=512 skip=1 count=32768` and
a FAT reader (`mcopy -i p1.img ::fip.bin donor-fip.bin`).

Not every Sipeed release gives the same bytes. The `fip.bin` of release
`20251230` (`2025-12-30-20-00-6073d5.img.xz`) has the same size, with the same
FSBL (build time `2025-12-29T19:21:03+08:00`), DDR parameters and OpenSBI,
but a different U-Boot in the LOADER_2ND part (29832 bytes differ from
offset 0x28208); sha256
`ead936116224c25467917859594d6f5f9af2fa883b52e19be86c780ff770494d`. That
U-Boot is never started (we are the monitor), but the FSBL does unpack it
to 0x8020_0020, below RUNADDR; only the donor above has been measured.
So set `LICHEERV_DONOR_SHA256`, so the script refuses a different donor.

## The cage

Proven on QEMU (2026-09-29) through the kernel's lifecycle: appspike in slot
1 and slot 2, one after the other on hart 1, each in its own partition behind
PMP plus Sv39, with all its tests green, exit 0 and the confirmed stop. The
policy (build, start, stop, revoke, quiet) is the same for both architectures
(`hopos/src/kooi.rs`); per ISA only the registers and the tables differ
(`hopos/src/cage_riscv.rs` next to arm64's `cage.rs`), and the resident list
of each core is one implementation (`cpu::el2::roster`):

```text
ARM (cage.rs)                     RISC-V (cage_riscv.rs)
stage-2 table + VMID              PMP whitelist (TOR) + Sv39 in the tail
trampoline, x0 = control page     cold boot by the switcher, a0 = page
mailbox + SEV, cold PSCI CPU_ON   BootPending in the list + msip
revoke: table zero + TLBI         CTX_REVOKE: yield, kill tick, rotation;
                                  or the reset block (C906L)
OS core (Hop next to the kernel)  the same shape: `cpu::riscv::oscore`
SMP units, share groups           one resident per app hart
```

- **Build** (`build`): first out of every resident list and the ctx block
  cleared, then the control page (entry, slot, `CTRL_IDLE_MODE` = yield, the
  wall clock, `CTRL_TIMEBASE_HZ`), the rings, the Sv39 table in
  `ABI_MAP_OFF` of the tail (the app RAM rwx, the tail as device without
  T-Head cache bits; a resident of the OS core shares the L1 with the kernel
  and gets the tail cached: on 2026-10-03 the pull over the wire went from
  3.9 to 9.8 MB/s), one TOR window over the claim plus the deny-all, the
  regime in `CTX_REGIME`, and the RX head for the doorbell
  (`CTX_RING_HEAD_PA`). The app window is 768 MB: the tail carries a root
  plus one table (`ABI_MAP_PAGES` = 2), so the window stays within the
  gigabyte of `LINK_BASE`.
- **Start** (`dispatch`): the slot in the list of its hart, state
  BootPending, the bell. On a cold boot the switcher reads the list again
  AFTER the state: a slot that moved to another hart never starts on two.
- **Sleep**: an app hart idles with the yield (a0 = wake time); a `wfi` from
  a resident never wakes. The switcher sleeps for it on the CLINT, and wakes
  on the wake time, the bell (then every sleeper gets one turn), or the
  doorbell (the RX head grew past the threshold at `CTRL_RX_DOOR`, like
  `el2::rx_due` on ARM).
- **Stop**: the graceful stop before the kill (b1ae2a8). The kernel takes the
  slot's ports away, writes the grace period in ms into `CTRL_KILL` on the
  control page (Hop asks 3 s; 1 stays "stop now" for the Go SDK) and kicks
  the hart. The app sees it in its heartbeat (50 ms), rings its stop doorbell
  (`HOPOS_APP_STOP grace_ms=N`) and exits. If it is not quiet within the
  grace period, `CTX_REVOKE` with 1 s (`REVOKE_GRACE`): the switcher reads it
  on every yield, on every kill tick (10 ms, MTIE only on while a resident
  runs; a tick resumes the same resident, no preemption), and on every round
  over a sleeper. A hart with a reset block and without a tick goes into
  reset and back into the switcher; the C906L has had the tick since
  2026-10-02 (the time slice of its shared hart), so there the reset is no
  longer the knife. Evict and detach come only after this.
- **Quiet** (`quiet`): the ctx state is Empty or Dead. The switcher writes
  Dead only after the full cache sweep of the hart (the teardown), so then
  the partition is really back.

What the board self-test (`board/qemuvirt-riscv/src/cage.rs`) proves on
every boot, before the glue: the cold boot with relocation, the yield and the
resume at `mepc + 4`, the exit, the whitelist (a store to the kernel:
mcause 7), the kill tick (a resident that never yields, dead 1.2 ms after its
revoke with a tick of 1 ms), and the revoke of a sleeper (Saved to Dead
without a resume).

## The OS core

The kernel's hart is shareable (PORT.md decision 2), as on arm64: a core like
any other, as share group `system` (`kern::pool::SYSTEM_GROUP`), with the
kernel as fixed resident with priority. A job with the tag
`"sharegroup":"system"` lives there, and so does a job without a group that
finds no free app core (`HOPOS_PLACE_SYSTEM`, docs/apps.md). Hop lives next
to the kernel (`Placement::hop`, share group `hop`) if the board chooses so
(`HOP_ON_OS_CORE`: QEMU yes, the LicheeRV no), and the idle of the executor
is a turn for the residents (`cpu/src/riscv/oscore.rs`, `RvSleeper::host`).
The transition is a function: `enter` saves ra, sp, gp, tp and s0..s11 of the
kernel, sets `mtvec` to its own trap entry and the resident's cage (PMP with
read-back, `satp`), and does `mret`. Every trap from the resident (a yield,
an exit, a fault, or a machine interrupt: the PLIC, the kick, the alarm on
the executor's deadline, which in S-mode are always taken) saves it in its
ctx block and returns into the kernel as if `enter` had finished. The
interrupt stays pending and the kernel takes it as soon as it opens its mask,
as after a `wfi`. The turn is at most 10 ms (`TURN_CAP_NS`). Who gets the
turn is decided by `cpu::el2::next` on riscv64 and arm64, with the rule of
the switcher of a shared app core (whoever has work, an idle is a yield, RX
behind the doorbell makes a sleeper due at once, the core only sleeps when
nobody has work); the kernel is not itself a resident and always goes first.
A revoke on the OS core is a request plus a kick (`CTX_REVOKE` and
`CTX_KICK_PENDING`, `el2::recall`), and `el2::next` is the only place that
turns it into a dead resident, before its next turn and without it running a
single instruction more: the way back (`settle`) does not read the revoke, on
both architectures. KVM does the same (`KVM_REQ_VM_DEAD`, read in
`vcpu_enter_guest`).

The kernel uses no f-registers (0 f-instructions in the image, tested by
`tools/qemu-riscv-test.sh`), so the transition does not save f0..f31: what a
resident had in them is still there on its next turn. Hop itself does use
them (38 f-instructions in `agentd-hopos`, measured 2026-09-29).

Measured 2026-09-29 on QEMU:

```text
oscore: hart 0 self-test timer=Some((Timer, 1645)) yield=Some((Yield, 75)) kick=Some((Ipi, 64)) exit=Some((Exit, 71)) (back, us) HOPOS_OS_SELFTEST ok
HOPOS_HOP_START slot=1 core=0 cpu=0 entry=0x50010000 part=0xa4000000+0x4000000 image=3614256 env=149
slot 1: hop: agent up node=hopos-qemu cluster=hopos agent=:8080 leader=:9080 cores=1 HOP_UP
HOPOS_SLOT_START slot=2 core=1 cpu=1 entry=0x50010000 part=0xa2000000+0x2000000
slot 2: HOPOS_APPSPIKE_DONE pass=9 fail=0
slot 1: hop: adopted 1 running cage(s) from the saved state HOP_ADOPTED
os(in=803 irq=5 ipi=0 timer=44 yield=754 exit=0 fault=0 idle=6218 res_ms=153 kicks=0)
```

`tools/qemu-riscv-test-hop.sh` is `tools/qemu-test-hop.sh` with
`HOP_ARCH=riscv64`. It builds `agentd-hopos` with `tools/hop-build.sh`, just
like the aarch64 tests: against the applib, abi and sync of this work tree.
It boots with `BOOTARGS=hopos.storage=stateful`, so `HOP_ADOPTED` after the
cold restart finds the saved state.

Who writes what (the cache-line rule, the C906 is not coherent,
2026-07-30): of the sched block the kernel only writes lines 1..3 (list,
length, alarm, bell, sleep, tick), the switcher line 0 (running slot,
rotor); of the ctx block the kernel writes everything before BootPending and
after that only `CTX_REVOKE` (a line of its own), the switcher the rest.

## The LicheeRV checklist, in boot order

Console: the UART0 pins of the LicheeRV (GND, TX, RX), 115200 8N1 (the FSBL
sets it up; HopOS does no init). Build with
`CFG=~/lrv.cfg APP=appspike sh image/licheerv-agent.sh /dev/diskN`, with at
least `hopos.node=<name>` in `lrv.cfg` (then a MAC of its own).

1. **The FSBL jumps into us.** Expect the bunny and `runtime ... none/riscv64`
   right after the FSBL lines. Nothing after the FSBL: RUNADDR does not match
   KERN_BASE (the script refuses an ELF entry that is not 0x8400_0000), or
   the donor still sets LOADER_2ND to a runaddr (the script zeroes the
   param2 block; "0 param2 blocks" means a different donor).
2. **`boot: LicheeRV Nano (SG2002), machine mode monitor`.** Then
   `board: CLINT: mtimecmp writable`. `HOPOS_CLINT_FAIL` means the c900
   CLINT does not hold the 32-bit write: the kernel then polls (works, uses
   more power). Never a read at 0xbff8 (`mtime`): it does not exist (bus
   error, 2026-07-30).
3. **`trng: WARNING ... HOPOS_RNG_INSECURE`.** Should always be there: there
   is no hardware TRNG. No secrets of value on this node.
4. **`watchdog: probing the DW-WDT at 0x3010000`**, then `DW-WDT answers, the
   watchdog task may arm it`. Silence after the probe line is a bus error on
   the WDT block: remove the probe and report.
5. **`config: hopos.cfg from the window in the image, N bytes HOPOS_CFG_UP`**
   (the window every kernel has, [boards.md](boards.md)). `HOPOS_CFG_NONE`:
   an empty window (built with `CFG=` without a path). Later in the boot
   `HOPOS_CFG_BAD` if the window is broken or not UTF-8 (`image/hopcfg.py`
   and `hop image` already refuse that when writing).
6. **`HOPOS_BOOT`** with `licheerv, EL3, 2 cores`. An `exception: illegal
   instruction` before this line is the T-Head regime (`mxstatus`
   THEADISAEE): compare `mxstatus` with the measured FSBL value 0xc0638000.
7. **`HOPOS_TICK 1..3`**: sleeping on the CLINT. Ticks that stutter or
   `sleeps` that does not go up: the `wfi` does not wake on `mtimecmp` (the
   2026-08-01 class); the 2 ms guard rail still keeps the node alive.
8. **The watchdog**: `watchdog: hardware reset armed (DW-WDT at 0x3010000, TOP
   13 (21474 ms), reset on expiry) ... HOPOS_WD_ARMED`, then after the lease
   `HOPOS_CANARY_LIVE`. Irreversible until the reset: for a UART post-mortem
   put `hopos.wd=off` in the config. A node that restarts after ~21 s without
   `HOPOS_CANARY_MISS` before it: the pets do not arrive (the restart write to
   CRR).
9. **The network**: `net: dwmac version 0x1037, PHY 0 id 0043:5649, link 100 Mbps
   full duplex`, then `HOPOS_NIC_UP` and `HOPOS_NET_UP`. Without `hopos.mac` and
   `hopos.node`: `HOPOS_MAC_FIXED` (a second LicheeRV on the same LAN
   collides). `version reads 0`: the clock gates (REG_CLK_EN_0 bit 25/26). `no PHY
   on the MDIO bus`: the ePHY recipe did not take (do not change the order).
   No lease: look at `rx_bad_len` and the diag line of the dwmac; a ring that
   goes silent after a few frames is the lesson of 2026-07-30 (an invalidate
   that throws away a CPU write), and that is why the dwmac puts everything on
   its own cache lines.
10. **No storage**: `disk: none` is correct; there is no SD driver. A cold
    boot therefore starts empty (stateless, see "Storage on riscv64").
11. **The role split** (since 2026-10-03): the kernel stays on the C906B
    (1 GHz), where the FSBL starts it: `the kern on hart 0 (C906B), app hart 1
    (C906L)` in the boot line. Only the PLIC of the C906B has the dwmac
    (source 31, see "Not done"), so `net: dwmac irq 31 on the PLIC
    HOPOS_NIC_IRQ` and `HOPOS_NET_PUMP` with the line instead of the 300 µs
    pump. The kernel sleeps there on `mtimecmp` with `wfi` (two weeks of
    production in Go); CLINT index and PLIC context are 0 (the CLINT and the
    PLIC are per core, every core is hart 0 to itself). It shares its core as
    group `system`: welcome (no tag, no free core) joins it with
    `HOPOS_PLACE_SYSTEM`. Hop has the C906L to itself
    (`HOP_ON_OS_CORE` = false on this board): `HOPOS_HOP_START slot=1
    core=1 cpu=1`.

    The lottery of the morning of 2026-10-03 was removed again the same day:
    the PLIC of the C906L has no ethernet source, the kernel belongs on the
    C906B.
12. **The app hart**: `cage: hart 1 (core 1) in the switcher: wake
    0x74004000, bell 0x0, kick 0x0, sleep cap 0 ticks, kill tick 250000 ticks,
    reset true HOPOS_RV_HART_UP`. The C906L comes out of reset at the reset
    entry and takes its first work from the mailbox. Every hart takes the
    T-Head regime (I-cache on: otherwise ~77x slower, measured 2026-08-18) and
    enters the switcher. It SPINS (no sleep): sleeping was never proven on
    the C906L, the C906B carried `wfi` for two weeks in Go but not with this
    switcher; there is no bell from the kernel to it (the CLINT is per core).
    Its comparator (`mtimecmp(0)`, per core) carries the 10 ms kill tick: it
    only fires while a resident runs and is the time slice of the apps that
    share a hart; on the C906L the reset block is the second knife. To see it
    on hardware: `tools/qemu-riscv-test-share.sh` on QEMU, and on the board a
    `BURN=1` next to welcome that keeps answering on :80.
13. **appspike, twice**: `HOPOS_RV_DISPATCH` with "picked up by the
    spinning switcher", `HOPOS_SLOT_START slot=1 core=1 cpu=1`, then the lines
    of the app. Expect `HOPOS_APPSPIKE_DONE pass=8 fail=1`: the FS test
    fails honestly (no disk), and so `exited with 1 HOPOS_SLOT_EXIT_FAIL`
    and after that still the stop and slot 2. What is the first real proof
    here:
    - the clock: the app measures itself with its own clock, so the test is
      the kernel's measurement line at the exit, `slot 1: +N ms app=3 ... beat=B`:
      the heartbeat beats every 50 ms of app time, so B should be about N/50
      (QEMU: +352 ms, beat=7). B at N/20 is an app that computes with 10 MHz
      on the 25 MHz of the SG2002: `CTRL_TIMEBASE_HZ` did not arrive;
    - the cage: a fault with `vec=0 esr=0x7` in the first lines is the
      TOR whitelist, which was never measured valid on this silicon
      (2026-07-30); `esr=0xc`/`0xd`/`0xf` is the Sv39 table (the T-Head bits,
      2026-07-31); `vec=64016` (`FAULT_CAGE_VERIFY` 0xFA11, minus 1 on the
      line) is a `pmpcfg0` that reads back differently from what the kernel
      wrote;
    - the caches: an app that hangs after READY or does not pass the NET
      test is a line that the kernel or the switcher did not sweep (the C906L
      is not coherent with the C906B).

## The cold flip on the LicheeRV

A new kernel over the network instead of via the SD card (docs/flip.md,
"riscv64"; cold only). On the Mac:

```sh
HOPOS_STAMP=<name> sh image/flip-bundle.sh licheerv    # Hop from tools/hop-build.sh, or STAGE=<hop.elf>
cd target && python3 -m http.server 8000
hop --agent <node>:8080 flip http://<mac>:8000/hopos-licheerv.flip $(cat target/hopos-licheerv.flip.sha256) --cold
```

Without `CFG=` the new kernel gets the config window of the running one
(`HOPOS_FLIP_CFG`). A kernel from before 2026-10-04 carried a window of its
own (`HOPOS.CFG.WINDOW`) and passes nothing to the new one: flip from there
with `CFG="image/cfg/default.cfg image/cfg/licheerv.cfg image/cfg/headless.cfg"`
(or your own). On the console (TCP 5555 or the UART), in order:
`HOP_FLIP_COLD_STOP`, `HOPOS_FLIP_COLD_ASKED`, `HOPOS_FLIP_CFG`,
`HOPOS_FLIP_STAGED ... staged at 0x86900000, cold`, `HOPOS_FLIP_COLD
stopped=N cores_off=1` (the C906L in reset), `HOPOS_FLIP_JUMP gen=G`, then the
bunny, `HOPOS_FLIP_BOOT gen=G HOPOS_FLIP_COLD_BOOT`, `HOPOS_BOOT gen=G
stamp=<name>` on the C906B, `HOPOS_RV_HART_UP` (the C906L out of reset),
`HOPOS_HOP_START`, `HOP_UP` and `HOPOS_FLIP_SETTLED`. The TCP console drops
with the jump and comes back after DHCP; what happens in between is only on
the UART (riscv64 has no black box yet). The new kernel found a handoff, so
it counts as a flip landing (`flip::landed`) and stays stateful.

What only the hardware proves: the trampoline on the C906 (`th.dcache.ciall`
and `th.icache.iall` before and after the copy), the C906L coming out of a
reset that held the old kernel, the DW watchdog that keeps running across the
jump (the old kernel pets it right before, the new one arms it at once as
boot guard), and a node whose pool is full: the bundle (10.7 MB) must first
fit in a partition, so remove jobs if Hop reports "out of memory". If the new
kernel fails before its console, only the watchdog or the power helps, and
the card starts the old kernel.

## Not done

- **Hop on the LicheeRV, on hardware**: since 3.0.1 the release bakes Hop
  (riscv64, `tools/hop-build.sh`) into the kernel as first resident
  (`STAGE=... ROLE=hop` of the image script, `HOPOS_EMBED_ROLE` in
  build.rs, `staged_role` of the board). The loop on QEMU virt is green, and
  since 2026-10-03 (R3) on the board too: Hop in slot 1 with
  `HOPOS_PRIVILEGE` (on this board in a 10 MiB partition, `HOP_MEM` in
  hopos/src/slots.rs; Hop measures 0.5 MB there), since R11 on the C906L to
  itself (core 1), welcome in slot 2, and after a cold flip Hop seeds welcome
  again from `hopos.init` (`HOP_INIT_SEEDED`).
- **The kick from an app to the kernel on the LicheeRV**: the shape exists
  since 2026-10-02 (`ecall` with a7 = 2; the switcher writes a 1 to
  `SCHED_OS_BELL`, the rotation of the OS core takes it as a yield to now;
  the app only kicks with `IDLE_KICK` on its control page). On QEMU virt the
  bell is `msip` of hart 0: appspike `dial_us` from a median of 7.5 and
  9.6 ms to 4.4 and 4.0 ms (ten runs per side, TCG). The C906L has no bell to
  the C906B (the CLINT is per core; `kick 0x0` on `HOPOS_RV_HART_UP`); the
  candidate is the mailbox of the CV181x (0x0190_0000, PLIC source 101 in
  the vendor DTS; Linux `drivers/mailbox/cv1800-mailbox.c`:
  `MBOX_EN_REG(cpu)`, then `MBOX_SET_REG` 0x60 with the channel bit, and the
  receiver clears with `MBOX_SET_CLR_REG`). Until that is proven, the kernel
  hears an app on the 300 µs poll of the NIC (`net::pump::NIC_POLL`).
- **The NIC interrupt on the LicheeRV does not exist on the C906L.** The TRM
  (sophgo-doc, SG200X, "Interrupt number and Interrupt source mapping for
  Slave RISCV C906 @ 700Mhz") has no ethernet source on the PLIC of the
  C906L (31 is UART1 there), and the vendor FreeRTOS for that hart sets
  `ETH0_SBD_INTR_O` to NA; that is why the kernel is on the C906B. There the
  dwmac hangs on PLIC source 31 (Linux `cv180x.dtsi`
  `SOC_PERIPHERAL_IRQ(15)` = 15 + 16, vendor DTS 31, TRM "Master RISCV
  C906" 31 Ethnet0), with the 10 ms guard rail; the kernel then hears an app
  on the 1 ms failsafe instead of the poll, because there is no bell.
  `HOPOS_NIC_IRQ` says at boot which of the two it became; NOT YET ON
  HARDWARE (the role split of item 11 is the first card that shows it).
- **SMP apps** on riscv64: one core per resident. Several residents on one
  hart does work (since 3.0.3 a cage does not count as a core): since
  2026-10-02 the switcher saves f0..f31 and `fcsr`, and on a shared hart the
  kill tick is the time slice (10 ms), the only preemption in HopOS
  (`tools/qemu-riscv-test-share.sh`).
- The warm kernel flip on riscv64: the switch code runs from the kernel image
  (on arm64 a copy sits in the plan region), so `cage_riscv::adopt` refuses
  and the flip is cold only. No black box for riscv64 either.
- The LicheeRV: sleeping in the switcher (the C906B carried `wfi` for two
  weeks in Go; a soak with this switcher, then a sleep bound in `app_hart`),
  and an SD driver (then `hopos.cfg` next to `fip.bin` instead of in the
  image).
