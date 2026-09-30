# Metingen: de lat van de Go-generatie, en v3 ernaast

PORT.md §1: `measurements.md` is de lat, een Rust-HOP mag op geen gemeten
getal langzamer zijn. Deze pagina zet de getallen van de Go-generatie
(`OLD/docs/measurements.md`, september 2026) per meting naast een lege
kolom per board voor v3, met het commando dat het v3-getal oplevert en de
marker waar het op de console staat. Een devicedag vult de kolommen.

MB/s is decimaal (1.000.000 bytes per seconde), zoals in de Go-tabellen.
Let op: de Go-netmeter zelf rekende MiB/s; `tools/netmeter` rekent
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
de Go-vitals, per test een marker met de meetwaarden als `key=value`. Eerst
plaatsen, dan per test (of `test=all`) starten; het getal staat op de
console en in `GET /api/state`. Een cel is de marker-staart van één run op
dat board, met de datum.

| Wat | Commando | Marker | v3 Pi 5 | v3 Pi 4 | v3 Radxa | v3 O6N | v3 M4 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Plaatsen | `curl -X POST -d '{"name":"vitals","driver":"hop","artifacts":[{"url":"http://192.168.1.208:8000/vitals.elf"}],"memory_limit":134217728,"cpu_shares":2048,"ports":{"http":8090}}' http://NODE:9080/v1/jobs` | `HOPOS_VITALS_UP port=8090` | `port=8090`, 2 cores (30-09) | `port=8090`, 2 core(s), slot 2 (30-09) | `HOPOS_VITALS_UP port=8090`, 2 cores, slot 2 (30-09) | `HOPOS_VITALS_UP port=8090` (10 cores, 512 MiB, volume `/data`, 30-09) |  |
| Alles achter elkaar | `curl 'http://NODE:8090/api/run?test=all'` | `HOPOS_VITALS_ALL failed= skipped=` | `failed=1 skipped=1` in 133 s, drie keer (rx zonder DNS, disk zonder schijf) (30-09) | `failed=0 skipped=1` in 133 s (disk; met `mb=16&url=.../rx.bin`) (30-09) | `failed=0 skipped=1` (disk), 135 s, drie keer (30-09) | `HOPOS_VITALS_ALL failed=0 skipped=0`, 134 tot 136 s, drie keer (30-09) |  |
| idle (passief) | `curl 'http://NODE:8090/api/run?test=idle'` | `HOPOS_VITALS_IDLE span= wakes_per_s= idle_pct= wake_cost=` | `span=58.0 wakes_per_s=1060` (2 cores, idle_pct n.v.t.); 1 core: `wakes_per_s=1087 idle_pct=99.44 wake_cost=5.14` (30-09) | `span=58.0 wakes_per_s=1011 idle_pct=99.0 wake_cost=10.3` (1 core; 2 cores: `wakes_per_s=1011`, idle_pct n/a) (30-09) | 1 core: `span=58.0 wakes_per_s=3003 idle_pct=97.9 wake_cost=7.11`; 2 cores: `span=58.0 wakes_per_s=2885` (idle_pct n/a) (30-09) | stil: `wakes_per_s=` 3296 tot 3358 (som over 10 cores), idle_pct n/a (SMP) (30-09) |  |
| cpu | `curl 'http://NODE:8090/api/run?test=cpu'` | `HOPOS_VITALS_CPU rate= burst=` | `rate=286 burst=1836` (285,48 tot 285,52, 30-09) | `rate=250 burst=2100` (30-09) | `rate=136 burst=3858` (30-09) | `rate=855..856 burst=612..613` (30-09) |  |
| smp | `curl 'http://NODE:8090/api/run?test=smp'` | `HOPOS_VITALS_SMP cores= serial= parallel= speedup=` | `cores=2 serial=235 parallel=118 speedup=2.00` (drie keer, 30-09) | `cores=2 serial=269 parallel=134 speedup=2.00` (30-09) | `cores=2 serial=494 parallel=247 speedup=2.00` (30-09) | `cores=10 serial=77.7..77.9 parallel=22.8 speedup=3.42` (30-09) |  |
| burn (120 s) | `curl 'http://NODE:8090/api/run?test=burn'` | `HOPOS_VITALS_BURN cores= rate_start= rate_end= degradation= temp_max=`, elke 10 s `HOPOS_VITALS_BURN_TICK` | `cores=2 rate_start=571 rate_end=571 degradation=-0.01`, geen `temp_max` (CTRL_TEMP 0; de kern zag 57,0 tot 66,9 °C op 1500 MHz) (30-09) | `cores=2 rate_start=499 rate_end=499 degradation=0.02`, geen temp_max (CTRL_TEMP 0; kern 1500 MHz, max 67,1 °C) (30-09) | `cores=2 rate_start=272 rate_end=272 degradation=-0.01`, geen temp_max (geen sensor) (30-09) | `cores=10 rate_start=5883 rate_end=5883 degradation=0.00`, geen temp_max (CTRL_TEMP 0); kern 2600 MHz, 48 tot 52 °C (30-09) |  |
| membw | `curl 'http://NODE:8090/api/run?test=membw'` | `HOPOS_VITALS_MEMBW buffer= copy= triad=` | `buffer=15 copy=8.74 triad=6.89` (copy 8,72 tot 8,75, 30-09) | `buffer=15 copy=3.42 triad=2.82` (30-09) | `buffer=15 copy=3.03 triad=2.21` (30-09) | `buffer=16 copy=33.9..35.0 triad=25.9..26.2` (GB/s) (30-09) |  |
| memlat | `curl 'http://NODE:8090/api/run?test=memlat'` | `HOPOS_VITALS_MEMLAT ns_32k= ... ns_8m= furthest=` | `ns_32k=2.67 ns_128k=2.67 ns_512k=7.30 ns_2m=15.7 ns_8m=58.5 furthest=58.5` (8m 58,3 tot 58,5, 30-09) | `ns_32k=3.33 ns_128k=9.43 ns_512k=15.6 ns_2m=21.5 ns_8m=108 furthest=108` (30-09) | `ns_32k=3.68 ns_128k=25.5 ns_512k=43.1 ns_2m=130 ns_8m=186 furthest=186` (30-09) | `ns_32k=1.54 ns_128k=1.54 ns_512k=3.02 ns_2m=19.6 ns_8m=31.0..31.1` (30-09) |  |
| alloc | `curl 'http://NODE:8090/api/run?test=alloc'` | `HOPOS_VITALS_ALLOC alloc= allocs_per_s= failed= heap_peak=` | `alloc=10302 allocs_per_s=314401 failed=0 heap_peak=49414` (10219 tot 10302, 30-09) | `alloc=3384 allocs_per_s=103265 failed=0 heap_peak=50265` (30-09) | `alloc=2763 allocs_per_s=84330 failed=0 heap_peak=49410` (30-09) | `alloc=19289..19318 allocs_per_s=588655..589525 failed=0 heap_peak=54286..54826` (30-09) |  |
| rx | `curl 'http://NODE:8090/api/run?test=rx&url=http://192.168.1.208:8000/big.bin'` | `HOPOS_VITALS_RX throughput= read= header= conns=` | `throughput=40.8 read=32 header=12.2 conns=1` (zes runs 4,9 tot 50,0, laptop op Wi-Fi, 30-09) | `throughput=37.3 read=64 header=13.8 conns=1` (laptop, Wi-Fi); van de O6N over de draad `throughput=48.6 read=64 header=6.54 conns=1` (30-09) | van .208 (Wi-Fi): `throughput=19.2 read=32 header=12.3 conns=1`; over de draad van de O6N (`url=http://192.168.1.205:8090/blob?mb=32`): `throughput=21.6 read=32 header=6.22 conns=1` (30-09) | `throughput=11.7 / 38.5 / 40.4` MB/s van de laptop over Wi-Fi, 32 MB (30-09) |  |
| tx | `curl -o /dev/null http://NODE:8090/blob?mb=64` | `HOPOS_VITALS_TX throughput= sent=` | `throughput=33.4 sent=64` (31,3 tot 33,4, laptop op Wi-Fi, 30-09) | `throughput=39.4 sent=64` (Wi-Fi) (30-09) | `throughput=17.4 sent=64` (Wi-Fi-host) (30-09) | `throughput=10.3..12.8 sent=256` naar de laptop over Wi-Fi (30-09) |  |
| up | `apps/vitals/perf.sh NODE:8090 64` (PUTs van 1 MiB naar `/sink`) | `HOPOS_VITALS_UPLOAD throughput= received= requests=` | `throughput=26.8 received=64 requests=64` (14,1 tot 37,6 met stiltes van ~2 s, curl -T, laptop op Wi-Fi, 30-09) | `throughput=21.8 received=64 requests=64` (Wi-Fi) (30-09) | `throughput=16.3 received=64 requests=64` (Wi-Fi-host) (30-09) | `throughput=17.6..21.3 received=256 requests=256` van de laptop over Wi-Fi (30-09) |  |
| disk | `curl 'http://NODE:8090/api/run?test=disk'` (of `perf.sh`) | `HOPOS_VITALS_DISK write= read= write_4k= floor_p50= floor_p99= chunk=` | `HOPOS_VITALS_SKIP test=disk` (geen schijf, 30-09) | `HOPOS_VITALS_SKIP test=disk` (geen schijf) (30-09) | `HOPOS_VITALS_SKIP test=disk` (geen schijf) (30-09) | `/data`, 256 MB: `write=196..199 read=92.3..93.6 write_4k=46.6..47.6 floor_p50=66 floor_p99=70..344 chunk=1024`; `kb=4`: `write=47.8..48.1 read=3.96..4.12`; `hole=1`: `read=298..303` (30-09) |  |
| storm | `curl 'http://NODE:8090/api/run?test=storm&addr=NODE_IP:8090'` | `HOPOS_VITALS_STORM rate= p50= p90= p99= errors=` | `rate=1402 p50=4.87 p90=5.01 p99=6.13 errors=0` (1398 tot 1410; de derde achter elkaar 94 tot 173 met p99 1006, `HOPOS_MASQ_SLOT_FULL`; in het slot 4906 tot 4940, 30-09) | `rate=3243 p50=1.98 p90=2.88 p99=3.04 errors=0` (eigen poort, zonder `addr`) (30-09) | `rate=1060 p50=7.66 p90=10.8 p99=11.8 errors=0` (eigen poort, zonder `addr`) (30-09) | eigen poort: `rate=9965..9996 p99=0.67..0.69 errors=0`; `addr=192.168.1.205:8090`: `rate=1656 / 1993 / 178 p99=4.42 / 4.39 / 1004 errors=0` (30-09) |  |
| rtt | `curl 'http://NODE:8090/api/run?test=rtt'` | `HOPOS_VITALS_RTT p50= p99= max= errors=` | `p50=1346 p99=28445 max=28445 errors=0` (p50 1265 tot 1348, 30-09) | `p50=1476 p99=6222 max=6222 errors=0` (30-09) | `p50=1984 p99=3507 max=3507 errors=0` (30-09) | `p50=1068..1071 p99=1083..1087 errors=0` (kern, stil); Hop 10.100.0.2:8080 `p50=2043..2046`; eigen poort `p50=19..20` (30-09) |  |
| timer | `curl 'http://NODE:8090/api/run?test=timer'` | `HOPOS_VITALS_TIMER oversleep_1ms_p50= oversleep_1ms_p99= oversleep_5ms_p50= oversleep_20ms_p50=` | `oversleep_1ms_p50=213 oversleep_1ms_p99=352 oversleep_5ms_p50=1068 oversleep_20ms_p50=631` (30-09) | `oversleep_1ms_p50=213 oversleep_1ms_p99=213 oversleep_5ms_p50=1068 oversleep_20ms_p50=631` (30-09) | `oversleep_1ms_p50=19 oversleep_1ms_p99=678 oversleep_5ms_p50=232 oversleep_20ms_p50=153` (30-09) | `oversleep_1ms_p50=7..48 oversleep_1ms_p99=356..706 oversleep_5ms_p50=99..241 oversleep_20ms_p50=26..66` (30-09) |  |

