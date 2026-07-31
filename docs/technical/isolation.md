# Isolation

The cage is hardware, not policy.

- **A hardware cage per app.** The level HOP owns — EL2 on ARM, machine mode
  on RISC-V — gives every slot a map of its own: a second-stage page table
  with its own VMID, or a locked whitelist plus a supervisor page table. Either
  way the app can name exactly its own partition, and the node, the neighbours
  and the devices are not unmapped but *unnameable*. See *One cage, two jobs*
  below for how one design covers both.
- **Whole cores, by default.** By default an app never shares a core with
  anyone — not even in time. No context switches, no SMT siblings: the
  classic cross-domain side channels (Meltdown/Spectre/MDS-style) lose their
  vector instead of being mitigated, and there is no mitigations tax — apps
  run at the silicon's spec clock. **Share when trusted:** a *sharegroup*
  packs apps you name onto a shared pool of whole cores, cooperatively (they
  yield on idle — no timer, no preemption). This never happens to you
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
  EL2 vectors, which park it. A cage violation prints the fault (ESR/FAR)
  on the console while every other slot keeps serving.

```
$ hop apply --name escape-probe    # deliberately reads outside its cage
slot 7: stage-2 fault — ESR 0x93c08007 FAR 0x9000f000 · core parked
slots 1-6, 8-126: unaffected, still serving
```

- **Small enough to audit.** The code that enforces all of this — cages,
  slots, ABI — is ~2,100 lines; the whole OS is ~11,900 (lines of code,
  excluding tests, comments and the optional GUI). A Linux node doing the
  same job trusts GRUB, the kernel (~30,000,000 lines), systemd, libc *and*
  a container runtime — HopOS is the whole node, bootloader included, in
  ~11,900. It fits in a single AI context window: audit it in one sitting,
  human or machine.

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

Two rows deserve their reasoning spelled out, because the obvious choice is the
wrong one. The whitelist is **not locked**: locking would add machine mode to
what PMP binds, which once held a machine-mode app in but now would only confine
HOP — and a locked deny-all shuts HOP's own switcher out of every address outside
the partitions, so the code that swaps two residents could then only live *inside*
one of them. And supervisor mode is **required**, not preferred: with the
whitelist unlocked, an app in machine mode would sit in a cage that does not bind
it, so a hart without `misa.S` parks instead of starting anything.

### The same invariant on RISC-V

The mechanism has to differ, because the XuanTie C906 has no hypervisor
extension: stage-2 does not exist on that silicon. So there the bound is a
**PMP whitelist** (`metal/kern/cage`): the loader stub grants the app exactly its
own partition plus whatever MMIO it was given and closes the list with a
deny-all. PMP always binds supervisor and user mode, so that list is the cage.

What carries the invariant is the second rule: the stub **reads the
configuration back and refuses to dispatch on a mismatch** — the counterpart of
refusing to boot below EL2. A cage that cannot be shown to stand does not get an
app. Measured on a LicheeRV Nano: the forbidden store faults with `mcause 7`, and
a hart reset hands back a provably clean slot, which is why a kill is a reset.

The entries are deliberately **not locked**. Locking adds machine mode to what
PMP binds, and while the app itself ran in machine mode that was the only thing
holding it in. Once the app moved to supervisor mode the whitelist does that job
by itself — and locking would then confine only HOP. That is not a detail: a
locked deny-all shuts HOP's own switcher out of memory outside the partitions, so
the code that swaps two residents on one hart could not live anywhere except
*inside* a partition — where app A can rewrite it and take over app B. Unlocked,
the cage is not weaker but stronger: the invariant sits in the privilege boundary
(an app in supervisor mode cannot touch PMP at all) and HOP keeps the freedom it
needs to change the cage.

### Why the app left machine mode

A locked whitelist confines an app in machine mode, which for a while let the
Go runtime stay untouched. That was a real trade, and it had a cost that only
shows up when you want two apps on one core: **above a machine-mode app there is
nothing**. HOP cannot preempt that hart, cannot swap its cage — locked entries
only clear on reset — and cannot offer it a wake-up, so an idle core can only
spin. One app per hart, forever.

So the app now runs in supervisor mode: `mret` into it, whitelist unchanged,
`medeleg` left at zero so *every* trap it takes lands in a machine-mode vector
that HOP owns. That is the EL2/EL1 split, spelled in RISC-V. The whitelist still
carries the invariant — a supervisor-mode app may rewrite its own `satp`, but the
hardware that walks those tables is itself subject to the whitelist, so
redrawing its address space reaches nothing outside the cage and harms only
itself.

The move is one file (`metal/cpu/slotstart`): the entry sequence touches only
supervisor registers, which machine mode may write too, so the *same* image runs
on a hart with or without supervisor mode. One artifact per architecture, and
HOP needs to know nothing about the mode when it places an app.

Measured on a LicheeRV Nano (2026-07-31): the app runs in supervisor mode and
serves; `misa` reports `S` and `U` present; a trap in a supervisor-mode app is
caught by HOP's vector and reported with `mcause`/`mepc`/`mtval` over the
network rather than to a serial line nobody is watching.

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
