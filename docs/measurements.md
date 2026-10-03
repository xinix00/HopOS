# Metingen: de lat van v2, en v3 ernaast

PORT.md §1: `measurements.md` is de lat, een Rust-HOP mag op geen gemeten
getal langzamer zijn. Deze pagina zet de lat van v2 (september 2026) per
meting naast een lege kolom per board voor v3, met het commando dat het v3-getal oplevert en de
marker waar het op de console staat. Een devicedag vult de kolommen.

MB/s is decimaal (1.000.000 bytes per seconde), zoals in de v2-tabellen.
Let op: de netmeter van v2 rekende MiB/s; `tools/netmeter` rekent
decimaal, dus zijn getallen liggen direct naast deze tabellen. Een cel is
een spreiding over drie runs (`--repeat 3` geeft `NETMETER_SPREAD`), nooit
alleen de beste.

## Het gereedschap

| Wat | Commando | Marker |
| --- | --- | --- |
| De bench in een slot (server) | `curl -X POST -d '{"name":"bench","driver":"hop","artifacts":[{"url":"http://LAPTOP:8000/bench.elf"}],"memory_limit":67108864,"ports":{"http":80}}' http://NODE:9080/v1/jobs` | `HOPOS_BENCH_UP role=serve port=80` |
| Doorvoer, rtt, storm, UDP van de host | `cargo run --release -p netmeter -- NODE:80 --repeat 3 --json > run.json` | `NETMETER phase=...`, `NETMETER_SPREAD` |
| App naar app in de node | job met `"env":{"BENCH":"pull","BENCH_PEER":"10.100.0.3:80","BENCH_BYTES":"419430400"}` (of `ping`, `push`) | `HOPOS_BENCH_PULL`, `HOPOS_BENCH_RTT`, `HOPOS_BENCH_COLD` |
| Last op een app-core | job met `"env":{"BURN":"1"}` (`BURN_WORK`, `BURN_REST` in s) | `HOPOS_BENCH_BURN` |
| Geheugen onder druk | job met `"env":{"THRASH":"1"}` | `HOPOS_BENCH_THRASH` |
| Multicast tussen twee apps (mDNS-groep 224.0.0.251:5353, door de switch) | eerst een job met `"env":{"MCAST":"listen"}` (joint de groep), dan een met `"env":{"MCAST":"send"}` (een probe per seconde); op QEMU groen op 29-09 in 4 s | `HOPOS_BENCH_UP role=mcast-listen`, `HOPOS_BENCH_MCAST recv=N` |
| De idle-meetlat van de OS-core | `hopos.idlestat=1` in `hopos.cfg` of de cmdline (een getal N: elke N s) | `HOPOS_IDLESTAT` |
| Schijf rauw en door hopfs | `hopos.nvmebench=1` (een meet-boot: de schijf blijft bij de bench) | `HOPOS_NVMEBENCH`, `_SEQ`, `_RAND` |
| De keten op QEMU | `sh tools/qemu-test-bench.sh` | `bench-keten groen` |
| Uren lang plaatsen en stoppen | `sh tools/soak.sh NODE` (`FLIP=1` voor de flip erbij) | `SOAK` per ronde, het rapport aan het eind |

De bench is `apps/bench` (de protocoltabel staat in `apps/bench/src/proto.rs`),
de host-kant `tools/netmeter`, de kern-kant `hopos/src/bench.rs`.

## Vitals

De board-dokter (`apps/vitals`, README daar): één app, de prestatietests van
vitals, per test een marker met de meetwaarden als `key=value`. Eerst
plaatsen, dan per test (of `test=all`) starten; het getal staat op de
console en in `GET /api/state`. Een cel is de marker-staart van één run op
dat board, met de datum.

