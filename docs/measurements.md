# Metingen per board

PORT.md §1: deze pagina is de lat. Een Rust-HOP mag op geen gemeten getal
langzamer zijn dan v2. Per board staat de laatste v3-meting naast het
v2-getal.

Stand: 04-10-2026 middag, één ronde met `tools/meet` (190 s voor zeven
borden): Pi 4 P90, Pi 5 Q90, Radxa X90, O6N O95, Altra A95, LicheeRV R90, M4
M40 (Hop 3.0.0). Daarvoor de ochtend: main 2e34a2f, P40 tot R40, de Altra
A56g (met de boot-stack-fix 3b1db38).
In een cel staat de nieuwste meting vooraan en vet, met stempel en datum;
de oudere erachter.

## Zo schrijf je deze pagina

1. **Eén sectie per board**, altijd in deze volgorde: O6N, Altra, Pi 5,
   Pi 4, Radxa, LicheeRV, M4, QEMU. Een nieuw board komt erbij als nieuwe
   sectie met dezelfde opbouw.
2. **Elke sectie heeft dezelfde opbouw** (met na *In rust* de tabel *Watchdog*): één regel *Opzet* (de stempel,
   de datum, de commit als je die weet, en hoe vitals geplaatst is), dan de
   tabellen *Vitals*, *Netwerk*, *In de node*, *In rust*, en waar ze er
   zijn *Opslag* en *Media*. Daarna hoogstens drie zinnen *Lezing* en een
   lijst *Bord*.
3. **Elke tabel heeft vier kolommen:** `Meting | v3 | v2 | Run`. In *v3*
   staat het getal, in *v2* het v2-getal van dit board (de lat, leeg als
   v2 er geen had), en in *Run* de stempel en de datum.
4. **De nieuwste meting vooraan, vet, met stempel en datum**; de oudere
   waarden blijven erachter staan zodat de lijn te zien is. Wat niet meer
   klopt (een fout die gefixt is) mag weg.
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
| De hele ronde: inrichting, vitals, bench in de node en over de draad, opruimen | `python3 tools/meet.py` (`--only pi4,pi5`, `--skip-wire`, `--peer IP`, `--stamp m4=M40` als de console de bootregel kwijt is, `--write` zet de cellen vooraan) | per bord `target/meet/<bord>.md`, de tijd per fase |
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
| Schijf, rauw en door hopfs | `hopos.nvmebench=1`, of de feature `nvmebench` in een meetkern | `HOPOS_NVMEBENCH`, `_SEQ`, `_RAND`, `_RANDQ` |
| Kern en Hop in rust | `GET :9080/v1/agents` (`kern_cpu_percent`, `kern_mem_bytes`, `hop_cpu_percent`, `hop_mem_bytes`, `temp_milli_c`), drie peilingen 10 s uit elkaar, alleen welcome geplaatst | |
| De keten op QEMU | `sh tools/qemu-test-bench.sh` | `bench-keten groen` |
| Uren lang plaatsen en stoppen | `sh tools/soak.sh NODE` (`FLIP=1` voor de flip erbij) | `SOAK` |

Code: `apps/vitals` (README daar), `apps/bench` (protocol in
`apps/bench/src/proto.rs`), `tools/netmeter`, `tools/meet.py`,
`hopos/src/bench.rs`.

Overal hetzelfde:
- `test=rx` faalt zonder `url`. De standaardbron (cachefly) vraagt een
  tunnel: `http: CONNECT is not supported`. Daarom geeft `test=all` met
  één core `failed=1` (rx) en `skipped=2` (smp met één core, disk zonder
  schijf).
- De vitals-*storm* gaat naar de eigen poort, de vitals-*rtt* naar de kern
  (10.100.0.1:10100).
- Op QEMU is WFE een no-op. Rijen over idle tellen alleen op ijzer.

## Orion O6N

Opzet: **O95, 04-10, tools/meet. Vitals met 4 cores (`core-class: big`), 128
MiB. Naast welcome, bench-serve. Over de draad tegen de M4 (meet-serve
:9100, vitals :8090).** Daarvoor: O40, 04-10, main 2e34a2f (warme flip).
Vitals met 1 core (`core-class: big`, core 6), 128 MiB, slot 4; `vitals4` op
:8091 met 4 big cores en 256 MiB voor smp en burn. bench-serve (:9000) stond
de hele tijd op core 2 en kreeg verkeer van de andere borden.

**Vitals**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| cpu, Msteps/s | **753 met `core-class: big` (O95, 04-10)**; 752 met `core-class: big` (O40, 04-10); 298 (A520); 754 met `core-class: big` (O2 03-10; O3 03-10) | | O95 04-10 |
| smp, speedup | **3,83 (4 cores) (O95, 04-10)**; 3,83 (4 big cores) (O40, 04-10); 3,99 (4 cores) (O2, 03-10) | | O95 04-10 |
| burn, Msteps/s, max °C | **2991 → 2991, 48,0 °C (4 cores, 10 s) (O95, 04-10)**; 762 → 762, 42 °C; 4 cores 3057, 43 °C (O40, 04-10); 299 → 299, 41 °C; 4 cores 1198 (O2, 03-10) | | O95 04-10 |
| membw copy / triad, GB/s | **42,0 / 28,2 (O95, 04-10)**; 39,8 / 28,4 (O40, 04-10); 16,7 / 11,4 (O2, 03-10) | | O95 04-10 |
| memlat 32 KB / 2 MB / 8 MB, ns | **1,74 / 20,5 / 32,2 (O95, 04-10)**; 1,74 / 20,6 / 32,2 (O40, 04-10); 2,76 / 38,9 / 38,9 (O2, 03-10) | | O95 04-10 |
| alloc, allocs/s | **519.917 (O95, 04-10)**; 519.412 (O40, 04-10); 191.466 (O2, 03-10) | | O95 04-10 |
| storm, conn/s (p99 ms) | **9507 (0,87) (O95, 04-10)**; 9853 (0,86) (O40, 04-10); 2348 (5,38) (O2, 03-10) | | O95 04-10 |
| rtt naar de kern p50 / p99, µs | **141 / 3134 (O95, 04-10)**; 99 / 7761 (O40, 04-10); 718 / 9471 (O2, 03-10) | | O95 04-10 |
| timer 1 ms, overslaap p50 / p99, µs | **48 / 638 (O95, 04-10)**; 48 / 71 (4 cores: 48 / 80) (O40, 04-10); 826 / 1193 (4 cores: 48 / 220) (O2, 03-10) | | O95 04-10 |
| idle, wekken/s | **43,0 (4 cores) (O95, 04-10)**; 48,9 (1 core, 99,4 % idle); 4 cores 52,5 (O40, 04-10); 51,5 (4 cores) (na de idle-fix, 30-09) | | O95 04-10 |