Een fout is `HOPOS_VITALS_FAIL test=<naam>`, overslaan `HOPOS_VITALS_SKIP
test=<naam>` (smp met één core, disk op een node zonder opslag). Op QEMU is
WFE een no-op: de idle-rij is een ijzer-rij.

## Netwerk van de host naar een app (TCP)

De Go-kolom is de Vitals-fixture (één core, 512 MiB), gemeten van node naar
node over de draad met de M4 als vaste peer (L83 punt 42 tot 53, 21-09), tenzij
anders vermeld. De v3-meting is `netmeter NODE:80 --phases in,out --bytes
209715200 --repeat 3` vanaf een bedrade host.

| Meting | Go, per board | v3 QEMU virt | v3 O6N | v3 Altra | v3 Pi 5 | v3 Pi 4 | v3 Radxa |
| --- | --- | --- | --- | --- | --- | --- | --- |
| De node in (`in`, MB/s) | O6N 111,3 tot 116,5; Altra 107,7 tot 110,8; Pi 5 57,0 tot 69,6; Radxa 55,8 tot 56,6; Pi 4 6,6 (2.2.6, gepold); M4 43,3 tot 46,7 (bundel 25, HTTP PUT van 64 MiB) | 122,3 (TCG, slirp) |  |  | niet gemeten: netmeter mag het LAN niet op (macOS); vitals up van de laptop over Wi-Fi 14,1 tot 37,6 | 16,6 tot 20,4 (host via Wi-Fi, 200 MB, relay); bedraad van de O6N (vitals rx, 64 MB) 43,8 tot 48,6 | 10,2 tot 19,3 (Wi-Fi-host, 64 MiB; over de draad van de O6N: vitals rx 20,5 tot 21,6) |
| De node uit (`out`, MB/s) | O6N 114,3 tot 117,6; Altra 108,0 tot 110,6; Pi 5 41,6 tot 50,6; Radxa 98,8 tot 99,6; Pi 4 42,3; M4 47,7 tot 52,3 (HTTP GET) | 71,4 (TCG, slirp) |  |  | niet gemeten: idem; vitals tx over Wi-Fi 31,3 tot 33,4 | 45,0 tot 45,9 (host via Wi-Fi, 200 MB, relay) | 13,8 tot 17,9 (Wi-Fi-host, 64 MiB) |

