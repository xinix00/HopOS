# Architecture

HopOS is a **multikernel**: the machine is divided by core, not by time.

```mermaid
flowchart LR
  subgraph n0["node — core 0 (+ hopos.cores-1)"]
    H["HOP: agent, leader & API<br/>drivers · net switch"]
  end
  subgraph s1["app — own core(s)"]
    A["own Go runtime<br/>own RAM · own net stack"]
  end
  subgraph s2["app — own core(s)"]
    B["own Go runtime<br/>own RAM · own net stack"]
  end
  H <-- "control page + rings" --> A
  H <-- "control page + rings" --> B
  E(["the node owns the cage: every app runs inside one"]) ~~~ s1
```

- **The node runtime is just an app too.** HOP runs on core 0 (plus
  `hopos.cores-1` extra cores if configured); Go's scheduler spreads it —
  nothing is pinned.
- **Every app is its own kernel.** Each job gets a full Go runtime on its
  own physical cores, one privilege level below the node, in its own memory
  partition behind a hardware cage (a stage-2 table on ARM; a PMP whitelist
  plus its own Sv39 table on RISC-V — see
  [isolation](isolation.md)). There is no shared kernel to call into: the
  app↔node ABI is a control page (status, heartbeat, kill, telemetry) and
  two message rings.
- **Two-phase loading.** The node starts a tiny baked-in *apploader* on the
  target core; it downloads the real image **on its own core and its own
  network stack**, straight into its own partition, then the app places
  itself and boots. A storm of 127 job starts never funnels through core 0.
- **No interrupts.** Everything polls; idle ARM cores sleep on the event
  stream (~1 ms granularity) and are woken by work, and a spare RISC-V hart
  pauses on a counter instead (no `wfi` — on the C906 that is not a
  guaranteed-to-return hint, and one wrong guess is a hart that never looks
  up again). Fewer moving parts, no IRQ routing, deterministic behaviour.
- **Discovery, not configuration.** UEFI boards read ACPI (MADT, MCFG,
  SPCR, GTDT); Pis read the device tree; the LicheeRV's map is board
  constants, because that SoC hands us no description of itself. The same job
  runs on any board — core count is your headroom.

Depth (Dutch design notes): [uefi](../archief/uefi.md),
[rpi5](../archief/rpi5.md), [memory layout](../archief/app-memory.md),
[layering rules](../archief/indeling.md).
