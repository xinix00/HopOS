# Measurements per board

PORT.md §1: this page is the bar. A Rust HOP may be slower than v2 on no
measured number. Each board lists the latest v3 measurement (*Nu*, "now"), the
best number v3 ever reached (*Hoogste v3*, "best v3") and the v2 number.

As of 2026-10-04 16:00: a clean round with `tools/meet` on 3.0.10 over all
seven boards. No round has been run on 3.0.11 to 3.0.13 yet. The history of
a cell is in git.

## How to write this page

1. **One section per board**, always in this order: O6N, Altra, Pi 5,
   Pi 4, Radxa, LicheeRV, M4, QEMU. A new board gets a new section with the
   same layout.
2. **Every section has the same layout**: one line *Opzet* (setup: the
   stamp, the date, the commit if you know it, and how vitals was placed),
   then the tables *Vitals*, *Netwerk* (network), *In de node* (inside the
   node), *In rust* (at rest), then *Watchdog*, and where they exist
   *Opslag* (storage) and *Media*. After that at most three sentences
   *Reading* and a list *Board*.
3. **Every table has four columns:** `Meting | Nu | Hoogste v3 | v2` (measurement, now, best v3, v2).
   *Nu* holds the number from the latest round (the stamp and the date
   appear once in *Opzet*; empty if the round does not measure the row).
   *Hoogste v3* holds the best number v3 ever reached, with `(stamp, dd-mm)`.
   *v2* holds the v2 number for this board (the bar; empty if v2 had none).
   The *Watchdog* table is not a measurement series and keeps
   `Meting | v3 | v2 | Run`.
4. **Best is the better direction per row**: more is better, except for
   the rows in `LAAG` in `tools/meet.py` (`memlat`, `rtt`, `timer`, `idle`,
   `wrtt`, `cycle`, `nrtt`, `ncold`, `kcpu`, `kmem`, `hop`, `temp`: rtt,
   memlat, oversleep, wakeups/s, connection cycle, kernel cpu, kernel
   memory, Hop and temperature). The first number in the cell decides, the
   next ones break a tie; a range counts by its first number.
   `tools/meet.py --write` overwrites *Nu* and only updates *Hoogste v3*,
   with the new stamp, when *Nu* is better.
5. **One run per point**, vitals with `secs=5`, bench with one pull per
   direction. Do a second run only for an odd number, and note the reason.
   If you do have more runs, write a range (`783–810`), never just the best.
6. **Bare numbers**, with the unit in the row name. Use a decimal comma and
   a point for thousands (`302.439`); `tools/meet.py` parses numbers that
   way. MB/s is decimal (1,000,000 bytes per second). The v2 netmeter
   counted MiB/s; `tools/netmeter` counts decimal.
7. **No text in a cell.** Peer, cores and size go in *Opzet*. A remark that
   explains a number (what the limit is, an unequal setup behind a *Hoogste v3*)
   goes on one line under the table. Stories belong in ALLES.md or in the
   boards-*.md files.
8. **Leave out what was not measured.** One exception: a row with a v2
   number stays, with `—` under *Hoogste v3*, so the open bar stays visible.
   What still has to be done goes on the line *Still to measure*.
9. **No Wi-Fi numbers.** A measurement to or from the laptop over Wi-Fi
   measures the Wi-Fi, not the board. Measure over the wire, node to node
   or from a wired host.
10. **A measurement that is the same on every board** (a failure reason, a
    property of the tool) goes once under *Tools*, not under every board.
11. **Some names are keys for `tools/meet.py --write` and stay in Dutch**
    until meet.py changes: the board section headings, the column headers
    (`Meting | Nu | Hoogste v3 | v2`, also the vocabulary of meet.py), the
    table titles
    `**Vitals**`, `**Netwerk**`, `**In de node**`, `**In rust**` and
    `**Opslag**`, the `Opzet:` prefix, the row names in `ROWS` and
    `OVERRIDE` in `tools/meet.py`, and the `(stamp, dd-mm)` form in *Hoogste
    v3*. Renaming one makes `--write` miss the row and add a new one. In the
    row names: *naar de kern* = to the kernel, *overslaap* = oversleep,
    *wekken/s* = wakeups/s, *De node in / uit* = into / out of the node,
    *over de draad* = over the wire, *koud* = cold, *Verbindingscyclus* =
    connection cycle, *hairpin naar zichzelf* = hairpin to itself, *App
    naar app* = app to app, *Kern-cpu* / *Kern-geheugen gebruikt / totaal*
    = kernel cpu / kernel memory used / total, *Temperatuur* = temperature,
    *schrijven / lezen* = write / read.

## Tools

