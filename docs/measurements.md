# Metingen per board

PORT.md §1: deze pagina is de lat. Een Rust-HOP mag op geen gemeten getal
langzamer zijn dan v2. Per board staat de laatste v3-meting naast het
v2-getal.

## Zo schrijf je deze pagina

1. **Eén sectie per board**, altijd in deze volgorde: O6N, Altra, Pi 5,
   Pi 4, Radxa, LicheeRV, M4, QEMU. Een nieuw board komt erbij als nieuwe
   sectie met dezelfde opbouw.
2. **Elke sectie heeft dezelfde opbouw:** één regel *Opzet* (de stempel,
   de datum, de commit als je die weet, en hoe vitals geplaatst is), dan de
   tabellen *Vitals*, *Netwerk*, *In de node*, *In rust*, en waar ze er
   zijn *Opslag* en *Media*. Daarna hoogstens drie zinnen *Lezing* en een
   lijst *Bord*.
3. **Elke tabel heeft vier kolommen:** `Meting | v3 | v2 | Run`. In *v3*
   staat het getal, in *v2* het v2-getal van dit board (de lat, leeg als
   v2 er geen had), en in *Run* de stempel en de datum.
4. **Alleen de laatste meting.** Een nieuwe meting vervangt de cel. Zet er
   geen "eerder", "ervoor" of reeks van stempels bij: die geschiedenis
   staat in git en in ALLES.md. Het v2-getal blijft wel staan.
5. **Eén run per punt**, vitals met `secs=5`, bench met één pull per
   richting. Doe een tweede run alleen bij een vreemd getal, en zet de
   reden erbij. Heb je toch meer runs, schrijf dan een bereik (`783–810`),
   nooit alleen de beste.
6. **Getallen kaal**, met de eenheid in de naam van de rij. Gebruik een
   decimale komma en een punt voor duizendtallen (`302.439`). MB/s is
   decimaal (1.000.000 bytes per seconde). De netmeter van v2 rekende
   MiB/s; `tools/netmeter` rekent decimaal.
7. **Een opmerking in een cel mag hoogstens een halve regel zijn**, tussen
   haakjes, en alleen als hij het getal verklaart: wat de grens is, welke
   peer, welke core. Verhalen horen in ALLES.md of in de boards-*.md.
8. **Wat niet gemeten is, laat je weg.** Er is één uitzondering: een rij
   met een v2-getal blijft staan, met `—` bij v3, zodat de open lat
   zichtbaar is. Wat nog moet komt in de regel *Nog te meten*.
9. **Geen Wi-Fi-getallen.** Een meting van of naar de laptop over Wi-Fi
   meet de Wi-Fi, niet het board. Meet over de draad, node naar node of
   vanaf een bedrade host.
10. **Een meting die op elk board hetzelfde is** (een faalreden, een
    eigenschap van de tool) komt één keer onder *Gereedschap*, niet bij
    elk board.

## Gereedschap

