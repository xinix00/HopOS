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
| idle (passief) | `curl 'http://NODE:8090/api/run?test=idle'` | `HOPOS_VITALS_IDLE span= wakes_per_s= idle_pct= wake_cost=` | `span=58.0 wakes_per_s=1060` (2 cores, idle_pct n.v.t.); 1 core: `wakes_per_s=1087 idle_pct=99.44 wake_cost=5.14` (30-09) | na de idle-fix (262ea1f, stempel I, nieuwe applib): `wakes_per_s=28.9` (2 cores), met cf21ac5 `wakes_per_s=21.6` en `dvfs: clock 600 MHz (quiet)`; ervoor `span=58.0 wakes_per_s=1011 idle_pct=99.0 wake_cost=10.3` (1 core; 2 cores: `wakes_per_s=1011`, idle_pct n/a) (30-09) | 1 core: `span=58.0 wakes_per_s=3003 idle_pct=97.9 wake_cost=7.11`; 2 cores: `span=58.0 wakes_per_s=2885` (idle_pct n/a) (30-09) | na de idle-fix (nieuwe applib, kern nog H): `wakes_per_s=51.5` (4 cores); ervoor stil: `wakes_per_s=` 3296 tot 3358 (som over 10 cores), idle_pct n/a (SMP) (30-09) |  |
| cpu | `curl 'http://NODE:8090/api/run?test=cpu'` | `HOPOS_VITALS_CPU rate= burst=` | `rate=286 burst=1836` (285,48 tot 285,52, 30-09) | `rate=250 burst=2100` (30-09) | `rate=136 burst=3858` (30-09) | `rate=855..856 burst=612..613` (30-09) |  |
| smp | `curl 'http://NODE:8090/api/run?test=smp'` | `HOPOS_VITALS_SMP cores= serial= parallel= speedup=` | `cores=2 serial=235 parallel=118 speedup=2.00` (drie keer, 30-09) | `cores=2 serial=269 parallel=134 speedup=2.00` (30-09) | `cores=2 serial=494 parallel=247 speedup=2.00` (30-09) | `cores=10 serial=77.7..77.9 parallel=22.8 speedup=3.42` (30-09) |  |
| burn (120 s) | `curl 'http://NODE:8090/api/run?test=burn'` | `HOPOS_VITALS_BURN cores= rate_start= rate_end= degradation= temp_max=`, elke 10 s `HOPOS_VITALS_BURN_TICK` | `cores=2 rate_start=571 rate_end=571 degradation=-0.01`, geen `temp_max` (CTRL_TEMP 0; de kern zag 57,0 tot 66,9 °C op 1500 MHz) (30-09) | `cores=2 rate_start=499 rate_end=499 degradation=0.02`, geen temp_max (CTRL_TEMP 0; kern 1500 MHz, max 67,1 °C) (30-09) | `cores=2 rate_start=272 rate_end=272 degradation=-0.01`, geen temp_max (geen sensor) (30-09) | `cores=10 rate_start=5883 rate_end=5883 degradation=0.00`, geen temp_max (CTRL_TEMP 0); kern 2600 MHz, 48 tot 52 °C (30-09) |  |
| membw | `curl 'http://NODE:8090/api/run?test=membw'` | `HOPOS_VITALS_MEMBW buffer= copy= triad=` | `buffer=15 copy=8.74 triad=6.89` (copy 8,72 tot 8,75, 30-09) | `buffer=15 copy=3.42 triad=2.82` (30-09) | `buffer=15 copy=3.03 triad=2.21` (30-09) | `buffer=16 copy=33.9..35.0 triad=25.9..26.2` (GB/s) (30-09) |  |
| memlat | `curl 'http://NODE:8090/api/run?test=memlat'` | `HOPOS_VITALS_MEMLAT ns_32k= ... ns_8m= furthest=` | `ns_32k=2.67 ns_128k=2.67 ns_512k=7.30 ns_2m=15.7 ns_8m=58.5 furthest=58.5` (8m 58,3 tot 58,5, 30-09) | `ns_32k=3.33 ns_128k=9.43 ns_512k=15.6 ns_2m=21.5 ns_8m=108 furthest=108` (30-09) | `ns_32k=3.68 ns_128k=25.5 ns_512k=43.1 ns_2m=130 ns_8m=186 furthest=186` (30-09) | `ns_32k=1.54 ns_128k=1.54 ns_512k=3.02 ns_2m=19.6 ns_8m=31.0..31.1` (30-09) |  |
| alloc | `curl 'http://NODE:8090/api/run?test=alloc'` | `HOPOS_VITALS_ALLOC alloc= allocs_per_s= failed= heap_peak=` | `alloc=10302 allocs_per_s=314401 failed=0 heap_peak=49414` (10219 tot 10302, 30-09) | `alloc=3384 allocs_per_s=103265 failed=0 heap_peak=50265` (30-09) | `alloc=2763 allocs_per_s=84330 failed=0 heap_peak=49410` (30-09) | `alloc=19289..19318 allocs_per_s=588655..589525 failed=0 heap_peak=54286..54826` (30-09) |  |
| rx | `curl 'http://NODE:8090/api/run?test=rx&url=http://192.168.1.208:8000/big.bin'` | `HOPOS_VITALS_RX throughput= read= header= conns=` | `throughput=40.8 read=32 header=12.2 conns=1` (zes runs 4,9 tot 50,0, laptop op Wi-Fi, 30-09) | `throughput=37.3 read=64 header=13.8 conns=1` (laptop, Wi-Fi); van de O6N over de draad `throughput=48.6 read=64 header=6.54 conns=1` (30-09) | van .208 (Wi-Fi): `throughput=19.2 read=32 header=12.3 conns=1`; over de draad van de O6N (`url=http://192.168.1.205:8090/blob?mb=32`): `throughput=21.6 read=32 header=6.22 conns=1` (30-09) | `throughput=11.7 / 38.5 / 40.4` MB/s van de laptop over Wi-Fi, 32 MB (30-09) |  |
| tx | `curl -o /dev/null http://NODE:8090/blob?mb=64` | `HOPOS_VITALS_TX throughput= sent=` | `throughput=33.4 sent=64` (31,3 tot 33,4, laptop op Wi-Fi, 30-09) | `throughput=39.4 sent=64` (Wi-Fi) (30-09) | `throughput=17.4 sent=64` (Wi-Fi-host) (30-09) | `throughput=10.3..12.8 sent=256` naar de laptop over Wi-Fi (30-09) |  |
| up | `apps/vitals/perf.sh NODE:8090 64` (PUTs van 1 MiB naar `/sink`) | `HOPOS_VITALS_UPLOAD throughput= received= requests=` | `throughput=26.8 received=64 requests=64` (14,1 tot 37,6 met stiltes van ~2 s, curl -T, laptop op Wi-Fi, 30-09) | `throughput=21.8 received=64 requests=64` (Wi-Fi) (30-09) | `throughput=16.3 received=64 requests=64` (Wi-Fi-host) (30-09) | `throughput=17.6..21.3 received=256 requests=256` van de laptop over Wi-Fi (30-09) |  |
| disk | `curl 'http://NODE:8090/api/run?test=disk'` (of `perf.sh`) | `HOPOS_VITALS_DISK write= read= write_4k= floor_p50= floor_p99= chunk=` | `HOPOS_VITALS_SKIP test=disk` (geen schijf, 30-09) | `HOPOS_VITALS_SKIP test=disk` (geen schijf) (30-09) | `HOPOS_VITALS_SKIP test=disk` (geen schijf) (30-09) | **AB-kern (main met heap, 01-10 middag, warme flip, vitals slot 2): `write=708 read=489..491 write_4k=50 floor_p50=59..60`; zonder heap (NH) `write=690..712 read=491..494 write_4k=52..53`: gelijk**; X-kern fb04980 (01-10 ochtend, koude boot): `write=992..1188 read=472..590` met vitals in slot 2, `read=373..376` in slot 4 (de plaatsing scheelt 30%), `write_4k` ~64, `floor_p50` 51 µs; `/data`, 256 MB (na de deur, H: `write=414 read=84.6 write_4k=45.0 floor_p50=72`): `write=196..199 read=92.3..93.6 write_4k=46.6..47.6 floor_p50=66 floor_p99=70..344 chunk=1024`; `kb=4`: `write=47.8..48.1 read=3.96..4.12`; `hole=1`: `read=298..303` (30-09) |  |
| storm | `curl 'http://NODE:8090/api/run?test=storm&addr=NODE_IP:8090'` | `HOPOS_VITALS_STORM rate= p50= p90= p99= errors=` | `rate=1402 p50=4.87 p90=5.01 p99=6.13 errors=0` (1398 tot 1410; de derde achter elkaar 94 tot 173 met p99 1006, `HOPOS_MASQ_SLOT_FULL`; in het slot 4906 tot 4940, 30-09) | `rate=3243 p50=1.98 p90=2.88 p99=3.04 errors=0` (eigen poort, zonder `addr`) (30-09) | `rate=1060 p50=7.66 p90=10.8 p99=11.8 errors=0` (eigen poort, zonder `addr`) (30-09) | eigen poort: `rate=9965..9996 p99=0.67..0.69 errors=0`; `addr=192.168.1.205:8090`: `rate=1656 / 1993 / 178 p99=4.42 / 4.39 / 1004 errors=0` (30-09) |  |
| rtt | `curl 'http://NODE:8090/api/run?test=rtt'` | `HOPOS_VITALS_RTT p50= p99= max= errors=` | `p50=1346 p99=28445 max=28445 errors=0` (p50 1265 tot 1348, 30-09) | `p50=1476 p99=6222 max=6222 errors=0` (30-09); na de deur van de switch (stempel H, dbc522f) `p50=133 p99=24718` | `p50=1984 p99=3507 max=3507 errors=0` (30-09); na de deur (H) `p50=365 p99=916`; met de idle-fix (I) `p50=268 p99=520` | `p50=1068..1071 p99=1083..1087 errors=0` (kern, stil; na de deur van de switch, stempel H: `p50=53 p99=64`); Hop 10.100.0.2:8080 `p50=2043..2046`; eigen poort `p50=19..20` (30-09) |  |
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
| De node in (`in`, MB/s) | O6N 111,3 tot 116,5; Altra 107,7 tot 110,8; Pi 5 57,0 tot 69,6; Radxa 55,8 tot 56,6; Pi 4 6,6 (2.2.6, gepold); M4 43,3 tot 46,7 (bundel 25, HTTP PUT van 64 MiB) | 122,3 (TCG, slirp) | node naar node over de draad (vitals rx, 256 MB, één verbinding, stempel G, 30-09): 76,4 tot 78,3 van de Pi 4 (4 verbindingen 78,1), 42,9 tot 43,3 van de Pi 5; geen peer zendt sneller, dus een ondergrens |  | node naar node over de draad (vitals rx, 256 MB, stempel dev, 30-09): 76,1 tot 85,2 van de O6N, 83,7 tot 92,8 van de Pi 4 | node naar node over de draad (vitals rx, 256 MB, stempel F, 30-09): 41,1 tot 43,8 van de O6N, 41,3 tot 42,5 van de Pi 5 (4 verbindingen 43,0); de ontvangkant is de grens | node naar node over de draad (vitals rx, stempel F, 30-09): 20,1 tot 20,5 van de O6N, 21,7 tot 21,9 van de Pi 4, 21,9 tot 22,1 van de Pi 5 (4 verbindingen 21,8) |
| De node uit (`out`, MB/s) | O6N 114,3 tot 117,6; Altra 108,0 tot 110,6; Pi 5 41,6 tot 50,6; Radxa 98,8 tot 99,6; Pi 4 42,3; M4 47,7 tot 52,3 (HTTP GET) | 71,4 (TCG, slirp) | node naar node over de draad (vitals rx op de peer, 256 MB, stempel G, 30-09): 76,1 tot 85,2 naar de Pi 5 (de snelste ontvanger), 41,1 tot 43,8 naar de Pi 4; drie ontvangers tegelijk samen ~90 (afgeleid uit de tx-markers); een ondergrens |  | node naar node over de draad (256 MB, stempel dev, 30-09): 42,9 tot 43,3 naar de O6N, 41,3 tot 42,5 naar de Pi 4; de zendkant is de grens | node naar node over de draad (256 MB, stempel F, 30-09): 76,4 tot 78,3 naar de O6N, 83,7 tot 92,8 naar de Pi 5 | node naar node over de draad (stempel F, 30-09): 21,2 tot 21,3 naar de O6N, 20,8 tot 21,0 naar de Pi 4, 21,4 tot 21,6 naar de Pi 5 |