| What | Command | Marker |
| --- | --- | --- |
| The whole round: setup, vitals, bench inside the node and over the wire, cleanup | `python3 tools/meet.py` (`--only pi4,pi5`, `--skip-wire`, `--peer IP`, `--art URL` for the source of `vitals-<arch>.elf` and `bench-<arch>.elf`, `--stamp m4=M40` when the console lost the boot line, `--date dd-mm` for the cells (default today), `--write` sets *Nu* and *Hoogste v3*, `--write --no-run` writes the fragments in `--out` without measuring, `--selftest` tests `--write`) | per board `target/meet/<board>.md`, the time per phase |
| Place vitals | `curl -X POST -d '{"name":"vitals","driver":"hop","artifacts":[{"url":"http://LAPTOP:8000/vitals.elf"}],"memory_limit":134217728,"cpu_shares":2048,"ports":{"http":8090}}' http://NODE:9080/v1/jobs` | `HOPOS_VITALS_UP port=8090` |
| Vitals, everything | `curl 'http://NODE:8090/api/run?test=all&secs=5'` | `HOPOS_VITALS_ALL failed= skipped=` |
| Vitals, one test | `curl 'http://NODE:8090/api/run?test=<name>'` (cpu, smp, burn, membw, memlat, alloc, storm, rtt, timer, idle, disk, rx) | `HOPOS_VITALS_<TEST> key=value ...`, `HOPOS_VITALS_FAIL`, `HOPOS_VITALS_SKIP` |
| The bench in a slot (server) | `curl -X POST -d '{"name":"bench","driver":"hop","artifacts":[{"url":"http://LAPTOP:8000/bench.elf"}],"memory_limit":67108864,"ports":{"http":80}}' http://NODE:9080/v1/jobs` | `HOPOS_BENCH_UP role=serve port=80` |
| From the host to an app | `cargo run --release -p netmeter -- NODE:80 --phases in,out,rtt,storm,udp --json > run.json` | `NETMETER phase=...` |
| App to app (inside the node or over the wire) | job with `"env":{"BENCH":"pull","BENCH_PEER":"IP:80","BENCH_BYTES":"419430400"}` (or `ping`, `push`) | `HOPOS_BENCH_PULL`, `HOPOS_BENCH_RTT`, `HOPOS_BENCH_COLD` |
| Load on an app core | job with `"env":{"BURN":"1"}` (`BURN_WORK`, `BURN_REST` in s) | `HOPOS_BENCH_BURN` |
| Memory under pressure | job with `"env":{"THRASH":"1"}` | `HOPOS_BENCH_THRASH` |
| Multicast between two apps | one job with `"env":{"MCAST":"listen"}`, then one with `"env":{"MCAST":"send"}` | `HOPOS_BENCH_MCAST recv=N` |
| The OS core at rest | `hopos.idlestat=1` in `hopos.cfg` or on the cmdline | `HOPOS_IDLESTAT` |
| Disk, raw and through hopfs | `hopos.nvmebench=1`, or the feature `nvmebench` in a measurement kernel | `HOPOS_NVMEBENCH`, `_SEQ`, `_RAND`, `_RANDQ` |
| Kernel and Hop at rest | `GET :9080/v1/agents` (`kern_cpu_percent`, `kern_mem_bytes`, `hop_cpu_percent`, `hop_mem_bytes`, `temp_milli_c`), three polls 10 s apart, only welcome placed | |
| The chain on QEMU | `sh tools/qemu-test-bench.sh` | `bench-keten groen` |
| Hours of placing and stopping | `ARTIFACT=http://LAPTOP:8000/bench.elf sh tools/soak.sh NODE [HOURS]` (`FLIP=1` with `FLIP_URL` and `FLIP_SHA` to add the flip) | `SOAK` |

Code: `apps/vitals` (README there), `apps/bench` (protocol in
`apps/bench/src/proto.rs`), `tools/netmeter`, `tools/meet.py`,
`hopos/src/bench.rs`.

The same everywhere:
- `test=rx` fails without `url`. The default source (cachefly) needs a
  tunnel: `http: CONNECT is not supported`. So `test=all` with one core
  gives `failed=1` (rx) and `skipped=2` (smp with one core, disk without a
  disk).
- The vitals *storm* goes to its own port, the vitals *rtt* to the kernel
  (10.100.0.1:10100).
- On QEMU WFE is a no-op. Idle rows only count on hardware.

## Orion O6N

Opzet: 3.0.10, 2026-10-04, tools/meet. Vitals with 4 cores (`core-class:
big`), 128 MiB. Over the wire against the M4 (meet-serve :9100, vitals
:8090).

**Vitals**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| cpu, Msteps/s | 752 | 754 (O3, 03-10) | |
| smp, speedup | 3,84 | 3,99 (O2, 03-10) | |
| burn begin → eind, Msteps/s / max °C | 2990 → 2990 / 47,0 | 2991 → 2991 / 48,0 (O95, 04-10) | |
| membw copy / triad, GB/s | 40,9 / 27,7 | 42,0 / 28,2 (O95, 04-10) | |
| memlat 32 KB / 2 MB / 8 MB, ns | 1,74 / 20,5 / 32,2 | 1,74 / 20,5 / 32,2 (O95, 04-10) | |
| alloc, allocs/s | 520.216 | 520.336 (3.0.10, 04-10) | |
| storm, conn/s (p99 ms) | 9971 (0,86) | 9971 (0,86) (3.0.10, 04-10) | |
| rtt naar de kern p50 / p99, µs | 139 / 992 | 99 / 7761 (O40, 04-10) | |
| timer 1 ms, overslaap p50 / p99, µs | 48 / 92 | 48 / 71 (O40, 04-10) | |
| idle, wekken/s | 44,4 | 43,0 (O95, 04-10) | |

**Netwerk**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| De node in, MB/s | 87,2 | 117,6 (O95, 04-10) | 111,3–116,5 |
| De node uit, MB/s | 95,9 | 113,0 (O40, 04-10) | 114,3–117,6 |
| rtt over de draad p50 / p99 / koud, µs | 203 / 271 / 293 | 73 / 114–119 / 75 (O40, 04-10) | 156–201 |
| Verbindingscyclus p50, ms | 0,78 | 0,30 (O40, 04-10) | 3,6 |
| Storm over de draad, conn/s (p99 ms) | 4905 (2,85) | 9584 (0,98) (O95, 04-10) | 844 (19,6) |
| Storm, hairpin naar zichzelf, conn/s (p99 ms) | 985 (12,1) | 1983 (10,0) (O40, 04-10) | |

