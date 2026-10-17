# vitals: the board doctor

One HopOS app that measures and benchmarks the vital functions of a node. For
bring-up and diagnosis of new boards: place vitals, open the page, press
**Run all**, and compare the report (button **Copy report**) with a healthy
board.

The tests measure the same as vitals in v2, with the same workload and the
same arithmetic, so the v3 numbers sit next to those of v2
(`docs/measurements.md`, the Vitals table per board).

| test | measures | tells you about | marker |
|---|---|---|---|
| idle *(passive, always running)* | idle %, wakes/s, cost per wake from `CTRL_IDLE` (0x48) and `CTRL_WAKES` (0x108) over the last 60 s | the app core's sleep: WFE with the event stream, the yield on a shared core | `HOPOS_VITALS_IDLE` |
| cpu | LCG steps/s on one core (with a yield per burst) | the hart's clock | `HOPOS_VITALS_CPU` |
| smp | first the shared heap (each core writes a block, core 0 reads it), then 64M steps serial versus split | do all harts really come up, in the same cage | `HOPOS_VITALS_SMP` |
| burn | sustained load on all cores, the rate and `CTRL_TEMP` per second | thermal throttling, DVFS clock-down, the heatsink | `HOPOS_VITALS_BURN`, every 10 s `HOPOS_VITALS_BURN_TICK` |
| membw | STREAM-like copy and triad | the DRAM controller | `HOPOS_VITALS_MEMBW` |
| memlat | pointer chase over 32 KB to 8 MB | the cache hierarchy, DRAM latency | `HOPOS_VITALS_MEMLAT` |
| alloc | allocation rate with ~2 MB live, refused allocations, largest free chunk | the app's heap under churn | `HOPOS_VITALS_ALLOC` |
| rx | download from an external source (plain http, with Content-Length) | throughput of app stack, switch, NAT, NIC, internet | `HOPOS_VITALS_RX` |
| tx | client-driven: `curl -o /dev/null http://node:8090/blob?mb=64` | the send side of the same path | `HOPOS_VITALS_TX` |
| up | client-driven: a series of 1 MiB PUTs to `/sink` (`perf.sh`) | the receive side of the same path | `HOPOS_VITALS_UPLOAD` (the first and every sixteenth) |
| disk | write and read through the system API (1 MiB per call), 4 KiB writes, bare `stat` calls | LAN path versus hopfs and disk: everything above the stat floor is disk | `HOPOS_VITALS_DISK` |
| storm | many short connections (dial, `GET /ping`, close) to its own port | connection setup under pressure | `HOPOS_VITALS_STORM` |
| rtt | bare TCP handshakes to the gateway (10.100.0.1, the system port 10100) | the floor of the internal path | `HOPOS_VITALS_RTT` |
| timer | oversleep at 1, 5 and 20 ms, forty times each | timer source, event stream, executor | `HOPOS_VITALS_TIMER` |

Each marker follows the line `vitals: <test> done in <ms> ms` and carries the
measurements as `key=value` (the same names as in the JSON). A failure is
`HOPOS_VITALS_FAIL test=<name>`, a skip `HOPOS_VITALS_SKIP test=<name>`, and
`test=all` ends with `HOPOS_VITALS_ALL failed=N skipped=N`. The start:
`HOPOS_VITALS_UP port=<n>`. A test that finds no room in the executor says
`HOPOS_VITALS_SPAWN`; an accept error is `HOPOS_VITALS_ACCEPT`.

## What was dropped, and why

- **sqlite**: there is no SQLite in the Rust slot (no C, no VFS over the
  system API). disk still measures a database's write pattern: the 256
  writes of 4 KiB.
- **gc**: Rust has no collector, so no cycles and no pauses to measure. In
  its place came **alloc**: the same pattern (blocks of 32 KiB, a live set of
  ~2 MB) against applib's allocator, with what does count there: the rate,
  the refusals and the fragmentation (the largest free chunk afterwards).
- **ramp** (how fast the clock scales up under sudden load): not in this
  round. The per-second rate of burn and the cpu test cover the clock; the
  ramp in 5 ms windows comes back if a board asks for it.