| Wat | Commando | Marker | v3 Pi 5 | v3 Pi 4 | v3 Radxa | v3 O6N | v3 M4 | v3 LicheeRV | v3 Altra |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Plaatsen | `curl -X POST -d '{"name":"vitals","driver":"hop","artifacts":[{"url":"http://192.168.1.208:8000/vitals.elf"}],"memory_limit":134217728,"cpu_shares":2048,"ports":{"http":8090}}' http://NODE:9080/v1/jobs` | `HOPOS_VITALS_UP port=8090` | `port=8090`, 1 core, 128 MiB, slot 3 (P4, 03-10); `port=8090`, 2 cores (30-09) | `port=8090`, 1 core, 128 MiB, slot 3 (`vitals-arm64.elf` van main, P2g, 03-10); `port=8090`, 2 core(s), slot 2 (30-09) | `port=8090`, 1 core, 128 MiB, slot 3 (X1, 03-10); `HOPOS_VITALS_UP port=8090`, 2 cores, slot 2 (30-09) | `port=8090`, 1 core, 128 MiB, slot 3; `vitals4` op :8091 met 4 cores (cpu_shares 4096, 256 MiB), slot 3 (O2, 03-10); `HOPOS_VITALS_UP port=8090` (10 cores, 512 MiB, volume `/data`, 30-09) | `port=8090`, 1 core, 64 MiB, slot 4 op core 4 (E-core, mpidr 0x80000003), naast spin en spin-tunnel van Derek (M1, 03-10) | system: `port=8090`, 1 core, 32 MiB, slot 3, zonder tag op core 0 (C906B) naast de kern en welcome (`HOPOS_PLACE_SYSTEM`); hop: `port=8090`, 1 core, 32 MiB, slot 3, tag `{"sharegroup":"hop"}` op core 1 (C906L) bij Hop; vitals 3.0.5, alleen welcome erbij (R14, 03-10 avond); `port=8090`, 1 core, 32 MiB, slot 5, zonder tag in `system` op core 0 naast de kern en stulp-plugins (stulp, stulp-plugins en cloudflared-lean van Derek draaiden mee) (R14, 03-10); `port=8090`, 1 core, 64 MiB (`vitals-riscv64.elf`, `memory_limit` 67108864; eerder, 02-10 op 3.0.3, weigerde de volle pool hem) (R3, 03-10) | `port=8090`, 1 core, 128 MiB, slot 3; `vitals4` op :8091 met 4 cores (cpu_shares 4096, 256 MiB), slot 3 (A4, 03-10) |
| Alles achter elkaar | `curl 'http://NODE:8090/api/run?test=all'` | `HOPOS_VITALS_ALL failed= skipped=` | `failed=1 skipped=2` in 29 s met `secs=5` (rx faalt, smp met één core, disk zonder schijf) (P4, 03-10); `failed=1 skipped=1` in 133 s, drie keer (rx zonder DNS, disk zonder schijf) (30-09) | `failed=1 skipped=2` in 29 s met `secs=5` (rx faalt, smp met één core, disk zonder schijf) (P2g, 03-10); `failed=0 skipped=1` in 133 s (disk; met `mb=16&url=.../rx.bin`) (30-09) | `failed=1 skipped=2` in 30 s met `secs=5` (rx faalt, smp met één core, disk zonder schijf) (X1, 03-10); `failed=0 skipped=1` (disk), 135 s, drie keer (30-09) | 1 core: `failed=1 skipped=1` in 29 s; 4 cores: `failed=1 skipped=0` in 24 s, met `secs=5` (rx faalt) (O2, 03-10); `HOPOS_VITALS_ALL failed=0 skipped=0`, 134 tot 136 s, drie keer (30-09) |   | system: `failed=1 skipped=2` in 30 s, twee keer; hop: `failed=1 skipped=2` in 30 tot 31 s, twee keer, met `secs=5` (rx faalt, smp met één core, disk zonder schijf) (R14, 03-10 avond); `failed=1 skipped=2` in 30 s met `secs=5` (rx faalt, smp met één core, disk zonder schijf) (R14, 03-10); rx en disk leeg (geen peer, geen schijf); `failed=` en `skipped=` niet genoteerd (R3, 03-10) | 1 core: `failed=1 skipped=1` in 29 s; 4 cores: `failed=1 skipped=0` in 24 s, met `secs=5` (rx faalt) (A4, 03-10) |
| idle (passief) | `curl 'http://NODE:8090/api/run?test=idle'` | `HOPOS_VITALS_IDLE span= wakes_per_s= idle_pct= wake_cost=` | `span=58.0 wakes_per_s=1060` (2 cores, idle_pct n.v.t.); 1 core: `wakes_per_s=1087 idle_pct=99.44 wake_cost=5.14` (30-09) | na de idle-fix (262ea1f, stempel I, nieuwe applib): `wakes_per_s=28.9` (2 cores), met cf21ac5 `wakes_per_s=21.6` en `dvfs: clock 600 MHz (quiet)`; ervoor `span=58.0 wakes_per_s=1011 idle_pct=99.0 wake_cost=10.3` (1 core; 2 cores: `wakes_per_s=1011`, idle_pct n/a) (30-09) | 1 core: `span=58.0 wakes_per_s=3003 idle_pct=97.9 wake_cost=7.11`; 2 cores: `span=58.0 wakes_per_s=2885` (idle_pct n/a) (30-09) | na de idle-fix (nieuwe applib, kern nog H): `wakes_per_s=51.5` (4 cores); ervoor stil: `wakes_per_s=` 3296 tot 3358 (som over 10 cores), idle_pct n/a (SMP) (30-09) | 1 core: `span=6.0 wakes_per_s=44.8 idle_pct=99.96 wake_cost=8.48` (de eerste 6 s, vóór de tests) (M1, 03-10) | system: `span=58.0 wakes_per_s=42.7 idle_pct=99.3 wake_cost=164` (stil, 70 s na de brand; in `all` `wakes_per_s=46.0..56.2 idle_pct=98.9..99.8 wake_cost=46.1..189`); hop: `span=58.0 wakes_per_s=60.9..89.3 idle_pct=96.5..97.7 wake_cost=384..386` (stil, de C906L gedeeld met Hop; in `all` `wakes_per_s=41.2..63.8 idle_pct=97.5..99.8`) (R14, 03-10 avond); 1 core: `span=22 wakes_per_s=56 idle_pct=77 wake_cost=4068` (R3, 03-10) |  |
| cpu | `curl 'http://NODE:8090/api/run?test=cpu'` | `HOPOS_VITALS_CPU rate= burst=` | `rate=284 burst=1843` (P4, 03-10); `rate=286 burst=1836` (285,48 tot 285,52, 30-09) | `rate=248 burst=2111` (P2g, 03-10); `rate=250 burst=2100` (30-09) | `rate=136 burst=3863` (X1, 03-10); `rate=136 burst=3858` (30-09) | 1 core `rate=298 burst=1756`; 4 cores `rate=300 burst=1750` (O2, 03-10); `rate=855..856 burst=612..613` (30-09) | `rate=538 burst=975` (E-core) (M1, 03-10) | system: `rate=87.7 burst=5975..5977` (in `all` 87.8..90.2); hop: `rate=62.6 burst=8375..8381` (in `all` 62.6..65.2), 0,71 van system, de 700 van 1000 MHz (R14, 03-10 avond); `rate=62.3 burst=8410` (core 0 gedeeld met de kern en stulp-plugins) (R14, 03-10); `rate=93.8 burst=5591` (895 bursts); de deeltoets, `cpu` 5 s naast welcome in één sharegroup op de C906B: `rate=93.6 burst=5600`, welcome intussen 200 in 12 tot 38 ms (zes metingen) (R3, 03-10) | 1 core `rate=494 burst=1061`; 4 cores `rate=495 burst=1060` (A4, 03-10) |
| smp | `curl 'http://NODE:8090/api/run?test=smp'` | `HOPOS_VITALS_SMP cores= serial= parallel= speedup=` | `cores=2 serial=235 parallel=118 speedup=2.00` (drie keer, 30-09) | `cores=2 serial=269 parallel=134 speedup=2.00` (30-09) | `cores=2 serial=494 parallel=247 speedup=2.00` (30-09) | `cores=4 serial=224 parallel=56.1 speedup=3.99` (vitals4) (O2, 03-10); `cores=10 serial=77.7..77.9 parallel=22.8 speedup=3.42` (30-09) |   | system en hop: `cores=1`, overgeslagen (één core) (R14, 03-10 avond); `cores=1` (één app-hart) (R3, 03-10) | `cores=4 serial=136 parallel=34.0 speedup=4.01` (vitals4) (A4, 03-10) |
| burn (120 s) | `curl 'http://NODE:8090/api/run?test=burn'` | `HOPOS_VITALS_BURN cores= rate_start= rate_end= degradation= temp_max=`, elke 10 s `HOPOS_VITALS_BURN_TICK` | `cores=1 rate_start=285 rate_end=285 degradation=0.00 temp_max=56.0` (15 s, P4, 03-10); `cores=2 rate_start=571 rate_end=571 degradation=-0.01`, geen `temp_max` (CTRL_TEMP 0; de kern zag 57,0 tot 66,9 °C op 1500 MHz) (30-09) | `cores=1 rate_start=249 rate_end=249 degradation=0.00 temp_max=54.5` (15 s, P2g, 03-10); `cores=2 rate_start=499 rate_end=499 degradation=0.02`, geen temp_max (CTRL_TEMP 0; kern 1500 MHz, max 67,1 °C) (30-09) | `cores=1 rate_start=136 rate_end=136 degradation=0.00`, geen temp_max (geen sensor) (15 s, X1, 03-10); `cores=2 rate_start=272 rate_end=272 degradation=-0.01`, geen temp_max (geen sensor) (30-09) | 1 core `rate_start=299 rate_end=299 degradation=0.00 temp_max=41` (15 s); 4 cores `rate_start=1198 rate_end=1198 degradation=0.00 temp_max=42` (10 s) (O2, 03-10); `cores=10 rate_start=5883 rate_end=5883 degradation=0.00`, geen temp_max (CTRL_TEMP 0); kern 2600 MHz, 48 tot 52 °C (30-09) |   | system: `cores=1 rate_start=88.0..88.2 rate_end=87.9..88.2 degradation=-0.29..0.34 temp_max=57.4` (30 s; in `all`, 15 s: 88.4..90.7); hop: `cores=1 rate_start=62.7..62.8 rate_end=62.6 degradation=0.06..0.32 temp_max=56.3..56.7` (30 s; in `all`, 15 s: 62.6..65.2) (R14, 03-10 avond); `cores=1 rate_start=87.5 rate_end=87.7 degradation=-0.17 temp_max=56.7` (15 s) (R14, 03-10); `cores=1 rate_start=93.8 rate_end=93.8 degradation=0.0` (R3, 03-10) | 1 core `rate_start=494 rate_end=494 degradation=0.00 temp_max=46` (15 s); 4 cores `rate_start=1979 rate_end=1979 degradation=0.00 temp_max=46` (10 s) (A4, 03-10) |
| membw | `curl 'http://NODE:8090/api/run?test=membw'` | `HOPOS_VITALS_MEMBW buffer= copy= triad=` | `buffer=15 copy=8.27 triad=6.88` (P4, 03-10); `buffer=15 copy=8.74 triad=6.89` (copy 8,72 tot 8,75, 30-09) | `buffer=15 copy=3.42 triad=2.80` (P2g, 03-10); `buffer=15 copy=3.42 triad=2.82` (30-09) | `buffer=15 copy=3.13 triad=2.25` (X1, 03-10); `buffer=15 copy=3.03 triad=2.21` (30-09) | 1 core `buffer=15 copy=16.7 triad=11.4`; 4 cores `buffer=16 copy=15.8 triad=10.8` (GB/s) (O2, 03-10); `buffer=16 copy=33.9..35.0 triad=25.9..26.2` (GB/s) (30-09) |   | system: `buffer=3 copy=2.15 triad=1.67`; hop: `buffer=3 copy=1.53..1.63 triad=1.23..1.28` (GB/s) (R14, 03-10 avond); `buffer=3 copy=2.17 triad=1.66` (GB/s) (R14, 03-10); `buffer=7 copy=2.25 triad=1.77` (GB/s) (R3, 03-10) | 1 core `buffer=15 copy=25.9 triad=24.0`; 4 cores `buffer=16 copy=26.0 triad=23.7` (GB/s) (A4, 03-10) |
| memlat | `curl 'http://NODE:8090/api/run?test=memlat'` | `HOPOS_VITALS_MEMLAT ns_32k= ... ns_8m= furthest=` | `ns_32k=2.67 ns_128k=2.67 ns_512k=7.30 ns_2m=16.5 ns_8m=58.9 furthest=58.9` (P4, 03-10); `ns_32k=2.67 ns_128k=2.67 ns_512k=7.30 ns_2m=15.7 ns_8m=58.5 furthest=58.5` (8m 58,3 tot 58,5, 30-09) | `ns_32k=3.33 ns_128k=9.38 ns_512k=15.6 ns_2m=28.3 ns_8m=111 furthest=111` (P2g, 03-10); `ns_32k=3.33 ns_128k=9.43 ns_512k=15.6 ns_2m=21.5 ns_8m=108 furthest=108` (30-09) | `ns_32k=3.68 ns_128k=25.5 ns_512k=43.1 ns_2m=125 ns_8m=183 furthest=183` (X1, 03-10); `ns_32k=3.68 ns_128k=25.5 ns_512k=43.1 ns_2m=130 ns_8m=186 furthest=186` (30-09) | 1 core `ns_32k=2.76 ns_128k=21.3 ns_512k=34.5 ns_2m=38.9 ns_8m=38.9`; 4 cores `ns_32k=2.76 ns_128k=21.4 ns_512k=34.5 ns_2m=37.7 ns_8m=38.6` (O2, 03-10); `ns_32k=1.54 ns_128k=1.54 ns_512k=3.02 ns_2m=19.6 ns_8m=31.0..31.1` (30-09) |   | system: `ns_32k=7.27..7.31 ns_128k=7.56..7.68 ns_512k=134..138 ns_2m=161..163 furthest=161..163`; hop: `ns_32k=10.2 ns_128k=132..133 ns_512k=162 ns_2m=169..171 furthest=169..171` (op de C906L valt 128 KB al uit de cache) (R14, 03-10 avond); `ns_32k=7.68 ns_128k=7.79 ns_512k=144 ns_2m=173 furthest=173` (geen 8m bij 32 MiB) (R14, 03-10); `ns_32k=7.1 ns_128k=7.2 ns_512k=132.6 ns_2m=163.1`; ns_8m niet genoteerd (R3, 03-10) | 1 core `ns_32k=1.54 ns_128k=1.54 ns_512k=4.58 ns_2m=5.46 ns_8m=27.6 furthest=27.6`; 4 cores `ns_2m=5.34 ns_8m=27.5` (A4, 03-10) |
| alloc | `curl 'http://NODE:8090/api/run?test=alloc'` | `HOPOS_VITALS_ALLOC alloc= allocs_per_s= failed= heap_peak=` | `alloc=9910 allocs_per_s=302439 failed=0 heap_peak=48889` (P4, 03-10); `alloc=10302 allocs_per_s=314401 failed=0 heap_peak=49414` (10219 tot 10302, 30-09) | `alloc=3307 allocs_per_s=100921 failed=0 heap_peak=48889` (P2g, 03-10); `alloc=3384 allocs_per_s=103265 failed=0 heap_peak=50265` (30-09) | `alloc=2881 allocs_per_s=87930 failed=0 heap_peak=48889` (X1, 03-10); `alloc=2763 allocs_per_s=84330 failed=0 heap_peak=49410` (30-09) | 1 core `alloc=6274 allocs_per_s=191466 failed=0 heap_peak=48889`; 4 cores `alloc=6389 allocs_per_s=194990 failed=0 heap_peak=51193` (O2, 03-10); `alloc=19289..19318 allocs_per_s=588655..589525 failed=0 heap_peak=54286..54826` (30-09) |   | system: `alloc=1693..1846 allocs_per_s=51664..56344 failed=0 heap_peak=12190..12506`; hop: `alloc=1119..1350 allocs_per_s=34163..41209 failed=0 heap_peak=11991..12506` (R14, 03-10 avond); `alloc=1785 allocs_per_s=54465 failed=0 heap_peak=12007` (R14, 03-10); `alloc=1918 allocs_per_s=58526 failed=0 heap_peak=24307` (heap 62652 KB) (R3, 03-10) | 1 core `alloc=18252 allocs_per_s=557016 failed=0 heap_peak=48889`; 4 cores `alloc=18200 allocs_per_s=555405 failed=0` (A4, 03-10) |
| rx | `curl 'http://NODE:8090/api/run?test=rx&url=http://192.168.1.208:8000/big.bin'` | `HOPOS_VITALS_RX throughput= read= header= conns=` | `HOPOS_VITALS_FAIL test=rx` zonder `url` (de standaardbron, cachefly: `http: CONNECT is not supported (never a tunnel)`) (P4, 03-10); `throughput=40.8 read=32 header=12.2 conns=1` (zes runs 4,9 tot 50,0, laptop op Wi-Fi, 30-09) | `HOPOS_VITALS_FAIL test=rx` zonder `url` (de standaardbron, cachefly: `http: CONNECT is not supported (never a tunnel)`) (P2g, 03-10); `throughput=37.3 read=64 header=13.8 conns=1` (laptop, Wi-Fi); van de O6N over de draad `throughput=48.6 read=64 header=6.54 conns=1` (30-09) | `HOPOS_VITALS_FAIL test=rx` zonder `url` (de standaardbron, cachefly: `http: CONNECT is not supported (never a tunnel)`) (X1, 03-10); van .208 (Wi-Fi): `throughput=19.2 read=32 header=12.3 conns=1`; over de draad van de O6N (`url=http://192.168.1.205:8090/blob?mb=32`): `throughput=21.6 read=32 header=6.22 conns=1` (30-09) | `HOPOS_VITALS_FAIL test=rx` zonder `url` (de standaardbron, cachefly: `http: CONNECT is not supported (never a tunnel)`) (O2, 03-10); `throughput=11.7 / 38.5 / 40.4` MB/s van de laptop over Wi-Fi, 32 MB (30-09) |   | system en hop: `HOPOS_VITALS_FAIL test=rx` zonder `url` (de standaardbron, cachefly: `http: CONNECT is not supported (never a tunnel)`) (R14, 03-10 avond); `HOPOS_VITALS_FAIL test=rx` zonder `url` (de standaardbron, cachefly: `http: CONNECT is not supported (never a tunnel)`) (R14, 03-10); leeg: geen peer (R3, 03-10) | `HOPOS_VITALS_FAIL test=rx` zonder `url` (de standaardbron, cachefly: `http: CONNECT is not supported (never a tunnel)`) (A4, 03-10) |
| tx | `curl -o /dev/null http://NODE:8090/blob?mb=64` | `HOPOS_VITALS_TX throughput= sent=` | `throughput=33.4 sent=64` (31,3 tot 33,4, laptop op Wi-Fi, 30-09) | `throughput=39.4 sent=64` (Wi-Fi) (30-09) | `throughput=17.4 sent=64` (Wi-Fi-host) (30-09) | `throughput=10.3..12.8 sent=256` naar de laptop over Wi-Fi (30-09) |  |  |  |
| up | `apps/vitals/perf.sh NODE:8090 64` (PUTs van 1 MiB naar `/sink`) | `HOPOS_VITALS_UPLOAD throughput= received= requests=` | `throughput=26.8 received=64 requests=64` (14,1 tot 37,6 met stiltes van ~2 s, curl -T, laptop op Wi-Fi, 30-09) | `throughput=21.8 received=64 requests=64` (Wi-Fi) (30-09) | `throughput=16.3 received=64 requests=64` (Wi-Fi-host) (30-09) | `throughput=17.6..21.3 received=256 requests=256` van de laptop over Wi-Fi (30-09) |  |  |  |
| disk | `curl 'http://NODE:8090/api/run?test=disk'` (of `perf.sh`) | `HOPOS_VITALS_DISK write= read= write_4k= floor_p50= floor_p99= chunk=` | `HOPOS_VITALS_SKIP test=disk` (geen schijf) (P4, 03-10); `HOPOS_VITALS_SKIP test=disk` (geen schijf, 30-09) | `HOPOS_VITALS_SKIP test=disk` (geen schijf) (P2g, 03-10); `HOPOS_VITALS_SKIP test=disk` (geen schijf) (30-09) | `HOPOS_VITALS_SKIP test=disk` (geen schijf) (X1, 03-10); `HOPOS_VITALS_SKIP test=disk` (geen schijf) (30-09) | 1 core `write=714 read=748 write_4k=42.3 floor_p50=70 floor_p99=79 chunk=1024`; 4 cores `write=820 read=778 write_4k=41.2 floor_p50=70 floor_p99=90` (64 MB, `/vitals-disk.bin`) (O2, 03-10); **AB-kern (main met heap, 01-10 middag, warme flip, vitals slot 2): `write=708 read=489..491 write_4k=50 floor_p50=59..60`; zonder heap (NH) `write=690..712 read=491..494 write_4k=52..53`: gelijk**; X-kern fb04980 (01-10 ochtend, koude boot): `write=992..1188 read=472..590` met vitals in slot 2, `read=373..376` in slot 4 (de plaatsing scheelt 30%), `write_4k` ~64, `floor_p50` 51 µs; `/data`, 256 MB (na de deur, H: `write=414 read=84.6 write_4k=45.0 floor_p50=72`): `write=196..199 read=92.3..93.6 write_4k=46.6..47.6 floor_p50=66 floor_p99=70..344 chunk=1024`; `kb=4`: `write=47.8..48.1 read=3.96..4.12`; `hole=1`: `read=298..303` (30-09) |   | system en hop: `HOPOS_VITALS_SKIP test=disk` (geen schijf) (R14, 03-10 avond); `HOPOS_VITALS_SKIP test=disk` (geen schijf) (R14, 03-10); leeg: geen schijf (R3, 03-10) | 1 core `write=873 read=1062 write_4k=74.5 floor_p50=46 floor_p99=50 chunk=1024`; 4 cores `write=874 read=1034 write_4k=70.6 floor_p50=49 floor_p99=53` (NVMe WD_BLACK SN770, 64 MB) (A4, 03-10) |
| storm | `curl 'http://NODE:8090/api/run?test=storm&addr=NODE_IP:8090'` | `HOPOS_VITALS_STORM rate= p50= p90= p99= errors=` | `rate=4777 p50=0.94 p90=1.47 p99=1.58 errors=0` (eigen poort) (P4, 03-10); `rate=1402 p50=4.87 p90=5.01 p99=6.13 errors=0` (1398 tot 1410; de derde achter elkaar 94 tot 173 met p99 1006, `HOPOS_MASQ_SLOT_FULL`; in het slot 4906 tot 4940, 30-09) | `rate=3204 p50=1.75 p90=2.73 p99=2.91 errors=0` (eigen poort) (P2g, 03-10); `rate=3243 p50=1.98 p90=2.88 p99=3.04 errors=0` (eigen poort, zonder `addr`) (30-09) | `rate=1038 p50=7.74 p90=12.3 p99=13.3 errors=0` (eigen poort) (X1, 03-10); `rate=1060 p50=7.66 p90=10.8 p99=11.8 errors=0` (eigen poort, zonder `addr`) (30-09) | 1 core `rate=2348 p50=3.11 p90=5.02 p99=5.38 errors=0`; 4 cores `rate=2460 p99=5.38` (eigen poort) (O2, 03-10); eigen poort: `rate=9965..9996 p99=0.67..0.69 errors=0`; `addr=192.168.1.205:8090`: `rate=1656 / 1993 / 178 p99=4.42 / 4.39 / 1004 errors=0` (30-09) | `rate=4872 / 4821 / 4820 p50=0.69 / 1.10 / 1.12 p99=3.62 / 5.13 / 3.93 errors=0` (eigen poort, drie runs) (M1, 03-10) | system: `rate=330..411 p50=19.7..23.4 p90=32.0..33.2 p99=33.8..40.0 errors=0`; hop: `rate=270..311 p50=26.2..29.2 p90=40.6..40.7 p99=42.2..42.5 errors=0` (eigen poort) (R14, 03-10 avond); `rate=383 p50=19.4 p90=35.0 p99=37.7 errors=0` (eigen poort) (R14, 03-10); `rate=413.5 p50=20.0 p90=32.3 p99=34.2 errors=0` (200 verbindingen, 8 werkers, naar zichzelf: `addr=10.100.0.4:8090`, `/ping`) (R3, 03-10) | 1 core `rate=9958 p50=0.55 p90=0.86 p99=0.92 errors=0`; 4 cores `rate=9964 p99=0.92` (eigen poort) (A4, 03-10) |
| rtt | `curl 'http://NODE:8090/api/run?test=rtt'` | `HOPOS_VITALS_RTT p50= p99= max= errors=` | `p50=167 p99=9134 max=9134 errors=0` (kern) (P4, 03-10); `p50=1346 p99=28445 max=28445 errors=0` (p50 1265 tot 1348, 30-09) | `p50=323 p99=5543 max=5543 errors=0` (kern 10.100.0.1:10100) (P2g, 03-10); `p50=1476 p99=6222 max=6222 errors=0` (30-09); na de deur van de switch (stempel H, dbc522f) `p50=133 p99=24718` | `p50=1555 p99=6691 max=6691 errors=0` (kern) (X1, 03-10); `p50=1984 p99=3507 max=3507 errors=0` (30-09); na de deur (H) `p50=365 p99=916`; met de idle-fix (I) `p50=268 p99=520` | 1 core `p50=718 p99=9471`; 4 cores `p50=718 p99=7557 errors=0` (kern) (O2, 03-10); `p50=1068..1071 p99=1083..1087 errors=0` (kern, stil; na de deur van de switch, stempel H: `p50=53 p99=64`); Hop 10.100.0.2:8080 `p50=2043..2046`; eigen poort `p50=19..20` (30-09) | `p50=62 p99=72`; `p50=65 p99=75`; `p50=66 p99=85`; `p50=62 p99=76`; `p50=76 p99=135`, `errors=0` (kern 10.100.0.1:10100, vijf runs) (M1, 03-10) | system: `p50=2525..2617 p99=3677..4366 max=3677..4366 errors=12` (alleen, twee keer 12 fouten; in `all` `p50=4494..4532 p99=16034..32018 errors=0`); hop: `p50=2797..2850 p99=4540..5317 errors=12` (alleen; in `all` `p50=4229..4274 p99=13722..14330 errors=0`) (kern) (R14, 03-10 avond); `p50=4622 p99=33006 max=33006 errors=0` (kern) (R14, 03-10); `p50=3654 p99=27759` (kern 10.100.0.1:10100) (R3, 03-10) | 1 core `p50=110 p99=5179 max=5179 errors=0`; 4 cores `p50=109 p99=5021` (kern) (A4, 03-10) |
| timer | `curl 'http://NODE:8090/api/run?test=timer'` | `HOPOS_VITALS_TIMER oversleep_1ms_p50= oversleep_1ms_p99= oversleep_5ms_p50= oversleep_20ms_p50=` | `oversleep_1ms_p50=213 oversleep_1ms_p99=332 oversleep_5ms_p50=1068 oversleep_20ms_p50=631` (P4, 03-10); `oversleep_1ms_p50=213 oversleep_1ms_p99=352 oversleep_5ms_p50=1068 oversleep_20ms_p50=631` (30-09) | `oversleep_1ms_p50=213 oversleep_1ms_p99=215 oversleep_5ms_p50=1068 oversleep_20ms_p50=631` (P2g, 03-10); `oversleep_1ms_p50=213 oversleep_1ms_p99=213 oversleep_5ms_p50=1068 oversleep_20ms_p50=631` (30-09) | `oversleep_1ms_p50=820 oversleep_1ms_p99=1750 oversleep_5ms_p50=689 oversleep_20ms_p50=478` (X1, 03-10); `oversleep_1ms_p50=19 oversleep_1ms_p99=678 oversleep_5ms_p50=232 oversleep_20ms_p50=153` (30-09) | 1 core `oversleep_1ms_p50=826 oversleep_1ms_p99=1193 oversleep_5ms_p50=242 oversleep_20ms_p50=434`; 4 cores `oversleep_1ms_p50=48 oversleep_1ms_p99=220 oversleep_5ms_p50=242 oversleep_20ms_p50=91` (O2, 03-10); `oversleep_1ms_p50=7..48 oversleep_1ms_p99=356..706 oversleep_5ms_p50=99..241 oversleep_20ms_p50=26..66` (30-09) |   | system: `oversleep_1ms_p50=231..233 oversleep_1ms_p99=2186..3234 oversleep_5ms_p50=198..203 oversleep_20ms_p50=218` (alleen; in `all` `oversleep_1ms_p50=753..813 oversleep_1ms_p99=12697..13410 oversleep_5ms_p50=611..659 oversleep_20ms_p50=406..436`); hop: `oversleep_1ms_p50=370..371 oversleep_1ms_p99=399..1050 oversleep_5ms_p50=382..391 oversleep_20ms_p50=379..383` (alleen; in `all` `oversleep_1ms_p50=56..861 oversleep_1ms_p99=3037..3868 oversleep_5ms_p50=529..653 oversleep_20ms_p50=467..494`) (R14, 03-10 avond); `oversleep_1ms_p50=779 oversleep_1ms_p99=14607 oversleep_5ms_p50=643 oversleep_20ms_p50=323` (R14, 03-10); `oversleep_1ms_p50=815 oversleep_1ms_p99=2625 oversleep_5ms_p50=776 oversleep_20ms_p50=391` (R3, 03-10) | 1 core `oversleep_1ms_p50=236 oversleep_1ms_p99=1022 oversleep_5ms_p50=255 oversleep_20ms_p50=108`; 4 cores `oversleep_1ms_p50=236 oversleep_1ms_p99=311 oversleep_5ms_p50=255 oversleep_20ms_p50=121` (A4, 03-10) |

