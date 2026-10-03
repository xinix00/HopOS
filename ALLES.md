# ALLES

Wat er nog moet, sinds de eerste boot op ijzer (30-09-2026). Alleen de
to-do's; wat af is gaat eruit. Stand van de borden in de tabel; de getallen
in docs/measurements.md, de details per board in docs/boards-*.md.

## De tabel

De afvinkmatrix van de Go-tijd (OLD/docs/support.md: boot, idle en klokken,
devices en diensten per board), nu voor v3 en bijgehouden op ijzer. Legenda:
✓ gezien op het board, ○ gebouwd maar op dit board nog niet gezien, ✗ ontbreekt
of faalt, en een streep waar het bewust niet komt. Stand 03-10-2026, avond (LicheeRV R17, O6N O9, Pi 4 P8g, Radxa X2, Pi 5 P6g, Altra A12g, M4 M1; main loopt voor op de release v3.0.6 en Hop v3.0.7).
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

- [ ] OS-core: de O6N zit na de snoei van de deur, de switch-ronde en lazy
      FP (b6878f9) op rtt p50 86 us (was 308): het doel onder 100 is daar
      gehaald. LicheeRV rtt 527 (was 644), doel onder 300: de agent kijkt
      naar de riscv-kant (de executor-ronde, de C906-barrières), meten
      zodra Derek klaar is op het bord.
- [ ] Ontdubbelen: geland en op ijzer gezien zijn Placer (koude flip
      O6N), stmmac (LicheeRV; Radxa zodra de dvfs-agent klaar is),
      poll_until (NIC up op O6N, Pi 4, Pi 5, Altra, LicheeRV; M4 nog), de
      negen kleine plus één ARP-tabel met lean v3.1.9 (`HOPOS_FLIP_NAT
      restored=1 of=1` en de canary overal; Pi 4 hairpin twee keer 1900
      conn/s), NVMe als één kern (`HOPOS_DISK_QUEUE depth=16` op O6N en
      Altra, `HOPOS_NVME_UP`; de bench met `hopos.nvmebench=1` en de M4 nog).
      Agenten nog bezig: kooi/OS-core per ISA, Radxa-dvfs, de riscv-kant van
      de beurt, de LicheeRV over de draad.
- [ ] LicheeRV over de draad: pull van de O6N 3,92 MB/s op een link van 100
      Mbps (plafond ~11,5), lokaal 20,5. Agent op het bord (A/B tegen R15,
      microbench van de framekopie, de dwmac-tellers, de pomp).
- [ ] Pi 4: vitals `rx` via de NAT naar de Mac gaf een leeg resultaat en
      een warme flip tijdens die pull een 502; uitzoeken (de Mac is geen
      meetpunt, wel een NAT-pad).
- [ ] De volgende bump: HopOS 3.0.7 met tag, Hop erop naar 3.0.8, release.sh,
      media (main heeft sinds v3.0.6 en Hop v3.0.7 de avondfixes van 03-10:
      de NAT-recycler, de Radxa over de draad, de OS-core-beurt, de koude
      flip vooraf, de heap-melding); daarmee het M4-image via Recovery
      (spin daarna opnieuw POSTen) en de Altra-stick.

### Fixen

- [ ] Hop: chunked transfer weigert; een plaatsing zonder capaciteit blijft
      proberen (ruis); na een flip één `NEXT_STORE failed`.
- [ ] vitals: de standaard-rx-URL werkt niet (`CONNECT is not supported`).
- [ ] M4: het transport van een bulk-app in kleinere brokken met een yield
      (4 KiB-calls van de buurman blijven rond 19 tot 20).
- [ ] De boot-stack van 256 KB heeft 43 KB marge: gui::usb::run en het
      FsActor-blok naar de heap, een wachtpost in qemu-test.sh.
- [ ] Pi 5: het glas (de firmware weigert elke framebuffer sinds de
      herflash).

### Op ijzer te zien

- [ ] Overal: de echte watchdogtoets (kabel eruit of Hop stoppen, reset
      binnen de termijn); Altra ook de SBSA-watchdog en de koude flip.
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

- [ ] De ring doet het cache-onderhoud op head en tail altijd: eerst meten
      (bench pull M4 en O6N met en zonder), dan 20 tot 30 regels.
- [ ] O6N schrijven ~700 MB/s tegen 800 tot 1188; app-opslag O6N 414
      schrijven en 85 lezen.
- [ ] De OS-core als grens voor veel kleine calls: meten met een echte
      database van 100 GB of meer.
- [ ] `hopos.codecdemo` op de O6N.

### Later

- [ ] Radxa: TSADC geeft geen code; geen serienummer-terugval voor de MAC.
- [ ] Altra RNG: de firmware heeft geen SMCCC-TRNG (TRNG_VERSION
      NOT_SUPPORTED, 03-10) en het EFI_RNG_PROTOCOL hing er in juli; blijft
      jitter tot iemand efi-rng daar met een tijdslimiet durft te proberen.
- [ ] Guard-pagina op UEFI en RISC-V; de device-op (19) zonder hosttests; de
      schijf-interruptlijn buiten QEMU virt.
- [ ] Hop op de host: SIGTERM; de S3-lock met twee HopOS-nodes op ijzer.
- [ ] De plugins als node (hop-gui, hoplb, hopdns, hopprom, hoplockserver,
      cloudflared-lean met een echt token) en Replica (`sqlite-persist`).
- [ ] applib `leave_group`; lean: de IPv6-baan en `Stack::leave_group`;
      docs/boards-radxa.md bijwerken.

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