## Latentie en verbindingen

| Meting | Go, per board | Commando, marker | v3 QEMU virt | v3 O6N | v3 Altra | v3 Pi 5 | v3 Pi 4 | v3 Radxa |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| rtt, open verbinding | O6N p50 156 / 201 / 200 µs (irq, drie runs); M4 p50 50 / 62 / 66 µs | `netmeter --phases rtt --rtt 200`, `NETMETER phase=rtt ... p50= p99=` | p50 1180 µs, p99 3472 µs |  |  | niet gemeten (netmeter: `No route to host`, macOS Local Network) | p50 5354 tot 5945 µs, p99 92 tot 103 ms (Wi-Fi) | p50 4176 tot 5040 µs, p99 90,9 tot 96,3 ms (Wi-Fi-host) |
| Verbindingscyclus, één tegelijk | O6N 3,6 ms; Altra 5,1 ms; Pi 5 1,1 tot 3,6 ms; Radxa 4,1 ms; LicheeRV 16 tot 19 ms | `netmeter --phases storm`, `p50=` | p50 1,44 ms |  |  | niet gemeten (idem) | p50 11,2 tot 15,5 ms (Wi-Fi plus relay) | p50 9,2 tot 10,9 ms (Wi-Fi-host) |
| Storm, verbindingen per seconde | O6N 844 conn/s, p99 19,6 ms (irq); M4 6379 conn/s, p99 1,9 ms | `netmeter --phases storm --storm 1000`, `conn_per_s=` | 638 conn/s, p99 2,7 ms |  |  | niet gemeten (idem); vitals storm door de NAT 1398 tot 1410 conn/s, p99 5,9 tot 6,9 ms | 27 tot 72 conn/s, p99 108 tot 165 ms (storm 200, Wi-Fi plus relay) | 71 tot 88 conn/s bij `--storm 200` (Wi-Fi-host) |
| UDP-echo, rtt en verlies | geen Go-getal | `netmeter --phases udp`, `NETMETER phase=udp ... lost=` | niet op QEMU (de hostfwd is TCP) |  |  | niet gemeten (idem) | p50 5851 tot 6342 µs, lost 0 (Wi-Fi) | p50 3220 tot 6760 µs, lost 0 (Wi-Fi-host) |