Een fout is `HOPOS_VITALS_FAIL test=<naam>`, overslaan `HOPOS_VITALS_SKIP
test=<naam>` (smp met één core, disk op een node zonder opslag). Op QEMU is
WFE een no-op: de idle-rij is een ijzer-rij.

## Netwerk van de host naar een app (TCP)

De v2-kolom is de Vitals-fixture (één core, 512 MiB), gemeten van node naar
node over de draad met de M4 als vaste peer (L83 punt 42 tot 53, 21-09), tenzij
anders vermeld. De v3-meting is `netmeter NODE:80 --phases in,out --bytes
209715200 --repeat 3` vanaf een bedrade host.

| Meting | v2, per board | v3 QEMU virt | v3 O6N | v3 Altra | v3 Pi 5 | v3 Pi 4 | v3 Radxa | v3 LicheeRV |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| De node in (`in`, MB/s) | O6N 111,3 tot 116,5; Altra 107,7 tot 110,8; Pi 5 57,0 tot 69,6; Radxa 55,8 tot 56,6; Pi 4 6,6 (2.2.6, gepold); M4 43,3 tot 46,7 (bundel 25, HTTP PUT van 64 MiB) | 122,3 (TCG, slirp) | node naar node over de draad (vitals rx, 256 MB, één verbinding, stempel G, 30-09): 76,4 tot 78,3 van de Pi 4 (4 verbindingen 78,1), 42,9 tot 43,3 van de Pi 5; geen peer zendt sneller, dus een ondergrens |  | node naar node over de draad (vitals rx, 256 MB, stempel dev, 30-09): 76,1 tot 85,2 van de O6N, 83,7 tot 92,8 van de Pi 4 | node naar node over de draad (vitals rx, 256 MB, stempel F, 30-09): 41,1 tot 43,8 van de O6N, 41,3 tot 42,5 van de Pi 5 (4 verbindingen 43,0); de ontvangkant is de grens | node naar node over de draad (vitals rx, stempel F, 30-09): 20,1 tot 20,5 van de O6N, 21,7 tot 21,9 van de Pi 4, 21,9 tot 22,1 van de Pi 5 (4 verbindingen 21,8) |  |
| De node uit (`out`, MB/s) | O6N 114,3 tot 117,6; Altra 108,0 tot 110,6; Pi 5 41,6 tot 50,6; Radxa 98,8 tot 99,6; Pi 4 42,3; M4 47,7 tot 52,3 (HTTP GET) | 71,4 (TCG, slirp) | node naar node over de draad (vitals rx op de peer, 256 MB, stempel G, 30-09): 76,1 tot 85,2 naar de Pi 5 (de snelste ontvanger), 41,1 tot 43,8 naar de Pi 4; drie ontvangers tegelijk samen ~90 (afgeleid uit de tx-markers); een ondergrens |  | node naar node over de draad (256 MB, stempel dev, 30-09): 42,9 tot 43,3 naar de O6N, 41,3 tot 42,5 naar de Pi 4; de zendkant is de grens | node naar node over de draad (256 MB, stempel F, 30-09): 76,4 tot 78,3 naar de O6N, 83,7 tot 92,8 naar de Pi 5 | node naar node over de draad (stempel F, 30-09): 21,2 tot 21,3 naar de O6N, 20,8 tot 21,0 naar de Pi 4, 21,4 tot 21,6 naar de Pi 5 |  |