Best from O40 (out, rtt and cycle): against the Pi 5 with the O6N busy.

**In de node**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| App naar app, MB/s | 5156 | 5156 (3.0.10, 04-10) | |
| rtt warm p50 / p99, µs | 13 / 37 | 13 / 37 (3.0.10, 04-10) | |
| rtt koud p50, µs | 13 | 13 (3.0.10, 04-10) | |

**In rust**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| Kern-cpu, % | 1 | 1 (O40, 04-10) | |
| Kern-geheugen gebruikt / totaal, MiB | 37,4–37,5 / 224 | 31,4 / 224 (O95, 04-10) | |
| Hop geheugen, MiB / cpu, % | 0,53 / 1 | 0,41 / 1 (3.0.10, 04-10) | |
| Temperatuur, °C | 42,0 | 40,0 (O40, 04-10) | |

**Watchdog**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Watchdog test (measurement kernel `wdtest`: Hop stops after 60 s) | **47 s, but the stick kernel O2 first landed on the old flip handover in DRAM and only did a cold boot after the boot guard (2.5 min); post-mortem of the intermediate boot, not of O41w (O41w, 04-10)** | | O41w 04-10 |

**Opslag**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| Raw 1 MiB, write / read, MB/s | | — | 3056,8 / 2705,6 |
| hopfs 1 MiB, write / read, MB/s | | 3584 / 2992 (nvmebench, 04-10) | 2970,1 / 2716,7 |
| App-opslag via vitals disk, schrijven / lezen, MB/s | 1580 / 1934 | 1580 / 1934 (3.0.10, 04-10) | 556,8–625,4 / 727,5–796,8 |
| 4 KiB schrijven via de app, MB/s | 81,7 | 84,2 (O40, 04-10) | 15,15–17,08 |
| Random 4 KiB read by 1 / 2 / 4 / 8 / 9 apps, requests/s | | 7.983 / 16.561 / 29.052 / 46.868 / 50.150 (O43, 04-10) | |
| Same without a disk (`hole=1`), requests/s | | 22.694 / 46.643 / 56.672 / 78.017 / 78.226 (O43, 04-10) | |

The two Dutch rows are the vitals disk rows (`OVERRIDE` in `tools/meet.py`):
app storage via vitals disk, and 4 KiB write via the app. v2 raw is cached,
v2 app storage 256 MiB. Through apps: the drive does 15.6k raw with one read
in flight and 71k with sixteen; without a disk the OS core is saturated
(~12.8 µs per call).

**Media**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| 4K P010 through the grant (apps/decode), fps | | 85,7 (30-09) | 27,25 |

v2 bar 24 fps; 85.7 fps is 2134 MB/s.

Reading: the OS core round trip on O40 is 77 to 82 µs with welcome and
bench-serve running; the 55 of O16h was in an empty node. The 298 against
855 on 2026-09-30 is the small core, not a regression. Since 68a86c6 the O6N
clocks up to full within one round when a compute job sits in a slot.

Still to measure: the rest of the `nvmebench` rows in the table (the kernel
of 2026-10-04 reached SEQ read 3263 MB/s, RANDQ 16 in flight 71k IOPS);
media after a cold boot.

Board:
- Boot: VHE, kick via SGI 1. Timer: CNTHP (PPI 26).
- NIC: RTL8125B, MSI-X via the ITS (LPI 8192).
- Watchdog: SBSA, 8.5 s.
- RNG: no FEAT_RNG or SMCCC-TRNG; the kernel uses EFI_RNG_PROTOCOL
  (`hopos.efirng=1`), the slots use jitter.
- Temperature: SCMI. Clock policy: five `_CPC` domains.
- Console on screen: GOP 1920x1080. USB: 10 xHCIs, Blu-ray over USB-BOT.
- Storage: NVMe Lexar 4 TB. Hardware decoder: Linlon V8.

## Ampere Altra

Opzet: 3.0.10, 2026-10-04, tools/meet. Vitals with 4 cores, 128 MiB. Over
the wire against the O6N (bench-serve :9000, vitals :8090).

**Vitals**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| cpu, Msteps/s | 495 | 495 (A95, 04-10) | |
| smp, speedup | 3,97 | 4,01 (A4, 03-10) | |
| burn begin → eind, Msteps/s / max °C | 1979 → 1979 / 40,0 | 1979 → 1979 / 40,0 (A95, 04-10) | |
| membw copy / triad, GB/s | 24,6 / 23,4 | 25,9 / 24,0 (A4, 03-10) | |
| memlat 32 KB / 2 MB / 8 MB, ns | 1,54 / 5,33 / 27,5 | 1,54 / 5,33 / 27,5 (A95, 04-10) | |
| alloc, allocs/s | 550.511 | 557.016 (A4, 03-10) | |
| storm, conn/s (p99 ms) | 9573 (0,92) | 9958 (0,92) (A4, 03-10) | |
| rtt naar de kern p50 / p99, µs | 376 / 3340 | 84 / 158 (A56g, 04-10) | |
| timer 1 ms, overslaap p50 / p99, µs | 309 / 1040 | 236 / 1022 (A4, 03-10) | |
| idle, wekken/s | 21,8 | 21,2 (A95, 04-10) | |
| disk schrijven / lezen / 4 KiB schrijven, MB/s | 1337 / 2196 / 77,6 | 1364 / 2213 / 79,1 (3.0.10, 04-10) | |