## Latentie en verbindingen

| Meting | Go, per board | Commando, marker | v3 QEMU virt | v3 O6N | v3 Altra | v3 Pi 5 | v3 Pi 4 | v3 Radxa |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| rtt, open verbinding | O6N p50 156 / 201 / 200 µs (irq, drie runs); M4 p50 50 / 62 / 66 µs | `netmeter --phases rtt --rtt 200`, `NETMETER phase=rtt ... p50= p99=` | p50 1180 µs, p99 3472 µs | app naar app over de draad (`BENCH=ping` naar de bench op de Pi 5, stempel H, 30-09): p50 200 / 227 / 223 µs, p99 0,57 tot 2,6 ms; koud p50 197 tot 245 µs |  | app naar app over de draad (`BENCH=ping` naar de bench op de O6N, stempel dev, 30-09): p50 238 / 206 / 206 µs, p99 1,1 tot 1,4 ms; koud p50 1508 tot 1687 µs | app naar app over de draad (`BENCH=ping` naar de bench op de O6N, stempel H, 30-09): p50 1212 / 1213 / 1213 µs, p99 4,2 tot 10,9 ms; koud p50 1290 tot 1300 µs | app naar app over de draad (`BENCH=ping` naar de bench op de O6N, stempel I, 30-09): p50 289 / 289 / 287 µs, p99 1,0 tot 1,4 ms; koud p50 643 tot 788 µs |
| Verbindingscyclus, één tegelijk | O6N 3,6 ms; Altra 5,1 ms; Pi 5 1,1 tot 3,6 ms; Radxa 4,1 ms; LicheeRV 16 tot 19 ms | `netmeter --phases storm`, `p50=` | p50 1,44 ms | over de draad (vitals storm `n=1` naar de Pi 5, dial, GET, close; stempel H, 30-09): p50 2,43 ms, 381 conn/s (drie runs; de derde met een SYN na 1 s) |  | over de draad (vitals storm `n=1`, stempel dev, 30-09): naar de O6N p50 5,9 ms, na de koude herstart 2,36 tot 2,42 ms; naar de Pi 4 7,4 ms | over de draad (vitals storm `n=1`, 30-09): naar de O6N p50 4,85 (F) / 2,43 / 4,86 ms (H); naar de Pi 5 6,1 tot 7,3 ms (F) | over de draad (vitals storm `n=1`, 30-09): naar de O6N p50 4,67 / 1,78 / 4,37 ms (kaart-kern en H); naar de Pi 4 4,9 tot 6,0 ms (F) |
| Storm, verbindingen per seconde | O6N 844 conn/s, p99 19,6 ms (irq); M4 6379 conn/s, p99 1,9 ms | `netmeter --phases storm --storm 1000`, `conn_per_s=` | 638 conn/s, p99 2,7 ms | over de draad (vitals storm, 200 verbindingen, 8 werkers, stempel H, 30-09): naar de Pi 5 4976 / 99 / 3298 conn/s (p50 0,8 tot 1,0 ms; de 99 is een SYN na 1 s bij `HOPOS_MASQ_SLOT_FULL`); naar de Pi 4 3298, naar de Radxa 583 |  | over de draad (vitals storm, 200, 8 werkers, stempel dev, 30-09): naar de O6N 697, na de koude herstart 3284 / 4893 / 97 (SYN na 1 s); naar de Pi 4 612 tot 656 conn/s, p99 38 tot 45 ms | over de draad (vitals storm, 200, 8 werkers, 30-09): naar de O6N 970 (F) / 1398 / 696 / 606 (H), p99 24 tot 49 ms; naar de Pi 5 606 tot 652 | over de draad (vitals storm, 200, 8 werkers, 30-09): naar de O6N 756 / 1616 / 699 / 656 (kaart-kern en H), p99 7 tot 24 ms; naar de Pi 4 580 tot 617; naar de Pi 5 618 tot 699 (F) |
| UDP-echo, rtt en verlies | geen Go-getal | `netmeter --phases udp`, `NETMETER phase=udp ... lost=` | niet op QEMU (de hostfwd is TCP) | niet gemeten (vitals en bench hebben geen UDP-client over de draad) |  | niet gemeten | p50 5851 tot 6342 µs, lost 0 (Wi-Fi, geen lat) | p50 3220 tot 6760 µs, lost 0 (Wi-Fi, geen lat) |

