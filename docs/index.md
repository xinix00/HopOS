# HopOS documentation

HopOS runs native Go applications on ARM64 and RISC-V using TamaGo. The node assigns memory and cores, starts and stops applications, handles shared I/O, and maintains the job state needed to manage them.

Each application has its own memory partition and runtime. Applications can use dedicated cores or explicitly share a core pool with trusted applications. Sharing cores does not merge their memory partitions. SMP assigns multiple dedicated cores to one application.

Application system calls use the existing internal network path. The node identifies the caller and dispatches the operation through that application's servicer. A logical doorbell tells a resident to inspect published work; physical wake mechanisms depend on the architecture and board.

A kernel flip replaces the node kernel while preserving running applications. Its procedure, compatibility requirements, and failure behavior are documented below.

## Use HopOS

- [Getting started](getting-started.md) — select, configure, install, and boot an image; run a first job.
- [Applications](apps.md) — write and build an app; choose dedicated cores, trusted sharing, or SMP.
- [Operations](operations.md) — deploy and replace jobs, inspect logs, use files and volumes, and recover a node.
- [Kernel flip](kernel-flip.md) — update a running kernel and understand what is preserved.

## Understand the implementation

- [Architecture](architecture.md) — the shared framework, architecture and board boundaries, and source layout.
- [Lifecycle and ownership](lifecycle.md) — allocation, publication, waiting, confirmed termination, and reuse.
- [Networking and system calls](networking.md) — caller, transport, frame rings, switching, NAT, and notification.

## Reference

- [Configuration](configuration.md) — node settings, job fields, units, and defaults.
- [Boards and drivers](boards-drivers.md) — boot methods, memory, cores, devices, and board-specific behavior.
- [Development](development.md) — toolchain, dependencies, image builds, and verification commands.

## Measurements and coverage

- [Measurements](measurements.md) — throughput, latency, memory budgets, and measurement conditions.
- [Implementation and test status](status.md) — available features, tested candidates, board coverage, and remaining checks.

This set describes the v2.2.2 source baseline. Hardware results identify the candidate on which they were obtained. Physical network IRQ integration is a planned follow-up; physical boards currently use receive polling.

[Previous documentation (v1)](v1/index.md) is retained for comparison.