## Latentie en verbindingen

| Meting | v2, per board | Commando, marker | v3 QEMU virt | v3 O6N | v3 Altra | v3 Pi 5 | v3 Pi 4 | v3 Radxa | v3 LicheeRV |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| rtt, open verbinding | O6N p50 156 / 201 / 200 µs (irq, drie runs); M4 p50 50 / 62 / 66 µs | `netmeter --phases rtt --rtt 200`, `NETMETER phase=rtt ... p50= p99=` | p50 1180 µs, p99 3472 µs | app naar app over de draad (`BENCH=ping` naar de bench op de Pi 5, stempel H, 30-09): p50 200 / 227 / 223 µs, p99 0,57 tot 2,6 ms; koud p50 197 tot 245 µs |  | app naar app over de draad (`BENCH=ping` naar de bench op de O6N, stempel dev, 30-09): p50 238 / 206 / 206 µs, p99 1,1 tot 1,4 ms; koud p50 1508 tot 1687 µs | app naar app over de draad (`BENCH=ping` naar de bench op de O6N, stempel H, 30-09): p50 1212 / 1213 / 1213 µs, p99 4,2 tot 10,9 ms; koud p50 1290 tot 1300 µs | app naar app over de draad (`BENCH=ping` naar de bench op de O6N, stempel I, 30-09): p50 289 / 289 / 287 µs, p99 1,0 tot 1,4 ms; koud p50 643 tot 788 µs |  |
| Verbindingscyclus, één tegelijk | O6N 3,6 ms; Altra 5,1 ms; Pi 5 1,1 tot 3,6 ms; Radxa 4,1 ms; LicheeRV 16 tot 19 ms | `netmeter --phases storm`, `p50=` | p50 1,44 ms | over de draad (vitals storm `n=1` naar de Pi 5, dial, GET, close; stempel H, 30-09): p50 2,43 ms, 381 conn/s (drie runs; de derde met een SYN na 1 s) |  | over de draad (vitals storm `n=1`, stempel dev, 30-09): naar de O6N p50 5,9 ms, na de koude herstart 2,36 tot 2,42 ms; naar de Pi 4 7,4 ms | over de draad (vitals storm `n=1`, 30-09): naar de O6N p50 4,85 (F) / 2,43 / 4,86 ms (H); naar de Pi 5 6,1 tot 7,3 ms (F) | over de draad (vitals storm `n=1`, 30-09): naar de O6N p50 4,67 / 1,78 / 4,37 ms (kaart-kern en H); naar de Pi 4 4,9 tot 6,0 ms (F) | v2-lat 16 tot 19 ms; v3 vitals storm naar zichzelf (`addr=10.100.0.4:8090`, 200 verbindingen, 8 werkers; R3, 03-10): p50 20,0 ms, 413 conn/s, niet over de draad en niet één tegelijk. Vanaf het LAN welcome 200 in 12 ms. Over Wi-Fi van de laptop (02-10, 3.0.3 met vier verweesde slots): Hop `GET :9080/v1/status` p50 13,4 ms (connect 5,5 ms, ping 3,3 ms min) |
| Storm, verbindingen per seconde | O6N 844 conn/s, p99 19,6 ms (irq); M4 6379 conn/s, p99 1,9 ms | `netmeter --phases storm --storm 1000`, `conn_per_s=` | 638 conn/s, p99 2,7 ms | over de draad (vitals storm, 200 verbindingen, 8 werkers, stempel H, 30-09): naar de Pi 5 4976 / 99 / 3298 conn/s (p50 0,8 tot 1,0 ms; de 99 is een SYN na 1 s bij `HOPOS_MASQ_SLOT_FULL`); naar de Pi 4 3298, naar de Radxa 583 |  | over de draad (vitals storm, 200, 8 werkers, stempel dev, 30-09): naar de O6N 697, na de koude herstart 3284 / 4893 / 97 (SYN na 1 s); naar de Pi 4 612 tot 656 conn/s, p99 38 tot 45 ms | over de draad (vitals storm, 200, 8 werkers, 30-09): naar de O6N 970 (F) / 1398 / 696 / 606 (H), p99 24 tot 49 ms; naar de Pi 5 606 tot 652 | over de draad (vitals storm, 200, 8 werkers, 30-09): naar de O6N 756 / 1616 / 699 / 656 (kaart-kern en H), p99 7 tot 24 ms; naar de Pi 4 580 tot 617; naar de Pi 5 618 tot 699 (F) |  |
| UDP-echo, rtt en verlies | geen v2-getal | `netmeter --phases udp`, `NETMETER phase=udp ... lost=` | niet op QEMU (de hostfwd is TCP) | niet gemeten (vitals en bench hebben geen UDP-client over de draad) |  | niet gemeten | p50 5851 tot 6342 µs, lost 0 (Wi-Fi, geen lat) | p50 3220 tot 6760 µs, lost 0 (Wi-Fi, geen lat) |  |