Best rtt to the kernel from a second run (in `test=all` 280 / 9218).

**Netwerk**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| De node in, MB/s | 80,4 | 88,8 (3.0.10, 04-10) | 107,7–110,8 |
| De node uit, MB/s | 109,4 | 110,6 (3.0.10, 04-10) | 108,0–110,6 |
| rtt over de draad p50 / p99 / koud, µs | 172 / 226 / 249 | 172 / 226 / 249 (3.0.10, 04-10) | |
| Verbindingscyclus p50, ms | 0,93 | 0,90 (A95, 04-10) | 5,1 |
| Storm, hairpin naar zichzelf, conn/s (p99 ms) | 983 (12,7) | 990–1074 (12,4–14,4) (A56g, 04-10) | |
| Storm over de draad, conn/s (p99 ms) | 4956 (3,54) | 4956 (3,54) (3.0.10, 04-10) | |

**In de node**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| App naar app, MB/s | 5738 | 5762 (3.0.10, 04-10) | |
| rtt warm p50 / p99, µs | 14 / 26 | 14 / 25 (3.0.10, 04-10) | |
| rtt koud p50, µs | 21 | 15 (3.0.10, 04-10) | |

**In rust**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| Kern-cpu, % | 1 | 1 (A56g, 04-10) | |
| Kern-geheugen gebruikt / totaal, MiB | 31,8–31,9 / 224 | 31,3 / 224 (3.0.10, 04-10) | |
| Hop geheugen, MiB / cpu, % | 0,55 / 1 | 0,42 / 1 (3.0.10, 04-10) | |
| Temperatuur, °C | 39,0–40,0 | 38,0 (A56g, 04-10) | |

**Watchdog**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Watchdog test (measurement kernel `wdtest`: Hop stops after 60 s) | **Hop stopped 09:36:35, reset within 22 s; back on the stick kernel A4 at 09:40:14 (the firmware boot takes over 3 min); no post-mortem, the black box does not survive a reset on the Altra; then main plus fix warm from A4 (A58w, 04-10)** | | A58w 04-10 |

**Opslag**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| Raw 1 MiB, write / read, MB/s | | 2466 / 2109 (A57b, 04-10) | |
| hopfs 1 MiB, write / read, MB/s | | 2493 / 2251 (A57b, 04-10) | |
| Sequential 1 GiB, write / read, MB/s | | 2549 / 2459 (A57b, 04-10) | |
| Random 4 KiB, write / read, IOPS | | 141.000 / 24.500 (A57b, 04-10) | |
| Random 4 KiB read, 16 in flight, IOPS | | 186.000 (A57b, 04-10) | |
| Random 4 KiB read by 1 app, per call / bundle 4 / bundle 16, requests/s | | 18.827 / 38.787 / 100.418 (A74f, 04-10) | |
| Same, 4 apps, requests/s | | 54.293 / 99.353 / 249.103 (A74f, 04-10) | |
| Same, 8 apps, requests/s | | 84.995 / 123.567 / 305.060 (A74f, 04-10) | |

Through apps with the bundle (`read_many`, fd11581); two bundles with 1 app
159,369; before the bundle (A56g) 18,918 / 55,645 / 78,944 per call.

Board:
- NIC: **igb on MSI-X via the ITS of its root complex (ITS 7, LPI 8224),
  first interrupt after 4 µs (A56g, 2026-10-04)**; before that deliberately
  polled (the `_PRT` INTx kills the SoC). Kick via SGI 1.
- Watchdog: SBSA, 12.0 s. RNG: **no SMCCC-TRNG (`TRNG_VERSION
  NOT_SUPPORTED`), jitter (2026-10-04)**; before that SMCCC-TRNG or `rndr`.
  Temperature: SMpro.
- Console on screen: GOP, only after a cold boot (after a warm flip
  `HOPOS_FB_NONE`). Storage: NVMe SN770 500 GB.
- Black box: does not survive a reset on the Altra
  (`HOPOS_FLIP_BLACKBOX_EMPTY`).

## Raspberry Pi 5

Opzet: 3.0.10, 2026-10-04, tools/meet. Vitals with 2 cores, 128 MiB. Over
the wire against the O6N (bench-serve :9000, vitals :8090).

**Vitals**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| cpu, Msteps/s | 285 | 285 (Q40, 04-10) | |
| smp, speedup | 2,00 | 2,00 (30-09) | |
| burn begin → eind, Msteps/s / max °C | 571 → 571 / 61,5 | 571 → 571 / 62,0 (3.0.10, 04-10) | |
| membw copy / triad, GB/s | 8,31 / 6,89 | 8,32 / 6,89 (3.0.10, 04-10) | |
| memlat 32 KB / 2 MB / 8 MB, ns | 2,67 / 15,4 / 58,4 | 2,67 / 15,4 / 58,4 (Q90, 04-10) | |
| alloc, allocs/s | 291.390 | 302.439 (P4, 03-10) | |
| storm, conn/s (p99 ms) | 4899 (1,56) | 4911 (1,56) (Q90, 04-10) | |
| rtt naar de kern p50 / p99, µs | 237 / 997 | 165 / 9141 (Q40, 04-10) | |
| timer 1 ms, overslaap p50 / p99, µs | 213 / 1000 | 213 / 215 (Q40, 04-10) | |
| idle, wekken/s | 20,7 | 20,6 (Q40, 04-10) | |