| Wat | Commando | Marker |
| --- | --- | --- |
| Vitals plaatsen | `curl -X POST -d '{"name":"vitals","driver":"hop","artifacts":[{"url":"http://LAPTOP:8000/vitals.elf"}],"memory_limit":134217728,"cpu_shares":2048,"ports":{"http":8090}}' http://NODE:9080/v1/jobs` | `HOPOS_VITALS_UP port=8090` |
| Vitals, alles | `curl 'http://NODE:8090/api/run?test=all&secs=5'` | `HOPOS_VITALS_ALL failed= skipped=` |
| Vitals, één test | `curl 'http://NODE:8090/api/run?test=<naam>'` (cpu, smp, burn, membw, memlat, alloc, storm, rtt, timer, idle, disk, rx) | `HOPOS_VITALS_<TEST> key=value ...`, `HOPOS_VITALS_FAIL`, `HOPOS_VITALS_SKIP` |
| De bench in een slot (server) | `curl -X POST -d '{"name":"bench","driver":"hop","artifacts":[{"url":"http://LAPTOP:8000/bench.elf"}],"memory_limit":67108864,"ports":{"http":80}}' http://NODE:9080/v1/jobs` | `HOPOS_BENCH_UP role=serve port=80` |
| Van de host naar een app | `cargo run --release -p netmeter -- NODE:80 --phases in,out,rtt,storm,udp --json > run.json` | `NETMETER phase=...` |
| App naar app (in de node of over de draad) | job met `"env":{"BENCH":"pull","BENCH_PEER":"IP:80","BENCH_BYTES":"419430400"}` (of `ping`, `push`) | `HOPOS_BENCH_PULL`, `HOPOS_BENCH_RTT`, `HOPOS_BENCH_COLD` |
| Last op een app-core | job met `"env":{"BURN":"1"}` (`BURN_WORK`, `BURN_REST` in s) | `HOPOS_BENCH_BURN` |
| Geheugen onder druk | job met `"env":{"THRASH":"1"}` | `HOPOS_BENCH_THRASH` |
| Multicast tussen twee apps | een job met `"env":{"MCAST":"listen"}`, dan een met `"env":{"MCAST":"send"}` | `HOPOS_BENCH_MCAST recv=N` |
| De OS-core in rust | `hopos.idlestat=1` in `hopos.cfg` of op de cmdline | `HOPOS_IDLESTAT` |
| Schijf, rauw en door hopfs | `hopos.nvmebench=1`, of de feature `nvmebench` in een meetkern | `HOPOS_NVMEBENCH`, `_SEQ`, `_RAND` |
| Kern en Hop in rust | `GET :9080/v1/agents` (`kern_cpu_percent`, `kern_mem_bytes`, `hop_cpu_percent`, `hop_mem_bytes`, `temp_milli_c`), drie peilingen 10 s uit elkaar, alleen welcome geplaatst | |
| De keten op QEMU | `sh tools/qemu-test-bench.sh` | `bench-keten groen` |
| Uren lang plaatsen en stoppen | `sh tools/soak.sh NODE` (`FLIP=1` voor de flip erbij) | `SOAK` |

Code: `apps/vitals` (README daar), `apps/bench` (protocol in
`apps/bench/src/proto.rs`), `tools/netmeter`, `hopos/src/bench.rs`.

Overal hetzelfde:
- `test=rx` faalt zonder `url`. De standaardbron (cachefly) vraagt een
  tunnel: `http: CONNECT is not supported`. Daarom geeft `test=all` met
  één core `failed=1` (rx) en `skipped=2` (smp met één core, disk zonder
  schijf).
- De vitals-*storm* gaat naar de eigen poort, de vitals-*rtt* naar de kern
  (10.100.0.1:10100).
- Op QEMU is WFE een no-op. Rijen over idle tellen alleen op ijzer.

## Orion O6N

Opzet: O2, 03-10, main 8a91d57, Hop d785ef5. Vitals met 1 core, 128 MiB,
slot 3. Zonder `core-class` krijgt een job de eerste vrije core, en dat is
een kleine A520 (4 klein, 7 groot).

**Vitals**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| cpu, Msteps/s | 298 (A520); 754 met `core-class: big` | | O2 03-10; O3 03-10 |
| smp, speedup | 3,99 (4 cores) | | O2 03-10 |
| burn, Msteps/s, max °C | 299 → 299, 41 °C; 4 cores 1198 | | O2 03-10 |
| membw copy / triad, GB/s | 16,7 / 11,4 | | O2 03-10 |
| memlat 32 KB / 2 MB / 8 MB, ns | 2,76 / 38,9 / 38,9 | | O2 03-10 |
| alloc, allocs/s | 191.466 | | O2 03-10 |
| storm, conn/s (p99 ms) | 2348 (5,38) | | O2 03-10 |
| rtt naar de kern p50 / p99, µs | 718 / 9471 | | O2 03-10 |
| timer 1 ms, overslaap p50 / p99, µs | 826 / 1193 (4 cores: 48 / 220) | | O2 03-10 |
| idle, wekken/s | 51,5 (4 cores) | | na de idle-fix, 30-09 |

**Netwerk**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| De node in, MB/s | 76,4–78,3 (van de Pi 4; geen peer zendt sneller: ondergrens) | 111,3–116,5 | G 30-09 |
| De node uit, MB/s | 76,1–85,2 (naar de Pi 5; ondergrens) | 114,3–117,6 | G 30-09 |
| rtt over de draad p50, µs | 200–227 (naar de Pi 5), koud 197–245 | 156–201 | H 30-09 |
| Verbindingscyclus p50, ms | 2,43 (naar de Pi 5) | 3,6 | H 30-09 |
| Storm over de draad, conn/s | 3298–4976 (naar de Pi 5) | 844 (p99 19,6 ms) | H 30-09 |

