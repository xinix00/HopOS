# ALLES

Wat er nog moet, sinds de eerste boot op ijzer (30-09-2026). Alleen de
to-do's; wat af is gaat eruit. Stand van de borden in de tabel; de getallen
in docs/measurements.md, de details per board in docs/boards-*.md.

## De tabel

De afvinkmatrix van de Go-tijd (OLD/docs/support.md: boot, idle en klokken,
devices en diensten per board), nu voor v3 en bijgehouden op ijzer. Legenda:
✓ gezien op het board, ○ gebouwd maar op dit board nog niet gezien, ✗ ontbreekt
of faalt, en een streep waar het bewust niet komt. Stand 03-10-2026, middag (de LicheeRV op R14, de O6N op O2, de Pi 4 op P1, de Radxa op X1, de Pi 5 op P1, de Altra op A4 met NVMe; alles van main 8a91d57 met Hop d785ef5).
Een cel zegt alleen of het slaagt, met hooguit de stempel of een paar
woorden waarom niet; een gepolde NIC is geen ✓. De getallen staan in
docs/measurements.md, de details per board in docs/boards-*.md.

| | QEMU virt | Pi 5 | Pi 4 | Radxa | Altra | O6N | M4 | LicheeRV |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Boot, EL2, kooi, zelftest | ✓ | ✓ P1, 03-10 | ✓ P1, 03-10; koud ✓ de VL805-firmware laadt (P1g) | ✓ X1, 03-10 | ✓ A3, 03-10 (eerste v3-boot; venster A000, 128 cores) | ✓ O2, 03-10 | ✓ M7 | ✓ R13, 03-10 |
| Hop als bewoner, welcome door de DNAT | ✓ | ✓ P1, in `system` | ✓ P1, in `system` | ✓ X1, in `system` | ✓ A3, in `system` | ✓ O2, in `system` | ✓ M1, HOPOS_HOP_RESUMED met HOPOS_PRIVILEGE | ✓ R10, 03-10 |
| Kern-flip, warm | ✓ | ✓ P2g tot P4g, 03-10; koud weigert vroeg en Hop zet de taken terug (P4, op ijzer) | ✓ I | ✓ I | ✓ A4g, 03-10 (gen 2, bewoners en NAT mee) | ✓ H; I ○ koude boot, fix de61b4b | ✓ M1, 03-10 (gen 2, Hop met bevoegdheid mee, spin door); koud – geen PSCI | – niet op riscv64; koud ✓ R10, 03-10 |
| NIC met interrupt | ✓ | ✓ | – gepold, zoals Go | ✓ | – gepold, bewust (igb) | ✓ | ✓ M1, 03-10 (AIC 1253, eerste interrupt na 6 us) | ✓ R13, 03-10 |
| Hardware-IRQ (NIC, kick, timer) | ✓ | ✓ | ✓, NIC gepold | ✓ | ✓ A3 (ITS, timer); NIC gepold | ✓ | ✓ M1 (timer-FIQ, fast IPI, NIC op de AIC) | ✓ R13, 03-10 |
| Gebruik per taak, van de kern en van Hop (cpu, geheugen; slot 0, systeemtaken) | ✓ arm64, 03-10 | ✓ P1, 03-10 | ✓ P1, 03-10 | ✓ X1, 03-10 | ✓ A3, 03-10 | ✓ O2, 03-10 | ✓ M1, 03-10 | ✓ R13, 03-10 |
| Off-link door de NAT, SNTP | ✓ | ✓ | ✓ | ○ niet nagelopen | ✓ A3 (welcome van GitHub, klok van Hop) | ○ niet nagelopen | ○ niet nagelopen | ○ niet nagelopen |
| Watchdog gewapend en geaaid | – geen watchdog in virt | ✓ P1, canary | ✓ P1, canary | ✓ X1, canary | ✓ A3, SBSA, canary | ✓ O2, canary | ✓ | ✓ R3, 03-10 |
| Hardware-RNG voor de kern | ✗ alleen jitter | ✓ | ✓ | ✓ | ✗ alleen jitter (geen efi-rng gezien) | ○ wacht op de stick | ✗ alleen jitter | ✗ geen bron |
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

### Vandaag nog (03-10)

- [ ] M4 (op M1 sinds 03-10 avond, NIC op interrupt, wekker en bevoegdheid
      gezien): nog de scrub (twee keer dezelfde Go-app), de
      koude-flipweigering, en een nieuw image via Recovery met main.
- [ ] De prestatie-agent: `scratchpad/fixes/perf.patch` op measurements.md
      toepassen als hij klaar is.
- [ ] De bump: Hop en HopOS op de volgende versie, release.sh, media.

