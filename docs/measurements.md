# Measurements

Measurements describe a particular machine, build, and workload. This page keeps throughput, latency, and resource budgets separate. MB/s denotes decimal megabytes per second; MiB denotes a binary memory size of 1,048,576 bytes.

## Storage

| Machine | Operation | Result | Measurement source and conditions |
| --- | --- | --- | --- |
| Mac mini M4 | Storage write throughput | Approximately **4000 MB/s** | Measured by Derek on the M4. Drive model, exact workload, build, cache conditions, and repetition count are not recorded with this result. It was not repeated during the 6 September release review. |
| Mac mini M4 | First filesystem metadata call after kernel flip | Approximately **200.47 ms** | Release-review fixture; internal system-connection recovery occurred on this first call. |
| Mac mini M4 | First 16-MiB write after the metadata call | Approximately **9.19 ms** | Same diagnostic sequence. Content and cleanup were checked; this is a single call timing, not a sustained-throughput benchmark. |

The app storage path is app API → internal network → node system service → application servicer → file layer → storage driver. An end-to-end API measurement includes this path. It should not be relabelled as raw device throughput. See [networking](networking.md) and [operations](operations.md).

## Kernel-flip latency and continuity

The Pi 4 acceptance fixture kept the same two incoming TCP socket objects throughout three kernel flips. It checked echoed content, application boot markers, task identity, and an SMP checksum after each transition.

| Pi 4 generation | Flip requests | Highest observed echo RTT |
| --- | ---: | ---: |
| 1 | 1 | 5.220 s |
| 2 | 1 | 8.014 s |
| 3 | 1 | 4.323 s |

These are results from the 2.2.1 candidates tested on 6 September. They demonstrate connection continuity with a measurable response delay. They are not measurements of v2.2.2's revised download path. M4, Pi 5, and Radxa also completed three-flip sequences; their exact scope is in [status](status.md).

## Memory and download behavior

| Item | Size or observation | Scope |
| --- | --- | --- |
| LicheeRV node kernel window | **32 MiB** | Current board declaration; unchanged by the v2.2.2 fix. |
| LicheeRV app pool before a flip loan | **222 MiB** | Three regions: 126 + 64 + 32 MiB. The largest free contiguous region and live allocations determine placement. |
| v2.2.1 failing download declaration | **6,150,136 bytes** | With zero payload bytes sent, UART recorded an allocation failure in `readBundle`. |
| Runtime allocation rejected in that reproduction | **8 MiB** | UART: `cannot allocate 8388608-byte block (8028160 in use)`. This is an allocator diagnostic, not a measurement of total free DRAM. |
| v2.2.2 download work buffer | **32 KiB** | The bundle is stored in the borrowed new kernel window. Parser and placement use bounded buffers; no bundle-sized kernel-heap allocation is required by this path. |

A reserved flip window temporarily reduces available pool space. It is returned on a failed attempt. This is separate from the kernel's own declared RAM and from each application's memory limit. See [kernel flip](kernel-flip.md) and [lifecycle](lifecycle.md).

## Recovery and polling

| Item | Observation | Scope |
| --- | --- | --- |
| M4 watchdog recovery | **167.13 s** from flip request to cold recovery | Controlled stalled boot after arming the flip boot guard. The node returned without a manual reset; internal expiry markers were not recovered from the black box. |
| LicheeRV watchdog recovery | **89.30 s** in the counted-download diagnostic | Cold return after a failed download. The later UART reproduction established the allocation failure directly. |
| Physical-board receive polling interval | **300 µs** | Configured fallback in `hopnet`; an interval, not a measured CPU-usage or power figure. |
| QEMU IRQ wait bound | **10 ms** | The receive loop retains a bounded wait on the IRQ path; this is not a claim that every wait reaches the bound. |

Wake correctness, idle CPU use, and energy consumption are separate measurements. Functional wake tests do not establish a board's power consumption. Physical network IRQ integration and its energy comparison are planned after the current release work.

## Recording further results

Record the build or artifact hash, machine and device, configuration, workload, amount of data, timing boundaries, repetitions, and whether data was cached. For writes, state whether completion measures API return or a device durability guarantee. Preserve the raw result and show the measured spread rather than only the best run.

The release record is [logbook L28 and L34–L41](v1/technical/release-logboek.md), with [compact hardware evidence](v1/technical/release-evidence/2026-09-06-hardware.json). The original local traces are identified there. See [development](development.md) for the existing verification tools.
