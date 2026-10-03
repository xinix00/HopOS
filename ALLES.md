# ALLES

De ene lijst van wat er nog moet, sinds de eerste boot op ijzer (30-09-2026).
Sinds 03-10 in de volgorde van aanpak: ronde voor ronde, en binnen een ronde
per node. Wat af is gaat eruit, niet doorgestreept. Afspraak: één lijst,
hier; `docs/README.md` wijst hierheen. De leesreviews staan in
docs/review-2026-10-01.md (de opruiming) en docs/review-port-2026-10-02.md
(Go naast Rust, de acht gaten; zeven ervan zijn op 03-10 gedicht, op ijzer
nog te bewijzen in ronde 1).

## De tabel

De afvinkmatrix van de Go-tijd (OLD/docs/support.md: boot, idle en klokken,
devices en diensten per board), nu voor v3 en bijgehouden op ijzer. Legenda:
✓ gezien op het board, ○ gebouwd maar op dit board nog niet gezien, ✗ ontbreekt
of faalt, en een streep waar het bewust niet komt. Stand 03-10-2026, middag (de LicheeRV op R14, de O6N op O2, de Pi 4 op P1, de Radxa op X1, de Pi 5 op P1, de Altra-stick A1; alles van main 8a91d57 met Hop d785ef5).
Een cel zegt alleen of het slaagt, met hooguit de stempel of een paar
woorden waarom niet; een gepolde NIC is geen ✓. De getallen staan in
docs/measurements.md, de details per board in docs/boards-*.md.

| | QEMU virt | Pi 5 | Pi 4 | Radxa | Altra | O6N | M4 | LicheeRV |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Boot, EL2, kooi, zelftest | ✓ | ✓ P1, 03-10 | ✓ P1, 03-10; koud ✓ de VL805-firmware laadt (P1g) | ✓ X1, 03-10 | ○ nog nooit geboot | ✓ O2, 03-10 | ✓ M7 | ✓ R13, 03-10 |
| Hop als bewoner, welcome door de DNAT | ✓ | ✓ P1, in `system` | ✓ P1, in `system` | ✓ X1, in `system` | ○ nog nooit geboot | ✓ O2, in `system` | ✓ | ✓ R10, 03-10 |
| Kern-flip, warm | ✓ | ✗ net dood na de landing (de zwarte doos van P1 zegt het, agent bezig); koud ✓ | ✓ I | ✓ I | ○ nog nooit geboot | ✓ H; I ○ koude boot, fix de61b4b | ✓ 01-10; koud – geen PSCI | – niet op riscv64; koud ✓ R10, 03-10 |
| NIC met interrupt | ✓ | ✓ | – gepold, zoals Go | ✓ | – gepold, bewust | ✓ | ✗ gepold; INTx via de AIC nog niet bedraad | ✓ R13, 03-10 |
| Hardware-IRQ (NIC, kick, timer) | ✓ | ✓ | ✓, NIC gepold | ✓ | ○ nog nooit geboot | ✓ | ✓, NIC gepold | ✓ R13, 03-10 |
| Gebruik per taak, van de kern en van Hop (cpu, geheugen; slot 0, systeemtaken) | ✓ arm64, 03-10 | ✓ P1, 03-10 | ✓ P1, 03-10 | ✓ X1, 03-10 | ○ nog nooit geboot | ✓ O2, 03-10 | ○ niet nagelopen | ✓ R13, 03-10 |
| Off-link door de NAT, SNTP | ✓ | ✓ | ✓ | ○ niet nagelopen | ○ nog nooit geboot | ○ niet nagelopen | ○ niet nagelopen | ○ niet nagelopen |
| Watchdog gewapend en geaaid | – geen watchdog in virt | ✓ P1, canary | ✓ P1, canary | ✓ X1, canary | ○ nog nooit geboot | ✓ O2, canary | ✓ | ✓ R3, 03-10 |
| Hardware-RNG voor de kern | ✗ alleen jitter | ✓ | ✓ | ✓ | ○ nog nooit geboot | ○ wacht op de stick | ✗ alleen jitter | ✗ geen bron |
| Hardware-RNG voor de slots (CTRL_RNG_SEED, fc5348f) | ○ alleen jitter | ✓ P1, 03-10 | ✓ F | ✓ F | ○ nog nooit geboot | ○ wacht op de stick | ○ alleen jitter | ○ niet nagelopen |
| Temperatuur in de tik | – geen sensor in virt | ✓ | ✓ | ✗ sensor converteert niet | ○ nog nooit geboot | ✓ | ✗ niet gebouwd | ✓ R5, 03-10 |
| Klokbeleid (dvfs) | – geen klok in virt | ✓ | ✓ | – klok van de firmware, bewust | ○ nog nooit geboot | ✓ | ✓ M7 | – vaste klok |
| Console op het glas | ✓ | ✗ firmware weigert sinds de herflash | ✓ | ✓ | ○ nog nooit geboot | ✓ | – bewust uit | – geen scherm |
| USB xHCI (HID, display-app) | ✓ | ○ niets ingeplugd | ✓ P2g, 03-10 (enable slot met completion, d107b31) | ○ niets ingeplugd | ○ nog nooit geboot | ✓ | – niet gepland | – niet gepland |
| Opslag (hopfs, volumes, OP_SYNC) | ✓ | – bewust geen schijf | – geen schijf | – stateless, bewust | ○ nog nooit geboot | ✓ | ✓ M24 | – geen SD-driver |
| Console op 5555 | ✓ | ✓ | ✓ | ✓ | ○ nog nooit geboot | ✓ | ✓ | ○ niet nagelopen |
| Hardwaredecoder (media-smaak) | – geen media-smaak | – geen media-smaak | – geen media-smaak | – geen media-smaak | – geen media-smaak | ✓ 30-09, weg tot de koude boot | – geen media-smaak | – geen media-smaak |
| Kaart of stick klaar in `target/` | – niets te flashen | ✓ P1 geschreven | ✓ P1 | ✓ X1 | ✓ A1 op de stick | ✓ O2 op de stick | ✓ M24 | ✓ R13 |

