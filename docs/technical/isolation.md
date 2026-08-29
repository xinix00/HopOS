# Isolation

The cage is hardware, not policy.

- **A hardware cage per app.** The level HOP owns — EL2 on ARM, machine mode
  on RISC-V — gives every slot a map of its own: a second-stage page table
  with its own VMID, or a PMP whitelist plus a supervisor page table. Either
  way the app can name exactly its own partition, and the node, the neighbours
  and the devices are not unmapped but *unnameable*. See *One cage, two jobs*
  below for how one design covers both.
- **Whole cores, by default.** By default an app never shares a core with
  anyone — not even in time. No context switches, no SMT siblings: the
  classic cross-domain side channels (Meltdown/Spectre/MDS-style) lose their
  vector instead of being mitigated, and there is no mitigations tax — apps
  run at the silicon's spec clock. **Share when trusted:** a *sharegroup*
  packs apps you name onto a shared pool of whole cores, cooperatively (they
  yield on idle — no preemption; a timer exists only to *wake* a core that
  every cage-mate put to sleep, never to take one away from a running app).
  This never happens to you
  involuntarily — an attacker can't land on your core, because the only apps
  sharing it are ones you grouped together. Inside such a group the timing
  side channels exist between cage-mates, as on any shared core, but they
  already trust each other; the memory cage below never softens, and ungrouped
  apps keep the full guarantee. Cores are the physical headroom; sharegroups
  let you run more apps than cores when the isolation trade is yours to make.
- **Zero syscalls, no app-initiated MMIO.** The entire ABI is a control page
  and two rings. By default a cage touches no device registers at all —
  devices are programmed only by the node. One specific device window can be
  handed to a single cage — the framebuffer, for a display app — as an
  explicit, node-granted DeviceGrant (off unless you wire it): still
  node-granted, never app-initiated, and an app can never aim DMA. Firmware
  calls (SMC) from a cage trap at EL2 — there is no legitimate app SMC.
- **Kill is revocation.** Stopping a stubborn app doesn't ask it nicely: the
  node revokes its stage-2 map and the core faults synchronously into the
  EL2 vectors, which park it (on RISC-V the node resets the hart, which hands
  back a provably clean slot). A cage violation prints the fault (ESR/FAR, or
  `mcause`/`mepc`/`mtval`) on the console while every other slot keeps serving.

```
$ run apply --name escape-probe    # deliberately reads outside its cage
slot 7: stage-2 fault — ESR 0x93c08007 FAR 0x9000f000 · core parked
slots 1-6, 8-126: unaffected, still serving
```

- **Small enough to audit.** The code that enforces all of this — cages, slots,
  ABI — is ~4,025 lines, and the whole node is what the table below adds up to.
  The rung you have to *trust* is that first one; the board layer — its NIC
  and PHY drivers included — is a swappable outer shell, already outside
  every cage.

### Small enough to actually read

Lines of code, excluding tests, comments and blanks — counted **the way the
compiler sees it** by [`tools/loc.go`](../../tools/loc.go), which runs
`go list -deps` per release image. A file that only links for one instruction
set or one board counts only there; example apps, probes and dev targets
don't ride along. If it isn't in the image you boot, it isn't in the number.
The numbers are the **headless** image; the gui flavour is the opt-in it is,
listed last.

| layer | lines |
|---|---|
| **isolation core** — cages, slots, ABI, object store | ~4,025 |
| app runtime + node mains | ~525 |
| network stack — switch, NAT, DHCP, IPv6 | ~1,475 |
| portable drivers — console, NVMe, PCIe | ~1,475 |
| boot config + the board contract | ~150 |
| **portable Go — in every node** | **~7,650** |
| `arm64` — EL2 + stage-2, PSCI, SMP | ~1,325 |
| `riscv64` — machine mode + PMP cage, slot stub | ~1,350 |
| board: Ampere Altra / any UEFI box — ACPI, MMU, igb, SMpro | ~2,325 |
| board: Raspberry Pi 5 — RP1, gem, PCIe, mailbox | ~2,325 |
| board: Raspberry Pi 4 — genet, mailbox | ~1,900 |
| board: Radxa Zero 3E — dwmac4 + PHY, TSADC, TRNG | ~2,400 |
| board: LicheeRV Nano — dwmac, ePHY, CLINT | ~1,850 |
| board: Mac mini M4 — iBoot handoff, spin-table, tg3, dual console | ~2,100 |
| **lean** — the node's own standard library, a separate module, identical in every node: TCP/IP, TLS, HTTP, DHCP, S3, ELF | ~10,700 |
| gui, opt-in (`-tags gui`) — framebuffer grant + USB input (xHCI, DWC3, HID) | ~2,000 |
| gui, board wiring on top — Radxa scanout ~950 · Pi 4 ~400 · Pi 5 ~15 | |