**In de node**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| App naar app, MB/s | 1686–1689 | | O2 03-10 |
| rtt warm p50 / p99, µs | 55 / 193 | | O16h 04-10, main 1e652b5 |
| rtt koud p50, µs | 60 | | O16h 04-10 |

**In rust**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Kern-cpu | 4 % | | O2 03-10 |
| Kern-geheugen | 37,5 van 224 MiB (16,7 %) | | O2 03-10 |
| Hop | 1 %, 0,44–0,46 MiB, core 0 | | O2 03-10 |
| Temperatuur | 41,0 °C | | O2 03-10 |

**Opslag**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Rauw 1 MiB, schrijven / lezen, MB/s | — | 3056,8 / 2705,6 (gecachet) | |
| hopfs 1 MiB, schrijven / lezen, MB/s | — | 2970,1 / 2716,7 | |
| App-opslag via vitals disk, schrijven / lezen, MB/s | 714 / 748 (64 MB) | 556,8–625,4 / 727,5–796,8 (256 MiB) | O2 03-10 |
| 4 KiB schrijven via de app, MB/s | 42,3 | 15,15–17,08 | O2 03-10 |

**Media**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| 4K P010 door de grant (apps/decode), fps | 85,7 (2134 MB/s) | 27,25 (lat 24 fps) | 30-09 |

Lezing: de 298 tegen 855 op 30-09 is de kleine core, geen regressie. Sinds
68a86c6 klokt de O6N binnen één ronde naar vol als er een rekenaar in een
slot staat.

Nog te meten: een boot met `nvmebench` (rauw, hopfs, 4 KiB IOPS).

Bord:
- Boot: VHE, kick via SGI 1. Timer: CNTHP (PPI 26).
- NIC: RTL8125B, MSI-X via de ITS (LPI 8192).
- Watchdog: SBSA, 8,5 s.
- RNG: geen FEAT_RNG of SMCCC-TRNG; de kern gebruikt EFI_RNG_PROTOCOL
  (`hopos.efirng=1`), de slots jitter.
- Temperatuur: SCMI. Klokbeleid: vijf `_CPC`-domeinen.
- Console op het glas: GOP 1920x1080. USB: 10 xHCI's, Blu-ray over
  USB-BOT.
- Opslag: NVMe Lexar 4 TB. Hardwaredecoder: Linlon V8.

## Ampere Altra

Opzet: A4, 03-10. Vitals met 1 core, 128 MiB, slot 3; `vitals4` op :8091
met 4 cores en 256 MiB.

**Vitals**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| cpu, Msteps/s | 494 | | A4 03-10 |
| smp, speedup | 4,01 (4 cores) | | A4 03-10 |
| burn, Msteps/s, max °C | 494 → 494, 46 °C; 4 cores 1979 | | A4 03-10 |
| membw copy / triad, GB/s | 25,9 / 24,0 | | A4 03-10 |
| memlat 32 KB / 2 MB / 8 MB, ns | 1,54 / 5,46 / 27,6 | | A4 03-10 |
| alloc, allocs/s | 557.016 | | A4 03-10 |
| storm, conn/s (p99 ms) | 9958 (0,92) | | A4 03-10 |
| rtt naar de kern p50 / p99, µs | 110 / 5179 | | A4 03-10 |
| timer 1 ms, overslaap p50 / p99, µs | 236 / 1022 | | A4 03-10 |
| disk schrijven / lezen, MB/s | 873 / 1062 (NVMe SN770, 64 MB; 4 KiB 74,5) | | A4 03-10 |

**Netwerk**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| De node in, MB/s | — | 107,7–110,8 | |
| De node uit, MB/s | — | 108,0–110,6 | |
| Verbindingscyclus p50, ms | — | 5,1 | |

**In de node**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| App naar app, MB/s | 4970–4972 | | A4 03-10 |
| rtt warm p50 / p99, µs | 21 / 37 | | A4 03-10 |
| rtt koud p50, µs | 31–47 | | A4 03-10 |

**In rust**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Kern-cpu | 5 % | | A4 03-10 |
| Kern-geheugen | 31,8 van 224 MiB (14,2 %) | | A4 03-10 |
| Hop | 1 %, 0,46 MiB, core 0 | | A4 03-10 |
| Temperatuur | 46,0 °C | | A4 03-10 |