## In de node: app naar app door de switch

| Meting | v2 | Commando, marker | v3 QEMU virt | v3 O6N | v3 Altra | v3 Pi 5 | v3 Pi 4 | v3 Radxa | v3 LicheeRV |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Twee apps op één node, 400 MB | M4: 769 / 764 MB/s (v3 M4 01-10, M13 tot M15 met de slotstaart Normal en de core-start na een flip, 800 MiB: pull op een E-core 4375 tot 4404 en 3810 tot 4359, op een P-core 6043 tot 6123 MB/s, bad=0; M20 met de switch van ring naar ring: E 4448 tot 4468, P 6075 tot 6103; M33 met alle fixes van de review en de wachtrij: P 6379 / 6383; 40 GiB 2813; op M7 nog 52,05 / 52,10 met de tune en 47,11 met pstate=off) | `BENCH=pull BENCH_BYTES=419430400`, `HOPOS_BENCH_PULL` | 62,9 MB/s (16 MiB, TCG; met de deur van de switch, dbc522f; ervoor 6,9) | 1685,62 / 1689,22 MB/s, bad=0 (O2, 03-10); **1495 tot 1711 MB/s (X-kern fb04980 en de apps van dezelfde boom: memcpy, ringbelofte, frames in plaats, venster ring min twee frames, 01-10)**; 259,09 (K-kern na de koude boot, lean v3.1.3, 01-10); 141,50 / 141,50 / 143,72 (stempel H, met de deur, 30-09) | 4970,01 / 4972,25 MB/s, bad=0 (A4, 03-10) | 783,17 / 809,51 MB/s, bad=0 (P4, 03-10); 6,53 / 6,64 / 6,64 MB/s (kaart-kern dev, zonder de deur, 30-09) | 375,17 / 378,23 MB/s, bad=0 (P2g, 03-10); **434,80 / 407,06 / 407,48 MB/s (PR1, main efcd0d4 met de kopieën minder, 01-10: de OS-core vol, rxfull 0, de A72 op 1500 MHz is de grens)**; 461,47 / 423,66 / 421,66 (AA-kern 9e5d53a, main met de heap-crate, 01-10); 418 tot 463 (X-kern fb04980, 01-10); 271,79 (stempel K, lean v3.1.3, 01-10); 100,61 / 100,62 met lean v3.0.0 en v3.1.2 (de ontvangstring bleef op 16 KiB, zie ALLES.md); 26,68 / 23,62 / 25,38 (stempel H, met de deur, 30-09; eerder 25,9) | 283,31 / 283,83 MB/s, bad=0 (X1, 03-10); **257 tot 262 MB/s (RX1 en RX2, 01-10: de slotstaart Normal in de kernmap zoals M8 op de M4; 40 GiB op 261 MB/s zonder fouten; de OS-core vol op 816 MHz, de klok van de firmware)**; 29,83 (AB-kern, 01-10, met een corrupte RX-ring: de Radxa-kern mapt de pool Device, dus geen ringbelofte); 29,1 (lean v3.1.3, 01-10); 19,89 / 19,89 / 19,89 (stempel I, 30-09); vóór de deur 6,46 tot 6,49 | 18,52 MB/s (R14, 03-10 avond; 100 MB, serve en pull allebei in `system` op de C906B naast de kern, twee runs gelijk, bad=0, 1 zero window) |
| rtt app naar app, warm | geen v2-tabel (schedbench) | `BENCH=ping`, `HOPOS_BENCH_RTT` | p50 115 µs, p99 340 µs (met de deur van de switch, dbc522f; ervoor 2238 / 2447) | p50 42 / 42 / 42 µs, p99 65 tot 77 µs (O2, 03-10); p50 44 / 45 / 39 µs, p99 53 tot 68 µs (H, 30-09) | p50 21 / 21 / 21 µs, p99 37 µs (A4, 03-10) | p50 28 / 28 / 28 µs, p99 35 tot 38 µs (P4, 03-10); p50 29 / 29 / 29 µs, p99 31 tot 41 µs (dev, 30-09) | p50 45 / 45 / 47 µs, p99 53 tot 142 µs (P2g, 03-10); p50 48 / 48 / 48 µs, p99 55 tot 84 µs (H, 30-09) | p50 99 / 100 / 99 µs, p99 121 tot 172 µs (X1, 03-10); p50 77 / 77 / 77 µs, p99 122 tot 159 µs (I, 30-09); vóór de deur 78 tot 79 | p50 1049 us, p99 1278 us (R14, 03-10; beide residenten op de OS-core; dat is de tikperiode, dus de gewekte bewoner krijgt zijn beurt pas bij de timer in plaats van bij de kick: fout, open in ALLES) |
| rtt na 1 s stilte (koud) | geen v2-tabel | `BENCH=ping`, `HOPOS_BENCH_COLD` | p50 375 µs (dbc522f; ervoor 2494) | p50 54 / 68 / 53 µs (O2, 03-10); p50 57 / 51 / 56 µs (H, 30-09) | p50 31 / 47 / 31 µs (A4, 03-10) | p50 34 / 34 / 31 µs (P4, 03-10); p50 1239 / 1239 / 1239 µs (dev, zonder de deur, 30-09) | p50 56 / 53 / 55 µs (P2g, 03-10); p50 54 / 53 / 74 µs (H, 30-09) | p50 113 / 106 / 114 µs (X1, 03-10); p50 142 / 124 / 144 µs (I, 30-09); vóór de deur 599 tot 742 | p50 1059 us, max 1090 us (R14, 03-10) |

