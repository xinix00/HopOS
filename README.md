# HopOS

**A bare-metal operating system for edge computing. No Linux. One static binary *is* the OS.**

This is HopOS v3, written in Rust. An app whose source is Go, such as
cloudflared, runs as it is ([docs/go-apps.md](docs/go-apps.md)).

The idea: core 0 runs the kernel. Every app runs on its
own hardware, inside its own hardware-enforced memory partition, natively on
its own core or a core it explicitly shares. No shell, no libc, no userland,
no processes.

The kernel is mechanism only. HOP, the orchestrator, is the
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
netdev/     the NIC contract; blkdev/ the block device contract
cpu/        architecture: boot, vectors, EL2 with the switcher and the OS-core
            rotation, PSCI, IRQ, idle, TRNG, the kernel flip jump
fw/         firmware readers: FDT, ACPI, bootcfg
driver/     one crate per device: pl011, ns16550, gicv2, gicv3, pcie, virtio
            (mmio and pci), virtionet, virtioblk, nvme, vcmail, brcmpcie,
            nic/{mdio, igb, rtl8126, genet, gem, stmmac}, scmi, smpro, dvfs
net/        the switch, NAT, node networking
kern/       slots, cage, hopfs, the system API, the kernel flip, the console
board/      the Board contract; qemuvirt, uefi, o6n, altra, raspi, rpi4,
            rpi5, rk3566
applib/     the app runtime; apps link against it
appspike/   the ABI test app
apps/       the apps: welcome, the page on a published port
hopos/      the kernel binary; picks exactly one board
image/      the image builders per board; tools/ the gate and the QEMU tests
docs/       the per-board checklists
```

Build and test: `sh tools/gate.sh` (host tests, clippy, fmt, target builds),
then `sh tools/qemu-test.sh`, `sh tools/qemu-test-hop.sh`,
`sh tools/qemu-test-welcome.sh`, `sh tools/qemu-test-flip.sh`, `sh tools/qemu-uefi-test.sh` and
`sh tools/qemu-rpi4-test.sh`. Images: `image/uefi-run.sh` (`BOARD=uefi|o6n|altra`),
`image/rpi4.sh`, `image/rpi5.sh`, `image/radxa-zero3.sh`, `image/flip-bundle.sh`,
with the firmware in `image/firmware/` and the shared node configs in
`image/cfg/`. A release: `sh tools/release.sh <version>` (every board,
headless and headfull, the flip bundles and the apps; `jobs/README.md`).