The app-runtime rung is small because the *loader* that used to live there is
gone: the node streams a job's image straight into its partition, so there is
no second Go runtime to link (see [architecture](architecture.md)).

A node is **portable + one ISA + one board, plus lean**, never two of
either: an Altra stick is ~11,300 lines of metal, a Pi 5 ~11,300, a Pi 4
~10,875, a Radxa ~11,375, the LicheeRV ~10,850 and the Mac mini M4 ~11,075. All dependencies together
are the same ~10,700 of lean everywhere — shown, not hidden. What is *not*
ours in the image is the tamago runtime and a CA-roots bundle: the TCP/IP
stack, TLS, HTTP, DHCP, the S3 client and the ELF loader are lean, written
here — no gVisor, no forks. (What lean *offers* beyond that — HTTP/2 for
cloudflared-lean, opt-in IPv6 — only counts where a program actually links
it.) You audit one tree, never the union. **A headless image
links zero graphics — and zero USB.** The gui flavour adds the grant that
hands one app the framebuffer and the input chain (xHCI, the DWC3 core in
host mode, the HID boot protocol) — owned by HOP, not granted to
apps, because a bus-master doing DMA cannot be caged without an IOMMU; apps
receive input *events*, never registers. On the Radxa the gui flavour also
carries its own scanout, because that board's firmware doesn't light the
connector. Windowing, compositing and the browser are ordinary caged apps in
[their own repo](https://github.com/xinix00/hop-os-surf). The board rung is
where new hardware lands: the Radxa port put its GMAC glue in a ~2,400 board
rung and its scanout behind the gui tag, while the portable core moved ~50
lines.

A Linux node doing the same job trusts GRUB, the kernel (~30,000,000 lines),
systemd, libc *and* a container runtime. **HopOS is the whole node —
bootloader included — in ~11,300 lines**, and even its dependencies are
readable. **The machine you actually booted fits in
a single AI context window**, so you can audit it in one sitting, human or
machine.

### One cage, two jobs

A cage does two things, and separating them is what makes the same design fit
two very different instruction sets:

| | **bound** — what the app may reach | **relocate** — where the app thinks it is |
|---|---|---|
| ARM | stage-2 table | the same stage-2 table |
| RISC-V (C906) | PMP whitelist | a supervisor page table |

**Bounding** is the invariant. **Relocating** is a convenience with one purpose:
every slot sees itself at the same address, so a single artifact per
architecture runs in any slot. On ARM the two coincide — one table does both,
which is why they were never named apart. On RISC-V they are two mechanisms, and
pulling them apart is what made the RISC-V port a translation of the ARM design
rather than a second design.

Same idea, different letters, all the way down. **This table is authoritative.**
Anything that changes privilege level, the bounding mechanism, or translation
changes this table first; the package and file comments follow it. That rule is
not ceremony — while this port was being built, several comments described the
*previous* model (app in machine mode, locked entries, no translation) for hours
after the code did the opposite, and a comment that misstates the isolation
invariant is worse than no comment at all.

| | ARM | RISC-V |
|---|---|---|
| the level HOP owns | EL2 | machine mode |
| the level an app runs in | EL1 | **supervisor mode** — a hart without `misa.S` is refused |
| what bounds the app | stage-2 table | PMP whitelist, **not locked** |
| what relocates the app | the same stage-2 table | a separate page table under `satp` (Sv39) |
| who programs the cage | HOP, before the ERET | the per-partition cage stub, which reads it back before jumping |
| how a violation is caught | stage-2 fault → EL2 vectors | trap → machine-mode vector (`medeleg` = 0) |
| how a resident yields | `HVC` #1 | `ecall`, `a7` = 0 |
| how a resident exits | `HVC` #0 | `ecall`, `a7` = 1 |
| how HOP kills a slot | revoke stage-2 + TLBI | hart reset |
| what an app is linked at | the canonical slot-1 IPA | the same canonical link address |

### The same invariant on RISC-V

The mechanism has to differ, because the XuanTie C906 has no hypervisor
extension: stage-2 does not exist on that silicon. So there the bound is a
**PMP whitelist** (`metal/kern/cage`): the cage stub grants the app exactly its
own partition plus whatever MMIO it was given and closes the list with a
deny-all. PMP always binds supervisor and user mode, so that list is the cage.

What carries the invariant is the second rule: the stub **reads the
configuration back and refuses to dispatch on a mismatch** — the counterpart of
refusing to boot below EL2. A cage that cannot be shown to stand does not get an
app. Measured on a LicheeRV Nano: the forbidden store faults with `mcause 7`, and
a hart reset hands back a provably clean slot, which is why a kill is a reset.

**The app runs in supervisor mode**, and both of the surprising choices around
that follow from wanting two apps on one hart.

The entries are deliberately **not locked**. Locking adds machine mode to what
PMP binds — the only thing that held an app in back when the app itself ran in
machine mode. Now the privilege boundary does that job (a supervisor-mode app
cannot touch PMP at all), so locking would confine nobody but HOP: a locked
deny-all shuts HOP's own switcher out of every address outside the partitions,
so the code that swaps two residents on one hart could only live *inside* a
partition — where app A can rewrite it and take over app B. Unlocked, the cage
is not weaker but stronger.

And supervisor mode is **required**, not preferred: above a machine-mode app
there is nothing. HOP cannot preempt that hart, cannot swap its cage (locked
entries only clear on reset) and cannot offer it a wake-up — one app per hart,
forever. So `mret` puts the app in supervisor mode with `medeleg` at zero, so
*every* trap it takes lands in a machine-mode vector HOP owns; that is the
EL2/EL1 split, spelled in RISC-V. A hart without `misa.S` would leave the app
in machine mode inside a cage that does not bind it, so the stub parks instead
of starting anything. The app may rewrite its own `satp`, but the hardware
walking those tables is itself subject to the whitelist, so redrawing its
address space reaches nothing outside the cage.

The move was one file (`metal/cpu/slotstart`): the entry sequence touches only
supervisor registers, which machine mode may write too, so the *same* image runs
on a hart with or without supervisor mode. Measured on a LicheeRV Nano
(2026-07-31): the app runs in supervisor mode and serves, `misa` reports `S` and
`U`, and a trap inside it is caught by HOP's vector and reported with
`mcause`/`mepc`/`mtval` over the network rather than to a serial line nobody is
watching.

### Two residents on one hart

With a level above the app, sharing a hart becomes the same contract ARM already
proved, translated register for register (`metal/cpu/mmode/switch.s` next to
`metal/cpu/el2/switch.s`). The app yields with `ecall` where ARM uses `HVC`; the
handler saves what the next resident must not inherit, walks the resident list
round-robin, and either resumes a saved resident or cold-boots a pending one with
`mret` instead of `ERET`. Nothing here is new design — the states, the list and
the rotation are the ARM ones.

Three things genuinely differ, and all three are properties of the architecture:

- **No spare register on entry.** ARM already has `SP_EL2`; here `csrrw sp,
  mscratch, sp` is the only way to reach a usable pointer. So the cage stub hands
  the hart its scheduling block in `mscratch` before letting the app in — HOP-owned
  memory that sits in no cage, so the app can neither read nor redirect it.
- **`mepc + 4`.** After an `ecall`, `mepc` points at the instruction itself. Without
  the increment a resident yields again on every resume. `ELR_EL2` is already past
  the `HVC`, so ARM never had this.
- **Cache maintenance, both ways.** The switcher runs in machine mode, which has no
  MMU, so its writes are cacheable — and HOP's hart is not coherent with the app
  hart on this silicon. Every word HOP writes must be invalidated before reading and
  every word HOP reads flushed after writing. That also fixes the layout: the
  scheduling block is split **by writer along cache lines**, because two writers in
  one 64-byte line lose data — whoever writes the line back also writes back the
  other's bytes as they stood at *its* fetch. Same lesson as the ring heads in
  ABI 3, and it bites harder here because HOP mutates the resident list while the
  switcher rotates. On ARM this block is device-mapped and all of it is free.

Where the trap lands is what makes the second app possible. Before, a fault went
to a handler inside the app's own partition, which could only park — it could not
hand the hart to a neighbour, and it sat in memory the app itself could rewrite.
Now every trap lands in HOP's image, outside every partition. That address only
became usable when the whitelist stopped being locked.

Measured on hardware (LicheeRV Nano, 2026-07-31): **two apps share one hart.** A
web server in a 64 MB cage at 0x80000000 and a Cloudflare tunnel in a 124 MB cage
at 0x88200000 — both relocated onto the same link address, both TOR-encoded, both
`running` with zero restarts. The server answers in 14 ms while the tunnel's QUIC
and HTTP/2 prechecks pass, and each reports ~37 % of the hart: cooperative sharing,
measured rather than argued.

Two lessons from getting there are worth more than the result. The first is that a
translation bug and a cache bug look alike from a distance, and only one of them is
fixed by a cache operation: marking the per-slot page tables *global* let the
hardware keep a previous resident's translation across a `satp` switch, which the
specification calls out explicitly as a software error. The second is that the
crash which looked exactly like that — a resident resuming onto a wild PC — was
neither: a hand-written yield stub declared a stack frame, and the assembler's
prologue put the return address at `0(sp)` where the stub then spilled a
floating-point register over it. The faulting address was the bit pattern of a
`float64`. Both bugs were only reachable with two residents, because a lone app
never yields.

Honest limits: shared last-level cache and DRAM channels exist on any
hardware; the node itself is trusted (that's what the small TCB is for);
the Go runtime is the app's kernel — a bug there stays inside that app's
cage; and apps in a sharegroup share their pool's cores in time (opt-in),
so the timing side channels apply between cage-mates — the memory and
network cage still holds, and a runaway member starves only its own group.