**Netwerk**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| De node in, MB/s | **117,6 (big cores, bench pull van de M4, 256 MiB) (O95, 04-10)**; 109,3 (bench pull van de Radxa, 400 MB; O6N druk) (O40, 04-10); 76,4–78,3 (van de Pi 4; geen peer zendt sneller: ondergrens) (G, 30-09) | 111,3–116,5 | O95 04-10 |
| De node uit, MB/s | **98,0 (big cores, bench push naar de M4, 256 MiB) (O95, 04-10)**; 113,0 (bench push naar de Pi 5, 400 MB; O6N druk) (O40, 04-10); 76,1–85,2 (naar de Pi 5; ondergrens) (G, 30-09) | 114,3–117,6 | O95 04-10 |
| rtt over de draad p50, µs | **121 (naar de M4, p99 134), koud 123 (O95, 04-10)**; 73 (naar de Pi 5, p99 114–119), koud 75 (O6N druk) (O40, 04-10); 200–227 (naar de Pi 5), koud 197–245 (H, 30-09) | 156–201 | O95 04-10 |
| Verbindingscyclus p50, ms | **0,51 (naar de M4, vitals storm n=1) (O95, 04-10)**; 0,30 (naar vitals op de Pi 5, storm n=1; O6N druk) (O40, 04-10); 2,43 (naar de Pi 5) (H, 30-09) | 3,6 | O95 04-10 |
| Storm over de draad, conn/s | **9584 (naar de M4, p99 0,98 ms) (O95, 04-10)**; 4928 (naar de Pi 5, p99 6,52 ms; O6N druk) (O40, 04-10); 3298–4976 (naar de Pi 5) (H, 30-09) | 844 (p99 19,6 ms) | O95 04-10 |
| Storm, hairpin naar zichzelf, conn/s (p99 ms) | **891 (12,5) (O95, 04-10)**; 1983 (10,0) (O40, 04-10) | | O95 04-10 |

**In de node**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| App naar app, MB/s | **1766 (big cores, 400 MB) (O95, 04-10)**; 1745 (eigen big cores, 400 MB); op de OS-core 1092 (O40, 04-10); 1686–1689 (O2, 03-10) | | O95 04-10 |
| rtt warm p50 / p99, µs | **29 / 54 (O95, 04-10)**; 32 / 87 (eigen big cores); op de OS-core 77–82 / 219–223 (O40, 04-10); 55 / 193 (O16h 04-10, main 1e652b5) | | O95 04-10 |
| rtt koud p50, µs | **31 (O95, 04-10)**; 32; op de OS-core 80–83 (O40, 04-10); 60 (O16h, 04-10) | | O95 04-10 |

**In rust**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Kern-cpu | **1 % (O95, 04-10)**; 1 % (O40, 04-10); 4 % (O2, 03-10) | | O95 04-10 |
| Kern-geheugen | **31,4 van 224 MiB (14,0 %) (O95, 04-10)**; 31,9 van 224 MiB (14,2 %) (O40, 04-10); 37,5 van 224 MiB (16,7 %) (O2, 03-10) | | O95 04-10 |
| Hop | **1 %, 0,45–0,46 MiB, core 2 (O95, 04-10)**; 1 %, 0,55–0,58 MiB, core 0 (O40, 04-10); 1 %, 0,44–0,46 MiB, core 0 (O2, 03-10) | | O95 04-10 |
| Temperatuur | **43,0 °C (O95, 04-10)**; 40,0 °C (O40, 04-10); 41,0 °C (O2, 03-10) | | O95 04-10 |

**Watchdog**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Watchdogtoets (meetkern `wdtest`: Hop stopt na 60 s) | **47 s, maar de stickkern O2 landde eerst op de oude flip-overdracht in het DRAM en gaf pas na de boot-guard een koude boot (2,5 min); post-mortem van de tussenboot, niet van O41w (O41w, 04-10)** | | O41w 04-10 |

**Opslag**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Rauw 1 MiB, schrijven / lezen, MB/s | — | 3056,8 / 2705,6 (gecachet) | |
| hopfs 1 MiB, schrijven / lezen, MB/s | **3584 / 2992 (nvmebench, 04-10)** | 2970,1 / 2716,7 | nvmebench 04-10 |
| App-opslag via vitals disk, schrijven / lezen, MB/s | **1021 / 1761 (256 MB) (O95, 04-10)**; 921 / 1799 (256 MiB); 64 MB 914 / 1868 (O40, 04-10); 714 / 748 (64 MB) (O2, 03-10) | 556,8–625,4 / 727,5–796,8 (256 MiB) | O95 04-10 |
| 4 KiB schrijven via de app, MB/s | **75,8 (O95, 04-10)**; 84,2 (256 calls, p50 46 µs); in de 64 MB-run 109 (O40, 04-10); 42,3 (O2, 03-10) | 15,15–17,08 | O95 04-10 |
| Willekeurig 4 KiB lezen door apps, opdrachten/s | **echt: 1 app 7.983 (p50 125 µs), 2 apps 16.561, 4 apps 29.052, 8 apps 46.868, 9 apps 50.150 (p50 179; de drive met één lees per app in de lucht, rauw 15,6k met één en 71k met zestien); zonder schijf (`hole=1`): 22.694, 46.643, 56.672, 78.017, 78.226 (de OS-core vol: ~12,8 µs per call, Hop op de OS-core krijgt dan 1 %) (O43, 04-10)** | | O43 04-10 |

**Media**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| 4K P010 door de grant (apps/decode), fps | 85,7 (2134 MB/s) | 27,25 (lat 24 fps) | 30-09 |

Lezing: de OS-core-rondreis is op O40 77 tot 82 µs met welcome en
bench-serve erbij; de 55 van O16h was in een lege node. De 298 tegen 855
op 30-09 is de kleine core, geen regressie. Sinds 68a86c6 klokt de O6N
binnen één ronde naar vol als er een rekenaar in een slot staat.

Nog te meten: de rest van de `nvmebench`-regels in de tabel (de kern van
04-10 haalde SEQ lezen 3263 MB/s, RANDQ 16 tegelijk 71k IOPS); media na
een koude boot.

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

Opzet: **A95, 04-10, tools/meet. Vitals met 4 cores, 128 MiB. Naast welcome.
Over de draad tegen de O6N (bench-serve :9000, vitals :8090).** Daarvoor:
A56g, 04-10, main 2e34a2f plus de boot-stack-fix (nu in main als 3b1db38),
warm geflipt vanaf de stickkern A4; Hop van de stick (3.0.6). Vitals met 1
core, 128 MiB, slot 3; `vitals4` op :8091 met 4 cores en 256 MiB. Opslag op
de meetkern A57b (`nvmebench`), de watchdogtoets op A58w. Gewone main zonder
de fix kwam vanaf A4 niet op (boot-stack 264 KB van 256), dus release v3.0.7
boot op de Altra niet.