## De idle-meetlat van de OS-core

`hopos.idlestat=1`, stilte (geen verkeer, Hop en één app), de regel
`idle: N wakes/s ... HOPOS_IDLESTAT`. De kolommen die tellen: wekken per
seconde, lege RX-rondes per seconde, werk na de failsafe per seconde.

| Meting | v2 | v3 QEMU virt | v3 O6N | v3 Altra | v3 Pi 5 | v3 Pi 4 | v3 Radxa |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Wekken per seconde, stil | gepold ~3.300, op de interrupt ~100 (de vangrail) | ~820 |  |  | ~900 (`sleeps`), ~2.000 polls/s, ~70 NIC-irq/s; tijdens vitals burn ~1.900 |  | idlestat uit; `HOPOS_TICK`: ~1.700 tot 1.900 slaapjes/s, NIC-irq ~75/s |
| Lege RX-rondes per seconde, stil | gepold 3.333, op de interrupt ~100 | ~94 | | | | | |
| RX-pomprondes Pi 5, stil | 824 (gepold), 113 (irq) |  |  |  | ~70 NIC-interrupts/s, ~2.000 polls/s |  |  |
| NIC-interrupts, O6N | 1.813 in 40 s met een rtt-run; 3.333 polls/s gepold | | | | | | |
| Tijd van de bewoners (Hop) op de OS-core, stil | geen v2-getal | 0,3 % |  |  |  |  | 0,14 % (`HOPOS_TICK` res_ms 1,4 ms/s) |

Tijdens `BURN=1` op een app-core hoort de regel van de OS-core niet te
bewegen (de last staat op een andere core); stijgen de wekken of de kicks
van de OS-core met de last mee, dan lekt de app-core naar de kern.

## Opslag

`hopos.nvmebench=1`. De v2-regels waren 16 MiB per commandomaat; v3 drukt
dezelfde regels (`nvme bench: 1024 KiB x16 ...`) plus sequentieel over de
staart (hoogstens 1 GiB) en 4 KiB willekeurig.

| Meting | v2 | v3 QEMU virt (virtio-blk, 64 MiB-schijf) | v3 O6N | v3 Altra | v3 Pi 5 |
| --- | --- | --- | --- | --- | --- |
| Rauw, 1 MiB-opdrachten, 16 MiB, schrijven / lezen | O6N gecachet 3056,8 / 2705,6; ongecachet 49,1 / 59,1; M4 5389 tot 5756 / 1824 tot 1971 MB/s (v3 M4 01-10, M14 met het ANS-datablok write-back: 4936,1 / 1718,7; 256 KiB 4742,8 / 944,9; 64 KiB 4073,3 / 430,5; M23 met de ANS asynchroon: 4952 / 1925, 4 KiB schrijven 1160) | 1260,9 / 1221,4 MB/s | één boot met hopos.nvmebench=1 nodig (stick-cfg) | | |
| hopfs, 1 MiB-calls, 16 MiB | O6N 2970,1 / 2716,7; M4 5480 tot 5632 / 1781 tot 1816 MB/s (v3 M4 01-10, M14: 4931,1 / 1770,8; 64 KiB 3918,1 / 444,5; M23: 5015 / 2026) | 1187,0 / 1207,2 MB/s | één boot met hopos.nvmebench=1 nodig (stick-cfg) | | |
| Sequentieel over de staart (tot 1 GiB) | geen v2-getal (v3 M4 01-10, M14: 1 GiB 4803,9 / 1652,3 MB/s; M23: 4758 / 1629) | 32 MiB: 1293,0 / 1238,2 MB/s | één boot met hopos.nvmebench=1 nodig (stick-cfg) | | |
| Willekeurig 4 KiB | geen v2-getal (O6N: 4 KiB-overschrijvingen via de app 15,15 tot 17,08 MB/s; v3 M4 01-10, M14: 162315 / 11871 IOPS, 664,8 / 48,6 MB/s, M23 140k / 11,9k IOPS, één monster per boot; via de app 4 KiB-schrijven 90,5 tot 90,8 MB/s) | 32051 / 32123 IOPS | geen IOPS-meting; 4 KiB-overschrijvingen via de app 45,4 tot 48,9 MB/s (v2 15,15 tot 17,08); 4 KiB-lezen 3,96 tot 4,12 MB/s |  |  |
| Transport kern naar app zonder schijf (vitals `test=disk&mb=256&hole=1`, gaten uit het RAM van de kern) | geen v2-getal (v3 M4 01-10: 1554 MB/s op M15; 1681 op M17 met de switch die poort 0 in de ring leest; 1846 op M18 met de kernstack die in de host-ring zendt; 1858 tot 1861 op M21, 1879 tot 1917 op M24; de kosten per MiB op de OS-core van 0,645 naar 0,533 ms) | | | | |
| Replica (SQLite 3.53.4 op HopFS, `journal_mode=DELETE`, `synchronous=EXTRA`, elke sync een OP_SYNC) | v2: Spin op de M4 draaide, geen getal | geen QEMU-getal in deze tabel (`tools/qemu-persist.py` in de replica-repo) | | | |
| ... smoke (RAM-SQLite, twee slots) | | | M4 01-10 (M26): `REPLICA_SQLITE_OK` in slot 3 en 4, 33 writes, 6 syncs, 1,23 tot 1,33 ms | | |
| ... persist (volume op HopFS: schrijven, app-herstart, herstel, pipeline, reparatie, annulering, eigenaar) | | | M4 01-10 (M26): alle markers (`PERSIST_WRITE`, dan `PERSIST_READ`, `RESTORED_COLD_OK`, `PIPELINE_OK`, `PIPELINE_REPAIR_OK`, `SQLITE_CANCEL_OK`, `OWNER_OK`); de hele proef 190 tot 218 ms alleen (M33: 193 ms, over een flip heen hersteld), 255 tot 325 ms naast een app die 1 GiB schrijft en leest (M32 met de wachtrij: 247 tot 254) (die zakt dan van 1433 tot 1517 naar 866 tot 894 lezen); de koude herstart nog met de knop te doen | | |
| ... 4 KiB-schrijfjes als RPC-lus (vitals `write_4k`, het slechtste geval) | | | M4 01-10: 82 MB/s alleen (20.000 calls/s, 50 µs), 15 tot 19 naast een bulk-app | | |
| ... willekeurig 4 KiB lezen door apps (vitals `rand=N`), opdrachten per seconde | geen v2-getal | | M4 01-10, M26 (één call tegelijk in de actor) naar M32 (de wachtrij: de actor met zestien calls, de ANS op zestien tags): één app 7.600 naar 7.800; twee apps samen 9.200 naar 14.400; vier apps samen 9.300 naar 25.000 (p50 410 naar 140 µs); acht apps samen 36.000 met de OS-core vol; rauw in de kern met zestien tegelijk 175.055; M33 (alle fixes erin): één app 7.600, twee 14.500, vier 25.500 | | |
| App-opslag, 256 MiB, 1 MiB-calls | O6N 556,83 tot 625,43 / 727,49 tot 796,83; M4 1598 tot 1657 / 1064 tot 1072 MB/s (v3 M4 01-10, M14 en M15 met vitals-M15 op twee P-cores: 1055 tot 1279 / 808 tot 886 MB/s, de schijf plus hopfs 0,59 ms per MiB en het transport kern naar app 0,66 ms per MiB liepen na elkaar; M24 met de ANS asynchroon, diepte 2 en read-ahead: 1269 tot 1270 / 1683 tot 1696, 1 GiB 1285 / 1590 tot 1633; zie ALLES.md) | nog geen v3-rol (bench heeft geen FS-rol) | 196,3 tot 199,2 / 92,3 tot 93,6 MB/s (vitals disk op `/data`, drie runs; ook via perf.sh 196,5 tot 198,0 / 93,0 tot 93,5); stempel H met vitals op 4 cores: 87,7 tot 88,6 / 87,7 tot 88,3 MB/s, 4 KiB-schrijven 32,4 tot 33,3 MB/s, stat-vloer p50 63 tot 72 µs (drie runs, 30-09) |  |  |

