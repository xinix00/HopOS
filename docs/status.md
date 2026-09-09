# Implementation and test status

Source baseline: **v2.2.2**, 6 September 2026. The source commit is `2e9011e` with the pool-download change in `09981e1`. A feature's presence, a successful build, and a hardware acceptance result are recorded separately.

## Available functions

| Function | Implementation | Verification scope |
| --- | --- | --- |
| Shared compute framework on ARM64 and RISC-V | Present | Host tests, target builds, QEMU integration, and physical lifecycle tests. |
| Per-app memory partitions and confirmed release | Present | Twenty lifecycle cycles on five boards; sampled memory canaries and reuse tests provide additional coverage. |
| Dedicated cores and explicit trusted core sharing | Present | Shared-neighbor survival tested on five boards. Class-aware sharing and subsequent SMP placement tested on selected ARM boards. |
| Multiple dedicated cores within one app | Present | Two-worker compute alongside shared residents on M4, Pi 4, Pi 5, and Radxa. LicheeRV has one app hart. |
| Logical doorbell and wait/wake protocol | Present | Progress and repeated quiet-wake checks. Not a complete per-board power or idle measurement. |
| Kernel flip with resident adoption | Present | Three-flip sequences with persistent app connections on four ARM boards. RISC-V's revised downloader still requires a hardware run. |
| Download into the borrowed kernel window | Present in v2.2.2 | Host tests, targeted race checks, all target builds, a RISC-V cold image, and dual-link flip-bundle construction pass. Hardware acceptance is pending. |
| Network switching, port publishing, and node management | Present | HTTP compute/content checks, lifecycle operations, persistent connections, and fresh agent logs. |
| File and volume API | Present where storage is available | M4 content and cleanup checks across flips. Storage devices and access differ by board. |
| Watchdog recovery | Board-dependent | Controlled recovery on M4 and observed recovery after LicheeRV download failures. The internal agent probe does not test external NIC reception. |
| Physical network IRQ | Planned follow-up | QEMU virt implements the IRQ path. The physical boards currently use polling. |

See [boards and drivers](boards-drivers.md) for what is wired on each board, and [measurements](measurements.md) for the associated numbers.

## Hardware acceptance

App-core counts describe the tested machines. “Passed” covers the stated test, not every possible workload or configuration.

| Machine | ISA | App cores | 20 lifecycle cycles | Sharing / neighbor survival | SMP compute | Three flips and subsequent management |
| --- | --- | ---: | --- | --- | --- | --- |
| Mac mini M4 | ARM64 | 9 | Passed | Passed | Passed | Generations 11–13; incoming and outgoing app TCP connections retained. |
| Raspberry Pi 4 | ARM64 | 3 | Passed | Passed | Passed | Generations 1–3 on 2.2.1 candidates; the same two incoming sockets throughout. Release and replacement passed. |
| Raspberry Pi 5 | ARM64 | 3 | Passed | Passed | Passed | Generations 7–9 on 2.2.1 candidates; the same two incoming sockets. Generation 9 required one retry after a pre-jump download dial timeout. |
| Radxa Zero 3E | ARM64 | 3 | Passed | Passed | Passed | Generations 3–5; incoming and outgoing app TCP connections retained. |
| LicheeRV Nano | RISC-V | 1 | Passed | Single-buffer candidate: cloudflared + Stulp + plugins (206 MiB) start; three plugin replacements preserve neighbors | Not applicable | Window-reuse candidate: generations 1–4 retain the resident and the same TCP socket throughout. Swaps 2 and 4 return to the original kernel window; all three user apps fit afterward. |
| QEMU virt | ARM64 | Configuration-dependent | Separate integration coverage | Separate integration coverage | Separate integration coverage | Emulation results are retained separately from physical-board results. |
| UEFI / Altra | ARM64 | Machine-dependent | Not signed off in this round | Not signed off in this round | Not signed off in this round | Builds pass; no new physical acceptance result from this round. |

The successful ARM flip sequences predate the new downloader. They establish the recorded candidates' preservation and management behavior. They do not automatically validate the revised v2.2.2 transport. The causes of earlier Pi failures were not conclusively established by the later successful runs.

## Source verification for v2.2.2

- The existing host and target gate passed, including the Apple, Pi, Radxa, UEFI, QEMU, and RISC-V build variants.
- Targeted race tests passed for kernel-flip parsing/transport, placement, and layout.
- A broader standalone slots race run stopped at `checkptr` in an existing ownership fixture using raw memory operations. The ordinary slots tests passed in the gate; this is not recorded as a successful full slots race run.
- The RISC-V cold image and flip bundle were built. Dual-link relocation checking passed.

Gate output: [v2.2.2 host and target checks](v1/technical/release-evidence/2026-09-06-222-host-target-gate.log). The code baseline and limits are recorded in [logbook L41](v1/technical/release-logboek.md).

## Remaining release work

The LicheeRV startup OOM and cold-window reuse issues pass hardware retesting on the current candidate. The original three jobs start in one sharegroup; four kernel swaps retain one test resident and the same host TCP socket. Swaps two and four return to the original kernel window, and the complete 206 MiB workload fits afterward without reboot. See [logbook L53](v1/technical/release-logboek.md) and [compact evidence](v1/technical/release-evidence/2026-09-07-licheerv-window-reuse.json). These results identify tested candidates, not the previously published stock v2.2.2.

Transport follow-up found and corrected a false-idle timeout in HOP: UI progress batching delayed the idle-clock refresh. A continuously slow fixture fails on the old code and completes on the corrected candidate; genuinely silent streams still abort and release their claim. Ten original-source downloads before the fix, ten local downloads, and ten original-source downloads after it pass. The original isolated stall did not recur, so its cause is not established. See [transport evidence](v1/technical/release-evidence/2026-09-07-licheerv-transport.json).

The corrected HOP dependency is uncommitted and unpublished. Publish it and update the production HopOS dependency pin before making a release claim; the hardware candidate used an isolated local replacement (L55).

The cage/core separation candidate removes the allocation rule that skipped cage 1 for shared apps. After two LicheeRV FLIPs, Stulp, Stulp-plugins and cloudflared-lean start from the unchanged definitions in that order, using cages 1–3. Stulp now occupies the configured `10.100.0.2`; Derek reports that Stulp works correctly again without an observed regression (logbook L60). Mixed sharing/SMP compute, stop/reuse and live FLIP with a retained SMP TCP connection pass in QEMU. See [L59 evidence](v1/technical/release-evidence/2026-09-07-cage-core-separation.json). Reusable cold reservations are enabled on LicheeRV. Other boards require their persistent boot structures to be accounted for before their original reservations can enter the pool. The explicit reboot option has been removed; online updates use FLIP.

The remaining hardware/I/O comparisons and the thirty-minute combined compute/I/O run remain in the [release plan](v1/technical/release-afronding.md). Final acceptance identifies the actual tested release artifacts. Physical network IRQ integration follows separately after this round.

## Evidence records

The existing [release logbook](v1/technical/release-logboek.md) remains the authoritative chronological record. [Compact hardware evidence](v1/technical/release-evidence/2026-09-06-hardware.json) and the linked gate logs are versioned. Full local traces and test scripts under `docs/v1/reviews/` are not published by the current ignore rules; their paths in the logbook identify local records, not public download links.