**Netwerk**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| De node in, MB/s | 83,0 | 111,3 (Q40, 04-10) | 57,0–69,6 |
| De node uit, MB/s | 108,7 | 109,5 (Q90, 04-10) | 41,6–50,6 |
| rtt over de draad p50 / p99 / koud, µs | 164 / 205 / 257 | 73 / 118 / 88 (Q40, 04-10) | |
| Verbindingscyclus p50, ms | 0,84 | 0,62 (Q40, 04-10) | 1,1–3,6 |
| Storm over de draad, conn/s (p99 ms) | 4917 (2,90) | 4965 (2,46) (Q90, 04-10) | |
| Storm, hairpin naar zichzelf, conn/s (p99 ms) | 1983 (5,16) | 1983 (5,16) (3.0.10, 04-10) | |

Best from Q40 (in, rtt and cycle): with the O6N busy.

**In de node**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| App naar app, MB/s | 782 | 783–810 (P4, 03-10) | |
| rtt warm p50 / p99, µs | 9 / 24 | 9 / 24 (3.0.10, 04-10) | |
| rtt koud p50, µs | 9 | 9 (Q40, 04-10) | |
| OS core quiet, sleeps/s / NIC irq/s | | ~900 / ~70 (idlestat) | |

v2 OS core quiet: 113 RX rounds/s (irq).

**In rust**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| Kern-cpu, % | 1 | 1 (Q40, 04-10) | |
| Kern-geheugen gebruikt / totaal, MiB | 30,4–30,5 / 127,5 | 29,9 / 127,5 (3.0.10, 04-10) | |
| Hop geheugen, MiB / cpu, % | 0,52 / 1 | 0,39 / 1 (3.0.10, 04-10) | |
| Temperatuur, °C | 55,4–56,0 | 50,5–51,0 (P4, 03-10) | |

**Watchdog**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Watchdog test (measurement kernel `wdtest`: Hop stops after 60 s) | **30 s to the card kernel P4; post-mortem seen (Q41w, 04-10)** | | Q41w 04-10 |

Reading: out is half of in. The GEM transmits with single words from NC,
the same shape as the Radxa's dwmac4 before X2.

Board:
- Warm flip: works, landed three times within 10 s (2026-10-04).
- Watchdog: BCM PM, 12 s.
- Console on screen: seen via a flip on the first card. Since the reflash
  the firmware refuses.

## Raspberry Pi 4

Opzet: 3.0.10, 2026-10-04, tools/meet. Vitals with 2 cores, 128 MiB. Over
the wire against the O6N (bench-serve :9000, vitals :8090).

**Vitals**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| cpu, Msteps/s | 250 | 250 (P40, 04-10) | |
| smp, speedup | 2,00 | 2,00 (30-09) | |
| burn begin → eind, Msteps/s / max °C | 499 → 499 / 57,9 | 499 → 499 / 57,9 (3.0.10, 04-10) | |
| membw copy / triad, GB/s | 3,39 / 2,84 | 3,44 / 2,84 (3.0.10, 04-10) | |
| memlat 32 KB / 2 MB / 8 MB, ns | 3,33 / 22,6 / 110 | 3,33 / 21,7 / 110 (P40, 04-10) | |
| alloc, allocs/s | 93.845 | 100.921 (P2g, 03-10) | |
| storm, conn/s (p99 ms) | 3300 (2,92) | 3300 (2,92) (3.0.10, 04-10) | |
| rtt naar de kern p50 / p99, µs | 448 / 2678 | 322 / 5312 (P40, 04-10) | |
| timer 1 ms, overslaap p50 / p99, µs | 213 / 1027 | 213 / 213 (P40, 04-10) | |
| idle, wekken/s | 20,6 | 20,3 (P40, 04-10) | |

**Netwerk**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| De node in, MB/s | 63,8 | 71,9 (P40, 04-10) | 6,6 |
| De node uit, MB/s | 112,1 | 112,9 (P70, 04-10) | 42,3 |
| rtt over de draad p50 / p99 / koud, µs | 1213 / 1221 / 1225 | 1212–1213 / — / 1290–1300 (H, 30-09) | |
| Verbindingscyclus p50, ms | 2,43 | 2,43 (P40, 04-10) | |
| Storm, hairpin naar zichzelf, conn/s (p99 ms) | 1086 (9,36) | 1942–1989 (5,6–6,8) (P40, 04-10) | |
| Storm over de draad, conn/s (p99 ms) | 1966 (4,95) | 2465 (4,88) (P90, 04-10) | |

Best in from P40: with the O6N busy. v2 in: 2.2.6, polled. Hairpin P40: five
runs.

**In de node**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| App naar app, MB/s | 399 | 399 (3.0.10, 04-10) | |
| rtt warm p50 / p99, µs | 15 / 27 | 15 / 27 (3.0.10, 04-10) | |
| rtt koud p50, µs | 19 | 19 (3.0.10, 04-10) | |

**In rust**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| Kern-cpu, % | 1 | 1 (3.0.10, 04-10) | |
| Kern-geheugen gebruikt / totaal, MiB | 30,4 / 127,5 | 29,9 / 127,5 (3.0.10, 04-10) | |
| Hop geheugen, MiB / cpu, % | 0,52 / 1 | 0,38–0,39 / 1 (3.0.10, 04-10) | |
| Temperatuur, °C | 51,6–52,5 | 51,1–51,6 (P40, 04-10) | |

**Watchdog**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Watchdog test (measurement kernel `wdtest`: Hop stops after 60 s) | **27.5 s to the card kernel P1; post-mortem seen (P41, 04-10)** | | P41 04-10 |

Reading: app to app inside the node is limited by the OS core being
saturated (A72 at 1500 MHz).

