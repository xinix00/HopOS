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
| De idle-meetlat van de OS-core | `hopos.idlestat=1` in `hopos.cfg` of de cmdline (een getal N: elke N s) | `HOPOS_IDLESTAT` |
| Schijf rauw en door hopfs | `hopos.nvmebench=1` (een meet-boot: de schijf blijft bij de bench) | `HOPOS_NVMEBENCH`, `_SEQ`, `_RAND` |
| De keten op QEMU | `sh tools/qemu-test-bench.sh` | `bench-keten groen` |
| Uren lang plaatsen en stoppen | `sh tools/soak.sh NODE` (`FLIP=1` voor de flip erbij) | `SOAK` per ronde, het rapport aan het eind |

De bench is `apps/bench` (de protocoltabel staat in `apps/bench/src/proto.rs`),
de host-kant `tools/netmeter`, de kern-kant `hopos/src/bench.rs`.

## Netwerk van de host naar een app (TCP)

De Go-kolom is de Vitals-fixture (één core, 512 MiB), gemeten van node naar
node over de draad met de M4 als vaste peer (L83 punt 42 tot 53, 21-09), tenzij
anders vermeld. De v3-meting is `netmeter NODE:80 --phases in,out --bytes
209715200 --repeat 3` vanaf een bedrade host.

| Meting | Go, per board | v3 QEMU virt | v3 O6N | v3 Altra | v3 Pi 5 | v3 Pi 4 | v3 Radxa |
| --- | --- | --- | --- | --- | --- | --- | --- |
| De node in (`in`, MB/s) | O6N 111,3 tot 116,5; Altra 107,7 tot 110,8; Pi 5 57,0 tot 69,6; Radxa 55,8 tot 56,6; Pi 4 6,6 (2.2.6, gepold); M4 43,3 tot 46,7 (bundel 25, HTTP PUT van 64 MiB) | 122,3 (TCG, slirp) | | | | | |
| De node uit (`out`, MB/s) | O6N 114,3 tot 117,6; Altra 108,0 tot 110,6; Pi 5 41,6 tot 50,6; Radxa 98,8 tot 99,6; Pi 4 42,3; M4 47,7 tot 52,3 (HTTP GET) | 71,4 (TCG, slirp) | | | | | |

## Latentie en verbindingen

| Meting | Go, per board | Commando, marker | v3 QEMU virt | v3 O6N | v3 Altra | v3 Pi 5 | v3 Pi 4 | v3 Radxa |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| rtt, open verbinding | O6N p50 156 / 201 / 200 µs (irq, drie runs); M4 p50 50 / 62 / 66 µs | `netmeter --phases rtt --rtt 200`, `NETMETER phase=rtt ... p50= p99=` | p50 1180 µs, p99 3472 µs | | | | | |
| Verbindingscyclus, één tegelijk | O6N 3,6 ms; Altra 5,1 ms; Pi 5 1,1 tot 3,6 ms; Radxa 4,1 ms; LicheeRV 16 tot 19 ms | `netmeter --phases storm`, `p50=` | p50 1,44 ms | | | | | |
| Storm, verbindingen per seconde | O6N 844 conn/s, p99 19,6 ms (irq); M4 6379 conn/s, p99 1,9 ms | `netmeter --phases storm --storm 1000`, `conn_per_s=` | 638 conn/s, p99 2,7 ms | | | | | |
| UDP-echo, rtt en verlies | geen Go-getal | `netmeter --phases udp`, `NETMETER phase=udp ... lost=` | niet op QEMU (de hostfwd is TCP) | | | | | |

## In de node: app naar app door de switch

| Meting | Go | Commando, marker | v3 QEMU virt | v3 O6N | v3 Altra | v3 Pi 5 | v3 Pi 4 | v3 Radxa |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Twee apps op één node, 400 MB | M4: 769 / 764 MB/s | `BENCH=pull BENCH_BYTES=419430400`, `HOPOS_BENCH_PULL` | 6,9 MB/s (16 MiB, TCG) | | | | | |
| rtt app naar app, warm | geen Go-tabel (schedbench) | `BENCH=ping`, `HOPOS_BENCH_RTT` | p50 2238 µs, p99 2447 µs | | | | | |
| rtt na 1 s stilte (koud) | geen Go-tabel | `BENCH=ping`, `HOPOS_BENCH_COLD` | p50 2494 µs | | | | | |

## De idle-meetlat van de OS-core

`hopos.idlestat=1`, stilte (geen verkeer, Hop en één app), de regel
`idle: N wakes/s ... HOPOS_IDLESTAT`. De kolommen die tellen: wekken per
seconde, lege RX-rondes per seconde, werk na de failsafe per seconde.

| Meting | Go | v3 QEMU virt | v3 O6N | v3 Altra | v3 Pi 5 | v3 Pi 4 | v3 Radxa |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Wekken per seconde, stil | gepold ~3.300, op de interrupt ~100 (de vangrail) | ~820 | | | | | |
| Lege RX-rondes per seconde, stil | gepold 3.333, op de interrupt ~100 | ~94 | | | | | |
| RX-pomprondes Pi 5, stil | 824 (gepold), 113 (irq) | | | | | | |
| NIC-interrupts, O6N | 1.813 in 40 s met een rtt-run; 3.333 polls/s gepold | | | | | | |
| Tijd van de bewoners (Hop) op de OS-core, stil | geen Go-getal | 0,3 % | | | | | |

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
| Willekeurig 4 KiB | geen Go-getal (O6N: 4 KiB-overschrijvingen via de app 15,15 tot 17,08 MB/s) | 32051 / 32123 IOPS | | | |
| App-opslag, 256 MiB, 1 MiB-calls | O6N 556,83 tot 625,43 / 727,49 tot 796,83; M4 1598 tot 1657 / 1064 tot 1072 MB/s | nog geen v3-rol (bench heeft geen FS-rol) | | | |

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
