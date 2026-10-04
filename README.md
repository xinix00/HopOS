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
heap/       the allocator of kernel and apps: free lists, a ceiling, counters
sync/       Signal, Stop, channels, Local, select, Pool: the concurrency primitives
executor/   one executor per core: tasks, timers, the idle governor
abi/        the kernel/app contract: layout, rings, control page, system API
netdev/     the NIC contract; blkdev/ the block device contract
cpu/        architecture: boot, vectors, EL2 with the switcher and the OS-core
            rotation, PSCI, IRQ, idle, TRNG, the kernel flip jump; cpu/src/riscv
            the same for riscv64 in machine mode (PMP plus Sv39 cage)
fw/         firmware readers: FDT, ACPI, bootcfg
driver/     one crate per device: pl011, ns16550, gicv2, gicv3, aic, pcie,
            brcmpcie, virtio (mmio and pci), virtionet, virtioblk, nvme, vcmail,
            nic/{mdio, igb, rtl8126, genet, gem, stmmac, tg3}, scmi, smpro,
            dvfs, smc, rtkit, rng200, rkrng, fb, usb/{xhci, dwc3, hid}, codec
net/        the switch, NAT, node networking
kern/       slots, cage, hopfs, the system API, the kernel flip, the console
board/      the Board contract and the config window; qemuvirt, uefi, o6n,
            altra, raspi, rpi4, rpi5, rk3566, apple (Mac mini M4),
            qemuvirt-riscv, licheerv (LicheeRV Nano)
gui/        the framebuffer grant, the USB input service, the RK3566 scanout
media/      the O6N video codec (mve) and the optical drive
applib/     the app runtime; apps link against it
appspike/   the ABI test app
apps/       welcome, bench, vitals, display, decode, syncprobe, cloudflared-lean
go/         apps whose source is Go (cloudflared), built with TamaGo
jobs/       job specs for the apps on the nodes
hopos/      the kernel binary; picks exactly one board
image/      the image builders per board; tools/ the gate, the QEMU tests, the
            release, mkcard, netmeter and loc
docs/       the per-board checklists and the planes; start at docs/README.md
```

Build and test: `sh tools/gate.sh` (host tests, clippy, fmt, target builds of
every board), then the QEMU tests. The one list of those, and the one table
of image scripts per board, is [docs/README.md](docs/README.md); how to build
and flash a board is in its own doc there. A release: `sh tools/release.sh
<version>` (every board, headless and headfull, the flip bundles and the apps;
[jobs/README.md](jobs/README.md)).