## In de node: app naar app door de switch

| Meting | Go | Commando, marker | v3 QEMU virt | v3 O6N | v3 Altra | v3 Pi 5 | v3 Pi 4 | v3 Radxa |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Twee apps op één node, 400 MB | M4: 769 / 764 MB/s | `BENCH=pull BENCH_BYTES=419430400`, `HOPOS_BENCH_PULL` | 62,9 MB/s (16 MiB, TCG; met de deur van de switch, dbc522f; ervoor 6,9) |  |  |  |  | 6,46 tot 6,49 MB/s |
| rtt app naar app, warm | geen Go-tabel (schedbench) | `BENCH=ping`, `HOPOS_BENCH_RTT` | p50 115 µs, p99 340 µs (met de deur van de switch, dbc522f; ervoor 2238 / 2447) |  |  |  |  | p50 78 tot 79 µs, p99 123 tot 174 µs |
| rtt na 1 s stilte (koud) | geen Go-tabel | `BENCH=ping`, `HOPOS_BENCH_COLD` | p50 375 µs (dbc522f; ervoor 2494) |  |  |  |  | p50 599 tot 742 µs |

## De idle-meetlat van de OS-core

`hopos.idlestat=1`, stilte (geen verkeer, Hop en één app), de regel
`idle: N wakes/s ... HOPOS_IDLESTAT`. De kolommen die tellen: wekken per
seconde, lege RX-rondes per seconde, werk na de failsafe per seconde.

