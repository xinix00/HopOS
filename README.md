# HopOS

**A bare-metal Go operating system for edge computing. No Linux — one static Go binary *is* the OS.**

HopOS turns a multi-core ARM64 board into a small fleet of single-purpose computers. Core 0 runs **HOP**, the orchestrator-kernel: it fetches Go apps over the network and runs each one *natively on its own dedicated CPU core*, inside its own hardware-enforced memory partition, with its own IP address and its own network stack. There is no shell, no libc, no userland, no processes — killing an app means switching its core off.

## Why

- **Supply chain security.** The machine runs Go and only Go. No package manager, no userland, no dynamic linker, no C of our own — the entire external dependency tree fits in one `go.sum`. Classic exploit chains (dropping a shell, executing a payload) have no foothold: there is no shell and no exec.
- **Simplicity that doesn't cost performance.** Apps are plain Go, cross-compiled to native ARM64 bare-metal images ([TamaGo](https://github.com/usbarmory/tamago)). No VMs, no WASM, no interpreters, no containers: 0% overhead, full clock speed.
- **Software in the shape of the machine.** A modern SoC is a set of independent computers that happen to share a package. HopOS treats it that way: apps are placed on cores explicitly and declaratively — no heuristic scheduler, no time-slicing, no shared mutable memory between apps, ever.

## The model

```
firmware ──boot──▶ one Go image (EL2)
                        │
   ┌────────────────────┴───────────────────────────────┐
   │ core 0: HOP — kernel + orchestrator                │
   │   L2 frame switch + NAT · NVMe + file layer        │
   │   mailbox rings · PSCI core lifecycle · fb console │
   └───────┬────────────────────────────────────────────┘
           │ PSCI CPU_ON/OFF · per-slot message rings
   ┌───────┴────────────────────────────────────────────┐
   │ cores 1..N: app slots — per app:                   │
   │   1..N dedicated cores                             │
   │   its own memory partition (stage-2 MMU cage)      │
   │   its own IP/MAC + TCP/IP stack (gVisor, pure Go)  │
   │   native Go code, full clock speed                 │
   └────────────────────────────────────────────────────┘
```

- **Dedicated cores, one app each.** HOP loads an image into a slot partition and starts the core via PSCI — milliseconds. Done or killed = core reset, slot free. Cores are never time-sliced or shared between apps.
- **1 to N cores per app**: an app can be given multiple dedicated cores, with Go's own runtime spreading its goroutines across them over a shared heap. Sharing within one app is one trust domain — app-to-app isolation is unaffected. Proven in QEMU and on Raspberry Pi 4 and 5 hardware.
- **Isolation is hardware, not policy.** HopOS requires an EL2 boot: every slot runs inside a stage-2 MMU cage and can't even *address* HOP's memory or another slot's. This is an invariant, not an option — an EL1 boot is refused.
- **One artifact for every slot.** App images are linked once at a canonical address; the stage-2 mapping *is* the relocation. No per-slot builds, no relocation shims.
- **Apps never share memory with each other.** Cooperation happens through messages (per-slot ring buffers to HOP, network between apps) and through shared *files* — never shared mutable state across app boundaries.

## What an app gets

### CPU — 1 to N dedicated cores
An app runs on cores that belong to it exclusively: no context switches, no preemption by other apps, full clock speed. Multi-core apps keep the same model — each core is still exclusive to that one app; the Go runtime distributes goroutines across them. Placement is declarative by core class (`big` / `mid` / `small` on tri-cluster boards) — what requires a heuristic EAS scheduler on Linux is a manifest field here. Hang detection and hard-kill (via SGI) reset only the affected app's cores; every other slot keeps running.

### Memory — split at the hardware level
A job asks for exactly the memory it needs — one gets 128 MB, another 640 MB — and HOP carves precisely that from a single pool (dynamic partitions, not fixed slabs). The boundary is enforced by per-core stage-2 page tables, so a compromised or crashed app is physically confined to its own partition. Total RAM is discovered from the device tree at boot, like everything else here: universal mechanisms over board-specific ones.

### Network — a real stack per app, full NAT on the node
Every slot has its own MAC and IP and runs its own TCP/IP stack over frame rings. HOP itself is deliberately minimal: an L2 frame switch plus full NAT —