**Vitals**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| cpu, Msteps/s | **495 (A95, 04-10)**; 494 (A56g, 04-10); 494 (A4, 03-10) | | A95 04-10 |
| smp, speedup | **3,97 (4 cores) (A95, 04-10)**; 4,00 (4 cores) (A56g, 04-10); 4,01 (4 cores) (A4, 03-10) | | A95 04-10 |
| burn, Msteps/s, max °C | **1979 → 1979, 40,0 °C (4 cores, 10 s) (A95, 04-10)**; 494 → 494, 38 °C; 4 cores 1979 → 1978 (A56g, 04-10); 494 → 494, 46 °C; 4 cores 1979 (A4, 03-10) | | A95 04-10 |
| membw copy / triad, GB/s | **24,6 / 23,4 (A95, 04-10)**; 25,0 / 23,3 (A56g, 04-10); 25,9 / 24,0 (A4, 03-10) | | A95 04-10 |
| memlat 32 KB / 2 MB / 8 MB, ns | **1,54 / 5,33 / 27,5 (A95, 04-10)**; 1,54 / 5,45 / 27,6 (A56g, 04-10); 1,54 / 5,46 / 27,6 (A4, 03-10) | | A95 04-10 |
| alloc, allocs/s | **548.866 (A95, 04-10)**; 551.123 (A56g, 04-10); 557.016 (A4, 03-10) | | A95 04-10 |
| storm, conn/s (p99 ms) | **9832 (0,91) (A95, 04-10)**; 9358 (0,94) (A56g, 04-10); 9958 (0,92) (A4, 03-10) | | A95 04-10 |
| rtt naar de kern p50 / p99, µs | **301 / 3845 (A95, 04-10)**; 84 / 158 (tweede run; in test=all 280 / 9218) (A56g, 04-10); 110 / 5179 (A4, 03-10) | | A95 04-10 |
| timer 1 ms, overslaap p50 / p99, µs | **310 / 776 (A95, 04-10)**; 310 / 435 (A56g, 04-10); 236 / 1022 (A4, 03-10) | | A95 04-10 |
| idle, wekken/s | **21,2 (4 cores) (A95, 04-10)**; 24,1 (1 core, 99,7 % idle) (A56g, 04-10) | | A95 04-10 |
| disk schrijven / lezen, MB/s | **1284 / 2237 (64 MB; 4 KiB 79,9) (A95, 04-10)**; 1127 / 2188 (NVMe SN770, 64 MB; 4 KiB 146) (A56g, 04-10); 873 / 1062 (NVMe SN770, 64 MB; 4 KiB 74,5) (A4, 03-10) | | A95 04-10 |

**Netwerk**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| De node in, MB/s | **84,3 (bench pull van de O6N, 256 MiB) (A95, 04-10)**; 86,4 (bench pull 400 MB van de O6N; O6N druk) (A56g, 04-10) | 107,7–110,8 | A95 04-10 |
| De node uit, MB/s | **110,3 (bench push naar de O6N, 256 MiB) (A95, 04-10)**; 108,8 (bench push naar de O6N; O6N druk) (A56g, 04-10) | 108,0–110,6 | A95 04-10 |
| rtt over de draad p50, µs | **179 (naar de O6N, p99 257), koud 270 (A95, 04-10)**; 178 (naar de O6N, p99 429), koud 308 (O6N druk) (A56g, 04-10) | | A95 04-10 |
| Verbindingscyclus p50, ms | **0,90 (naar de O6N, vitals storm n=1) (A95, 04-10)** | 5,1 | A95 04-10 |
| Storm, hairpin naar zichzelf, conn/s (p99 ms) | **710 (16,8) (A95, 04-10)**; 990–1074 (12,4–14,4) (A56g, 04-10) | | A95 04-10 |
| Storm over de draad, conn/s | **4910 (naar de O6N, p99 4,27 ms) (A95, 04-10)** | | A95 04-10 |

**In de node**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| App naar app, MB/s | **5733 (400 MB) (A95, 04-10)**; 5384 (A56g, 04-10); 4970–4972 (A4, 03-10) | | A95 04-10 |
| rtt warm p50 / p99, µs | **15 / 27 (A95, 04-10)**; 15 / 34 (A56g, 04-10); 21 / 37 (A4, 03-10) | | A95 04-10 |
| rtt koud p50, µs | **20 (A95, 04-10)**; 21 (A56g, 04-10); 31–47 (A4, 03-10) | | A95 04-10 |

**In rust**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Kern-cpu | **1 % (A95, 04-10)**; 1 % (A56g, 04-10); 5 % (A4, 03-10) | | A95 04-10 |
| Kern-geheugen | **31,8 van 224 MiB (14,2 %) (A95, 04-10)**; 31,8 van 224 MiB (14,2 %) (A56g, 04-10); 31,8 van 224 MiB (14,2 %) (A4, 03-10) | | A95 04-10 |
| Hop | **1 %, 0,55–0,56 MiB, core 0 (A95, 04-10)**; 1 %, 0,55–0,57 MiB, core 0 (A56g, 04-10); 1 %, 0,46 MiB, core 0 (A4, 03-10) | | A95 04-10 |
| Temperatuur | **39,0 °C (A95, 04-10)**; 38,0 °C (A56g, 04-10); 46,0 °C (A4, 03-10) | | A95 04-10 |

**Watchdog**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Watchdogtoets (meetkern `wdtest`: Hop stopt na 60 s) | **Hop gestopt 09:36:35, reset binnen 22 s; terug op de stickkern A4 om 09:40:14 (de firmware-boot duurt ruim 3 min); geen post-mortem, de zwarte doos overleeft op de Altra geen reset; daarna main plus fix warm vanaf A4 op (A58w, 04-10)** | | A58w 04-10 |

**Opslag**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Rauw 1 MiB, schrijven / lezen, MB/s | **2466 / 2109 (NVMe SN770 500 GB) (A57b, 04-10)** | | A57b 04-10 |
| hopfs 1 MiB, schrijven / lezen, MB/s | **2493 / 2251 (A57b, 04-10)** | | A57b 04-10 |
| Sequentieel 1 GiB, schrijven / lezen, MB/s | **2549 / 2459 (A57b, 04-10)** | | A57b 04-10 |
| Willekeurig 4 KiB, schrijven / lezen, IOPS | **141.000 / 24.500 (A57b, 04-10)** | | A57b 04-10 |
| Willekeurig 4 KiB lezen, 16 tegelijk, IOPS | **186.000 (762 MB/s) (A57b, 04-10)** | | A57b 04-10 |
| Willekeurig 4 KiB lezen door apps, opdrachten/s | **met de bundel (`read_many`, fd11581; A74f, 04-10): 1 app één per call 18.827, bundel 4 38.787, bundel 16 100.418, twee bundels 159.369; 4 apps 54.293 / 99.353 / 249.103; 8 apps 84.995 / 123.567 / 305.060; vóór de bundel (A56g) 18.918 / 55.645 / 78.944** | | A74f 04-10 |

Bord:
- NIC: **igb op MSI-X via de ITS van zijn root-complex (ITS 7, LPI 8224),
  eerste interrupt na 4 µs (A56g, 04-10)**; eerder bewust gepold (de
  `_PRT`-INTx doodt de SoC). Kick via SGI 1.
