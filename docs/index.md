# HopOS documentation

HopOS is the Go-only OS: a ~6 MB signed image is the entire node, every app
runs on its own physical cores behind a hardware cage, and all state lives
in S3 — not on the machine. Two architectures, arm64 and riscv64, under one
app ABI. Product page: [gethop.org/hopos](https://gethop.org/hopos/).

## Quick start

1. **[Flash & boot](boot.md)** — the [imager](https://github.com/xinix00/hop-imager) writes a card, verifies it and finds your nodes, in one window without a terminal; or take the image files by hand (UEFI stick, Raspberry Pi or Radxa SD card, RISC-V SD image, QEMU). Signed images: [newest release](https://github.com/xinix00/HopOS/releases/latest).
2. **[Configure](config.md)** — the six lines that define a node; same keys on every board, editable before or after writing the card.
3. **[Write an app](app.md)** — compile a Go program for HopOS and run it as a job.

## Technical

- **[Architecture](technical/architecture.md)** — one orchestrator core, every app its own kernel.
- **[Isolation](technical/isolation.md)** — the hardware cage: stage-2, whole cores, zero syscalls.
- **[Networking](technical/networking.md)** — a network stack per app, a switch in the node.
- **[Stateless](technical/stateless.md)** — state on S3, not on metal.
- **[Geheugen](geheugen-ontwerp.md)** (NL, intern) — de keten van DRAM tot app-heap: wat HOP zelf houdt, hoe een partitie uitgedeeld en teruggegeven wordt, welke plafonds er zijn, en de vallen met datum.
- **GUI — SURF** — network-transparent windows: an app draws anywhere in the
  cluster, a display node composites it, and a window fails over when HOP
  restarts its app elsewhere. The node-side display grant ships in `metal/gui`;
  the windowing, compositor and browser-KVM stack is built as plain HopOS apps
  in the [hop-os-surf](https://github.com/xinix00/hop-os-surf) repo. Prebuilt
  SURF apps (display, browser, clock, …) are on the
  [releases page](https://github.com/xinix00/hop-os-surf/releases).

## Related

- **HOP, the orchestrator** (jobs, CLI, cluster, S3 state) has its own docs:
  [gethop.org/hop/docs](https://gethop.org/hop/docs/)
- **Design notes** — the engineering dossiers behind all of this (bring-up
  logs, silicon errata, measurements; Dutch): [archief/](archief/)