Board:
- Warm flip: six times, gen 4 on stamp I.
- Console on screen: 32 bpp.

## Radxa Zero 3W

Opzet: 3.0.10, 2026-10-04, tools/meet. Vitals with 2 cores, 128 MiB. Over
the wire against the O6N (bench-serve :9000, vitals :8090).

**Vitals**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| cpu, Msteps/s | 326 | 326 (3.0.10, 04-10) | |
| smp, speedup | 1,99 | 2,00 (30-09) | |
| burn begin → eind, Msteps/s | 648 → 648 | 648 → 648 (3.0.10, 04-10) | |
| membw copy / triad, GB/s | 4,67 / 3,89 | 4,85 / 3,89 (X40, 04-10) | |
| memlat 32 KB / 2 MB / 8 MB, ns | 1,72 / 92,0 / 145 | 1,54 / 92,4 / 146 (3.0.10, 04-10) | |
| alloc, allocs/s | 99.757 | 99.790 (X90, 04-10) | |
| storm, conn/s (p99 ms) | 2421 (5,21) | 2421 (5,23) (X90, 04-10) | |
| rtt naar de kern p50 / p99, µs | 825 / 5731 | 609 / 681 (X40, 04-10) | |
| timer 1 ms, overslaap p50 / p99, µs | 365 / 854 | 365 / 854 (3.0.10, 04-10) | |
| idle, wekken/s | 21,0 | 20,8 (X40, 04-10) | |

**Netwerk**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| De node in, MB/s | 79,2 | 111,2 (X20, 04-10) | 55,8–56,6 |
| De node uit, MB/s | 108,2 | 112,4 (X20, 04-10) | 98,8–99,6 |
| rtt over de draad p50 / p99 / koud, µs | 182 / 713 / 266 | 178 / 221 / 282 (X90, 04-10) | |
| Verbindingscyclus p50, ms | 1,72 | 0,96 (X40, 04-10) | 4,1 |
| Storm over de draad, conn/s (p99 ms) | 1650 (8,12) | 1650 (8,12) (3.0.10, 04-10) | |
| Storm, hairpin naar zichzelf, conn/s (p99 ms) | 479 (23,8) | 620–757 (16,4–17,3) (X40, 04-10) | |

Best in and out from X20: the serve on the O6N on a core of its own. Cycle
and hairpin X40: with the O6N busy.

**In de node**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| App naar app, MB/s | 390 | 391 (X90, 04-10) | |
| rtt warm p50 / p99, µs | 25 / 62 | 25 / 62 (3.0.10, 04-10) | |
| rtt koud p50, µs | 42 | 40 (X40, 04-10) | |
| OS core quiet, sleeps/s / NIC irq/s | | ~1700–1900 / ~75 (`HOPOS_TICK`) | |

OS core quiet: residents 0.14 %.

**In rust**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| Kern-cpu, % | 1 | 1 (3.0.10, 04-10) | |
| Kern-geheugen gebruikt / totaal, MiB | 31,9–32,0 / 64 | 31,9–32,0 / 64 (3.0.10, 04-10) | |
| Hop geheugen, MiB / cpu, % | 0,45–0,46 / 1 | 0,39 / 1 (3.0.10, 04-10) | |

Temperature: no sensor.

**Watchdog**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Watchdog test (measurement kernel `wdtest`: Hop stops after 60 s) | **~100 s (DW watchdog TOP 89 s) to the card kernel X1; post-mortem seen (X42, 04-10)** | | X42 04-10 |

Reading: when the O6N's serve runs in `system` (on the O6N's OS core), the
Radxa only gets 22–23 MB/s in. Then the sender is the limit, not the Radxa.

Board:
- NIC interrupt on SPI 64.
- Warm flip: five times, gen 3 on stamp I.
- Console on screen: HDMI, without EDID. USB: two DWC3s up.

## LicheeRV Nano

Opzet: 3.0.10, 2026-10-04, tools/meet. Vitals with 1 core (`sharegroup:
system`), 32 MiB. Over the wire against the O6N (bench-serve :9000, vitals
:8090).

**Vitals**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| cpu, Msteps/s | 93,3 | 93,3 (3.0.10, 04-10) | |
| burn begin → eind, Msteps/s / max °C | 93,4 → 93,4 / 56,7 | 93,4 → 93,4 / 56,7 (3.0.10, 04-10) | |
| membw copy / triad, GB/s | 2,29 / 1,78 | 2,29 / 1,78 (R40, 04-10) | |
| memlat 32 KB / 2 MB, ns | 7,12 / 158 | 7,12 / 158 (3.0.10, 04-10) | |
| alloc, allocs/s | 58.237 | 58.357 (3.0.10, 04-10) | |
| storm, conn/s (p99 ms) | 431 (34,0) | 517 (30,4) (R40, 04-10) | |
| rtt naar de kern p50 / p99, µs | 2932 / 7104 | 471 / 1051 (R40, 04-10) | |
| timer 1 ms, overslaap p50 / p99, µs | 552 / 2731 | 63 / 2717 (R40, 04-10) | |
| idle, wekken/s | 45,9 | 42,7 (R14, 03-10) | |

**Netwerk**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| De node in, MB/s | 11,3 | 11,4 (3.0.10, 04-10) | |
| De node uit, MB/s | 9,3 | 9,3 (3.0.10, 04-10) | |
| Verbindingscyclus p50, ms | 6,24 | 6,24 (3.0.10, 04-10) | 16–19 |
| rtt over de draad p50 / p99 / koud, µs | 326 / 504 / 448 | 248 / 374 / 291 (3.0.10, 04-10) | |
| Storm over de draad, conn/s (p99 ms) | 347 (40,4) | 347 (40,4) (3.0.10, 04-10) | |
| Storm, hairpin naar zichzelf, conn/s (p99 ms) | 124 (148) | 124 (148) (3.0.10, 04-10) | |

