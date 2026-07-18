# Isolation

The cage is hardware, not policy.

- **Stage-2 translation per app.** The hypervisor level (EL2) gives every
  slot its own second-stage page table with its own VMID. The app can name
  exactly its own partition — the node, the neighbours and the devices are
  not unmapped, they are *unnameable*.
- **Whole cores.** An app never shares a core with anyone — not even in
  time. No context switches, no SMT siblings: the classic cross-domain
  side channels (Meltdown/Spectre/MDS-style) lose their vector instead of
  being mitigated. And there is no mitigations tax — apps run at the
  silicon's spec clock.
- **Zero syscalls, zero MMIO.** The entire ABI is a control page and two
  rings. Devices are programmed only by the node; an app cannot aim DMA.
  Firmware calls (SMC) from a cage trap at EL2 — there is no legitimate
  app SMC.
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
  slots, ABI — is ~2,850 lines; the whole OS is ~11,600. A Linux node doing
  the same job trusts the kernel (~30,000,000 lines) *plus* systemd, libc
  and a container runtime — HopOS is the whole node in ~11,600. It fits in
  a single AI context window: audit it in one sitting, human or machine.

Honest limits: shared last-level cache and DRAM channels exist on any
hardware; the node itself is trusted (that's what the small TCB is for);
the Go runtime is the app's kernel — a bug there stays inside that app's
cage.