- **Port publishing (DNAT):** stateless `node-IP:port → slot-IP:port` header rewriting with incremental checksums (RFC 1624).
- **Outbound (masquerade/PAT):** TCP and UDP with lightweight connection tracking, so apps can dial out — DNS, HTTPS, QUIC — plus a passively learning neighbor cache for the L2 next hop.

Core 0 never terminates TCP on behalf of the apps: it rewrites headers and forwards frames. Apps compute; HOP moves data.

### Storage — NVMe as scratch space and sharing plane
This is an edge system: durable state lives in object storage, not on the node. The NVMe drive is raw block — no ext4, no VFS, no fsck — managed by HOP alone with a minimal file layer, and it serves two purposes: **temp/scratch storage** and **sharing between apps**. Each app starts with an empty private root and mounts shared volumes explicitly; the mount table is the access boundary on disk, exactly like the stage-2 cage is in RAM. The primary sharing pattern: one app writes a file (e.g. a SQLite database), N instances mmap it read-only — each gets a private copy in its own partition, queries run at memory speed, zero locks. Reboot = clean slate, by design.

### Framebuffer — logs, not graphics
There is no GPU driver and no mode-setting. HopOS writes its log console into the linear framebuffer the firmware already switched on, discovered through the same two universal mechanisms Linux's `simplefb`/`efifb` use (device-tree simple-framebuffer, or UEFI GOP). Boot and app logs appear on HDMI, so you can see what a node is doing without a UART cable.

## What it deliberately doesn't have

No shell. No exec, no second binary, no users. No persistence. No VMs, WASM or containers. No heuristic schedulers, no DVFS governors (an idle core is simply switched off or parked in WFE). No display driver. No Linux.

## Hardware

| Target | Status |
|---|---|
| QEMU `-M virt` | Full system: slots, isolation, hard-kill, NAT in/out, storage, fb console — marker-based regression suite |
| Raspberry Pi 5 | **Runs the full multikernel on real silicon**: stage-2 isolation, hard-kill and multi-core apps (shared-heap SMP, cross-core GC) proven on the A76 cores |
| Raspberry Pi 4 | **Runs the full multikernel on real silicon**: same P1 acceptance as the Pi 5, proven on the A72 cores |
| Radxa Orion O6N (12-core CIX P1) | Primary production target: 1 HOP core + 11 app slots across big/mid/small clusters |

The Pi 5 boot requirements are non-obvious and documented in [`sd-rpi5/`](sd-rpi5/): the EEPROM bootloader validates images as Linux kernels unless `os_check=0`, silently ignores `kernel_address`, and always loads raw images at `0x80000`.

## Repository layout

```
metal/    the OS: boot + boards (rpi4, rpi5, qemu-virt), stage-2 isolation,
          message rings, L2 switch + NAT, NVMe + file layer, PSCI core
          lifecycle, device-tree parsing, framebuffer console
image/    build & run scripts (QEMU demo/agent, SD-card probe images)
sd-rpi4/  SD-card payload + flashing notes (Dutch)
sd-rpi5/  SD-card payload + flashing notes (Dutch)
```

## Building & running

Everything cross-compiles with the [tamago-go](https://github.com/usbarmory/tamago-go) toolchain (`GOOS=tamago GOARCH=arm64`); no SDK, no cross-C-toolchain.

```sh
# Full system in QEMU (requires qemu-system-aarch64; always runs with EL2):
TAMAGO=~/tamago-go/bin/go image/qemu-run.sh          # demo / regression markers
TAMAGO=~/tamago-go/bin/go image/qemu-run.sh agent    # the real agent + leader API

# SD-card probe image for a Raspberry Pi 5:
TAMAGO=~/tamago-go/bin/go image/rpi5-probe.sh
```

The probes and the QEMU demo build from public modules only. `metal/cmd/hopos` — the full agent — additionally depends on the HOP orchestrator, which lives in a separate module that is not public yet.

## Status

Working today: the full multikernel (slots, stage-2 isolation, dynamic memory partitions, hard-kill), multi-core apps (1 to N dedicated cores per app on a shared heap), per-app networking with full NAT, NVMe storage with shared volumes, and framebuffer + UART consoles — proven in QEMU and on Raspberry Pi 4 and 5 hardware. On the roadmap: Orion O6N bring-up, native NIC and NVMe drivers at line rate, and ed25519 signing of app images.

Built on [TamaGo](https://github.com/usbarmory/tamago) (bare-metal Go) and [gVisor's netstack](https://gvisor.dev) (pure-Go TCP/IP).

## License

[MIT](LICENSE)
