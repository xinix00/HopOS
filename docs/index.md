# HopOS documentation

HopOS is the Go-only OS: a ~15 MB signed image is the entire node, every app
runs on its own physical cores behind a hardware cage, and all state lives
in S3 — not on the machine. Product page: [gethop.org/hopos](https://gethop.org/hopos/).

## Quick start

1. **[Flash & boot](boot.md)** — get a node running: UEFI stick, Raspberry Pi SD card, or QEMU in 5 minutes.
2. **[Configure](config.md)** — the six lines that define a node; same keys on every board.
3. **[Write an app](app.md)** — compile a Go program for HopOS and run it as a job.

## Technical

- **[Architecture](technical/architecture.md)** — one orchestrator core, every app its own kernel.
- **[Isolation](technical/isolation.md)** — the hardware cage: stage-2, whole cores, zero syscalls.
- **[Networking](technical/networking.md)** — a network stack per app, a switch in the node.
- **[Stateless](technical/stateless.md)** — state on S3, not on metal.
- **[GUI — SURF](gui-ontwerp.md)** — network-transparent windows: an app
  draws anywhere in the cluster, a display node composites it, and a window
  fails over when HOP restarts its app elsewhere. The node-side display grant
  ships in `metal/gui`; the windowing, compositor and browser-KVM stack is
  built as plain HopOS apps in the companion `hop-os-surf` repo (P1–P2 working
  in QEMU). Design dossier (Dutch): gui-ontwerp.md.

## Related

- **HOP, the orchestrator** (jobs, CLI, cluster, S3 state) has its own docs:
  [gethop.org/hop/docs](https://gethop.org/hop/docs/)
- **Design notes** — the engineering dossiers behind all of this (bring-up
  logs, silicon errata, measurements; Dutch): [archief/](archief/)
