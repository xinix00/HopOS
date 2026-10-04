# ALLES

Wat er nog moet, sinds de eerste boot op ijzer (30-09-2026). Alleen de
to-do's; wat af is gaat eruit. Stand van de borden in de tabel; de getallen
in docs/measurements.md, de details per board in docs/boards-*.md.

## De tabel

De afvinkmatrix van de Go-tijd (OLD/docs/support.md: boot, idle en klokken,
devices en diensten per board), nu voor v3 en bijgehouden op ijzer. Legenda:
✓ gezien op het board, ○ gebouwd maar op dit board nog niet gezien, ✗ ontbreekt
of faalt, en een streep waar het bewust niet komt. Stand 04-10-2026, ochtend (main 2e34a2f: Pi 4 P40, Pi 5 Q40, Radxa X40, O6N O40, M4 M40, LicheeRV R40; Altra nog A4 op de stick; release v3.0.7 is uit, Hop v3.0.7 volgt met 3.0.8).
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

- [ ] De meetronde van 04-10 afmaken: de Altra (agent bezig: sinds de flip
      naar de bench-kern A13b komt elke flip vanaf de stickkern A4, warm en
      koud, terug op A4 met een lege zwarte doos); de
      draadmetingen opnieuw met een stille O6N, met de M4 als tegenpartij
      voor de O6N; de watchdogtoets op de M4 na een nieuwe Hop via
      Recovery.
- [ ] Na HopOS v3.0.7 (uit, 36 assets, apps vernieuwd): Hop 3.0.8 met de pin
      op v3.0.7 en de lean-tags naar v3.1.9 (hop-gui ook opnieuw tegen
      v3.0.7); dan de media: nieuwe Radxa-kaart (v3.0.5 stormt na 2 GiB
      verkeer door de MMC-maskers), het M4-image via Recovery (Hop is daar
      nog 3.0.0 zonder telemetrie en de leader kent geen jobs, dus een reset
      of koude flip brengt spin niet terug; spin daarna opnieuw POSTen) en
      de Altra-stick.

### Fixen

- [ ] MAGIC + CONFIG in het image, zoals v2: een vast venster met een magic
      en padding voor de configfile, zodat de Hop-imager de image van elk
      bord kan schrijven en de config erin zet (en de kern hem vindt);
      dan stelt Derek alles weer in zonder losse cfg's en sticks.
- [ ] Hop: chunked transfer weigert; een plaatsing zonder capaciteit blijft
      proberen (ruis); na elke flip `HOP_STORE_KERNEL NEXT_STORE failed
      (timed out)` (04-10 op Pi 4, Pi 5 en M4).
- [ ] Na een harde reset raapt de stickkern de oude flip-overdracht in het
      DRAM op en landt erop als warme flip (O6N 04-10: O2 na de
      watchdogtoets van O41w). Hop stopt meteen, pas de boot-guard geeft
      een koude boot (2,5 min), en de post-mortem is die van de tussenboot.
      Agent `handoff` is bezig.
- [ ] Een koude flip met dezelfde bundel stopt eerst de taken
      (`HOP_FLIP_COLD_STOP`), dan weigert de kern (`HOPOS_FLIP_REFUSED same
      bundle`) en komt welcome niet terug, terwijl `/v1/jobs` hem noemt
      (Pi 4, P10g).
- [ ] LicheeRV: elke boot `HOPOS_OS_SELFTEST_FAIL` (alle vier de proeven
      melden `Irq`, R40); een job met `cpu_shares` krijgt `HOP_NO_CAPACITY`
      (cpu 0/0 shares, Hop ziet `HOPOS_CORES=0`).
- [ ] vitals: de standaard-rx-URL werkt niet (`CONNECT is not supported`).
- [ ] M4: het transport van een bulk-app in kleinere brokken met een yield
      (4 KiB-calls van de buurman blijven rond 19 tot 20).
- [ ] De boot-stack van 256 KB heeft 43 KB marge: gui::usb::run en het
      FsActor-blok naar de heap, een wachtpost in qemu-test.sh.
- [ ] Pi 5: het glas (de firmware weigert elke framebuffer sinds de
      herflash).

### Op ijzer te zien

- [ ] Altra: de koude flip (de SBSA-watchdog en de watchdogtoets zitten in
      de meetronde bij Nu).
- [ ] Core-reclaim in een sharegroup met een rekenaar
      (`HOPOS_CORE_RECLAIM` na 2 s).
- [ ] Radxa: de scrub van een Device-gemapte pool (twee keer dezelfde
      tamago-app); op de M4 op 03-10 gezien.
- [ ] Pi 4: HID en de display-app op de VL805.
- [ ] O6N: de mediaketen met Lumen (MMC, HEVC, WebDAV) na een koude boot,
      en daarna `DELETE`: een gewone job en een flipbundel moeten plaatsen
      (op 30-09 zat de oude bump-heap vol; nu meldt de tik
      `HOPOS_HEAP_REFUSED` als dat gebeurt).

### Meten

- [ ] O6N schrijven ~700 MB/s tegen 800 tot 1188; app-opslag O6N 414
      schrijven en 85 lezen.
- [ ] De OS-core als grens voor veel kleine calls: meten met een echte
      database van 100 GB of meer.
- [ ] Radxa: de hairpin-storm haalt 620 tot 757 conn/s met p99 16 ms,
      tegen 1900 op de Pi 4: waarom.
- [ ] M4: app naar app op een P-core 4350 MB/s tegen 6379 op M33, en niet
      sneller dan op een E-core: waarom.

### Later

- [ ] Radxa: TSADC geeft geen code (daardoor geen thermische rem op 1800 MHz;
      `hopos.mhz=1416` klemt); geen serienummer-terugval voor de MAC.
- [ ] O6N: een zender op de OS-core (bench-serve in `system`) haalt over de
      draad maar 22 tot 23 MB/s, op een eigen core 111 (gezien bij de
      Radxa-meting, 04-10); dat is de prijs van de OS-core voor een app die
      zelf zendt.
- [ ] LicheeRV: apps in groep `hop` op de C906L houden een device-staart
      (4,3 MB/s over de draad); gecachet kan pas als de app zelf
      cache-onderhoud doet.
- [ ] Altra RNG: de firmware heeft geen SMCCC-TRNG (TRNG_VERSION
      NOT_SUPPORTED, 03-10) en het EFI_RNG_PROTOCOL hing er in juli; blijft
      jitter tot iemand efi-rng daar met een tijdslimiet durft te proberen.
- [ ] Guard-pagina op UEFI en RISC-V; de device-op (19) zonder hosttests; de
      schijf-interruptlijn buiten QEMU virt.
- [ ] Hop op de host: SIGTERM; de S3-lock met twee HopOS-nodes op ijzer.
- [ ] De plugins als node (hop-gui, hoplb, hopdns, hopprom, hoplockserver,
      cloudflared-lean met een echt token) en Replica (`sqlite-persist`).
- [ ] applib `leave_group` en lean `Stack::leave_group`.

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
