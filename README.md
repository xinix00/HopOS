# HopOS

**A bare-metal Go operating system for edge computing. No Linux — one static Go binary *is* the OS.**

HopOS turns a multi-core ARM64 board into a small fleet of single-purpose computers. Core 0 runs **HOP**, the orchestrator-kernel: it hands out cores, memory partitions and network identities, and dispatches. Every app then does its own work on its own hardware — it downloads its image over its own network stack and places itself inside its own hardware-enforced memory partition, *natively on its own dedicated CPU core*. There is no shell, no libc, no userland, no processes — killing an app means switching its core off.

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

- **Dedicated cores, one app each.** HOP builds the stage-2 cage, starts the core via PSCI and dispatches — milliseconds. Done or killed = core reset, slot free. Cores are never time-sliced or shared between apps.
- **Core 0 never does the apps' work.** The kernel core dispatches and supervises; it copies no images and terminates no TCP. An app downloads its own image through its own network stack and places it inside its own partition (self-placement — the cage makes that safe: anything the loader gets wrong stays confined to its own slot). Placement scales with the number of app cores — 127 parallel loaders on the Altra instead of one serialized kernel core — and HOP never even needs to read an app's image.
- **1 to N cores per app**: an app can be given multiple dedicated cores, with Go's own runtime spreading its goroutines across them over a shared heap. Sharing within one app is one trust domain — app-to-app isolation is unaffected. Proven in QEMU and on Raspberry Pi 4 and 5 hardware.
- **Isolation is hardware, not policy.** HopOS requires an EL2 boot: every slot runs inside a stage-2 MMU cage and can't even *address* HOP's memory or another slot's. This is an invariant, not an option — an EL1 boot is refused.
- **One artifact for every slot.** App images are linked once at a canonical address; the stage-2 mapping *is* the relocation. No per-slot builds, no relocation shims.
- **Apps never share memory with each other.** Cooperation happens through messages (per-slot ring buffers to HOP, network between apps) and through shared *files* — never shared mutable state across app boundaries.

## What an app gets

### CPU — 1 to N dedicated cores
An app runs on cores that belong to it exclusively: no context switches, no preemption by other apps, full clock speed. Multi-core apps keep the same model — each core is still exclusive to that one app; the Go runtime distributes goroutines across them. Placement is declarative by core class (`big` / `mid` / `small` on tri-cluster boards) — what requires a heuristic EAS scheduler on Linux is a manifest field here. Hang detection and hard-kill (via SGI) reset only the affected app's cores; every other slot keeps running.

> **Writing compute-heavy apps — yield cooperatively.** There is no OS underneath to steal your core back, and the bare-metal runtime has no signal-based async preemption. A tight loop that never blocks, allocates, or calls a function will therefore hog its core and **starve its own goroutines** — timers, heartbeats, telemetry — even though other apps are unaffected (the stage-2 cage confines the damage to your own core). This is the flip side of "full clock speed, no preemption": in a heavy `for {}` you must yield explicitly (`runtime.Gosched()`, a `time.Sleep`, or a channel/blocking op), where on a normal OS the scheduler would have bailed you out. An app that hangs itself still gets caught — HOP's heartbeat supervision restarts it — but that's the safety net, not the design.

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

## Writing an app — one build, every board

You develop against **HopOS, not a board**. An app never touches MMIO; everything it can see is either CPU architecture (registers, the generic timer) or the slot ABI (its control page, its message rings, its own network stack). So a single `GOOS=tamago` build produces **one artifact that runs on every HopOS node** — an Ampere Altra server core, a Raspberry Pi, QEMU — bit-for-bit the same file. The stage-2 cage is the relocation: images are linked once at the canonical slot address, and the MMU places them wherever the node has room. HOP patches the app's RAM size and slot number at load time.

A complete app:

```go
package main

import (
	"net/http"

	"hop-os/metal/app/applib"
	"hop-os/metal/app/applib/appnet"
)

func main() {
	app := applib.Init()     // READY + heartbeat + kill-flag + memory telemetry
	appnet.Up(app)           // the app's own TCP/IP stack, on its own NIC

	app.Logf("hello from slot %d", app.Slot)
	http.ListenAndServe(":"+app.Env("ER_PORT_WEB"), nil) // published by HOP (DNAT)
}
```

One build, no board tags:

```sh
GOOS=tamago GOARCH=arm64 ~/tamago-go/bin/go build -trimpath \
    -ldflags "-w -T 0x50010000 -R 0x1000" -o app.elf .
```

Ship `app.elf` anywhere (object storage, any HTTP server) and submit it — to any node, or a mixed fleet of them:

```sh
curl -X POST http://node:9080/v1/jobs -d '{
  "name": "web", "driver": "hop",
  "artifacts": [{"url": "https://cdn.example.com/app.elf"}],
  "memory_limit": 100663296, "cpu_shares": 1024}'
```

The node loads its embedded loader into a free slot; the app then **downloads its own image** on its own core, own network stack, into its own memory partition — the kernel never carries app payloads, and a broken download only ever costs that one slot. This is the container promise without the container matrix: no base images, no glibc/kernel versions, no per-board builds — one static Go binary, hardware isolation, every board. Write once, cage anywhere.

## What it deliberately doesn't have