- Watchdog: SBSA, 12,0 s. RNG: **geen SMCCC-TRNG (`TRNG_VERSION
  NOT_SUPPORTED`), jitter (04-10)**; eerder SMCCC-TRNG of `rndr`.
  Temperatuur: SMpro.
- Console op het glas: GOP, alleen na een koude boot (na een warme flip
  `HOPOS_FB_NONE`). Opslag: NVMe SN770 500 GB.
- Zwarte doos: overleeft op de Altra geen reset (`HOPOS_FLIP_BLACKBOX_EMPTY`).

## Raspberry Pi 5

Opzet: **Q90, 04-10, tools/meet. Vitals met 2 cores, 128 MiB. Naast welcome.
Over de draad tegen de O6N (bench-serve :9000, vitals :8090).** Daarvoor:
Q40, 04-10, main 2e34a2f. Vitals met 1 core, 128 MiB, slot 3 (core 2); smp
met 2 cores.

**Vitals**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| cpu, Msteps/s | **285 (Q90, 04-10)**; 285 (Q40, 04-10); 284 (P4, 03-10) | | Q90 04-10 |
| smp, speedup | **2,00 (2 cores) (Q90, 04-10)**; 2,00 (2 cores) (Q40, 04-10); 2,00 (2 cores) (30-09) | | Q90 04-10 |
| burn, Msteps/s, max °C | **571 → 571, 60,4 °C (2 cores, 10 s) (Q90, 04-10)**; 285 → 285, 60,9 °C (Q40, 04-10); 285 → 285, 56,0 °C (P4, 03-10) | | Q90 04-10 |
| membw copy / triad, GB/s | **8,31 / 6,89 (Q90, 04-10)**; 8,28 / 6,89 (Q40, 04-10); 8,27 / 6,88 (P4, 03-10) | | Q90 04-10 |
| memlat 32 KB / 2 MB / 8 MB, ns | **2,67 / 15,4 / 58,4 (Q90, 04-10)**; 2,67 / 16,5 / 58,9 (Q40, 04-10); 2,67 / 16,5 / 58,9 (P4, 03-10) | | Q90 04-10 |
| alloc, allocs/s | **290.657 (Q90, 04-10)**; 293.293 (Q40, 04-10); 302.439 (P4, 03-10) | | Q90 04-10 |
| storm, conn/s (p99 ms) | **4911 (1,56) (Q90, 04-10)**; 4854 (1,57); hairpin via 192.168.1.207:8090 3267 (4,36) (Q40, 04-10); 4777 (1,58) (P4, 03-10) | | Q90 04-10 |
| rtt naar de kern p50 / p99, µs | **237 / 991 (Q90, 04-10)**; 165 / 9141 (Q40, 04-10); 167 / 9134 (P4, 03-10) | | Q90 04-10 |
| timer 1 ms, overslaap p50 / p99, µs | **213 / 701 (Q90, 04-10)**; 213 / 215 (Q40, 04-10); 213 / 332 (P4, 03-10) | | Q90 04-10 |
| idle, wekken/s | **20,7 (2 cores) (Q90, 04-10)**; 20,6 (1 core, 99,7 % idle) (Q40, 04-10); 1087 (vóór de idle-fix) (30-09) | | Q90 04-10 |

**Netwerk**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| De node in, MB/s | **85,7 (bench pull van de O6N, 256 MiB) (Q90, 04-10)**; 111,3 (bench pull van de O6N, 256 MiB; O6N druk) (Q40, 04-10); 102,4 (bench pull van de O6N, 256 MiB) (P4, 03-10) | 57,0–69,6 | Q90 04-10 |
| De node uit, MB/s | **109,5 (bench push naar de O6N, 256 MiB) (Q90, 04-10)**; 55,0 (bench push naar de O6N; de GEM zendt nog uit NC; O6N druk) (Q40, 04-10); 51,9 (bench push naar de O6N; de GEM zendt nog uit NC) (P4, 03-10) | 41,6–50,6 | Q90 04-10 |
| rtt over de draad p50, µs | **165 (naar de O6N, p99 224), koud 227 (Q90, 04-10)**; 73 (naar de O6N, p99 118), koud 88 (O6N druk) (Q40, 04-10); 206–238 (naar de O6N), koud 1508–1687 (dev, 30-09) | | Q90 04-10 |
| Verbindingscyclus p50, ms | **0,85 (naar de O6N, vitals storm n=1) (Q90, 04-10)**; 0,62 (naar de O6N, vitals storm n=1; O6N druk) (Q40, 04-10); 2,36–2,42 (naar de O6N) (dev, 30-09) | 1,1–3,6 | Q90 04-10 |
| Storm over de draad, conn/s | **4965 (naar de O6N, p99 2,46 ms) (Q90, 04-10)**; 4899 (naar de O6N, p99 7,01 ms; O6N druk) (Q40, 04-10); 3284–4893 (naar de O6N) (dev, 30-09) | | Q90 04-10 |
| Storm, hairpin naar zichzelf, conn/s (p99 ms) | **1972 (5,23) (Q90, 04-10)** | | Q90 04-10 |

**In de node**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| App naar app, MB/s | **771 (400 MB) (Q90, 04-10)**; 774 (Q40, 04-10); 783–810 (P4, 03-10) | | Q90 04-10 |
| rtt warm p50 / p99, µs | **9 / 25 (Q90, 04-10)**; 15 / 25 (Q40, 04-10); 28 / 35–38 (P4, 03-10) | | Q90 04-10 |
| rtt koud p50, µs | **14 (Q90, 04-10)**; 9 (Q40, 04-10); 31–34 (P4, 03-10) | | Q90 04-10 |
| OS-core stil | ~900 slaapjes/s, ~70 NIC-irq/s | 113 RX-rondes/s (irq) | idlestat |

**In rust**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Kern-cpu | **1 % (Q90, 04-10)**; 1 % (Q40, 04-10); 2 % (P4, 03-10) | | Q90 04-10 |
| Kern-geheugen | **30,4–30,5 van 127,5 MiB (23,9 %) (Q90, 04-10)**; 30,4 van 127,5 MiB (23,8 %) (Q40, 04-10); 30,3 van 127,5 MiB (23,7 %) (P4, 03-10) | | Q90 04-10 |
| Hop | **1 %, 0,51–0,52 MiB, core 0 (Q90, 04-10)**; 1 %, 0,52–0,53 MiB, core 0 (Q40, 04-10); 1 %, 0,42–0,44 MiB, core 0 (P4, 03-10) | | Q90 04-10 |
| Temperatuur | **54,3–54,9 °C (Q90, 04-10)**; 57,1 °C (Q40, 04-10); 50,5–51,0 °C (P4, 03-10) | | Q90 04-10 |

**Watchdog**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Watchdogtoets (meetkern `wdtest`: Hop stopt na 60 s) | **30 s naar de kaartkern P4; post-mortem gezien (Q41w, 04-10)** | | Q41w 04-10 |

Lezing: uit is de helft van in. De GEM zendt met losse woorden uit NC,
dezelfde vorm als de dwmac4 van de Radxa vóór X2.