## Media: de hardwaredecoder van de O6N

| Meting | v2 | Commando, marker | v3 O6N |
| --- | --- | --- | --- |
| 4K P010 door de grant, apps/decode via Hop | 27,25 fps met Lumen op GAMEOFTHRONES_S1_D1 (27-09, met de software-encoder erachter); de lat is 24 fps = 597 MB/s | `POST /v1/jobs` met `decode.elf` en `DECODE_URL` (apps/decode/README.md), `HOPOS_DECODE fps= MBps=` | **85,7 fps, 2134 MB/s**: 240 beelden 3840x2160 p010 (14 MB x265-testclip van de laptop) in 2797 ms (30-09). Eerst 43,1 fps op 10 beelden, en een fault na 14 beelden zolang decode zijn happen van 1 MB midden in een NAL-eenheid knipte; sinds de knip op de laatste startcode heel |

## Wat hier (nog) niet gemeten wordt

- De kern-flip: de echo-rtt tijdens een flip (v2, Pi 4: 4,3 tot 8,0 s). `tools/soak.sh` met `FLIP=1` telt de flips en de overleving, niet de rtt.
- Geheugenbudgetten (v2: de LicheeRV-vensters): de kern-heap staat in de tweede `idle:`-regel (`kern heap ... used`), de app-heap in `HOPOS_BENCH_THRASH`.
- Watchdog-herstel en energie: een meting met een stopwatch en een meter, niet met een tool.

## De kern en Hop in rust (03-10)

`GET :9080/v1/agents` (`kern_cpu_percent`, `kern_mem_bytes` van
`kern_ram_bytes`, `hop_cpu_percent`, `hop_mem_bytes`, `temp_milli_c`) en de
taken met `state` `system` in `GET :9080/v1/tasks`, drie peilingen 10 s uit
elkaar, alleen welcome geplaatst (op de LicheeRV in plaats daarvan de
stulp-jobs van Derek), main van 03-10 met Hop 3.0.6.

| Meting | v3 O6N | v3 Altra | v3 Pi 5 | v3 Pi 4 | v3 Radxa | v3 LicheeRV |
| --- | --- | --- | --- | --- | --- | --- |
| Kern-cpu (`kern_cpu_percent`) | 4 / 4 / 4 % (O2) | 5 / 5 / 5 % (A4) | 2 / 2 / 2 % (P4) | 9 / 9 / 9 % (P2g) | 3 / 2 / 2 % (X1) | 5 / 9 / 7 % naast stulp, stulp-plugins en cloudflared-lean (R14) |
| Kern-geheugen (`kern_mem_bytes` van `kern_ram_bytes`) | 37,5 van 224 MiB (16,7 %) | 31,8 van 224 MiB (14,2 %) | 30,3 van 127,5 MiB (23,7 %) | 30,4 van 127,5 MiB (23,8 %) | 32,2 van 64 MiB (50,3 %) | 30,3 van 40 MiB (75,6 %) |
| Hop-cpu (`hop_cpu_percent`) | 1 / 1 / 1 %, core 0 | 1 / 1 / 1 %, core 0 | 1 / 1 / 1 %, core 0 | 1 / 1 / 1 %, core 0 | 1 / 1 / 1 %, core 0 | 2 / 1 / 2 %, core 1 (groep `hop`) |
| Hop-geheugen (`hop_mem_bytes` van `hop_ram_bytes`) | 0,44 tot 0,46 van 64 MiB | 0,46 van 64 MiB | 0,42 tot 0,44 van 64 MiB | 0,42 tot 0,44 van 64 MiB | 0,42 tot 0,44 van 64 MiB | 0,60 tot 0,62 van 10 MiB |
| Temperatuur (`temp_milli_c`) | 41,0 C | 46,0 C | 50,5 tot 51,0 C | 51,6 tot 52,1 C | geen veld (geen sensor) | 56,0 C |
| Systeemtaken in `/v1/tasks` | `kern` (pid 0, core 0, cpu 4 %, mem 16,7 %), `hop` (pid 1, core 0, 1 %, 0,6 tot 0,7 %); welcome 1 % op core 1 | `kern` (core 0, 5 %, 14,2 %), `hop` (core 0, 1 %, 0,7 %); welcome 1 % op core 1 | `kern` (core 0, 2 %, 23,7 %), `hop` (core 0, 1 %, 0,6 %); welcome 1 % op core 1 | `kern` (core 0, 9 %, 23,8 %), `hop` (core 0, 1 %, 0,6 %); welcome 1 % op core 1 | `kern` (core 0, 2 tot 3 %, 50,3 %), `hop` (core 0, 1 %, 0,6 %); welcome 1 % op core 1 | `kern` (core 0, 5 tot 9 %, 75,6 %), `hop` (core 1, 1 tot 2 %, 6,0 %); stulp 1 tot 4 % en cloudflared-lean 1 tot 23 % op core 1, stulp-plugins 1 tot 3 % op core 0 |

Lezing: de kern staat op de arm-borden tussen 2 en 9 procent in rust (de
Pi 4 het hoogst), Hop overal op 1 procent met minder dan een halve MiB. De
O6N draait op O2 een core op ~299 Msteps/s (30-09: 855); vitals4 brandt
daar 1198 (4 x 299.5), niet de 1663 van de O2-alinea hieronder. De Altra
(A4) haalt app naar app 4970 MB/s, de hoogste van alle borden.

## QEMU is geen ijzer

De QEMU-kolom (29-09, `sh tools/qemu-test-bench.sh`, TCG op een Mac M-serie,
4 cores, virtio-net via slirp) bewijst de keten, geen plafond: slirp
termineert TCP op de host, TCG rekent in software, en de vier vCPU's delen
de host. Dat app naar app in de node (6,9 MB/s) trager is dan host naar app
(71 tot 122 MB/s), zegt op QEMU vooral dat twee vCPU's elkaar via de kern
wekken; op ijzer is dat precies het getal om te vergelijken met de 769 MB/s
van de M4.

## Uit de tabel van ALLES.md (03-10)

De tabel in ALLES.md zegt sinds 03-10 alleen nog of iets slaagt. Wat de
cellen daarvoor droegen en nog niet hier of in een docs/boards-*.md stond,
staat hieronder, per board, zoals het er stond.

**QEMU virt**

- Console op het glas via `ramfb`, USB via `qemu-xhci`.
- Gebruik per taak (arm64, 03-10): vitals 100 tijdens de brand, 1 in rust.

**Raspberry Pi 5**

- Warme flip: sterft na de landing (stempels F en H, drie keer); de nieuwe kaart (H, met de zwarte doos) ligt in target/.
- Console op het glas: eerst gezien via een flip op de eerste kaart; sinds de herflash weigert de firmware.
- Kaart in target/: stempel I, 22:37.

**Raspberry Pi 4**

- Warme flip: zes keer, gen 4 op stempel I.
- Console op het glas: 32 bpp.
- Kaart in target/: stempel I, 22:37.

**Radxa Zero 3E**

- NIC met interrupt op SPI 64 (de rij Hardware-IRQ noemde INTID 64).
- Warme flip: vijf keer, gen 3 op stempel I.
- Console op het glas: HDMI, zonder EDID.
- USB: twee DWC3's up, niets ingeplugd.
- Kaart in target/: stempel I, 22:38, met de gepatchte Hop.

**Ampere Altra** (gebouwd, nog nooit geboot)

- NIC: igb gepold, bewust: de `_PRT`-INTx doodt de SoC.
- Kick via SGI 1.
- Watchdog: SBSA.
- RNG voor de kern: SMCCC-TRNG of `rndr` (fc5348f).
- Temperatuur: SMpro.
- Console op het glas: GOP.
- Opslag: NVMe.
- Stick in target/: 18:12.

**Orion O6N**