## Ronde 0: de ochtend van 03-10, vóór alles

Wat de build en de media blokkeert. Volgorde telt.

- [ ] **De build hoort bij Hop v3.0.5** (hop main d30a9f3, release v3.0.5 op
      GitHub): de kern stuurt SLOT_STATUS zonder maat het voorvoegsel van 88
      bytes (Hop tot v3.0.3 blijft werken, ook de Hop van de kaart over een
      flip), Hop 3.0.5 vraagt de volle 128; een Hop 3.0.4 op deze kern kan
      geen stand lezen. Media met Hop 3.0.0 tot 3.0.3 hoeven dus niet eerst,
      media met 3.0.4 wel. `tools/release.sh` neemt hop main vanzelf mee;
      de workspace-versie wordt de releaseversie (de guard weigert anders).
- [ ] **De M4** staat op R1 (gen 2) met Hop 3.0.0 zonder store-bevoegdheid
      (HOPOS_PRIVILEGE ontbrak na de flip: fix 72370b6) en met de
      SlotInfo-lus voor nieuwe plaatsingen (fix 425f192); spin en spin-tunnel
      draaien, maar spin kan zijn staat niet wegschrijven. Óf de bundel
      `scratchpad/art/hopos-apple-R2.flip` (warm, beide fixes), óf de knop.
      Daarna een nieuw image via Recovery met main en een cfg zonder
      `hopos.pstate=off` (nu nog D4b; `EMBED=` Hop 3.0.5): dan klopt ook de
      koude boot weer.
- [ ] **De Altra**: de stick van de release (hopos-altra-headless.img.gz)
      met de fixes van 03-10 (kaart 256 KB, HOPOS_UEFI_MAP_DROPPED, venster
      0xB000_0000 met de zes kandidaten als het bezet is, igb na CTRL.RST);
      v3 heeft daar nog nooit geboot.
- [ ] **De Pi 5**: eerst de nieuwe kaart (met de zwarte doos), dan pas
      flippen; de warme flip sterft daar na de landing (F, H en OP1, drie
      keer) en de koude flip weigert zodra er een app-core draaide. De
      kaart-kern is te oud voor de doos.

## Ronde 1: het ijzerbewijs van de fixes van 02 en 03-10

Eén boot of flip per board van main, en per board de regels die het bewijs
leveren. Rood is een regressie van de port-review, niet van het bord.