| Meting | Go | v3 QEMU virt | v3 O6N | v3 Altra | v3 Pi 5 | v3 Pi 4 | v3 Radxa |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Wekken per seconde, stil | gepold ~3.300, op de interrupt ~100 (de vangrail) | ~820 |  |  | ~900 (`sleeps`), ~2.000 polls/s, ~70 NIC-irq/s; tijdens vitals burn ~1.900 |  | idlestat uit; `HOPOS_TICK`: ~1.700 tot 1.900 slaapjes/s, NIC-irq ~75/s |
| Lege RX-rondes per seconde, stil | gepold 3.333, op de interrupt ~100 | ~94 | | | | | |
| RX-pomprondes Pi 5, stil | 824 (gepold), 113 (irq) |  |  |  | ~70 NIC-interrupts/s, ~2.000 polls/s |  |  |
| NIC-interrupts, O6N | 1.813 in 40 s met een rtt-run; 3.333 polls/s gepold | | | | | | |
| Tijd van de bewoners (Hop) op de OS-core, stil | geen Go-getal | 0,3 % |  |  |  |  | 0,14 % (`HOPOS_TICK` res_ms 1,4 ms/s) |

Tijdens `BURN=1` op een app-core hoort de regel van de OS-core niet te
bewegen (de last staat op een andere core); stijgen de wekken of de kicks
van de OS-core met de last mee, dan lekt de app-core naar de kern.

## Opslag