Bord:
- Warme flip: werkt, drie keer geland binnen 10 s (04-10).
- Watchdog: BCM PM, 12 s.
- Console op het glas: gezien via een flip op de eerste kaart. Sinds de
  herflash weigert de firmware.

## Raspberry Pi 4

Opzet: **P90, 04-10, tools/meet. Vitals met 2 cores, 128 MiB. Naast welcome.
Over de draad tegen de O6N (bench-serve :9000, vitals :8090).** Daarvoor:
P40, 04-10, main 2e34a2f. Vitals met 2 cores (`cpu_shares` 2048), 128 MiB,
slot 2 (`vitals-arm64.elf` van main); welcome stond er niet.

**Vitals**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| cpu, Msteps/s | **250 (P90, 04-10)**; 250 (P40, 04-10); 248 (P2g, 03-10) | | P90 04-10 |
| smp, speedup | **2,00 (2 cores) (P90, 04-10)**; 2,00 (2 cores) (P40, 04-10); 2,00 (2 cores) (30-09) | | P90 04-10 |
| burn, Msteps/s, max °C | **499 → 499, 57,4 °C (2 cores, 10 s) (P90, 04-10)**; 499 → 499 (2 cores, 10 s), 57,4 °C (P40, 04-10); 249 → 249, 54,5 °C (P2g, 03-10) | | P90 04-10 |
| membw copy / triad, GB/s | **3,43 / 2,83 (P90, 04-10)**; 3,38 / 2,83 (P40, 04-10); 3,42 / 2,80 (P2g, 03-10) | | P90 04-10 |
| memlat 32 KB / 2 MB / 8 MB, ns | **3,33 / 25,4 / 109 (P90, 04-10)**; 3,33 / 21,7 / 110 (P40, 04-10); 3,33 / 28,3 / 111 (P2g, 03-10) | | P90 04-10 |
| alloc, allocs/s | **98.639 (P90, 04-10)**; 100.743 (P40, 04-10); 100.921 (P2g, 03-10) | | P90 04-10 |
| storm, conn/s (p99 ms) | **3300 (2,90) (P90, 04-10)**; 3242 (2,94) (P40, 04-10); 3204 (2,91) (P2g, 03-10) | | P90 04-10 |
| rtt naar de kern p50 / p99, µs | **448 / 1853 (P90, 04-10)**; 322 / 5312 (P40, 04-10); 323 / 5543 (P2g, 03-10) | | P90 04-10 |
| timer 1 ms, overslaap p50 / p99, µs | **213 / 1020 (P90, 04-10)**; 213 / 213 (P40, 04-10); 213 / 215 (P2g, 03-10) | | P90 04-10 |
| idle, wekken/s | **20,6 (2 cores) (P90, 04-10)**; 20,3 (2 cores) (P40, 04-10); 21,6 (2 cores, dvfs 600 MHz quiet) (cf21ac5, 30-09) | | P90 04-10 |

**Netwerk**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| De node in, MB/s | **58,4 (bench pull van de O6N, 256 MiB) (P90, 04-10)**; 64,8 (bench pull van de O6N op een big core, 256 MiB; de genet leest uit WB met een veeg sinds bca6c70, dat gaf niets: de gepolde RX-weg is de rem) (P70, 04-10); 71,9 (bench pull van de O6N, 256 MiB; O6N druk) (P40, 04-10); 41,1–43,8 (van de O6N; de ontvangkant is de grens) (F, 30-09) | 6,6 (2.2.6, gepold) | P90 04-10 |
| De node uit, MB/s | **112,2 (bench push naar de O6N, 256 MiB) (P90, 04-10)**; 112,9 (bench push naar de O6N, 256 MiB) (P70, 04-10); 112,6 (P40, 04-10); 83,7–92,8 (naar de Pi 5) (F, 30-09) | 42,3 | P90 04-10 |
| rtt over de draad p50, µs | **1213 (naar de O6N, p99 1239), koud 1223 (P90, 04-10)**; 1213 (naar de O6N), koud 1224 (O6N druk) (P40, 04-10); 1212–1213 (naar de O6N), koud 1290–1300 (H, 30-09) | | P90 04-10 |
| Verbindingscyclus p50, ms | **2,43 (naar de O6N, vitals storm n=1) (P90, 04-10)**; 2,43 (vitals storm n=1 naar /ping van de O6N; O6N druk) (P40, 04-10); 2,43–4,86 (naar de O6N) (H, 30-09) | | P90 04-10 |
| Storm, hairpin naar zichzelf, conn/s (p99 ms) | **1085 (10,3) (P90, 04-10)**; 1942–1989 (5,6–6,8; vijf runs, geen MASQ_SLOT_FULL) (P40, 04-10); 1896–1917 (6,2–6,4) (P6g, 03-10) | | P90 04-10 |
| Storm over de draad, conn/s | **2465 (naar de O6N, p99 4,88 ms) (P90, 04-10)** | | P90 04-10 |

**In de node**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| App naar app, MB/s | **391 (400 MB) (P90, 04-10)**; 394 (P40, 04-10); 375–378 (P2g, 03-10) | | P90 04-10 |
| rtt warm p50 / p99, µs | **16 / 31 (P90, 04-10)**; 23 / 47–50 (P40, 04-10); 45–47 / 53–142 (P2g, 03-10) | | P90 04-10 |
| rtt koud p50, µs | **22 (P90, 04-10)**; 20 (P40, 04-10); 53–56 (P2g, 03-10) | | P90 04-10 |

**In rust**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Kern-cpu | **2 % (P90, 04-10)**; 2 % (P40, 04-10); 9 % (P2g, 03-10) | | P90 04-10 |
| Kern-geheugen | **30,4–30,5 van 127,5 MiB (23,9 %) (P90, 04-10)**; 30,4 van 127,5 MiB (23,8 %) (P40, 04-10); 30,4 van 127,5 MiB (23,8 %) (P2g, 03-10) | | P90 04-10 |
| Hop | **1 %, 0,51–0,52 MiB, core 0 (P90, 04-10)**; 1 %, 0,51–0,53 MiB, core 0 (P40, 04-10); 1 %, 0,42–0,44 MiB, core 0 (P2g, 03-10) | | P90 04-10 |
| Temperatuur | **51,6–52,5 °C (P90, 04-10)**; 51,1–51,6 °C (P40, 04-10); 51,6–52,1 °C (P2g, 03-10) | | P90 04-10 |

**Watchdog**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Watchdogtoets (meetkern `wdtest`: Hop stopt na 60 s) | **27,5 s naar de kaartkern P1; post-mortem gezien (P41, 04-10)** | | P41 04-10 |

Lezing: app naar app in de node wordt begrensd doordat de OS-core vol zit
(A72 op 1500 MHz).

Bord:
- Warme flip: zes keer, gen 4 op stempel I.
- Console op het glas: 32 bpp.

## Radxa Zero 3W