- [ ] Overal: `HOPOS_WD_CANARY_OK` na de boot, en de echte watchdogtoets
      (kabel eruit of Hop stoppen, reset binnen de termijn: Pi 12 s, O6N
      8,5 s, M4 30 s, Radxa 89 s). Zonder link bij de boot `HOPOS_NIC_RETRY`
      en daarna toch `HOPOS_NET_UP` (kabel er pas na de boot in). Een app
      die blijft rekenen (bench BURN) naast welcome: welcome blijft onder
      de 20 ms antwoorden (de RX-arm van applib, op QEMU 258 naar 3 ms).
- [ ] M4: `HOPOS_WAKER_UP` na de boot; vitals cpu op een app-core van ~100
      naar ~0 procent idle-last (Go: 47 wekken/s); met `hopos.tick=1` de
      tellers `waker(rounds= seen= kicks= rx=)` op de tik. Een koude flip
      weigert nu netjes zonder PSCI (`HOPOS_FLIP_COLD_NO_PSCI`); een
      mislukte landing reset via de WDT (`HOPOS_FLIP_RESET_WDT`). Bench pull
      app naar app opnieuw (NIC-buffers write-back): E 4450, P 6100 was M33.
- [ ] M4 en Radxa (de pool is daar Device gemapt): een tamago-app na een
      vorige huurder in dezelfde partitie start schoon (de scrub met
      clean+invalidate); de proef is twee keer dezelfde Go-app plaatsen.
- [ ] O6N en Altra: na een KOUDE boot de NIC-buffers write-back
      (`net-wb`); bench pull en vitals rx opnieuw (O6N v3 76 tot 78 tegen
      v2 111 tot 116 MB/s inkomend was het vermoeden).
- [ ] LicheeRV: de koude flip met de kern op de C906B op ijzer (R13), en
      voor de kolom in docs/measurements.md de netmeter-doorvoer en bench
      (een peer op de draad). Nog zonder bel van app naar kern (de
      CV181x-mailbox is de kandidaat), zonder SMP en zonder RNG. De loterij
      van 03-10 ochtend is dezelfde dag weer verwijderd: de PLIC van de
      C906L heeft geen ethernetbron, de kern hoort op de C906B.
- [ ] Een vervangen artefact in `apps` (`gh release upload --clobber`) komt
      pas na ongeveer twee minuten op een node aan: Hop bewaart niets (elke
      plaatsing haalt opnieuw, gemeten 03-10), maar GitHub cachet de 302 van
      releases/download zo lang, ook met no-cache. Dus bij een vervanging
      twee minuten wachten of een unieke naam per build; later `sha256` in
      de jobspec zodat een oud image een luide fout is.
- [ ] **De temperatuur per bord, werkt de sensor**: de rij "Temperatuur in
      de tik" per node aflopen. De LicheeRV is af (TEMPSEN, R5, 03-10). De
      Radxa (TSADC converteert niet),
      de Altra (SMpro), de M4 (○) en de O6N (SCMI, 39 C) nalopen; welk bord
      meldt wat in `temp=` van de tik en bij Hop (`temp_milli_c` in de
      heartbeat).
- [ ] Een sharegroup met een rekenaar: `HOPOS_CORE_RECLAIM` na 2 s en het
      nieuwe lid op (tools/qemu-test-reclaim.sh is het voorbeeld).
- [ ] De opruiming van 1 en 2 oktober, wat nog niet op ijzer gezien is:
      M4 start_one en de vectoringang 8, de flip-boot-watchdog (één keer
      wapenen, alleen HOPOS_BOOT_GUARD), core-class big op een P-core, de
      zwarte doos, aic op afgeleide tabeladressen, ANS via SART, rtkit en
      smc; Altra NVMe en igb via pcie::first_in. De Pi 4, de Radxa en de
      O6N zijn op 02-10 ochtend gedaan (OP1), de LicheeRV op 03-10 (R3).

## Ronde 2: de bordfouten die we kennen

Per node, de dingen die een node of een meting nu nog stuk maken.

### Orion O6N (o6n-1, 192.168.1.205)

- [ ] **Na `DELETE` van Lumen (10 cores, 16 GiB, codec- en USB-devices)
      weigert de kern elke plaatsing** ("unplaceable" binnen seconden, Hop
      ziet 10 vrije cores), ook de flipbundel; alleen een koude boot hielp.
      De kooi, partitie of devices komen niet vrij. Reproduceren op QEMU met
      een job met devices.