Nog te meten: het netwerk over de draad, en een boot met `nvmebench`.

Bord:
- NIC: igb, bewust gepold (de `_PRT`-INTx doodt de SoC). Kick via SGI 1.
- Watchdog: SBSA. RNG: SMCCC-TRNG of `rndr`. Temperatuur: SMpro.
- Console op het glas: GOP. Opslag: NVMe.

## Raspberry Pi 5

Opzet: P4, 03-10. Vitals met 1 core, 128 MiB, slot 3.

**Vitals**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| cpu, Msteps/s | 284 | | P4 03-10 |
| smp, speedup | 2,00 (2 cores) | | 30-09 |
| burn, Msteps/s, max °C | 285 → 285, 56,0 °C | | P4 03-10 |
| membw copy / triad, GB/s | 8,27 / 6,88 | | P4 03-10 |
| memlat 32 KB / 2 MB / 8 MB, ns | 2,67 / 16,5 / 58,9 | | P4 03-10 |
| alloc, allocs/s | 302.439 | | P4 03-10 |
| storm, conn/s (p99 ms) | 4777 (1,58) | | P4 03-10 |
| rtt naar de kern p50 / p99, µs | 167 / 9134 | | P4 03-10 |
| timer 1 ms, overslaap p50 / p99, µs | 213 / 332 | | P4 03-10 |
| idle, wekken/s | 1087 (vóór de idle-fix) | | 30-09 |

**Netwerk**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| De node in, MB/s | 102,4 (bench pull van de O6N, 256 MiB) | 57,0–69,6 | P4 03-10 |
| De node uit, MB/s | 51,9 (bench push naar de O6N; de GEM zendt nog uit NC) | 41,6–50,6 | P4 03-10 |
| rtt over de draad p50, µs | 206–238 (naar de O6N), koud 1508–1687 | | dev 30-09 |
| Verbindingscyclus p50, ms | 2,36–2,42 (naar de O6N) | 1,1–3,6 | dev 30-09 |
| Storm over de draad, conn/s | 3284–4893 (naar de O6N) | | dev 30-09 |

**In de node**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| App naar app, MB/s | 783–810 | | P4 03-10 |
| rtt warm p50 / p99, µs | 28 / 35–38 | | P4 03-10 |
| rtt koud p50, µs | 31–34 | | P4 03-10 |
| OS-core stil | ~900 slaapjes/s, ~70 NIC-irq/s | 113 RX-rondes/s (irq) | idlestat |

**In rust**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Kern-cpu | 2 % | | P4 03-10 |
| Kern-geheugen | 30,3 van 127,5 MiB (23,7 %) | | P4 03-10 |
| Hop | 1 %, 0,42–0,44 MiB, core 0 | | P4 03-10 |
| Temperatuur | 50,5–51,0 °C | | P4 03-10 |

Lezing: uit is de helft van in. De GEM zendt met losse woorden uit NC,
dezelfde vorm als de dwmac4 van de Radxa vóór X2.

Bord:
- Warme flip: sterft na de landing (F en H, drie keer).
- Console op het glas: gezien via een flip op de eerste kaart. Sinds de
  herflash weigert de firmware.

## Raspberry Pi 4

Opzet: P2g, 03-10. Vitals met 1 core, 128 MiB, slot 3
(`vitals-arm64.elf` van main).

**Vitals**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| cpu, Msteps/s | 248 | | P2g 03-10 |
| smp, speedup | 2,00 (2 cores) | | 30-09 |
| burn, Msteps/s, max °C | 249 → 249, 54,5 °C | | P2g 03-10 |
| membw copy / triad, GB/s | 3,42 / 2,80 | | P2g 03-10 |
| memlat 32 KB / 2 MB / 8 MB, ns | 3,33 / 28,3 / 111 | | P2g 03-10 |
| alloc, allocs/s | 100.921 | | P2g 03-10 |
| storm, conn/s (p99 ms) | 3204 (2,91) | | P2g 03-10 |
| rtt naar de kern p50 / p99, µs | 323 / 5543 | | P2g 03-10 |
| timer 1 ms, overslaap p50 / p99, µs | 213 / 215 | | P2g 03-10 |
| idle, wekken/s | 21,6 (2 cores, dvfs 600 MHz quiet) | | cf21ac5, 30-09 |