Opzet: **X90, 04-10, tools/meet. Vitals met 2 cores, 128 MiB. Naast welcome.
Over de draad tegen de O6N (bench-serve :9000, vitals :8090).** Daarvoor:
X40, 04-10, main 2e34a2f, warm geflipt van X20. Vitals met 2 cores
(`cpu_shares` 2048, slot 3, cores 2 en 3), 128 MiB. dvfs Auto: 816 MHz stil,
1800 MHz vol.

**Vitals**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| cpu, Msteps/s | **325 (X90, 04-10)**; 324–325 (1800 MHz; ook de run die stil op 816 begon) (X40, 04-10); 136 (X1, 03-10) | | X90 04-10 |
| smp, speedup | **1,99 (2 cores) (X90, 04-10)**; 2,00 (2 cores) (X40, 04-10); 2,00 (2 cores) (30-09) | | X90 04-10 |
| burn, Msteps/s | **646 → 647 (2 cores, 10 s) (X90, 04-10)**; 646 → 646 (2 cores, 10 s; geen sensor) (X40, 04-10); 136 → 136 (geen sensor) (X1, 03-10) | | X90 04-10 |
| membw copy / triad, GB/s | **4,66 / 3,89 (X90, 04-10)**; 4,85 / 3,89 (X40, 04-10); 3,13 / 2,25 (X1, 03-10) | | X90 04-10 |
| memlat 32 KB / 2 MB / 8 MB, ns | **1,63 / 92,2 / 146 (X90, 04-10)**; 1,61 / 91,6 / 146 (X40, 04-10); 3,68 / 125 / 183 (X1, 03-10) | | X90 04-10 |
| alloc, allocs/s | **99.790 (X90, 04-10)**; 99.457 (X40, 04-10); 87.930 (X1, 03-10) | | X90 04-10 |
| storm, conn/s (p99 ms) | **2421 (5,23) (X90, 04-10)**; 2374 (5,29) (X40, 04-10); 1038 (13,3) (X1, 03-10) | | X90 04-10 |
| rtt naar de kern p50 / p99, µs | **643 / 5752 (X90, 04-10)**; 609 / 681 (X40, 04-10); 1555 / 6691 (X1, 03-10) | | X90 04-10 |
| timer 1 ms, overslaap p50 / p99, µs | **365 / 943 (X90, 04-10)**; 365 / 985 (X40, 04-10); 820 / 1750 (X1, 03-10) | | X90 04-10 |
| idle, wekken/s | **21,0 (2 cores) (X90, 04-10)**; 20,8 (2 cores, stil 816 MHz) (X40, 04-10); 3003 (vóór de idle-fix) (30-09) | | X90 04-10 |

**Netwerk**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| De node in, MB/s | **81,3 (bench pull van de O6N, 256 MiB) (X90, 04-10)**; 78,5–93,5 (bench pull 256 MiB van de O6N; O6N druk) (X40, 04-10); 111,2 (bench pull van de O6N, serve op een eigen core) (X20, 04-10) | 55,8–56,6 | X90 04-10 |
| De node uit, MB/s | **109,4 (bench push naar de O6N, 256 MiB) (X90, 04-10)**; 109,5 (bench push 256 MiB naar de O6N; O6N druk) (X40, 04-10); 112,4 (X20, 04-10) | 98,8–99,6 | X90 04-10 |
| rtt over de draad p50, µs | **178 (naar de O6N, p99 221), koud 282 (X90, 04-10)**; 179–187 (naar de O6N), koud 319 (O6N druk) (X40, 04-10); 287–289 (naar de O6N), koud 643–788 (I, 30-09) | | X90 04-10 |
| Verbindingscyclus p50, ms | **1,61 (naar de O6N, vitals storm n=1) (X90, 04-10)**; 0,96 (vitals storm n=1 naar de O6N-vitals; O6N druk) (X40, 04-10); 1,78–4,67 (naar de O6N) (30-09) | 4,1 | X90 04-10 |
| Storm over de draad, conn/s (p99 ms) | **1395 (naar de O6N, p99 15,3 ms) (X90, 04-10)**; 1641 (10,7; naar de O6N-vitals, 8 werkers; O6N druk) (X40, 04-10); 656–1616 (naar de O6N; 7–24) (30-09) | | X90 04-10 |
| Storm, hairpin naar zichzelf, conn/s (p99 ms) | **482 (23,6) (X90, 04-10)**; 620–757 (16,4–17,3) (X40, 04-10) | | X90 04-10 |

**In de node**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| App naar app, MB/s | **391 (400 MB) (X90, 04-10)**; 390,8 (400 MB) (X40, 04-10); 283–284 (X1, 03-10) | | X90 04-10 |
| rtt warm p50 / p99, µs | **27 / 64 (X90, 04-10)**; 27 / 76 (X40, 04-10); 99–100 / 121–172 (X1, 03-10) | | X90 04-10 |
| rtt koud p50, µs | **47 (X90, 04-10)**; 40 (X40, 04-10); 106–114 (X1, 03-10) | | X90 04-10 |
| OS-core stil | ~1700–1900 slaapjes/s, ~75 NIC-irq/s, bewoners 0,14 % | | `HOPOS_TICK` |

**In rust**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Kern-cpu | **2 % (X90, 04-10)**; 2 % (X40, 04-10); 2–3 % (X1, 03-10) | | X90 04-10 |
| Kern-geheugen | **31,9–32,1 van 64 MiB (50,0 %) (X90, 04-10)**; 31,9 van 64 MiB (49,9 %) (X40, 04-10); 32,2 van 64 MiB (50,3 %) (X1, 03-10) | | X90 04-10 |
| Hop | **1 %, 0,45–0,46 MiB, core 0 (X90, 04-10)**; 1 %, 0,46–0,47 MiB, core 0 (X40, 04-10); 1 %, 0,42–0,44 MiB, core 0 (X1, 03-10) | | X90 04-10 |
| Temperatuur | geen sensor | | |

**Watchdog**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Watchdogtoets (meetkern `wdtest`: Hop stopt na 60 s) | **~100 s (DW-watchdog TOP 89 s) naar de kaartkern X1; post-mortem gezien (X42, 04-10)** | | X42 04-10 |

Lezing: als de serve van de O6N in `system` staat (op de OS-core van de
O6N), haalt de Radxa maar 22–23 MB/s in. Dan is de zender de grens, niet
de Radxa.

Bord:
- NIC-interrupt op SPI 64.
- Warme flip: vijf keer, gen 3 op stempel I.
- Console op het glas: HDMI, zonder EDID. USB: twee DWC3's up.

## LicheeRV Nano

Opzet: **R90, 04-10, tools/meet. Vitals met 1 core (`sharegroup: system`),
32 MiB. Naast stulp, stulp-plugins, cloudflared-lean. Over de draad tegen de
O6N (bench-serve :9000, vitals :8090).** Daarvoor: R40, 04-10, main 2e34a2f,
voor alles (vitals 3.0.7, 1 core, 32 MiB, slot 3, zonder tag in `system` op
core 0, de C906B, naast de kern; met alleen welcome erbij).