- Boot: VHE, kick via SGI 1 (`kick=(Ipi, 0 us)`, stempel G).
- Warme flip: gen 2 op H (gui-bundel); I geweigerd door de H-kern (de bundelpartitie van 8 KiB te krap, fix de61b4b zit in de I-stick), dus een koude boot.
- NIC: RTL8125B, MSI-X via de ITS (LPI 8192).
- Timer: CNTHP (PPI 26).
- Watchdog: SBSA, 8,5 s.
- RNG voor de kern: geen FEAT_RNG of SMCCC-TRNG, wel het EFI_RNG_PROTOCOL van de firmware (`hopos.efirng=1`, de volgende stick).
- RNG voor de slots: jitter (G); efi-rng met de volgende stick.
- Temperatuur: SCMI, 39 C in de agentlijst.
- Klokbeleid: vijf `_CPC`-domeinen.
- Console op het glas: GOP 1920x1080.
- USB: 10 xHCI's up, de Blu-ray-drive over USB-BOT leest de disc (Lumen).
- Opslag: NVMe Lexar 4 TB, hopfs hersteld (generatie 3456).
- Hardwaredecoder: Linlon V8; nu tijdelijk weg (gui-flip H) tot de koude boot.
- Stick in target/: stempel I, 22:38 (kern-fix bundelpartitie, verse Hop, efirng).

**Mac mini M4**

- Hop: ingebakken, `HOP_UP`, gaat warm mee over flips (`HOPOS_HOP_RESUMED`).
- Warme flip: gen 1 tot 8 op 01-10 (D4b naar M15), adoptie 3 van 3, de config reist mee; koud niet (geen CPU_OFF).
- NIC: tg3 BCM57766 gepold, geen MSI, INTx niet teruggebracht (driver/nic/tg3); MAC-filter na `link_up` (4be8b60).
- Watchdog: de canary is sinds R1 een self-dial naar Hop (`HOPOS_WD_CANARY_OK`), op alle borden.
- RNG voor de kern: geen FEAT_RNG, geen SMCCC, dus jitter.
- Opslag: ANS NVMe 414 GB, hopfs hersteld; de ANS asynchroon met read-ahead sinds M22.
- Console op 5555: `hopos.replay=45`.
- Image: D4b geïnstalleerd (pstate=off, zonder de fixes van 01-10); `art/hopos-apple.flip` = M24 (`cfg/m4-meet.cfg`, replay=0); een nieuw image met main gewenst.

**LicheeRV Nano**

- Boot met de loterij: gezien op R3 tot R10 (`HOPOS_LOTTERY_SWAPPED`, de PLIC-context van `clint_hart()`, ac79e91).
- Koude flip: gen 2 (R3) tot gen 10 (R10, 03-10), negen koude flips op één dag; vanaf R4 de symtab-stroom en de riscv-klok.
- Koude flip met de loterij: na de koude boot opnieuw `HOPOS_LOTTERY_SWAPPED`, `HOPOS_FLIP_SETTLED` na 70 s.
- Warme flip: `flip::WARM` weigert vóór de sprong.
- Hardware-IRQ: kick via MSIP van de CLINT per core; PLIC context 0 met de 102 bronnen van de C906B; andere externe lijnen gaan uit zodra ze vuren.
- Hardware-IRQ met de loterij: gezien, met de NIC gepold.
- Gebruik per taak (R10, 03-10, met de loterij): de kern 31,5 van 40 MB in rust, Hop 2 tot 3 %, welcome 1 % en 0,4 %.
- Gebruik per taak is te zien in /v1/agents naast de temperatuur, in /v1/tasks als `system`, en in de hop-gui op het bord zelf (:8081, agent openen).
- Gebruik per taak zonder de loterij, nog te zien: de kern-cpu in rust ver onder de 46 % (de lijn in plaats van de pomp).
- Watchdog: canary `HOPOS_WD_CANARY_OK` (R3, 03-10).
- Temperatuur: TEMPSEN 59,8 C bij de boot, in de tik en de heartbeat (R5, 03-10); Go `temp.go` geport; `HOPOS_TEMPSEN_UP` of `_NONE`.
- Kaart: R3 (fip met de loterij, Hop 3.0.6) draait op .150.

## De LicheeRV met en zonder loterij (03-10)

Zelfde bord, zelfde dag, zelfde vitals-riscv64 en welcome. Met de loterij
(R10) staat de kern op de C906L (700 MHz) en pollt hij de dwmac elke 300 us;
zonder (R11) staat hij op de C906B (1 GHz) met de dwmac op PLIC-bron 31 en
slaapt hij in wfi, Hop zit op de C906L en de apps in de sharegroup `system`
bij de kern.

| Meting | R10, loterij | R11, zonder loterij |
| --- | --- | --- |
| Kern-cpu in rust (slot 0) | 46 tot 48 % | 4 tot 5 % |
| HOPOS_TICK busy_ms per seconde in rust | 470 tot 490 | 74 tot 78 |
| NIC-interrupts per seconde in rust | 0 (gepold) | 51 tot 68 |
| Temperatuur in rust | 59,4 tot 59,8 C | 52 tot 57 C |
| welcome vanaf het LAN in rust | 12 ms | 12 tot 15 ms |
| vitals cpu 5 s, Msteps/s | 93,6 (C906B alleen, naast welcome in de sharegroup) | 60,6 (C906B gedeeld met de kern en welcome in `system`); 65,1 bij Hop op de C906L (groep `hop`, R12) |
| welcome tijdens de brand | 12 tot 38 ms | 18 tot 36 ms (brand in `system`); 13 tot 27 ms (brand op de C906L) |
| Hop (slot 1) in rust | 2 tot 3 % van de C906L, gedeeld met de kern | 1 tot 4 % van de C906L, alleen |

Lezing: zonder loterij is de kern tien keer stiller en het bord koeler, en
is de pomp geen vaste last meer op de core van de apps. Een rekenaar in
`system` krijgt op de C906B de idle van de kern (60,6 tegen 93,6 op een
eigen C906B); een rekenaar bij Hop op de C906L (groep `hop`) haalt 65,1,
precies de 700 van de 1000 MHz. Een job zonder tag valt bij de kern in
`system` (HOPOS_PLACE_SYSTEM), een job met de tag `hop` komt bij Hop op de
C906L, en een job die niet past geeft bij Hop één regel HOP_NO_CAPACITY
(R12, Hop 9c4a311, 03-10).

De vitals-reeks van 03-10 avond (R14, vitals 3.0.5, 32 MiB, alleen welcome
erbij, elke test twee keer; de cellen in de tabel Vitals): in `system` op
de C906B rekent vitals 87,7 Msteps/s, bij Hop op de C906L 62,6 (0,71, de
klokverhouding). Tijdens burn 30 s in `system` antwoordt welcome in 25 tot
60 ms (twaalf metingen, in rust 12 tot 21 ms), met vitals 100 % en welcome
3 tot 5 % op core 0, de kern 4 %, Hop 1 % op core 1, 57,0 tot 57,4 C.
Tijdens burn bij Hop antwoordt welcome in 13 tot 31 ms, met vitals 100 % op
core 1 naast Hop 1 %, de kern 5 %, welcome 0 tot 2 % op core 0, 56,3 tot
56,7 C. De rust na afloop (vitals weg, alleen welcome): de kern 5 / 4 / 4 %,
30,3 van 40 MiB, Hop 2 / 1 / 1 % met 0,55 tot 0,56 MiB, 57,0 C; Hop (slot 1)
in `HOPOS_SLOT_LOAD` 98 tot 99 % idle met 64 tot 146 wekken/s, welcome
(slot 2) 99 % met 46 tot 121. Geen `HOPOS_TICK` op de console: zonder
`hopos.tick=1` logt de kern alleen de eerste drie tikken na de boot, dus
busy_ms en `irq nic=` zijn deze keer niet gelezen. Twee bijvangsten: rtt
los gestart geeft vier keer van vier `errors=12` (p50 2,5 tot 2,9 ms),
binnen `all` nul fouten (p50 4,2 tot 4,5 ms); en memlat op de C906L valt
bij 128 KB al uit de cache (133 ns tegen 7,6 op de C906B).

## De O6N op O2 (03-10, main 8a91d57, Hop d785ef5)

Van de stick, koude boot, headfull met media; Hop in `system` op core 0
naast de kern (kern 4 tot 8 procent, Hop 4 tot 5 procent in rust), welcome
op core 1, 33 C via SCMI, canary OK, ringen write-back. vitals met één
core (`test=all`, 5 s): cpu 298,8 Msteps/s (burst 1755 us); smp 4 cores
speedup 3,99; burn 1663 Msteps/s zonder degradatie, 43 C; membw copy 15,8
GB/s, triad 10,8 GB/s; memlat 32 KB 2,8 ns, 2 MB 37,7 ns, 8 MB 38,5 ns;
alloc 6397 MB/s; disk 838 schrijven, 777 lezen, 4k 47,4 MB/s, vloer p50 75
us; storm 2457 conn/s p50 3,1 ms p99 5,3 ms; rtt naar de kern p50 715 us
p99 6,8 ms; timer overslaap 1 ms p50 48 us. Een vitals met vier cores
(cpu_shares 4096, 256 MB) in slot 3 draaide gewoon (de fout van 02-10,
"stopt na HOPOS_APP_MMU boven 3 GiB", kwam op O2 niet terug).