**Netwerk**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| De node in, MB/s | 41,1–43,8 (van de O6N; de ontvangkant is de grens) | 6,6 (2.2.6, gepold) | F 30-09 |
| De node uit, MB/s | 83,7–92,8 (naar de Pi 5) | 42,3 | F 30-09 |
| rtt over de draad p50, µs | 1212–1213 (naar de O6N), koud 1290–1300 | | H 30-09 |
| Verbindingscyclus p50, ms | 2,43–4,86 (naar de O6N) | | H 30-09 |
| Storm, hairpin naar zichzelf, conn/s (p99 ms) | 1896–1917 (6,2–6,4) | | P6g 03-10 |

**In de node**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| App naar app, MB/s | 375–378 | | P2g 03-10 |
| rtt warm p50 / p99, µs | 45–47 / 53–142 | | P2g 03-10 |
| rtt koud p50, µs | 53–56 | | P2g 03-10 |

**In rust**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Kern-cpu | 9 % | | P2g 03-10 |
| Kern-geheugen | 30,4 van 127,5 MiB (23,8 %) | | P2g 03-10 |
| Hop | 1 %, 0,42–0,44 MiB, core 0 | | P2g 03-10 |
| Temperatuur | 51,6–52,1 °C | | P2g 03-10 |

Lezing: app naar app in de node wordt begrensd doordat de OS-core vol zit
(A72 op 1500 MHz).

Bord:
- Warme flip: zes keer, gen 4 op stempel I.
- Console op het glas: 32 bpp.

## Radxa Zero 3E

Opzet: X1, 03-10, voor vitals (1 core, 128 MiB, slot 3). X20, 04-10, main
48df1e2, voor het netwerk.

**Vitals**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| cpu, Msteps/s | 136 | | X1 03-10 |
| smp, speedup | 2,00 (2 cores) | | 30-09 |
| burn, Msteps/s | 136 → 136 (geen sensor) | | X1 03-10 |
| membw copy / triad, GB/s | 3,13 / 2,25 | | X1 03-10 |
| memlat 32 KB / 2 MB / 8 MB, ns | 3,68 / 125 / 183 | | X1 03-10 |
| alloc, allocs/s | 87.930 | | X1 03-10 |
| storm, conn/s (p99 ms) | 1038 (13,3) | | X1 03-10 |
| rtt naar de kern p50 / p99, µs | 1555 / 6691 | | X1 03-10 |
| timer 1 ms, overslaap p50 / p99, µs | 820 / 1750 | | X1 03-10 |
| idle, wekken/s | 3003 (vóór de idle-fix) | | 30-09 |

**Netwerk**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| De node in, MB/s | 111,2 (bench pull van de O6N, serve op een eigen core) | 55,8–56,6 | X20 04-10 |
| De node uit, MB/s | 112,4 | 98,8–99,6 | X20 04-10 |
| rtt over de draad p50, µs | 287–289 (naar de O6N), koud 643–788 | | I 30-09 |
| Verbindingscyclus p50, ms | 1,78–4,67 (naar de O6N) | 4,1 | 30-09 |
| Storm over de draad, conn/s (p99 ms) | 656–1616 (naar de O6N; 7–24) | | 30-09 |

**In de node**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| App naar app, MB/s | 283–284 | | X1 03-10 |
| rtt warm p50 / p99, µs | 99–100 / 121–172 | | X1 03-10 |
| rtt koud p50, µs | 106–114 | | X1 03-10 |
| OS-core stil | ~1700–1900 slaapjes/s, ~75 NIC-irq/s, bewoners 0,14 % | | `HOPOS_TICK` |

**In rust**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Kern-cpu | 2–3 % | | X1 03-10 |
| Kern-geheugen | 32,2 van 64 MiB (50,3 %) | | X1 03-10 |
| Hop | 1 %, 0,42–0,44 MiB, core 0 | | X1 03-10 |
| Temperatuur | geen sensor | | |

Lezing: als de serve van de O6N in `system` staat (op de OS-core van de
O6N), haalt de Radxa maar 22–23 MB/s in. Dan is de zender de grens, niet
de Radxa.

Bord:
- NIC-interrupt op SPI 64.
- Warme flip: vijf keer, gen 3 op stempel I.
- Console op het glas: HDMI, zonder EDID. USB: twee DWC3's up.