`hopos.nvmebench=1`. De Go-regels waren 16 MiB per commandomaat; v3 drukt
dezelfde regels (`nvme bench: 1024 KiB x16 ...`) plus sequentieel over de
staart (hoogstens 1 GiB) en 4 KiB willekeurig.

| Meting | Go | v3 QEMU virt (virtio-blk, 64 MiB-schijf) | v3 O6N | v3 Altra | v3 Pi 5 |
| --- | --- | --- | --- | --- | --- |
| Rauw, 1 MiB-opdrachten, 16 MiB, schrijven / lezen | O6N gecachet 3056,8 / 2705,6; ongecachet 49,1 / 59,1; M4 5389 tot 5756 / 1824 tot 1971 MB/s | 1260,9 / 1221,4 MB/s | | | |
| hopfs, 1 MiB-calls, 16 MiB | O6N 2970,1 / 2716,7; M4 5480 tot 5632 / 1781 tot 1816 MB/s | 1187,0 / 1207,2 MB/s | | | |
| Sequentieel over de staart (tot 1 GiB) | geen Go-getal | 32 MiB: 1293,0 / 1238,2 MB/s | | | |
| Willekeurig 4 KiB | geen Go-getal (O6N: 4 KiB-overschrijvingen via de app 15,15 tot 17,08 MB/s) | 32051 / 32123 IOPS | geen IOPS-meting; 4 KiB-overschrijvingen via de app 45,4 tot 48,9 MB/s (Go 15,15 tot 17,08); 4 KiB-lezen 3,96 tot 4,12 MB/s |  |  |
| App-opslag, 256 MiB, 1 MiB-calls | O6N 556,83 tot 625,43 / 727,49 tot 796,83; M4 1598 tot 1657 / 1064 tot 1072 MB/s | nog geen v3-rol (bench heeft geen FS-rol) | 196,3 tot 199,2 / 92,3 tot 93,6 MB/s (vitals disk op `/data`, drie runs; ook via perf.sh 196,5 tot 198,0 / 93,0 tot 93,5) |  |  |

## Media: de hardwaredecoder van de O6N

| Meting | Go | Commando, marker | v3 O6N |
| --- | --- | --- | --- |
| 4K P010 door de grant, apps/decode via Hop | 27,25 fps met Lumen op GAMEOFTHRONES_S1_D1 (27-09, met de software-encoder erachter); de lat is 24 fps = 597 MB/s | `POST /v1/jobs` met `decode.elf` en `DECODE_URL` (apps/decode/README.md), `HOPOS_DECODE fps= MBps=` | **85,7 fps, 2134 MB/s**: 240 beelden 3840x2160 p010 (14 MB x265-testclip van de laptop) in 2797 ms (30-09). Eerst 43,1 fps op 10 beelden, en een fault na 14 beelden zolang decode zijn happen van 1 MB midden in een NAL-eenheid knipte; sinds de knip op de laatste startcode heel |

## Wat hier (nog) niet gemeten wordt

- De kern-flip: de echo-rtt tijdens een flip (Go, Pi 4: 4,3 tot 8,0 s). `tools/soak.sh` met `FLIP=1` telt de flips en de overleving, niet de rtt.
- Geheugenbudgetten (Go: de LicheeRV-vensters): de kern-heap staat in de tweede `idle:`-regel (`kern heap ... used`), de app-heap in `HOPOS_BENCH_THRASH`.
- Watchdog-herstel en energie: een meting met een stopwatch en een meter, niet met een tool.

## QEMU is geen ijzer

De QEMU-kolom (29-09, `sh tools/qemu-test-bench.sh`, TCG op een Mac M-serie,
4 cores, virtio-net via slirp) bewijst de keten, geen plafond: slirp
termineert TCP op de host, TCG rekent in software, en de vier vCPU's delen
de host. Dat app naar app in de node (6,9 MB/s) trager is dan host naar app
(71 tot 122 MB/s), zegt op QEMU vooral dat twee vCPU's elkaar via de kern
wekken; op ijzer is dat precies het getal om te vergelijken met de 769 MB/s
van de M4.