No shell. No exec, no second binary, no users. No persistence. No VMs, WASM or containers. No heuristic schedulers or load-guessing DVFS governors — an idle core is parked in WFE or switched off, and clock policy follows a deterministic idle signal: sustained full idle clocks the node down, the first real work clocks it straight back up (~10 ms). No display driver. No Linux.

## Hardware

| Target | Status |
|---|---|
| QEMU `-M virt` | Full system: slots, isolation, hard-kill, NAT in/out, storage, fb console — marker-based regression suite |
| Ampere Altra (128-core) | **Runs the full machine: all 127 application cores working jobs simultaneously** (384 GiB slot pool). Boots bare-metal through the generic UEFI + ACPI path: PE/COFF bootloader (`BOOTAA64.EFI`), ACPI discovery (cores, ECAM, UART, PSCI), own igb/I210 network driver, SMCCC TRNG. QEMU + EDK2 exercises the identical path |
| Raspberry Pi 5 | **Runs the full multikernel on real silicon** — stage-2 isolation, hard-kill and multi-core apps (shared-heap SMP, cross-core GC) proven on the A76 cores. Native networking (own PCIe link training + GEM drivers, DHCP, NTP); runs the full HOP agent as a node on the LAN |
| Raspberry Pi 4 | **Runs the full multikernel on real silicon** — same acceptance suite as the Pi 5, proven on the A72 cores. Native networking (own GENET v5 driver, DHCP, NTP); runs the full HOP agent as a node on the LAN |
| Radxa Orion O6N (12-core CIX P1) | Primary production target: 1 HOP core + 11 app slots across big/mid/small clusters |

The Pi 5 boot requirements are non-obvious and documented in [`sd-rpi5/`](sd-rpi5/): the EEPROM bootloader validates images as Linux kernels unless `os_check=0`, silently ignores `kernel_address`, and always loads raw images at `0x80000`.

C1-stepping BCM2712 silicon has an interconnect erratum (fabric deadlock when sustained PCIe inbound DMA coincides with fabric-wide operations, fixed in D0) that HopOS works around in three layers — see [docs/bcm2712-c1-erratum.md](docs/bcm2712-c1-erratum.md).

## Repository layout

```
metal/       the OS — one Go module, layered by trust and direction:
  abi/         the HOP↔app contract: control-page ABI, memory layout,
               message rings, content checksums
  kern/        the orchestrator: slots, stage-2 isolation cage, file
               layer, embedded app loader
  cpu/         the ARM64 layer: EL2, PSCI, SMP bring-up, idle, TRNG
  net/         HOP's network plane: L2 frame switch + NAT, DHCP
  driver/      device drivers, one package per device
               (nic/: GEM, GENET v5, igb/I210, virtio-net, MDIO)
  fw/          hardware discovery: device tree (FDT) and ACPI parsing
  board/       per-board wiring: qemu-virt, rpi4, rpi5, generic UEFI
  app/         the app side: runtime library, reference app, loader
  cmd/         the binaries: hopos (the agent), hopos-embed, probeuefi
  dev/         the MMIO primitive everything builds on
  out/         build output (gitignored)
image/       build & run scripts (QEMU demo/agent, SD-card images, UEFI ESP)
sd-rpi4/     SD-card payload + flashing notes (Dutch)
sd-rpi5/     SD-card payload + flashing notes (Dutch)
```

The placement and import-direction rules (apps can never link against
HOP internals — the app side sees only `abi/`) are documented in
[docs/indeling.md](docs/indeling.md) (Dutch).

## Building & running

Everything cross-compiles with the [tamago-go](https://github.com/usbarmory/tamago-go) toolchain (`GOOS=tamago GOARCH=arm64`); no SDK, no cross-C-toolchain.

```sh
# Full system in QEMU (requires qemu-system-aarch64; always runs with EL2):
TAMAGO=~/tamago-go/bin/go image/qemu-run.sh          # demo / regression markers
TAMAGO=~/tamago-go/bin/go image/qemu-run.sh agent    # the real agent + leader API

# SD-card acceptance image for a Raspberry Pi 5:
TAMAGO=~/tamago-go/bin/go image/rpi5-hopos.sh
```

The QEMU demo and the Pi acceptance images build from public modules only. `metal/cmd/hopos` — the full agent — additionally depends on the [HOP orchestrator](https://github.com/xinix00/hop), which is open source as well.

## Status

Working today: the full multikernel (slots, stage-2 isolation, dynamic memory partitions, hard-kill), multi-core apps (1 to N dedicated cores per app on a shared heap), self-placing apps (download + placement on the app's own core; core 0 only dispatches), per-app networking with full NAT, NVMe storage with shared volumes, and framebuffer + UART consoles — proven in QEMU, on Raspberry Pi 4 and 5, and on a 128-core Ampere Altra running all 127 application cores simultaneously. On the Pi 5 the network path is fully self-hosted: HopOS trains the PCIe link itself (the firmware doesn't) and drives the RP1 GEM NIC with its own drivers, then DHCP and NTP. On the roadmap: Orion O6N bring-up, NVMe on real hardware, and line-rate throughput.

Built on [TamaGo](https://github.com/usbarmory/tamago) (bare-metal Go) and [gVisor's netstack](https://gvisor.dev) (pure-Go TCP/IP).

## License

[MIT](LICENSE)
