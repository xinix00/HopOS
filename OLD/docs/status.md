# Implementation and test status

The O6N physical review of **18 September 2026** is recorded in [L77 evidence](v1/technical/release-evidence/2026-09-17-o6n-acceptance.json). The Pi4/Pi5/Radxa review of **9 September 2026** is recorded in [L72](v1/technical/release-logboek.md#l72--pi4-pi5-and-radxa-final-lifecycle-and-sustained-use-round-passed) with [source and artifact evidence](v1/technical/release-evidence/2026-09-09-board-acceptance.json). These are review builds, not a newly published release. Historical v2.2.2 source checks below refer to `2e9011e` plus the pool-download change in `09981e1`. A feature's presence, a successful build, and a hardware acceptance result are recorded separately.

## Available functions

| Function | Implementation | Verification scope |
| --- | --- | --- |
| Shared compute framework on ARM64 and RISC-V | Present | Host tests, target builds, QEMU integration, and physical lifecycle tests. |
| Per-app memory partitions and confirmed release | Present | Twenty lifecycle cycles on five boards; sampled memory canaries and reuse tests provide additional coverage. |
| Dedicated cores and explicit trusted core sharing | Present | Shared-neighbor survival tested on five boards. Class-aware sharing and subsequent SMP placement tested on selected ARM boards. |
| Multiple dedicated cores within one app | Present | Two-worker compute alongside shared residents on M4, Pi 4, Pi 5, and Radxa. LicheeRV has one app hart. |
| Logical doorbell and wait/wake protocol | Present | Progress and repeated quiet-wake checks. Not a complete per-board power or idle measurement. |
| Kernel flip with resident adoption | Present | Three-flip sequences with persistent app connections on four ARM boards; L72 rechecks Pi4/Pi5/Radxa. RISC-V window reuse and retained TCP pass in L53–L60. |
| Download into the borrowed kernel window | Present in v2.2.2 | Host/race/build checks plus LicheeRV window-reuse acceptance in L53–L60 and Pi4/Pi5/Radxa live replacements in L72. Exact candidate scope is recorded in the evidence. |
| Network switching, port publishing, and node management | Present | HTTP compute/content checks, lifecycle operations, persistent connections, and fresh agent logs. |
| File and volume API | Present where storage is available | M4 content and cleanup checks across flips. Storage devices and access differ by board. |
| Watchdog recovery | Board-dependent | Controlled recovery on M4 and observed recovery after LicheeRV download failures. The internal agent probe does not test external NIC reception. |
| Physical network IRQ | Planned follow-up | QEMU virt implements the IRQ path. The physical boards currently use polling. |

See the [complete board support checklist](support.md) for the current source inventory, including O6N and capability gaps, [boards and drivers](boards-drivers.md) for the device paths, and [measurements](measurements.md) for the associated numbers.

## Hardware acceptance

**O6N:** the 18 September ordering candidate passes lifecycle, heterogeneous shared-core replacement, all-eleven-core reclamation, three live kernel replacements and a full thirty-minute compute/network/NVMe run. A separate RXACK candidate passes two 9 GiB source-plus-backup integrity rounds. Exact artifact scopes, the corrected test-fixture OOM and subsequent checks are retained in [L77 evidence](v1/technical/release-evidence/2026-09-17-o6n-acceptance.json). The subsequent [USB/NVMe follow-up](v1/technical/release-evidence/2026-09-18-o6n-usb.json) starts all ten native xHCI controllers, detects the Logitech keyboard/mouse receiver, and restores display/input ownership across two more live FLIPs. Non-HID devices are no longer repeatedly enumerated. Physical cursor/click confirmation remains separate. A later display exit2 during desktop use is under investigation; the completed bounded FLIP checks do not establish overall GUI stability. The [matched NVMe comparison](v1/technical/release-evidence/2026-09-18-o6n-nvme.json) and cached-content checks pass.

App-core counts describe the tested machines. “Passed” covers the stated test, not every possible workload or configuration.

| Machine | ISA | App cores | 20 lifecycle cycles | Sharing / neighbor survival | SMP compute | Three flips and subsequent management |
| --- | --- | ---: | --- | --- | --- | --- |
| Mac mini M4 | ARM64 | 9 | Passed | Passed | Passed | Generations 11–13; incoming and outgoing app TCP connections retained. |
| Raspberry Pi 4 | ARM64 | 3 | Passed | Passed | Passed | L72 production64–67: generations 2–4 retain the same three resident tasks, boot identities and TCP sockets. Full lifecycle/reuse and quiet wake pass. |
| Raspberry Pi 5 | ARM64 | 3 | Passed | Passed | Passed | L72 final31–33: generations 8–10 retain three residents and TCP sockets. Two later UART-only replacements retain Welcome as generations 11–12. |
| Radxa Zero 3E | ARM64 | 3 | Passed | Passed | Passed | L72 final31–33: generations 7–9 retain the same three residents and TCP sockets; a fresh shared replacement starts afterward. |
| LicheeRV Nano | RISC-V | 1 | Passed | Single-buffer candidate: cloudflared + Stulp + plugins (206 MiB) start; three plugin replacements preserve neighbors | Not applicable | Window-reuse candidate: generations 1–4 retain the resident and the same TCP socket throughout. Swaps 2 and 4 return to the original kernel window; all three user apps fit afterward. |
| O6N | ARM64 | 11 | Passed | Mixed memory-size replacement and neighbor survival passed | Two-core mixed workload and eleven-core reclamation passed | Ordering generations 1–3 retain all three task/boot identities and the same TCP sockets; subsequent replacement/reuse passed. |
| QEMU virt | ARM64 | Configuration-dependent | Separate integration coverage | Separate integration coverage | Separate integration coverage | Emulation results are retained separately from physical-board results. |
| UEFI / Altra | ARM64 | Machine-dependent | Not signed off in this round | Not signed off in this round | Not signed off in this round | Builds pass; no new physical acceptance result from this round. |

The L72 Pi4/Pi5/Radxa sequences exercise the current review downloader and adoption path. Pi4's warm GENET failure is traced to retained packet counters disagreeing with reset descriptor pointers; initializing the pointers from those counters passes the recorded production sequence. Maximum individual Echo delays during consecutive replacements are recorded in L72: retained connections do not imply zero service pause. Earlier M4 and LicheeRV results retain their own artifact scope.

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

L72 closes the requested Pi4/Pi5/Radxa lifecycle and thirty-minute compute/network/Vitals round. All three return to Welcome only. Each accepted duration run completes without content errors, unexpected restarts or growing claims, and releases its test claims. Pi5's later boot-path updates have separately recorded live-FLIP/download checks. O6N bring-up is confirmed complete. L77 closes the recorded O6N lifecycle, live-FLIP, full thirty-minute compute/network/NVMe and final Vitals round, including automatic watchdog recovery from an intentionally stalled update. The remaining M4 round, its matched storage comparisons and release packaging remain in the [release plan](v1/technical/release-afronding.md). Final acceptance identifies the actual tested release artifacts. After this round, use the full [board support checklist](support.md) to select follow-up work; network IRQ is one item alongside idle, DVFS, sensors, storage and display.

## Evidence records

The existing [release logbook](v1/technical/release-logboek.md) remains the authoritative chronological record. [Compact hardware evidence](v1/technical/release-evidence/2026-09-06-hardware.json) and the linked gate logs are versioned. Full local traces and test scripts under `docs/v1/reviews/` are not published by the current ignore rules; their paths in the logbook identify local records, not public download links.


## Large-partition correction — 9 September 2026

The current candidate removes the old ARM 768 MiB app-window limit. ARM and RISC-V use the same physical reservation, release and FLIP-adoption lifecycle; architecture code supplies the translation mechanism and any additional table storage. M4 tests pass with 5 GiB and 18 GiB app partitions. The 18 GiB app, Spin and cloudflared survive a live FLIP with unchanged task IDs; the fixture retains its boot ID and checked memory patterns, and its complete claim is subsequently released. See [acceptance evidence](v1/technical/release-evidence/2026-09-09-large-partitions.json). This is memory-mapping evidence, not a disk-capacity or throughput result. RISC-V source/target checks pass; no new physical LicheeRV deployment is claimed for this change.
