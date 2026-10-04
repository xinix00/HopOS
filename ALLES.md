# ALLES

Wat er nog moet, sinds de eerste boot op ijzer (30-09-2026). Alleen de
to-do's; wat af is gaat eruit. Stand van de borden in de tabel; de getallen
in docs/measurements.md, de details per board in docs/boards-*.md.

## De tabel

De afvinkmatrix van de Go-tijd (OLD/docs/support.md: boot, idle en klokken,
devices en diensten per board), nu voor v3 en bijgehouden op ijzer. Legenda:
✓ gezien op het board, ○ gebouwd maar op dit board nog niet gezien, ✗ ontbreekt
of faalt, en een streep waar het bewust niet komt. Stand 04-10-2026, ochtend (main 182480b; Pi 4 P40, Pi 5 Q40, Radxa X40, O6N O40, M4 M40 met Hop 3.0.0, LicheeRV R40, Altra A56g met de boot-stack-fix; release v3.0.7, Hop v3.0.7).
Een cel zegt alleen of het slaagt, met hooguit de stempel of een paar
woorden waarom niet; een gepolde NIC is geen ✓. De getallen staan in
docs/measurements.md, de details per board in docs/boards-*.md.

| | QEMU virt | Pi 5 | Pi 4 | Radxa | Altra | O6N | M4 | LicheeRV |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Boot, EL2, kooi, zelftest | ✓ | ✓ P1, 03-10 | ✓ P1, 03-10; koud ✓ de VL805-firmware laadt (P1g) | ✓ X1, 03-10 | ✓ A3, 03-10 (eerste v3-boot; venster A000, 128 cores) | ✓ O2, 03-10 | ✓ M7 | ✓ R13, 03-10 |
| Hop als bewoner, welcome door de DNAT | ✓ | ✓ P1, in `system` | ✓ P1, in `system` | ✓ X1, in `system` | ✓ A3, in `system` | ✓ O2, in `system` | ✓ M1, HOPOS_HOP_RESUMED met HOPOS_PRIVILEGE | ✓ R10, 03-10 |
| Kern-flip, warm | ✓ | ✓ P2g tot P4g, 03-10; koud weigert vroeg en Hop zet de taken terug (P4, op ijzer) | ✓ I | ✓ I | ✓ A4g, 03-10 (gen 2, bewoners en NAT mee) | ✓ H; I ○ koude boot, fix de61b4b | ✓ M1, 03-10 (gen 2, Hop met bevoegdheid mee, spin door); koud – geen PSCI | – niet op riscv64; koud ✓ R10, 03-10 |
| NIC met interrupt | ✓ | ✓ | – gepold, zoals Go | ✓ | ✓ A8g, 03-10 (MSI-X via ITS 7 van zijn root-complex) | ✓ | ✓ M1, 03-10 (AIC 1253) | ✓ R13, 03-10 |
| Hardware-IRQ (NIC, kick, timer) | ✓ | ✓ | ✓, NIC gepold | ✓ | ✓ A8g (ITS, timer, NIC) | ✓ | ✓ M1 (timer-FIQ, fast IPI, NIC op de AIC) | ✓ R13, 03-10 |
| Gebruik per taak, van de kern en van Hop (cpu, geheugen; slot 0, systeemtaken) | ✓ arm64, 03-10 | ✓ P1, 03-10 | ✓ P1, 03-10 | ✓ X1, 03-10 | ✓ A3, 03-10 | ✓ O2, 03-10 | ✓ M1, 03-10 | ✓ R13, 03-10 |
| Off-link door de NAT, SNTP | ✓ | ✓ | ✓ | ○ niet nagelopen | ✓ A3 (welcome van GitHub, klok van Hop) | ○ niet nagelopen | ○ niet nagelopen | ○ niet nagelopen |
| Watchdog gewapend en geaaid | – geen watchdog in virt | ✓ P1, canary | ✓ P1, canary | ✓ X1, canary | ✓ A3, SBSA, canary | ✓ O2, canary | ✓ | ✓ R3, 03-10 |
| Hardware-RNG voor de kern | ✗ alleen jitter | ✓ | ✓ | ✓ | ✗ jitter: geen SMCCC-TRNG in de firmware, efi-rng hing (juli) | ○ wacht op de stick | ✗ alleen jitter | ✗ geen bron |
| Hardware-RNG voor de slots (CTRL_RNG_SEED, fc5348f) | ○ alleen jitter | ✓ P1, 03-10 | ✓ F | ✓ F | ○ jitter | ○ wacht op de stick | ○ alleen jitter | ○ niet nagelopen |
| Temperatuur in de tik | – geen sensor in virt | ✓ | ✓ | ✗ sensor converteert niet | ✓ A3, SMpro 47 C | ✓ | ✗ niet gebouwd | ✓ R5, 03-10 |
| Klokbeleid (dvfs) | – geen klok in virt | ✓ | ✓ | – klok van de firmware, bewust | – firmware-domein, bewust | ✓ | ✓ M7 | – vaste klok |
| Console op het glas | ✓ | ✗ firmware weigert sinds de herflash | ✓ | ✓ | ✓ A4, 03-10 (GOP 1024x768) | ✓ | – bewust uit | – geen scherm |
| USB xHCI (HID, display-app) | ✓ | ○ niets ingeplugd | ✓ P2g, 03-10 (enable slot met completion, d107b31) | ○ niets ingeplugd | ○ niets ingeplugd | ✓ | – niet gepland | – niet gepland |
| Opslag (hopfs, volumes, OP_SYNC) | ✓ | – bewust geen schijf | – geen schijf | – stateless, bewust | ✓ A4, 03-10 (NVMe SN770 500 GB, hopfs vers, commits) | ✓ | ✓ M24 | – geen SD-driver |
| Console op 5555 | ✓ | ✓ | ✓ | ✓ | ✓ A3 | ✓ | ✓ | ○ niet nagelopen |
| Hardwaredecoder (media-smaak) | – geen media-smaak | – geen media-smaak | – geen media-smaak | – geen media-smaak | – geen media-smaak | ✓ 30-09, weg tot de koude boot | – geen media-smaak | – geen media-smaak |
| Kaart of stick klaar in `target/` | – niets te flashen | ✓ P1 geschreven | ✓ P1 | ✓ X1 | ✓ A4 (gui) op de stick | ✓ O2 op de stick | ✓ M24 | ✓ R13 |