**Vitals**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| cpu, Msteps/s | **91,1 (R90, 04-10)**; 93,1 (R40, 04-10); 87,7 (R14, 03-10) | | R90 04-10 |
| burn, Msteps/s, max °C | **92,5 → 92,6, 56,7 °C (1 core, 10 s) (R90, 04-10)**; 93,0–93,4, 54,2 °C (R40, 04-10); 88,0–88,2, 57,4 °C (R14, 03-10) | | R90 04-10 |
| membw copy / triad, GB/s | **2,26 / 1,54 (R90, 04-10)**; 2,29 / 1,78 (R40, 04-10); 2,15 / 1,67 (R14, 03-10) | | R90 04-10 |
| memlat 32 KB / 2 MB, ns | **7,23 / 158 (geen 8 MB) (R90, 04-10)**; 7,12 / 159 (geen 8 MB bij 32 MiB) (R40, 04-10); 7,27 / 161 (geen 8 MB bij 32 MiB) (R14, 03-10) | | R90 04-10 |
| alloc, allocs/s | **57.518 (R90, 04-10)**; 58.272 (R40, 04-10); 51.664–56.344 (R14, 03-10) | | R90 04-10 |
| storm, conn/s (p99 ms) | **431 (33,9) (R90, 04-10)**; 517 (30,4) (R40, 04-10); 330–411 (33,8–40,0) (R14, 03-10) | | R90 04-10 |
| rtt naar de kern p50 / p99, µs | **2979 / 10.354 (R90, 04-10)**; 471 / 1051 (R40, 04-10); 2525–2617 / 3677–4366 (los gestart 12 fouten, in `all` 0) (R14, 03-10) | | R90 04-10 |
| timer 1 ms, overslaap p50 / p99, µs | **567 / 3836 (R90, 04-10)**; 63 / 2717 (R40, 04-10); 231–233 / 2186–3234 (R14, 03-10) | | R90 04-10 |
| idle, wekken/s | **48,3 (1 core, 99,9 % idle) (R90, 04-10)**; 46,1 (99,6 % idle) (R40, 04-10); 42,7 (99,3 % idle) (R14, 03-10) | | R90 04-10 |

**Netwerk**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| De node in, MB/s | **11,2 (`system`, bench pull van de O6N, 100 MiB) (R90, 04-10)**; 11,36 (`system`, core 0; het plafond van de link van 100 Mbps); `hop` 4,54 (R40, 04-10); 11,33 (het plafond van de link van 100 Mbps) (R32, 04-10) | | R90 04-10 |
| De node uit, MB/s | **9,2 (`system`, bench push naar de O6N, 100 MiB) (R90, 04-10)**; 9,26 (`system`); `hop` 5,08 (R40, 04-10); 9,05 (R31, 04-10) | | R90 04-10 |
| Verbindingscyclus p50, ms | **6,55 (naar de O6N, vitals storm n=1) (R90, 04-10)** | 16–19 | R90 04-10 |
| rtt over de draad p50, µs | **341 (naar de O6N, p99 478), koud 452 (R90, 04-10)** | | R90 04-10 |
| Storm over de draad, conn/s | **342 (naar de O6N, p99 42,1 ms) (R90, 04-10)** | | R90 04-10 |
| Storm, hairpin naar zichzelf, conn/s (p99 ms) | **117 (171) (R90, 04-10)** | | R90 04-10 |

**In de node**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| App naar app, MB/s | **63,1 (`system`, 400 MB) (R90, 04-10)**; 60,29 (`system`, core 0); `hop` 23,40 (core 1, naast Hop) (R40, 04-10); 35,6 (R31, 04-10) | | R90 04-10 |
| rtt warm p50 / p99, µs | **186 / 340 (R90, 04-10)**; 175 / 320 (`system`); `hop` 3536 / 4147 (R40, 04-10); 179 / 368 (R32, 04-10) | | R90 04-10 |
| rtt koud p50, µs | **213 (R90, 04-10)**; 203 (`system`); `hop` 2443 (R40, 04-10); 204 (R32, 04-10) | | R90 04-10 |

**In rust**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Kern-cpu | **2–3 % (R90, 04-10)**; 2 % (R40, 04-10); 5–9 % (R14, 03-10) | | R90 04-10 |
| Kern-geheugen | **30,0–30,3 van 40 MiB (75,4 %) (R90, 04-10)**; 30,0 van 40 MiB (75,0 %) (R40, 04-10); 30,3 van 40 MiB (75,6 %) (R14, 03-10) | | R90 04-10 |
| Hop | **1 %, 0,63–0,64 MiB, core 1 (R90, 04-10)**; 1 %, 0,64–0,68 van 10 MiB, core 1 (R40, 04-10); 1–2 %, 0,60–0,62 van 10 MiB, core 1 (R14, 03-10) | | R90 04-10 |
| Temperatuur | **56,0–56,3 °C (R90, 04-10)**; 53,5 °C (R40, 04-10); 56,0 °C (R14, 03-10) | | R90 04-10 |

**Watchdog**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Watchdogtoets (meetkern `wdtest`: Hop stopt na 60 s) | **41 s naar de kaartkern R14; geen post-mortem (riscv heeft er nog geen) (R41, 04-10)** | | R41 04-10 |

Lezing: op R40 haalt een app in `system` (core 0, `rings=wb`) 60,3 MB/s app
naar app en 175 µs rtt; in `hop` op de C906L (`rings=maintained`) 23,4 MB/s
en 3,5 ms. Bij Hop op de C906L (tag `sharegroup: hop`) haalt dezelfde vitals
62,6 Msteps/s. Dat is 0,71 van wat hij in `system` haalt, precies de 700
van 1000 MHz. Op de C906L valt memlat bij 128 KB al uit de cache (133 ns).
Zonder loterij (R11) gebruikt de kern in rust 4–5 % cpu, tegen 46–48 % met
loterij (R10).

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

Opzet: **M40, 04-10, tools/meet. Vitals met 2 cores, 128 MiB. Naast spin.
Over de draad tegen de O6N (bench-serve :9000, vitals :8090).** Daarvoor:
M40, 04-10, main 2e34a2f (warm vanaf M2). Vitals met 2 cores (`cpu_shares`
2048), 128 MiB, slot 4 op E-cores 4 en 5, naast spin en spin-tunnel. Hop is
daar nog 3.0.0 (geen telemetrie, dus geen *In rust*).

