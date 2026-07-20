# Isolation

The cage is hardware, not policy.

- **Stage-2 translation per app.** The hypervisor level (EL2) gives every
  slot its own second-stage page table with its own VMID. The app can name
  exactly its own partition — the node, the neighbours and the devices are
  not unmapped, they are *unnameable*.
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

Honest limits: shared last-level cache and DRAM channels exist on any
hardware; the node itself is trusted (that's what the small TCB is for);
the Go runtime is the app's kernel — a bug there stays inside that app's
cage; and apps in a sharegroup share their pool's cores in time (opt-in),
so the timing side channels apply between cage-mates — the memory and
network cage still holds, and a runaway member starves only its own group.