- [ ] **De console-listener op 5555 neemt na een meetronde geen
      verbindingen meer aan** (`nc -z` faalt; veel `nc | head` van de
      agents): lezersplaatsen lekken bij abrupt sluitende clients. Zelfde
      beeld op de Radxa na zijn koude herstart van 02-10.
- [ ] **vitals in slot 3 stopt direct na `HOPOS_APP_MMU`** (4 cores vanaf
      core 2, `part 0xbfe00000+0x10000000`, over de grens van 3 GiB; tien
      keer, geen app-regel, geen fault-regel); als eerste geplaatst (slot 2)
      draait hij, bench in slot 3 (`part 0x7f5600000`) ook.
- [ ] Lumen terugzetten na elke koude boot (spec in
      scratchpad/o6n-job-lumen.json, of in `hopos.init[]`; welcome erbij
      kost Lumen een core) en dan de mediaketen: MMC, de HEVC-encoder,
      WebDAV. De VPU had één herstelcyclus nodig ("incomplete power state");
      waarom.

### Raspberry Pi 5 (pi5-1, 192.168.1.207)

- [ ] De warme flip sterft na de landing: met de nieuwe kaart flippen en
      de doos lezen (`HOPOS_FLIP_BLACKBOX`); de oude kern zei "jump landed
      and the handover was consumed, died later in the new kernel's boot".
- [ ] De koude flip weigert zodra er een app-core draaide (CPU_OFF komt op
      de Pi 5 niet terug).
- [ ] Het glas: sinds de herflash weigert de firmware elke framebuffer
      (0x80000001), ook koud met 5 s geduld. Hangt het scherm eraan en
      stond het aan bij de power-on?

### Radxa Zero 3E (radxa-1, 192.168.1.241)

- [ ] De TSADC geeft geen geldige code (`HOPOS_TSADC_NONE`), zoals in Go op
      06-08; de init is die van Linux. Waarom converteert hij niet.
- [ ] De config zit in `hopos.ird`: alleen te wijzigen met `CFG=` of in de
      APPEND-regel; de koude flip weigert (stateless: warm flippen of de
      kaart herstarten).
- [ ] **De klok staat op 816 MHz** (de firmware; de rk3566 kan 1800, en per
      MHz doet hij hetzelfde als de Pi 4): hoger vraagt vdd_cpu tot 1,15 V
      via de RK8600 op i2c0 plus de SCMI-klok via TF-A. Spanning op het
      board, dus bewust niet blind gedaan; verwacht tot 2x app naar app.
- [ ] Klein uit de port-review: geen serienummer-terugval voor de MAC (elke
      Radxa zonder `hopos.node` krijgt dezelfde), xHCI zonder barrières
      binnen een TRB op Normal-NC, de VOP2-klokboom uit de keten (de reden
      klopt niet met de Go-probe), de UTMI-breedte onzeker nu usbdrd30
      zelf geklokt wordt.

### Mac mini M4 (m4-1, 192.168.1.122)

Image: `image/apple-m4.sh` met `CFG=` (cfg ingebakken op 0xF000) en
`EMBED=` (Hop ingebakken); install vanuit Recovery met `install.sh go` op
de stick. Console: USB-C-debugkabel, de kis-poort komt alleen na een verse
plug aan de Mac-kant gevolgd door `sudo macvdmtool reboot debugusb` (1 s).
Lezer `scratchpad/m4-watch.sh` op `/dev/cu.kis-100000-ch-0`. Sinds 01-10
ook op het LAN: 5555, 8080, 9080, 10100 (5555 draagt de vroege bootregels
niet).

- [ ] **De 4 KiB-schrijfjes van de ene app naast 1 MiB-calls van een andere
      app blijven rond 19 tot 20** (m4-mix onder een pure bulklezer; van 82
      alleen; M24). Niet de schijf. De coöperatieve OS-core: het transport
      van de bulk-app werkt in brokken van 1 MiB (`tcp_write`, 150 tot 180
      us per poll) en de calls van de andere wachten daar telkens achter.
      Fix: het transport eerlijk maken (kleinere brokken met een yield in
      de verbindingstaak en leannet); netklus, 50 tot 150 regels. Voor
      SQLite naast een bulk-lezer is dit het punt.