## Te doen

### Nu

- [ ] Agent op de Altra: gebundeld lezen door apps, zoals io_uring. Nu is
      elke lees van een app één RPC door de OS-core met één opdracht in de
      lucht (M4 01-10: 7.600 per app, 36.000 met acht apps, Linux op
      dezelfde M4 100.000 tot 200.000; rauw in de kern 175k en op de Altra
      289k met 16 tegelijk). Eén op die tot 16 leesopdrachten per aanroep
      bij de drive zet en met één kick terugkomt, `read_many` in applib,
      bench met een diepte per app; doel richting het rauwe plafond.
- [ ] Daarna HopOS 3.0.8 (het Altra-image van v3.0.7 boot niet, het
      config-venster en `hop image` zijn nieuw, de GEM van de Pi 5, handoff),
      Hop 3.0.8 (pin v3.0.8, lean v3.1.9, hop-gui opnieuw), en de media met
      `hop image --config`: Radxa-kaart (v3.0.5 stormt na 2 GiB door de
      MMC-maskers), Altra-stick, M4 via Recovery (Hop is daar nog 3.0.0 en
      de leader kent geen jobs: spin opnieuw POSTen, daarna pas de
      watchdogtoets daar).

### Fixen

- [ ] Hop op de OS-core verhongert als de OS-core vol zit: bij 4 of meer
      apps die de kern bestoken krijgt Hop op de O6N nog 1 % en antwoorden
      `/v1/agents` en de apps 10 s niet (04-10, nulmeting). Hop hoort een
      gegarandeerd deel van de OS-core te hebben, of op borden met cores
      genoeg niet op de OS-core te wonen.
- [ ] NVMe op PCI zonder interruptlijn: de pacer van blkdev::Queue pollt de
      eerste 100 us na elke opdracht elke ronde, dus de OS-core slaapt nooit
      zolang er een lees in de lucht is en kern-cpu zegt niets; MSI-X zoals
      de igb en de RTL8125 al hebben.
- [ ] Hop: chunked transfer weigert; een plaatsing zonder capaciteit blijft
      proberen (ruis); na elke flip `HOP_STORE_KERNEL NEXT_STORE failed
      (timed out)` (04-10 op Pi 4, Pi 5 en M4).
- [ ] vitals: de standaard-rx-URL werkt niet (`CONNECT is not supported`).

### Op ijzer te zien

- [ ] Het config-venster: de bootregel `HOPOS_CFG_WINDOW` per bord, een
      warme flip met `HOPOS_FLIP_CFG`, en `hop image --write` op een echte
      kaart (de LicheeRV eerst met `CFG=`, anders boot hij zonder config).
- [ ] De handoff-fix: het STALE-pad (een gepatchte stickkern die een oud
      paar vindt, dus pas met een 3.0.8-stick) en de nieuwe flip-ingang op
      de M4 en de Pi's (op de O6N gezien: O2 koud op met de doos van de
      toetskern, O2 warm naar main landt).
- [ ] Pi 4: de genet leest nu uit WB met een veeg (blok 160 Normal-WB in
      de boot-map, in dezelfde patch als de GEM van de Pi 5); op ijzer te
      zien: pull richting 110 met bad=0, push gelijk aan 112.
- [ ] Altra: de koude flip (met de boot-stack-fix moet hij nu lukken).
- [ ] Core-reclaim in een sharegroup met een rekenaar
      (`HOPOS_CORE_RECLAIM` na 2 s).
- [ ] Radxa: de scrub van een Device-gemapte pool (twee keer dezelfde
      tamago-app); op de M4 op 03-10 gezien.