### Per bord

- [ ] Radxa: de TSADC geeft geen geldige code (Go ook); de klok staat op
      816 MHz (de rk3566 kan 1800); de config zit in `hopos.ird` (alleen
      met `CFG=` te wijzigen); geen serienummer-terugval voor de MAC.
- [ ] Radxa en M4: de scrub van een Device-gemapte pool op ijzer (twee keer
      dezelfde tamago-app plaatsen).
- [ ] Pi 5: het glas (de firmware weigert elke framebuffer sinds de
      herflash). De koude flip weigert bewust (geen CPU_OFF terug).
- [ ] Pi 4: HID en de display-app op de VL805 nu de xHCI werkt.
- [ ] O6N: na `DELETE` van Lumen weigert de kern elke plaatsing tot een
      koude boot (reproduceren op QEMU met een job met devices); de
      5555-listener lekt lezersplaatsen na veel abrupte clients; Lumen
      terug na elke koude boot (`hopos.init[]`) en dan de mediaketen.
- [ ] M4: de 4 KiB-schrijfjes naast 1 MiB-calls blijven rond 19 tot 20
      (transport in kleinere brokken met een yield); de OS-core als grens
      voor veel kleine calls (eerst meten met een echte database).
- [ ] Altra: de SBSA-watchdog echt laten resetten; efi-rng voor de kern;
      de koude flip.
- [ ] Overal: de echte watchdogtoets (kabel eruit of Hop stoppen, reset
      binnen de termijn), en core-reclaim in een sharegroup met een
      rekenaar (`HOPOS_CORE_RECLAIM` na 2 s) op ijzer.

### Meten

- [ ] Storm door de NAT (hairpin) stokt 1 s per ronde op de Pi 4 (p99 1 s):
      verdacht de listen-backlog van 8 in leannet.
- [ ] Radxa over de draad ~21 MB/s (Go 56 in, 99 uit); Pi 5 uit ~43 MB/s.
- [ ] De ring doet het cache-onderhoud op head en tail altijd, ook bij
      Coherence::Hardware: eerst meten (bench pull M4 en O6N), dan 20 tot
      30 regels.
- [ ] O6N schrijven ~700 MB/s in de middag tegen 800 tot 1188; app-opslag
      O6N 414 schrijven en 85 lezen (Go beter).
- [ ] Na een flip zaait de kern uit jitter, niet uit de TRNG van het board.
- [ ] vitals: de standaard-rx-URL faalt zonder DNS in de env;
      `hopos.codecdemo` op de O6N meten.

### Hop

- [ ] Een rolling update met een vaste poort op één node slaagt nooit (de
      nieuwe wil :80 terwijl de oude hem houdt): bij een poortbotsing de
      oude eerst stoppen, of tellen als "wacht".
- [ ] Jobs over een koude boot zonder S3: in `hopos.init[]` (M4 spin en
      spin-tunnel, O6N lumen).
- [ ] De leader-API staat stil tijdens een download; chunked transfer
      weigert; een plaatsing zonder capaciteit blijft proberen (ruis).
- [ ] `sha256` in de jobspec (een vervangen artefact in `apps` komt door
      GitHub's cache pas na twee minuten; met de sha is een oud image een
      luide fout).
- [ ] Na een flip één `NEXT_STORE failed: system call timed out`.
- [ ] Hop op de host: SIGTERM; de S3-lock met twee HopOS-nodes op ijzer.
- [ ] De plugins als node (hop-gui, hoplb naar welcome, hopdns
      `welcome.hop.local`, hopprom, hoplockserver, cloudflared-lean met een
      echt token) en Replica (`sqlite-persist` op een node met schijf).

### Klein

- [ ] De console op 5555: Go's vraagvenster (`stats`, `disc`) en "vol"
      melden aan de client.
- [ ] De boot-stack van 256 KB heeft 43 KB marge: gui::usb::run en het
      FsActor-blok naar de heap, een wachtpost in qemu-test.sh.
- [ ] Guard-pagina op UEFI en RISC-V; de device-op (19) zonder hosttests;
      de schijf-interruptlijn buiten QEMU virt.
- [ ] Bewust gelaten tot er aanleiding is: de FP-lek tussen bewoners op een
      app-core, de M4-wekker (1000 wekken per seconde), de laatste woorden
      van een gestopte app, de Pi cmdline zonder lengtetoets, de
      Pi-mailbox die tot 500 ms spint, de O6N-console op de SPCR-UART, de
      O6N `_CPC`-klassenbron uit.
- [ ] applib `leave_group`; lean: de IPv6-baan en `Stack::leave_group`;
      docs/boards-radxa.md loopt achter.


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