## In de node: app naar app door de switch

| Meting | Go | Commando, marker | v3 QEMU virt | v3 O6N | v3 Altra | v3 Pi 5 | v3 Pi 4 | v3 Radxa |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Twee apps op één node, 400 MB | M4: 769 / 764 MB/s (v3 M4 01-10, M13 tot M15 met de slotstaart Normal en de core-start na een flip, 800 MiB: pull op een E-core 4375 tot 4404 en 3810 tot 4359, op een P-core 6043 tot 6123 MB/s, bad=0; M20 met de switch van ring naar ring: E 4448 tot 4468, P 6075 tot 6103; 40 GiB 2813; op M7 nog 52,05 / 52,10 met de tune en 47,11 met pstate=off) | `BENCH=pull BENCH_BYTES=419430400`, `HOPOS_BENCH_PULL` | 62,9 MB/s (16 MiB, TCG; met de deur van de switch, dbc522f; ervoor 6,9) | **1495 tot 1711 MB/s (X-kern fb04980 en de apps van dezelfde boom: memcpy, ringbelofte, frames in plaats, venster ring min twee frames, 01-10)**; 259,09 (K-kern na de koude boot, lean v3.1.3, 01-10); 141,50 / 141,50 / 143,72 (stempel H, met de deur, 30-09) |  | 6,53 / 6,64 / 6,64 MB/s (kaart-kern dev, zonder de deur, 30-09) | **434,80 / 407,06 / 407,48 MB/s (PR1, main efcd0d4 met de kopieën minder, 01-10: de OS-core vol, rxfull 0, de A72 op 1500 MHz is de grens)**; 461,47 / 423,66 / 421,66 (AA-kern 9e5d53a, main met de heap-crate, 01-10); 418 tot 463 (X-kern fb04980, 01-10); 271,79 (stempel K, lean v3.1.3, 01-10); 100,61 / 100,62 met lean v3.0.0 en v3.1.2 (de ontvangstring bleef op 16 KiB, zie ALLES.md); 26,68 / 23,62 / 25,38 (stempel H, met de deur, 30-09; eerder 25,9) | **257 tot 262 MB/s (RX1 en RX2, 01-10: de slotstaart Normal in de kernmap zoals M8 op de M4; 40 GiB op 261 MB/s zonder fouten; de OS-core vol op 816 MHz, de klok van de firmware)**; 29,83 (AB-kern, 01-10, met een corrupte RX-ring: de Radxa-kern mapt de pool Device, dus geen ringbelofte); 29,1 (lean v3.1.3, 01-10); 19,89 / 19,89 / 19,89 (stempel I, 30-09); vóór de deur 6,46 tot 6,49 |
| rtt app naar app, warm | geen Go-tabel (schedbench) | `BENCH=ping`, `HOPOS_BENCH_RTT` | p50 115 µs, p99 340 µs (met de deur van de switch, dbc522f; ervoor 2238 / 2447) | p50 44 / 45 / 39 µs, p99 53 tot 68 µs (H, 30-09) |  | p50 29 / 29 / 29 µs, p99 31 tot 41 µs (dev, 30-09) | p50 48 / 48 / 48 µs, p99 55 tot 84 µs (H, 30-09) | p50 77 / 77 / 77 µs, p99 122 tot 159 µs (I, 30-09); vóór de deur 78 tot 79 |
| rtt na 1 s stilte (koud) | geen Go-tabel | `BENCH=ping`, `HOPOS_BENCH_COLD` | p50 375 µs (dbc522f; ervoor 2494) | p50 57 / 51 / 56 µs (H, 30-09) |  | p50 1239 / 1239 / 1239 µs (dev, zonder de deur, 30-09) | p50 54 / 53 / 74 µs (H, 30-09) | p50 142 / 124 / 144 µs (I, 30-09); vóór de deur 599 tot 742 |

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
| Rauw, 1 MiB-opdrachten, 16 MiB, schrijven / lezen | O6N gecachet 3056,8 / 2705,6; ongecachet 49,1 / 59,1; M4 5389 tot 5756 / 1824 tot 1971 MB/s (v3 M4 01-10, M14 met het ANS-datablok write-back: 4936,1 / 1718,7; 256 KiB 4742,8 / 944,9; 64 KiB 4073,3 / 430,5; M23 met de ANS asynchroon: 4952 / 1925, 4 KiB schrijven 1160) | 1260,9 / 1221,4 MB/s | één boot met hopos.nvmebench=1 nodig (stick-cfg) | | |
| hopfs, 1 MiB-calls, 16 MiB | O6N 2970,1 / 2716,7; M4 5480 tot 5632 / 1781 tot 1816 MB/s (v3 M4 01-10, M14: 4931,1 / 1770,8; 64 KiB 3918,1 / 444,5; M23: 5015 / 2026) | 1187,0 / 1207,2 MB/s | één boot met hopos.nvmebench=1 nodig (stick-cfg) | | |
| Sequentieel over de staart (tot 1 GiB) | geen Go-getal (v3 M4 01-10, M14: 1 GiB 4803,9 / 1652,3 MB/s; M23: 4758 / 1629) | 32 MiB: 1293,0 / 1238,2 MB/s | één boot met hopos.nvmebench=1 nodig (stick-cfg) | | |
| Willekeurig 4 KiB | geen Go-getal (O6N: 4 KiB-overschrijvingen via de app 15,15 tot 17,08 MB/s; v3 M4 01-10, M14: 162315 / 11871 IOPS, 664,8 / 48,6 MB/s, M23 140k / 11,9k IOPS, één monster per boot; via de app 4 KiB-schrijven 90,5 tot 90,8 MB/s) | 32051 / 32123 IOPS | geen IOPS-meting; 4 KiB-overschrijvingen via de app 45,4 tot 48,9 MB/s (Go 15,15 tot 17,08); 4 KiB-lezen 3,96 tot 4,12 MB/s |  |  |
| Transport kern naar app zonder schijf (vitals `test=disk&mb=256&hole=1`, gaten uit het RAM van de kern) | geen Go-getal (v3 M4 01-10: 1554 MB/s op M15; 1681 op M17 met de switch die poort 0 in de ring leest; 1846 op M18 met de kernstack die in de host-ring zendt; 1858 tot 1861 op M21, 1879 tot 1917 op M24; de kosten per MiB op de OS-core van 0,645 naar 0,533 ms) | | | | |
| Replica (SQLite 3.53.4 op HopFS, `journal_mode=DELETE`, `synchronous=EXTRA`, elke sync een OP_SYNC) | Go: Spin op de M4 draaide, geen getal | geen QEMU-getal in deze tabel (`tools/qemu-persist.py` in de replica-repo) | | | |
| ... smoke (RAM-SQLite, twee slots) | | | M4 01-10 (M26): `REPLICA_SQLITE_OK` in slot 3 en 4, 33 writes, 6 syncs, 1,23 tot 1,33 ms | | |
| ... persist (volume op HopFS: schrijven, app-herstart, herstel, pipeline, reparatie, annulering, eigenaar) | | | M4 01-10 (M26): alle markers (`PERSIST_WRITE`, dan `PERSIST_READ`, `RESTORED_COLD_OK`, `PIPELINE_OK`, `PIPELINE_REPAIR_OK`, `SQLITE_CANCEL_OK`, `OWNER_OK`); de hele proef 190 tot 218 ms alleen, 255 tot 325 ms naast een app die 1 GiB schrijft en leest (die zakt dan van 1433 tot 1517 naar 866 tot 894 lezen); de koude herstart nog met de knop te doen | | |
| ... 4 KiB-schrijfjes als RPC-lus (vitals `write_4k`, het slechtste geval) | | | M4 01-10: 82 MB/s alleen (20.000 calls/s, 50 µs), 15 tot 19 naast een bulk-app | | |
| App-opslag, 256 MiB, 1 MiB-calls | O6N 556,83 tot 625,43 / 727,49 tot 796,83; M4 1598 tot 1657 / 1064 tot 1072 MB/s (v3 M4 01-10, M14 en M15 met vitals-M15 op twee P-cores: 1055 tot 1279 / 808 tot 886 MB/s, de schijf plus hopfs 0,59 ms per MiB en het transport kern naar app 0,66 ms per MiB liepen na elkaar; M24 met de ANS asynchroon, diepte 2 en read-ahead: 1269 tot 1270 / 1683 tot 1696, 1 GiB 1285 / 1590 tot 1633; zie ALLES.md) | nog geen v3-rol (bench heeft geen FS-rol) | 196,3 tot 199,2 / 92,3 tot 93,6 MB/s (vitals disk op `/data`, drie runs; ook via perf.sh 196,5 tot 198,0 / 93,0 tot 93,5); stempel H met vitals op 4 cores: 87,7 tot 88,6 / 87,7 tot 88,3 MB/s, 4 KiB-schrijven 32,4 tot 33,3 MB/s, stat-vloer p50 63 tot 72 µs (drie runs, 30-09) |  |  |

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