- [ ] Pi 4: HID en de display-app op de VL805.
- [ ] O6N: de mediaketen met Lumen (MMC, HEVC, WebDAV) na een koude boot,
      en daarna `DELETE`: een gewone job en een flipbundel moeten plaatsen
      (`HOPOS_HEAP_REFUSED` meldt het als de heap op is).

### Meten

- [ ] De pull van de O6N naar Pi 5 en Radxa zakte van 111 MB/s (gisteravond
      en 08:45) naar 85 (vanaf 10:00) en bleef daar, ook na een koude boot
      van de O6N en met de oude bench-ELF aan beide kanten (A/B: nieuw 85,
      oud 87, gemengd 62 tot 76); de push de andere kant op blijft 110. Wat
      verandert er in de ontvangers of op de draad in die uren.
- [ ] De OS-core als grens voor veel kleine calls: meten met een echte
      database van 100 GB of meer.
- [ ] Radxa: de hairpin-storm haalt 620 tot 757 conn/s met p99 16 ms,
      tegen 1900 op de Pi 4: waarom.
- [ ] M4: app naar app op een P-core 4350 MB/s tegen 6379 op M33, en niet
      sneller dan op een E-core: waarom.

### Later

- [ ] LicheeRV: de 16550-console wacht nog per teken (de PL011-borden
      hebben sinds de conport-pomp een niet-wachtende UART); een
      `write_nowait` dat tot de FIFO-diepte schrijft op THRE (Linux
      tx_loadsz). En de timer-overslaap p99 van 2,7 ms op de LicheeRV en 3
      tot 10 ms op de M4 is daarmee nog niet verklaard.
- [ ] OS-core WFE-pad: met een vitals tegen de applib van HEAD had 4 van 25
      runs één dial van 0,75 tot 1,3 ms: de kick van de app ging op +33 tot
      +82 us uit, de kern pakte de SYN pas een event-stream-periode later op
      (Pi 4, 04-10).
- [ ] Eerlijkheid op de OS-core: 4 KiB-calls van een buurman zakken van 82
      naar 15 tot 19 MB/s naast een bulk-app (M4, 01-10); er blijft
      voortgang, dus pas knippen (kleinere brokken met een yield) als het
      ergens knelt; eerst opnieuw meten op main met TURN_CAP.
- [ ] Pi 5: het glas (de firmware weigert elke framebuffer sinds de
      herflash, 0x80000001).
- [ ] Wachtpagina onder de boot-stack op UEFI en RISC-V (de overloop van
      302d257 schreef stil over .bss; na a6ceae5 is de diepste boot 156 KB
      van 256) en een wachtpost in qemu-test.sh; de device-op (19) zonder
      hosttests; de schijf-interruptlijn buiten QEMU virt.
- [ ] Radxa: TSADC geeft geen code (daardoor geen thermische rem op 1800 MHz;
      `hopos.mhz=1416` klemt); geen serienummer-terugval voor de MAC.
- [ ] LicheeRV: apps in groep `hop` op de C906L houden een device-staart
      (4,5 MB/s over de draad tegen 11,4 in `system`); gecachet kan pas als
      de app zelf cache-onderhoud doet.
- [ ] Altra RNG: de firmware heeft geen SMCCC-TRNG (TRNG_VERSION
      NOT_SUPPORTED, 03-10) en het EFI_RNG_PROTOCOL hing er in juli; blijft
      jitter tot iemand efi-rng daar met een tijdslimiet durft te proberen.
- [ ] Hop op de host: SIGTERM; de S3-lock met twee HopOS-nodes op ijzer.
- [ ] De plugins als node (hop-gui, hoplb, hopdns, hopprom, hoplockserver,
      cloudflared-lean met een echt token) en Replica (`sqlite-persist`).

## Het plafond: Linux of macOS op dezelfde M4 tegen HopOS (01-10 avond, M33)

Uit reviews en Asahi-metingen, niet door ons gemeten; HopOS wel.

| pad | Linux/macOS op de M4 | HopOS M33 |
| --- | --- | --- |
| sequentieel lezen, rauw | 3000 tot 3500 MB/s | 1925 rauw, 1690 door de app |
| sequentieel schrijven, rauw | 2500 tot 3000 | 4950 rauw, 1270 door de app |
| willekeurig 4 KiB lezen, één tegelijk | 15.000 tot 20.000 per seconde | 11.900 |
| willekeurig 4 KiB lezen, met wachtrij, één proces | 100.000 tot 200.000 | 175.000 rauw in de kern |
| willekeurig 4 KiB lezen, door apps | 100.000 tot 200.000 (met page cache veel meer) | 25.000 met vier apps, 36.000 met acht |
| app naar app, één stroom (loopback) | 5000 tot 10.000 MB/s | 6380 |
| hete data uit RAM | page cache, tientallen GB/s | geen cache |

App naar app zit op het Linux-niveau, de schijf zelf halen we (175.000 is
het ijzer). Wat ertussen zit, de OS-core met ~27 µs per call, is de
resterende factor vier tot acht voor veel kleine leesopdrachten door apps,
naast de page cache voor hete data. Derek (01-10): "ik vind dit al
behoorlijk". Geen haast.
