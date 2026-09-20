# Measurements

Measurements describe a particular machine, build, and workload. This page keeps throughput, latency, and resource budgets separate. MB/s denotes decimal megabytes per second; MiB denotes a binary memory size of 1,048,576 bytes.

## Storage

| Machine | Operation | Result | Measurement source and conditions |
| --- | --- | --- | --- |
| Mac mini M4 | Raw NVMe write / read, 1 MiB commands, 16 MiB, boot benchmark | **5389–5756 / 1824–1971 MB/s** | Three runs on 18 September (L78–L79) on APPLE SSD AP0512Z, HopOS range 395188 MB, driver commands on the tail of the HopOS window, completed I/O only. HopFS through 1 MiB calls: 5480–5632 / 1781–1816 MB/s. App path (Vitals, 256 MiB file, 1 MiB calls, three runs): 1713–1742 / 1120–1137 MB/s. The earlier developer-reported approximately 4000 MB/s figure is superseded. |
| Mac mini M4 | Vitals network, 64 MiB over 1 Gbit tg3 | **download 44.7–48.3, upload 38.7–41.4 MB/s** | Three repetitions on 18 September (L79), HTTP from the host, three big cores. |
| Mac mini M4 | App storage write / read | **1598–1657 / 1064–1072 MB/s** | 18 September: three 256 MiB files, 1 MiB calls, one-core/512 MiB Vitals fixture; existing Spin and cloudflared kept running. Full repeated-pattern readback and size checks passed. No FLIP or reboot. [Recorded results](v1/technical/release-evidence/2026-09-18-m4-nvme.json). |
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

## O6N review measurements — 18 September 2026

The tested board has a WD_BLACK SN770 500GB NVMe device and a Realtek RTL8125B port negotiated at 1 Gbit/s. Clocks remain under firmware control; network reception uses polling. These are review candidates, not a published release. Exact artifacts and results are recorded in [L77 evidence](v1/technical/release-evidence/2026-09-17-o6n-acceptance.json).

| Workload | Result | Conditions |
| --- | --- | --- |
| Sequential file write / read | 46.00–46.37 / 55.53–56.91 MB/s | Ordering candidate; three repetitions, Vitals app with 512 MiB and eleven cores; 256 MiB file, 1 MiB calls, through the full app storage path. |
| Small sequential overwrites | 15.15–17.08 MB/s | 256 calls of 4 KiB, 1 MiB total; no subsequent readback in this subtest. Not random IOPS or sustained database throughput. |
| Metadata call | 66.07–84.13 µs median across three runs | 200 Stat calls; excludes bulk data transport and copying. |
| LAN download / upload | 44.24–45.06 / 35.65–37.88 MB/s | Three repetitions of 64 MiB each; upload uses 1 MiB requests over one keep-alive connection. |
| Eleven-core sustained compute | 4397 → 4403 Msteps/s | 120 seconds of the Vitals integer workload; maximum reported temperature 44 °C. |
| Three live kernel replacements | All three apps and TCP sockets retained | Ordering candidate, generations 1–3; longest observed echo delay 9.18 seconds. |

Later GUI/input candidates retained all five resident tasks across two additional FLIPs. The existing display input stream reattached after 22.62 and 19.28 seconds from the request; a subsequent 35-second quiet check recorded no reconnect churn. These are automatic recovery times, not zero input interruption. Exact artifacts and scope are in the [USB follow-up](v1/technical/release-evidence/2026-09-18-o6n-usb.json).

A separate integrity test completed two passes, each with a 9 GiB source and a 9 GiB backup. It checked position-dependent contents throughout, matched full-file hashes, then removed both files before using a different pattern for the next pass. This is stronger content verification than the repeated-buffer performance workload. It establishes completed writes and readback, not power-loss durability or exact physical-block reuse.

A subsequent matched comparison isolated the DMA mapping cost. The baseline used Device memory for the data buffer. The candidate maps an isolated data block write-back cached and reuses the existing cache clean/invalidate operations; queues and PRP lists remain uncached. Commands remain synchronous, one at a time.

| Path and workload | Baseline write / read | Cached candidate write / read |
| --- | --- | --- |
| Raw NVMe, 1 MiB commands, 16 MiB total | 49.1 / 59.1 MB/s | 3056.8 / 2705.6 MB/s |
| HopFS, 1 MiB calls, 16 MiB total | 48.6 / 59.1 MB/s | 2970.1 / 2716.7 MB/s |
| App storage, 256 MiB file, 1 MiB calls | 45.61–46.40 / 55.37–56.56 MB/s | 556.83–625.43 / 727.49–796.83 MB/s |

Raw/HopFS figures are single boot-benchmark runs; app figures cover three repetitions with the same 512 MiB, eleven-core Vitals fixture. These short tests measure completed I/O, not sustained drive limits or power-loss durability. A separate two-pass64MiB fixture passed position-dependent full readback, overlapping writes and mixed512B–1MiB transfer lengths, including boundary crossings and cleanup. Exact bundles and results are in the [NVMe comparison record](v1/technical/release-evidence/2026-09-18-o6n-nvme.json).

## O6N network interrupt

| Item | Interrupt (bundle 45) | Polled (bundle 32, same tree) | Scope |
| --- | --- | --- | --- |
| Vitals rtt p50 | **156 / 201 / 200 µs** | 160 / 168 µs | Two-core Vitals fixture next to the display and launcher residents, 18 September evening (L80). |
| Vitals storm | 844 conn/s, p99 19.6 ms | 780 conn/s | Same fixture. |
| Interrupts claimed | 1,813 in 40 s including an rtt run (bundle 44) | 3,333 polls/s | Before the write-one-to-clear-all ack the same path claimed about 36,000 empty interrupts per second. |