- **syscall** (the stat floor next to a keep-alive GET to the agent): the
  stat floor is in disk (`floor_p50`, `floor_p99`); in v3 the agent is no
  longer a gateway but a resident in slot 1, and app to app is measured by
  `apps/bench` (`BENCH=ping`).
- **push** (upload to another app on the node): that is `apps/bench` with
  `BENCH=push`.
- **HOP_ADDR and HOP_KEY**: in Go the temperature came from Hop's agent API.
  In v3 the kernel puts it on the control page every second (`CTRL_TEMP`), so
  vitals no longer talks to Hop and needs no key. A board without a sensor
  gives 0, and then burn says there is no temperature.
- **VITALS_NOCTRL**: the Go app read the control page itself, and on the M4
  that read faulted (2026-08-31). In v3 applib already reads the control page
  for its own heartbeat (`Ctrl::get`, with the pull before it); an app that
  comes up can read it.

## The shape of v3

- **One test at a time.** `/api/run` claims a field in the table and starts
  the test as its own task; a second start gets 409. Everything that touches
  the table runs on the executor of core 0 (handbook §1.1, a readable table
  without a borrow across an `.await`).
- **More cores.** With `"cpu_shares":2048` (two cores) or more, smp and burn
  run over all of the app's cores: the work goes to the other core's executor
  with `smp::spawn_on`, the result comes back as atomics. With one core smp
  skips itself (`skipped`), like Go.
- **The triad computes on `u64`.** The apps' target is
  `aarch64-unknown-none-softfloat`: an `f64` addition is a library call there,
  and then the triad measures softfloat instead of memory. The bytes per
  element stay 24, so the number sits next to Go's.
- **memlat keeps Go's arithmetic**: a set labelled `size` has `size / 8`
  words of 4 bytes. That way the numbers sit next to the Go column.
- **storm** goes to its own address and its own port by default; the app's
  stack turns that around internally (loopback), so it measures the app stack
  and its server, client and server in one slot. For the hairpin through
  switch and NAT: `?addr=NODE_IP:8090` or `VITALS_STORM_ADDR`. Clean
  measuring is storming `/ping` from outside (`tools/netmeter`).
- **Memory**: every buffer comes from the heap with `try_reserve`; a
  partition that is too small gives an error in the report, not a panic.
  128 MB (`"memory_limit":134217728`) gives the memory tests room to measure
  beyond the caches; the bandwidth buffer is an eighth of the partition, at
  most 16 MB.
- **Graceful stop.** The acceptor runs until the kernel asks the app to stop
  (`HOPOS_APP_STOP`); applib's main shell then closes every connection and
  exits.

## Endpoints

```
GET  /                  the page: Run all, a button per test, Copy report
GET  /api/state         everything as JSON: node, idle, temperature, results
GET  /api/run?test=X    start a test (or test=all); 409 if one is running
GET  /ping              "pong": the target of storm
GET  /blob?mb=N         N MB to the client (default 32), the result as "tx"
PUT  /sink              a body of up to 1 MiB (leanhttp's limit), the series as "up"
GET  /health            "ok"
```

The parameters of `/api/run` (also with `test=all`, then for every test that
knows them):

| param | test | default | limits |
|---|---|---|---|
| `secs` | cpu, burn, alloc | 5, 120, 3 | 1..60, 10..600, 1..30 |
| `mb` | rx, disk | 32, 64 | 1..1024 |
| `n` | rx (connections), storm (workers), rtt (handshakes) | 1, 8, 100 | 1..8, 1..16, 10..2000 |
| `reqs` | storm | 200 | 10..10000 |
| `kb` | disk (chunk) | 1024 | 4..1024 |
| `url` | rx | `VITALS_RX_URL` | plain http |
| `addr` | storm, rtt | own port, 10.100.0.1:10100 | `ip:port` |
| `path` | disk | `/vitals-disk.bin` | a path in its own view, or a mount |
| `rand` | disk | 0 (off) | 0..1048576: random 4 KiB reads instead of the sequential test; with `secs` (1..600) it runs that long |
| `hole=1` | disk | off | reads holes: the kernel-to-app transport without disk |
| `depth` | disk with `rand` | 1 | 1..16: that many reads per call (a bundle, `read_many`); 1 is the plain read |
| `bundles` | disk with `rand` | 1 | 1..2: that many bundles at once, each over its own connection |