## LicheeRV Nano

Opzet: R14, 03-10 avond, voor vitals (vitals 3.0.5, 1 core, 32 MiB, slot
3, zonder tag in `system` op core 0, de C906B op 1 GHz, naast de kern; met
alleen welcome erbij). R32, 04-10, main 1e652b5, voor het netwerk en in de
node.

**Vitals**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| cpu, Msteps/s | 87,7 | | R14 03-10 |
| burn, Msteps/s, max °C | 88,0–88,2, 57,4 °C | | R14 03-10 |
| membw copy / triad, GB/s | 2,15 / 1,67 | | R14 03-10 |
| memlat 32 KB / 2 MB, ns | 7,27 / 161 (geen 8 MB bij 32 MiB) | | R14 03-10 |
| alloc, allocs/s | 51.664–56.344 | | R14 03-10 |
| storm, conn/s (p99 ms) | 330–411 (33,8–40,0) | | R14 03-10 |
| rtt naar de kern p50 / p99, µs | 2525–2617 / 3677–4366 (los gestart 12 fouten, in `all` 0) | | R14 03-10 |
| timer 1 ms, overslaap p50 / p99, µs | 231–233 / 2186–3234 | | R14 03-10 |
| idle, wekken/s | 42,7 (99,3 % idle) | | R14 03-10 |

**Netwerk**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| De node in, MB/s | 11,33 (het plafond van de link van 100 Mbps) | | R32 04-10 |
| De node uit, MB/s | 9,05 | | R31 04-10 |
| Verbindingscyclus p50, ms | — | 16–19 | |

**In de node**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| App naar app, MB/s | 35,6 | | R31 04-10 |
| rtt warm p50 / p99, µs | 179 / 368 | | R32 04-10 |
| rtt koud p50, µs | 204 | | R32 04-10 |

**In rust**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Kern-cpu | 5–9 % | | R14 03-10 |
| Kern-geheugen | 30,3 van 40 MiB (75,6 %) | | R14 03-10 |
| Hop | 1–2 %, 0,60–0,62 van 10 MiB, core 1 | | R14 03-10 |
| Temperatuur | 56,0 °C | | R14 03-10 |

Lezing: bij Hop op de C906L (tag `sharegroup: hop`) haalt dezelfde vitals
62,6 Msteps/s. Dat is 0,71 van wat hij in `system` haalt, precies de 700
van 1000 MHz. Op de C906L valt memlat bij 128 KB al uit de cache (133 ns).
Zonder loterij (R11) gebruikt de kern in rust 4–5 % cpu, tegen 46–48 % met
loterij (R10).

Nog te meten: de verbindingscyclus over de draad.

Bord:
- Loterij: gezien op R3 tot R10 (`HOPOS_LOTTERY_SWAPPED`, ac79e91). Na een
  koude flip komt hij opnieuw; `HOPOS_FLIP_SETTLED` na 70 s.
- Koude flip: gen 10 (R10). Warme flip: `flip::WARM` weigert vóór de
  sprong.
- IRQ: kick via MSIP van de CLINT; PLIC context 0 met 102 bronnen, de dwmac
  op bron 31. Met de loterij is de NIC gepold.
- Watchdog: canary `HOPOS_WD_CANARY_OK`. Temperatuur: TEMPSEN
  (`HOPOS_TEMPSEN_UP`).

## Mac mini M4

Opzet: M1, 03-10, voor vitals (1 core, 64 MiB, slot 4 op een E-core, naast
spin en spin-tunnel). M20 tot M33, 01-10, voor in de node en opslag.

**Vitals**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| cpu, Msteps/s | 538 (E-core) | | M1 03-10 |
| storm, conn/s (p99 ms) | 4820–4872 (3,6–5,1) | | M1 03-10 |
| rtt naar de kern p50 / p99, µs | 62–76 / 72–135 | | M1 03-10 |
| idle, wekken/s | 44,8 (99,96 % idle) | | M1 03-10 |

**Netwerk**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| De node in, MB/s | — | 43,3–46,7 (HTTP PUT) | |
| De node uit, MB/s | — | 47,7–52,3 (HTTP GET) | |
| rtt over de draad p50, µs | — | 50–66 | |
| Storm over de draad, conn/s | — | 6379 (p99 1,9 ms) | |

