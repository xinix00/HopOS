# HopOS documentation

HopOS is the Go-only OS: a 13 MB signed image is the entire node, every app
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

## Related

- **HOP, the orchestrator** (jobs, CLI, cluster, S3 state) has its own docs:
  [gethop.org/hop/docs](https://gethop.org/hop/docs/)
- **Design notes** — the engineering dossiers behind all of this (bring-up
  logs, silicon errata, measurements; Dutch): [archief/](archief/)