The three throughput measurements at once, from a client with curl and
python3:

```sh
apps/vitals/perf.sh <node[:8090]> [mb]     # download (/blob), upload (/sink), disk
```

## Config (jobspec env, everything has a default)

| env | default | meaning |
|---|---|---|
| `ER_PORT_HTTP` | `8080` | the published port (Hop sets it via `ports:{http:...}`) |
| `VITALS_RX_URL` | `http://cachefly.cachefly.net/100mb.test` | source of rx (plain http, with Content-Length; the node needs DNS, `DNS` in the env) |
| `VITALS_STORM_ADDR` | *(empty)* | target of storm as `ip:port`; empty is its own port |

## Placing

There is no `hop` CLI yet: the jobspec goes to Hop's leader with `curl`
(port 9080 on the node's address), as with welcome and bench. The ELF is on
an HTTP server the node can reach:

```sh
curl -X POST -H 'Content-Type: application/json' \
  -d '{"name":"vitals","driver":"hop",
       "artifacts":[{"url":"http://192.168.1.208:8000/vitals.elf"}],
       "memory_limit":134217728,"cpu_shares":2048,"ports":{"http":8090}}' \
  http://NODE:9080/v1/jobs
```

On the console: `HOP_JOB_PLACED`, the publication of port 8090
(`HOPOS_SLOT_PUBLISH`) and `vitals ...: serving http on ... HOPOS_VITALS_UP
port=8090`. Then `http://NODE:8090/` in the browser, or from the laptop:

```sh
curl 'http://NODE:8090/api/run?test=all'
curl -s http://NODE:8090/api/state | python3 -m json.tool
```

Remove: `curl -X DELETE http://NODE:9080/v1/jobs/vitals` (a graceful stop:
`HOPOS_APP_STOP`, `HOPOS_APP_SHUTDOWN`). Permanent: the same JSON on one
line after `hopos.init[]=` in `hopos.cfg`.

No share group: benchmark numbers should come from a dedicated core (the
timer and idle numbers on a shared core are interesting though; that is a
separate placement with `"tags":{"sharegroup":"..."}`).

## Build

```sh
cargo build --release --target aarch64-unknown-none-softfloat -p vitals
rust-objcopy --strip-debug target/aarch64-unknown-none-softfloat/release/vitals vitals.elf
cargo test -p vitals                                          # the arithmetic on the host
cargo clippy -p vitals --all-targets -- -D warnings
```

Without debug info (the symbols stay: placement reads them). One ELF for
every ARM64 board.

## Caveats

- Under QEMU TCG, WFE is a no-op: idle numbers are hardware numbers.
- With more than one core the idle percentage is `n/a`: `CTRL_IDLE` is the
  sum of every core's sleep, not a percentage of one wall clock. The wake
  rate stays the measured sum over the cores.
- storm storms through its own slot (client and server), so those numbers
  are a lower bound.
- rtt knocks on the kernel's system port; each handshake is a system
  connection for the kernel that closes right away, and the kernel says so
  with a line `system: slot N done` per handshake. A hundred handshakes are a
  hundred lines on the console; `?addr=` picks a quieter target.

On QEMU virt (2026-09-30, TCG, `cpu_shares` 2048, 64 MB, Hop in slot 1)
`test=all` ran green: twelve tests in 34 s, smp with a speedup of 1.93 on two
cores, disk 44.0 / 38.7 MB/s with a stat floor of 1191 µs. QEMU proves the
chain, not the ceiling.

## Layout

- `src/main.rs`: the start, the acceptor, a fixed pool of eight workers and
  the routes (the same shape as welcome).
- `src/run.rs`: the tests, the one-at-a-time claim, the table and the state
  JSON.
- `src/report.rs`: a report, pure: measurements, lines in a pre-reserved
  buffer, JSON and the marker tail.
- `src/idle.rs`: the sampler and the window.
- `src/cpu.rs`, `src/mem.rs`, `src/net.rs`, `src/disk.rs`: the tests.
- `src/page.rs`: the page, one static document.
- `perf.sh`: the client side of download, upload and disk.