**In de node**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| App naar app, MB/s | 63,8 | 63,8 (3.0.10, 04-10) | |
| rtt warm p50 / p99, µs | 167 / 331 | 167 / 331 (3.0.10, 04-10) | |
| rtt koud p50, µs | 196 | 196 (3.0.10, 04-10) | |

**In rust**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| Kern-cpu, % | 1 | 1 (3.0.10, 04-10) | |
| Kern-geheugen gebruikt / totaal, MiB | 30,0–30,1 / 40 | 30,0–30,1 / 40 (3.0.10, 04-10) | |
| Hop geheugen, MiB / cpu, % | 0,62 / 1 | 0,62 / 1 (3.0.10, 04-10) | |
| Temperatuur, °C | 56,0 | 53,5 (R40, 04-10) | |

**Watchdog**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Watchdog test (measurement kernel `wdtest`: Hop stops after 60 s) | **41 s to the card kernel R14; no post-mortem (riscv has none yet) (R41, 04-10)** | | R41 04-10 |

Reading: on R40 an app in `system` (core 0, `rings=wb`) gets 60.3 MB/s app
to app and 175 µs rtt; in `hop` on the C906L (`rings=maintained`) 23.4 MB/s
and 3.5 ms. With Hop on the C906L (tag `sharegroup: hop`) the same vitals
gets 62.6 Msteps/s. That is 0.71 of what it gets in `system`, exactly 700
of 1000 MHz. On the C906L memlat already falls out of the cache at 128 KB
(133 ns). Without the lottery (R11) the kernel uses 4–5 % cpu at rest,
against 46–48 % with the lottery (R10).

Board:
- Boot-hart lottery: seen on R3 to R10 (`HOPOS_LOTTERY_SWAPPED`, ac79e91),
  removed in 23e866a (2026-10-03). The kernel stays on the C906B, Hop runs
  on the C906L. After a cold flip `HOPOS_FLIP_SETTLED` after 70 s.
- Cold flip: gen 10 (R10). Warm flip: `flip::WARM` refuses before the
  jump.
- IRQ: kick via MSIP of the CLINT; PLIC context 0 with 102 sources, the
  dwmac on source 31. Under the lottery the NIC was polled.
- Watchdog: canary `HOPOS_WD_CANARY_OK`. Temperature: TEMPSEN
  (`HOPOS_TEMPSEN_UP`).

## Mac mini M4

Opzet: 3.0.10, 2026-10-04, tools/meet. Vitals with 2 cores, 128 MiB. Over
the wire against the O6N (bench-serve :9000, vitals :8090).

**Vitals**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| cpu, Msteps/s | 542 | 542 (3.0.10, 04-10) | |
| smp, speedup | 2,00 | 2,00 (M40, 04-10) | |
| burn begin → eind, Msteps/s | 1084 → 1084 | 1084 → 1084 (M40, 04-10) | |
| membw copy / triad, GB/s | 36,5 / 32,6 | 36,5 / 32,6 (3.0.10, 04-10) | |
| memlat 32 KB / 2 MB / 8 MB, ns | 1,84 / 7,04 / 16,0 | 1,84 / 7,04 / 16,0 (3.0.10, 04-10) | |
| alloc, allocs/s | 485.143 | 485.143 (3.0.10, 04-10) | |
| storm, conn/s (p99 ms) | 9593 (1,05) | 9882 (1,07) (M40, 04-10) | |
| rtt naar de kern p50 / p99, µs | 181 / 445 | 62–76 / 72–135 (M1, 03-10) | |
| timer 1 ms, overslaap p50 / p99, µs | 1023 / 1999 | 1023 / 1999 (3.0.10, 04-10) | |
| idle, wekken/s | 57,1 | 40,4 (3.0.10, 04-10) | |
| disk schrijven / lezen / 4 KiB schrijven, MB/s | 1206 / 1494 / 72,9 | 1206 / 1494 / 72,9 (3.0.10, 04-10) | |

**Netwerk**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| De node in, MB/s | 102,2 | 107,9 (M40, 04-10) | 43,3–46,7 |
| De node uit, MB/s | 80,1 | 117,4 (M40, 04-10) | 47,7–52,3 |
| rtt over de draad p50 / p99 / koud, µs | 200 / 384 / 302 | 116 / 146 / 129 (M40, 04-10) | 50–66 |
| Storm over de draad, conn/s (p99 ms) | 4882 (3,76) | 4951 (4,00) (3.0.10, 04-10) | 6379 (1,9) |
| Verbindingscyclus p50, ms | 0,81 | 0,54 (M40, 04-10) | |
| Storm, hairpin naar zichzelf, conn/s (p99 ms) | 1970 (5,06) | 2449 (5,09) (M40, 04-10) | |

v2 in and out: HTTP PUT and GET.

**In de node**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| App naar app, MB/s | 3188 | 6379–6383 (M33, 01-10) | 764–769 |
| rtt warm p50 / p99, µs | 42 / 54 | 42 / 54 (3.0.10, 04-10) | |
| rtt koud p50, µs | 43 | 43 (3.0.10, 04-10) | |
| App to app, 40 GiB, MB/s | | 2813 (M33, 01-10) | |

