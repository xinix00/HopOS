# HopOS

**A bare-metal operating system for edge computing. No Linux. One static binary *is* the OS.**

This is HopOS v3, written in Rust. The Go generation (v2.x, built on TamaGo)
lives in [OLD/](OLD/) with its own README, docs, drivers, images and build
scripts; its releases stay tagged and downloadable. The Go tree is the
specification the Rust tree is written from: every register sequence, every
measurement and every hard-won comment moves over with the code.

What does not change: the idea. Core 0 runs the kernel. Every app runs on its
own hardware, inside its own hardware-enforced memory partition, natively on
its own core or a core it explicitly shares. No shell, no libc, no userland,
no processes.

What changes: the kernel is mechanism only. HOP, the orchestrator, is the
first resident: an app with one privilege no other app has, the right to
place, stop and replace apps. The kernel owns the cage, the switch, the
storage and the clock; HOP owns the policy.

How the code is written: the haas.software Rust handbook (`rustdoc/README.md`
next to this repository) and its port notes (`rustdoc/PORT.md`).

## Layout

```
dev/        MMIO primitive and barriers, host stubs; imports nothing
bounded/    bounded collections, fallible
sync/       Signal, Stop, channels, Local: the concurrency primitives
executor/   one executor per core: tasks, timers, the idle governor
abi/        the kernel/app contract: layout, rings, control page, system API
cpu/        architecture: boot, vectors, EL2 and stage-2, PSCI, IRQ
fw/         firmware readers: FDT, ACPI, ADT
driver/     one crate per device
net/        the switch, NAT, node networking
kern/       slots, cage, hopfs, kernel flip, console
board/      one crate per board
applib/     the app runtime; apps link against it
appspike/   the ABI test app
hopos/      the kernel binary; picks exactly one board
```