- [ ] **De OS-core is de grens voor veel kleine calls** (M32): vier apps
      25.000 en acht apps 36.000 willekeurige 4 KiB-blokken per seconde,
      de schijf doet er 175.000; bij acht apps busy_ms 999, ~27 µs per
      call. Verder vraagt een goedkoper callpad of I/O buiten de OS-core.
      Eerst meten met een echte database van 100 GB of meer.

### Raspberry Pi 4 (pi4-1, 192.168.1.40)

- [ ] HID en de display-app op de VL805 (na de koude boot van ronde 1).

### Ampere Altra (altra-1)

- [ ] Eerste boot (ronde 0), dan: SMpro-temperatuur, de SBSA-watchdog,
      NVMe en igb, 5555, de flip. Geen guard-pagina onder de stack; de NVMe
      pollt.

## Ronde 3: meten en de prestatiepunten

De vier rapporten staan in docs/measurements.md (kolommen v3). Stand 01-10:
app naar app M4 4450 (E) en 6100 (P), O6N 1500 tot 1600, Pi 4 407 tot 435,
Radxa 257 tot 262; NVMe door de app op de M4 1270 schrijven en 1690 lezen.
Alles boven Go behalve schrijven door de app op de M4 en willekeurig lezen.

- [ ] De kolommen vullen: Pi 5 en Radxa vitals `test=all`; LicheeRV
      (ronde 1); de netmeter-doorvoer per board (`netmeter NODE:80 --repeat
      3`, bench in een slot) in de tabellen Netwerk en Latentie. De Mac
      hangt op Wi-Fi en macOS laat netmeter niet op het LAN: host-getallen
      zijn Wi-Fi, dus node naar node over de draad.
- [ ] **Storm door de NAT (hairpin) stokt 1 s per ronde** op de Pi 4: p50
      2,5 ms, p99 1002 ms, 96 conn/s. Verdachte: leannet's listen-backlog
      van 8 (`TCP_BACKLOG`); een backlog van 64 in lean (tag) is de lean
      fix. De NAT-fix a102e51 (RST = gesloten) staat sinds R1 op de Pi 4,
      de Radxa en de O6N: de storm herhalen.
- [ ] **Radxa over de draad ~21 MB/s in beide richtingen** (Go 56 in, 99
      uit): 2,6x en 4,7x onder de lat. De 816 MHz (ronde 2) is de eerste
      verdachte, de ring de tweede (zie hieronder).
- [ ] **Pi 5 uit ~43 MB/s** (Go 42 tot 51): op de onderkant; in, en de Pi 4
      beide kanten, boven de lat.
- [ ] **De ring doet het cache-onderhoud op head en tail altijd**, ook bij
      Coherence::Hardware; Go sloeg het op coherente ringen over en cleande
      de gepeekte RX-kop één keer per burst (T13 03-09: de helft; bulk Hop
      naar app tot 4x, 04-09). Eerst meten (bench pull M4 en O6N met en
      zonder), dan 20 tot 30 regels.
- [ ] **O6N schrijven: ~700 MB/s in de middag tegen 800 tot 1188 in de
      ochtend** met dezelfde kerncode: koude boot op main en opnieuw meten;
      de NVMe-staat en de plaatsing van de core tellen mee.
- [ ] **App-opslag O6N** (1 MiB-calls): schrijven 414, lezen 85 MB/s (Go
      557 tot 625 / 727 tot 797); het datablok van de NVMe Normal-NC en
      `poll_op` kopieert met vluchtige loads. Fix: `BLK_DATA` Normal-WB
      (de driver doet al push en pull). Bewijs na de koude boot:
      `hopos.nvmebench=1` en vitals `test=disk`.
- [ ] **Na een flip zaait de kern uit jitter, niet uit de TRNG van de
      firmware** (O6N: efi-rng is een boot service). Klein: 64 bytes zaad
      van de koude boot in de feitenpagina, of de DRBG-stand in de handoff.
- [ ] vitals: de standaard-rx-URL (cachefly) faalt zonder DNS in de env;
      rx-duren vallen op stappen van 100 ms.
- [ ] `hopos.codecdemo` op de O6N, de kernmeting, naast de 85,7 fps van
      apps/decode; de mediaketen na de koude boot.