## M4 network interrupt

| Item | Interrupt (bundle 25) | Polled (bundle 14, same tree) | Scope |
| --- | --- | --- | --- |
| Vitals rtt p50 | **50 / 62 / 66 µs** | 48 / 63 / 54 µs | Two-core Vitals fixture, 19 September morning (L82). |
| Vitals storm | 6379 conn/s, p99 1.9 ms | 6537 conn/s, p99 1.9 ms | Same fixture. |
| 64 MiB into the node (HOP RX) | **43.3–46.7 MB/s** | 36.6–38.3 MB/s | HTTP PUT from the workstation over 1 Gbit. |
| 64 MiB out of the node (HOP TX) | 47.7–52.3 MB/s | 44.1–46.9 MB/s | HTTP GET. |

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
| O6N watchdog recovery | **146.39 s** from request to normal agent return | Empty-node diagnostic deliberately stalls before the agent. UART confirms the unchanged boot guard expiry, then automatic cold recovery into the normal EFI. |
| Physical-board receive polling interval | **300 µs** | Configured fallback in `hopnet`; an interval, not a measured CPU-usage or power figure. |
| QEMU IRQ wait bound | **10 ms** | The receive loop retains a bounded wait on the IRQ path; this is not a claim that every wait reaches the bound. |

Wake correctness, idle CPU use, and energy consumption are separate measurements. Functional wake tests do not establish a board's power consumption. Physical network IRQ integration and its energy comparison are planned after the current release work.

## Recording further results

Record the build or artifact hash, machine and device, configuration, workload, amount of data, timing boundaries, repetitions, and whether data was cached. For writes, state whether completion measures API return or a device durability guarantee. Preserve the raw result and show the measured spread rather than only the best run.

The release record is [logbook L28 and L34–L41](v1/technical/release-logboek.md), with [compact hardware evidence](v1/technical/release-evidence/2026-09-06-hardware.json). The original local traces are identified there. See [development](development.md) for the existing verification tools.


### Ampere Altra, polled igb, ring 64 → 256 (19 September, bundle 80)

| transfer | ring 64 | ring 256 |
|---|---|---|
| 64 MiB upload into the node | 4.6 / 5.0 / 5.2 MB/s | 36.8 / 34.8 / 36.2 MB/s |
| 64 MiB download from the node | 41.9 / 43.2 / 42.3 MB/s | 42.8 / 42.5 / 42.1 MB/s |
| 32 MB from the internet | 7.8 MB/s | 36.5 MB/s (line-bound) |

Same Vitals fixture (2 cores, 512 MiB), polled at 300 µs, no interrupt. Evidence: `docs/v1/reviews/2026-09-19-ampere/ab-ring256.json`.


> **Caveat (20 September):** all transfer figures measured from the Mac at 192.168.1.208 travelled over its Wi-Fi interface; the link tops out near 50 MB/s with 3–90 ms latency. Node-side ceilings must be re-measured from a wired host (L83 point 20).

## Node to node and in-node, 20 September 2026

| Path | Result | Conditions |
| --- | --- | --- |
| Vitals on the M4 pulling from Vitals on the Ampere, wired, 400 MB | 43.8–44.5 MB/s either way | L83 point 21; the Ampere polled its NIC at the 1.3 ms event-stream tick, the M4 on its NIC interrupt. |
| Two Vitals on the same M4 pulling from each other through the switch, 400 MB | **769 / 764 MB/s** | M4 bundle 33 (L83 point 26); one-core and two-core apps; app pumps woke about 260 times per 400 MB. |
| Mac (Wi-Fi) pulling 200 MB from an M4 app | 56.3 MB/s (one core), 36.7 (two cores) | Wi-Fi ceiling, see above; HOP sent 196 direct RX kicks/s to the sending app. |
| Vitals on the M4 (bundle 34, NIC polled) and on the O6N (polled stick kernel) pulling 400 MB from each other, wired | **103.4 / 114.2 MB/s** (O6N from M4), **101.2 / 101.7 MB/s** (M4 from O6N) | L83 point 27; the M4 on its NIC interrupt gave 45 MB/s on the same wire because a lone frame waited for the 10 ms interrupt failsafe. |
| Vitals on the M4 (bundle 35, tg3 interrupt with the re-enable work check) and on the O6N (bundle 45, RTL interrupt) pulling 400 MB from each other, wired | **109.3 / 112.2 MB/s** (O6N from M4), **118.3 / 118.3 MB/s** (M4 from O6N), first byte 1.2–1.5 ms | L83 point 28; connection cycle over the wire 2.0 / 1.3 ms p50. |
| Two-core Vitals on the M4 (bundle 38) and on the O6N (bundle 45) pulling 400 MB from each other, wired | **111.5 / 114.4 MB/s**, first byte 1.0 ms | L83 point 29; app-to-app connection storms 775 and 1,069 conn/s without errors. |

The per-connection window sits at its 480 KB cap on both paths; the node-to-node figure is window ÷ round trip. The 21 ms connection cycle (10.5 ms round trip) over the wire came from the M4 waiting for its tg3 interrupt failsafe on lone frames; polled, the same wire carries 100–114 MB/s.