**In de node**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| App naar app, MB/s | 6379–6383 (P-core); E-core 4448–4468 | 764–769 | M33, M20 01-10 |
| App naar app, 40 GiB, MB/s | 2813 | | M33 01-10 |

**Opslag**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Rauw 1 MiB, schrijven / lezen, MB/s | 4952 / 1925 | 5389–5756 / 1824–1971 | M23 01-10 |
| hopfs 1 MiB, schrijven / lezen, MB/s | 5015 / 2026 | 5480–5632 / 1781–1816 | M23 01-10 |
| Sequentieel 1 GiB, schrijven / lezen, MB/s | 4758 / 1629 | | M23 01-10 |
| Willekeurig 4 KiB, schrijven / lezen, IOPS | 140.000 / 11.900 | | M23 01-10 |
| Kern naar app zonder schijf (`hole=1`), MB/s | 1879–1917 | | M24 01-10 |
| App-opslag 256 MiB, schrijven / lezen, MB/s | 1269–1270 / 1683–1696 | 1598–1657 / 1064–1072 | M24 01-10 |
| 4 KiB schrijven als RPC-lus, MB/s | 82 (naast een bulk-app 15–19) | | 01-10 |
| Willekeurig 4 KiB lezen door apps, opdrachten/s | 1 app 7600, 2 apps 14.500, 4 apps 25.500 | | M33 01-10 |
| Replica (SQLite op HopFS), persist-proef, ms | 193, over een flip heen | | M33 01-10 |

Lezing: rauw schrijven en app-opslag schrijven liggen onder de v2-lat.

Nog te meten: het netwerk over de draad, en vitals op een P-core.

Bord:
- Hop: ingebakken, gaat warm mee over flips (`HOPOS_HOP_RESUMED`).
- Warme flip: gen 1 tot 8 op 01-10, adoptie 3 van 3. Koud niet (geen
  CPU_OFF).
- NIC: tg3 BCM57766 gepold (geen MSI). RNG: jitter.
- Opslag: ANS NVMe 414 GB, asynchroon met read-ahead sinds M22.
- Console op 5555: `hopos.replay=45`.
- Geïnstalleerd image: D4b. Er is een nieuw image met main nodig.

## QEMU virt

Opzet: 29-09, `sh tools/qemu-test-bench.sh`, TCG op een Mac M-serie, 4
cores, virtio-net via slirp, virtio-blk met een schijf van 64 MiB.

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| De node in, MB/s | 122,3 | | 29-09 |
| De node uit, MB/s | 71,4 | | 29-09 |
| rtt host naar app p50 / p99, µs | 1180 / 3472 | | 29-09 |
| Verbindingscyclus p50, ms | 1,44 | | 29-09 |
| Storm, conn/s (p99 ms) | 638 (2,7) | | 29-09 |
| App naar app in de node, MB/s | 62,9 (16 MiB) | | dbc522f |
| rtt in de node warm p50 / p99, µs | 115 / 340 | | dbc522f |
| rtt in de node koud p50, µs | 375 | | dbc522f |
| OS-core stil | ~820 wekken/s, ~94 lege RX-rondes/s, bewoners 0,3 % | | 29-09 |
| Rauw 1 MiB, schrijven / lezen, MB/s | 1260,9 / 1221,4 | | 29-09 |
| hopfs 1 MiB, schrijven / lezen, MB/s | 1187,0 / 1207,2 | | 29-09 |
| Sequentieel 32 MiB, schrijven / lezen, MB/s | 1293,0 / 1238,2 | | 29-09 |
| Willekeurig 4 KiB, schrijven / lezen, IOPS | 32.051 / 32.123 | | 29-09 |
| Multicast tussen twee apps | groen in 4 s | | 29-09 |

Lezing: QEMU bewijst de keten, geen plafond. Slirp termineert TCP op de
host, TCG rekent in software, en de vCPU's delen de host.

Bord: console via `ramfb`, USB via `qemu-xhci`.

## Wat nergens gemeten wordt

- De echo-rtt tijdens een kern-flip (v2, Pi 4: 4,3 tot 8,0 s).
  `tools/soak.sh FLIP=1` telt flips en overleving, niet de rtt.
- UDP over de draad. Vitals en bench hebben geen UDP-client.
- Watchdog-herstel en energie: daarvoor zijn een stopwatch en een meter
  nodig.