**Vitals**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| cpu, Msteps/s | **541 (M40, 04-10)**; 541 (E-core); 587 met `core-class: big` (P-core 7) (M40, 04-10); 538 (E-core) (M1, 03-10) | | M40 04-10 |
| smp, speedup | **2,00 (2 cores) (M40, 04-10)**; 2,00 (2 E-cores) (M40, 04-10) | | M40 04-10 |
| burn, Msteps/s | **1084 → 1084 (2 cores, 10 s) (M40, 04-10)**; 1083 → 1083 (2 E-cores; geen sensor zonder `hopos.smc=1`) (M40, 04-10) | | M40 04-10 |
| membw copy / triad, GB/s | **36,2 / 32,0 (M40, 04-10)**; 36,3 / 32,4 (M40, 04-10) | | M40 04-10 |
| memlat 32 KB / 2 MB / 8 MB, ns | **1,84 / 7,19 / 24,7 (M40, 04-10)**; 1,85 / 7,06 / 19,7 (M40, 04-10) | | M40 04-10 |
| alloc, allocs/s | **481.101 (M40, 04-10)**; 475.502 (M40, 04-10) | | M40 04-10 |
| storm, conn/s (p99 ms) | **9882 (1,07) (M40, 04-10)**; 9540 (1,08); hairpin via 192.168.1.122:8090 3285 (4,22) (M40, 04-10); 4820–4872 (3,6–5,1) (M1, 03-10) | | M40 04-10 |
| rtt naar de kern p50 / p99, µs | **181 / 708 (M40, 04-10)**; 130 / 146 (M40, 04-10); 62–76 / 72–135 (M1, 03-10) | | M40 04-10 |
| timer 1 ms, overslaap p50 / p99, µs | **1024 / 3442 (M40, 04-10)**; 1021–1027 / 2815–10324 (twee runs: p99 vreemd) (M40, 04-10) | | M40 04-10 |
| idle, wekken/s | **40,9 (2 cores) (M40, 04-10)**; 47,9 (M40, 04-10); 44,8 (99,96 % idle) (M1, 03-10) | | M40 04-10 |
| disk schrijven / lezen, MB/s | **1101 / 1357 (64 MB; 4 KiB 53,2) (M40, 04-10)**; 1046 / 1309 (64 MB; 4 KiB 110) (M40, 04-10) | | M40 04-10 |

**Netwerk**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| De node in, MB/s | **107,9 (bench pull van de O6N, 256 MiB) (M40, 04-10)**; 65,2–68,8 (bench pull van de O6N, 256 MiB; O6N druk) (M40, 04-10) | 43,3–46,7 (HTTP PUT) | M40 04-10 |
| De node uit, MB/s | **117,4 (bench push naar de O6N, 256 MiB) (M40, 04-10)**; 116,5 (bench push naar de O6N, 256 MiB; lijnsnelheid; O6N druk) (M40, 04-10) | 47,7–52,3 (HTTP GET) | M40 04-10 |
| rtt over de draad p50, µs | **116 (naar de O6N, p99 146), koud 129 (M40, 04-10)**; 198 (naar de O6N, p99 6567), koud 343 (O6N druk) (M40, 04-10) | 50–66 | M40 04-10 |
| Storm over de draad, conn/s | **4871 (naar de O6N, p99 4,38 ms) (M40, 04-10)** | 6379 (p99 1,9 ms) | M40 04-10 |
| Verbindingscyclus p50, ms | **0,54 (naar de O6N, vitals storm n=1) (M40, 04-10)** | | M40 04-10 |
| Storm, hairpin naar zichzelf, conn/s (p99 ms) | **2449 (5,09) (M40, 04-10)** | | M40 04-10 |

**In de node**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| App naar app, MB/s | **3104 (400 MB) (M40, 04-10)**; 4331 (E-cores 4 en 5); P-cores 7 en 8 4346–4512 (twee runs) (M40, 04-10); 6379–6383 (P-core); E-core 4448–4468 (M33, M20 01-10) | 764–769 | M40 04-10 |
| rtt warm p50 / p99, µs | **44 / 50 (M40, 04-10)**; 46 / 48 (E); P 41 / 234 (M40, 04-10) | | M40 04-10 |
| rtt koud p50, µs | **45 (M40, 04-10)**; 50 (E); P 43 (M40, 04-10) | | M40 04-10 |
| App naar app, 40 GiB, MB/s | 2813 | | M33 01-10 |

**Watchdog**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Watchdogtoets (meetkern `wdtest`: Hop stopt na 60 s) | — (niet gedaan: Hop daar is nog 3.0.0 en de leader kent geen jobs) | | |

**Opslag**

| Meting | v3 | v2 | Run |
| --- | --- | --- | --- |
| Rauw 1 MiB, schrijven / lezen, MB/s | 4952 / 1925 | 5389–5756 / 1824–1971 | M23 01-10 |
| hopfs 1 MiB, schrijven / lezen, MB/s | 5015 / 2026 | 5480–5632 / 1781–1816 | M23 01-10 |
| Sequentieel 1 GiB, schrijven / lezen, MB/s | 4758 / 1629 | | M23 01-10 |
| Willekeurig 4 KiB, schrijven / lezen, IOPS | 140.000 / 11.900 | | M23 01-10 |
| Kern naar app zonder schijf (`hole=1`), MB/s | **1511 (256 MiB) (M40, 04-10)**; 1879–1917 (M24, 01-10) | | M40 04-10 |
| App-opslag 256 MiB, schrijven / lezen, MB/s | **1288 / 1675 (4 KiB 111) (M40, 04-10)**; 1269–1270 / 1683–1696 (M24, 01-10) | 1598–1657 / 1064–1072 | M40 04-10 |
| 4 KiB schrijven als RPC-lus, MB/s | 82 (naast een bulk-app 15–19) | | 01-10 |
| Willekeurig 4 KiB lezen door apps, opdrachten/s | **met de bundel (`read_many`, fd11581; M70b, 04-10): 1 app één per call 8.399, bundel 4 21.758, bundel 16 55.834, twee bundels 80.521; 4 apps 28.843 / 77.829 / 161.485; 6 apps 39.744 / 104.493 / 171.345 (rauw 175k met zestien); naast spin**; zonder schijf (`hole=1`, alleen het pad app naar OS-core naar app): 1 app 30.986 (p50 28 µs), 2 apps 41.701, 4 apps 66.089, 6 apps 76.577; naast spin; echt lezen niet gemeten (schrijft eerst 256 MiB per app) (M40, 04-10)**; 1 app 7600, 2 apps 14.500, 4 apps 25.500 | | M40 04-10 |
| Replica (SQLite op HopFS), persist-proef, ms | 193, over een flip heen | | M33 01-10 |

Lezing: op M40 haalt app naar app op een P-core 4350 MB/s waar M33 6379
deed, niet sneller dan op een E-core; Hop is daar nog 3.0.0. Rauw schrijven
en app-opslag schrijven liggen onder de v2-lat.

Nog te meten: *In rust* (na een Hop met telemetrie), de watchdogtoets.

Bord:
- Hop: ingebakken, gaat warm mee over flips (`HOPOS_HOP_RESUMED`).
- Warme flip: **gen 4 op M40, 04-10 (3 bewoners, 18 NAT-flows, Hop mee)**;
  gen 1 tot 8 op 01-10, adoptie 3 van 3. Koud niet (geen CPU_OFF).
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
- Energie: daarvoor is een meter nodig. Watchdog-herstel staat sinds 04-10
  per board in *Watchdog* (meetkern `wdtest`, de tijd van de console).