## Ronde 4: Hop

- [ ] **Een rolling update met een vaste poort op één node slaagt nooit**:
      de nieuwe taak wil :80 terwijl de oude hem houdt, en Hop probeert
      elke paar seconden opnieuw. Fix in Hop: bij een poortbotsing op
      dezelfde node de oude eerst stoppen (recreate), of de botsing als
      "wacht" tellen. Omweg: DELETE en opnieuw POSTen.
- [ ] Jobs over een koude boot: Hop herstelt alleen uit S3 (lege S3 = de
      init-jobs); zonder S3 horen de vaste jobs in `hopos.init[]` van de
      config (M4: spin en spin-tunnel; O6N: lumen).
- [ ] De leader-API staat stil tijdens een download (één aanroep tegelijk,
      de download zit erin): de download in een eigen taak.
- [ ] Hop's downloader weigert chunked transfer ("serve it with a
      Content-Length"): of chunked lezen, of luid in de docs van de jobspec.
- [ ] Na een flip meldt Hop één keer `NEXT_STORE failed: system call timed
      out`: de lange wacht van de store-taak liep over de flip heen.
- [ ] Hop blijft een plaatsing zonder capaciteit opnieuw proberen (een vloed
      `HOP_JOB_FAILED`; onschuldig, maar ruis).
- [ ] Hop op de host: SIGTERM (std heeft geen signaal-API: beslissing); op
      HopOS: de S3-lock alleen met hosttests, twee HopOS-nodes naast elkaar
      alleen op ijzer.
- [ ] De plugins als node: hop-gui als job op de Pi 5 en het dashboard in de
      browser; hoplb naar welcome op `Host:` (WebSocket: een 101 wordt 502,
      geen verbindingspool); hopdns die `welcome.hop.local` beantwoordt
      (DNS over TCP, split horizon, CNAME's); hopprom op `/metrics`;
      hoplockserver op een volume als lock van twee nodes; cloudflared-lean
      als job met een echt token.
- [ ] Replica: `sqlite-persist` op een node met schijf; de database-eigenaar
      en de lease-heartbeat van de hostapp; een S3-weg vanuit het slot of
      een Store-adapter op de store-ops. Op GitHub (`xinix00/replica`)
      nog geen remote.

## Ronde 5: klein, en de rest

- [ ] De console op 5555: Go's vraagvenster (`printf 'stats\n' | nc node
      5555`, `disc`) is niet geport, alleen de stroom; de listener meldt
      "vol" niet aan de client (één `write` vóór `close`).
- [ ] **De boot-stack van 256 KB heeft 43 KB marge**: de future van
      gui::usb::run (123 KB) en het FsActor-blok (85 KB) op de heap zoals de
      Lifecycle, en een wachtpost in qemu-test.sh (rood boven 224 KB).
- [ ] Guard-pagina op UEFI en RISC-V; de verhuisde kern op een heap-stack
      van 64 KB zonder guard.
- [ ] De device-op (19): async BOT, MMC en `deviceabi` zonder eigen
      hosttests en zonder QEMU-proef.
- [ ] De schijf-interruptlijn buiten QEMU virt (UEFI, rk3566, RISC-V, NVMe).
- [ ] Uit de port-review, bewust gelaten tot er aanleiding is: de FP van
      de ene bewoner lekt naar de volgende op een app-core (Go ook); de
      wekker op de M4 kost de kern-core 1000 wekken per seconde (Go ook);
      de laatste woorden van een gestopte app gaan verloren (post-mortem);
      de Pi cmdline.txt zonder lengtetoets; de mailbox van de Pi spint tot
      500 ms op core 0 bij klokwerk; de O6N-console op de SPCR-UART waar Go
      de header-UART nam (werkt nu, kosten niet gemeten); de O6N
      `_CPC`-klassenbron staat uit (`highest: 0`), de MPIDR-tabel beslist.
- [ ] applib: `leave_group` vraagt lean; de timebase voor RISC-V komt van de
      control-page. lean: de IPv6-baan van leannet, `Stack::leave_group`.
- [ ] docs/boards-radxa.md loopt achter (zegt "niets in v3 op ijzer"); het
      memattr-commentaar over het Normal-register ook.

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