Best app to app (M33) on a P-core; *Nu* on two E-cores.

**Watchdog**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Watchdog test (measurement kernel `wdtest`: Hop stops after 60 s) | — (not done: Hop there is still 3.0.0 and the leader knows no jobs) | | |

**Opslag**

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| Raw 1 MiB, write / read, MB/s | | 4952 / 1925 (M23, 01-10) | 5389–5756 / 1824–1971 |
| hopfs 1 MiB, write / read, MB/s | | 5015 / 2026 (M23, 01-10) | 5480–5632 / 1781–1816 |
| Sequential 1 GiB, write / read, MB/s | | 4758 / 1629 (M23, 01-10) | |
| Random 4 KiB, write / read, IOPS | | 140.000 / 11.900 (M23, 01-10) | |
| Kernel to app without a disk (`hole=1`), MB/s | | 1879–1917 (M24, 01-10) | |
| App storage 256 MiB, write / read / 4 KiB write, MB/s | | 1288 / 1675 / 111 (M40, 04-10) | 1598–1657 / 1064–1072 |
| 4 KiB write as an RPC loop, MB/s | | 82 (01-10) | |
| Random 4 KiB read by 1 app, per call / bundle 4 / bundle 16, requests/s | | 8.399 / 21.758 / 55.834 (M70b, 04-10) | |
| Same, 4 apps, requests/s | | 28.843 / 77.829 / 161.485 (M70b, 04-10) | |
| Same, 6 apps, requests/s | | 39.744 / 104.493 / 171.345 (M70b, 04-10) | |
| Same without a disk (`hole=1`), 1 / 2 / 4 / 6 apps per call, requests/s | | 30.986 / 41.701 / 66.089 / 76.577 (M40, 04-10) | |
| Replica (SQLite on HopFS), persist test, ms | | 193 (M33, 01-10) | |

Without a disk last 1511 (M40, 256 MiB). RPC loop: next to a bulk app
15–19. Through apps: with the bundle (`read_many`, fd11581), next to spin;
two bundles with 1 app 80,521, raw 175k with sixteen. Before the bundle
7600 / 14,500 / 25,500 for 1 / 2 / 4 apps. Replica: across a flip.

Reading: on M40 app to app on a P-core gets 4350 MB/s where M33 did 6379,
no faster than on an E-core; Hop there is still 3.0.0. Raw write and app
storage write are below the v2 bar.

Still to measure: *In rust* (after a Hop with telemetry), the watchdog
test.

Board:
- Hop: built in, carried warm across flips (`HOPOS_HOP_RESUMED`).
- Warm flip: **gen 4 on M40, 2026-10-04 (3 residents, 18 NAT flows, Hop
  along)**; gen 1 to 8 on 2026-10-01, adoption 3 of 3. Not cold (no
  CPU_OFF).
- NIC: tg3 BCM57766 polled (no MSI). RNG: jitter.
- Storage: ANS NVMe 414 GB, asynchronous with read-ahead since M22.
- Console on 5555: `hopos.replay=45`.
- Installed image: D4b. A new image with main is needed.

## QEMU virt

Opzet: 2026-09-29, `sh tools/qemu-test-bench.sh`, TCG on an M-series Mac,
4 cores, virtio-net via slirp, virtio-blk with a 64 MiB disk.

| Meting | Nu | Hoogste v3 | v2 |
| --- | --- | --- | --- |
| Into the node, MB/s | 122,3 | 122,3 (29-09) | |
| Out of the node, MB/s | 71,4 | 71,4 (29-09) | |
| rtt host to app p50 / p99, µs | 1180 / 3472 | 1180 / 3472 (29-09) | |
| Connection cycle p50, ms | 1,44 | 1,44 (29-09) | |
| Storm, conn/s (p99 ms) | 638 (2,7) | 638 (2,7) (29-09) | |
| App to app inside the node, MB/s | 62,9 | 62,9 (dbc522f) | |
| rtt inside the node warm p50 / p99, µs | 115 / 340 | 115 / 340 (dbc522f) | |
| rtt inside the node cold p50, µs | 375 | 375 (dbc522f) | |
| OS core quiet, wakeups/s / empty RX rounds/s | ~820 / ~94 | ~820 / ~94 (29-09) | |
| Raw 1 MiB, write / read, MB/s | 1260,9 / 1221,4 | 1260,9 / 1221,4 (29-09) | |
| hopfs 1 MiB, write / read, MB/s | 1187,0 / 1207,2 | 1187,0 / 1207,2 (29-09) | |
| Sequential 32 MiB, write / read, MB/s | 1293,0 / 1238,2 | 1293,0 / 1238,2 (29-09) | |
| Random 4 KiB, write / read, IOPS | 32.051 / 32.123 | 32.051 / 32.123 (29-09) | |
| Multicast between two apps, s to green | 4 | 4 (29-09) | |

App to app with 16 MiB; OS core quiet: residents 0.3 %.

Reading: QEMU proves the chain, not a ceiling. Slirp terminates TCP on the
host, TCG computes in software, and the vCPUs share the host.

Board: console via `ramfb`, USB via `qemu-xhci`.

## What is measured nowhere

- The echo rtt during a kernel flip (v2, Pi 4: 4.3 to 8.0 s).
  `FLIP=1 tools/soak.sh` counts flips and survival, not the rtt.
- UDP over the wire. Vitals and bench have no UDP client.
- Energy: that needs a meter. Watchdog recovery is listed per board under
  *Watchdog* since 2026-10-04 (measurement kernel `wdtest`, the time from
  the console).
