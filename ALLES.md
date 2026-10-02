# ALLES

De ene lijst van wat er nog moet, sinds de eerste boot op ijzer (30-09-2026).
Per node wat er open staat, daaronder wat overal geldt. Wat af is gaat eruit,
niet doorgestreept. Afspraak: één lijst, hier; `docs/README.md` wijst hierheen.

## De tabel

De afvinkmatrix van de Go-tijd (OLD/docs/support.md: boot, idle en klokken,
devices en diensten per board), nu voor v3 en bijgehouden op ijzer. Legenda:
✓ gezien op het board, ○ gebouwd maar op dit board nog niet gezien, ✗ ontbreekt
of faalt, en een streep waar het bewust niet komt. Stand 30-09-2026, avond.

| | QEMU virt | Pi 5 | Pi 4 | Radxa | Altra | O6N | M4 | LicheeRV |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Boot, EL2, kooi, zelftest | ✓ | ✓ | ✓ (koud: SError bij de VL805; fix d0b6bd3 wacht op een koude boot) | ✓ | ○ | ✓ VHE, kick via SGI 1 (`kick=(Ipi, 0 us)`, stempel G) | ✓ iBoot-boot (cfg in het image op 0xF000), EL2; kooi en zelftest ok, tune aan zonder SError (M7 en later) | ○ |
| Hop als bewoner, welcome door de DNAT | ✓ | ✓ | ✓ | ✓ | ○ | ✓ | ✓ ingebakken, HOP_UP, gaat warm mee over flips (HOPOS_HOP_RESUMED) | ○ |
| Kern-flip, warm | ✓ | ✗ sterft na de landing (F en H, 3x); de nieuwe kaart (H, met de zwarte doos) ligt in target/ | ✓ (6x, gen 4 op I) | ✓ (5x, gen 3 op I) | ○ | ✓ gen 2 op H (gui-bundel); I geweigerd door de H-kern (bundelpartitie 8 KiB te krap, fix de61b4b zit in de I-stick): koude boot | ✓ gen 1 tot 8 op 01-10 (D4b naar M15), adoptie 3 van 3, de config reist mee; koud: – (geen CPU_OFF) | – |
| NIC met interrupt | ✓ | ✓ MSI-X via de MIP | – (GENET gepold, zoals Go) | ✓ SPI 64 | – (igb gepold, bewust) | ✓ RTL8125B, MSI-X via de ITS (LPI 8192) | ✓ tg3 BCM57766, gepold; MAC-filter na link_up (4be8b60); app naar app E 4450, P 6100 MB/s (M21), transport kern naar app 1860 | ○ (dwmac) |
| Off-link door de NAT, SNTP | ✓ | ✓ | ✓ | ○ | ○ | ○ | ○ | ○ |
| Watchdog gewapend en geaaid | – | ✓ PM (12 s) | ✓ PM | ✓ DW-WDT (89 s) | ○ SBSA | ✓ SBSA (8,5 s) | ✓ Apple WDT 30 s, canary op Hop's hartslag (HOPOS_CANARY_LIVE) | ○ DW-WDT |
| Hardware-RNG voor de kern | ✗ (jitter) | ✓ RNG200 | ✓ RNG200 | ✓ rk3568-rng | ○ SMCCC-TRNG of rndr (fc5348f) | ○ efi-rng: geen FEAT_RNG of SMCCC-TRNG, wel het EFI_RNG_PROTOCOL van de firmware (`hopos.efirng=1`, volgende stick) | ○ jitter (geen FEAT_RNG, geen SMCCC) | ✗ (niets) |
| Hardware-RNG voor de slots (CTRL_RNG_SEED, fc5348f) | ○ jitter | ○ (kaart-kern van vóór fc5348f) | ✓ rng200 (F) | ✓ rk3568-rng (F) | ○ | ○ jitter (G); efi-rng met de volgende stick | ○ jitter | ○ |
| Temperatuur in de tik | – | ✓ mailbox | ✓ mailbox | ✗ TSADC converteert niet (Go ook niet) | ○ SMpro | ✓ SCMI (39 C in de agentlijst) | ○ | – |
| Klokbeleid (dvfs) | – | ✓ 1500/800 | ✓ 1500/600 | – | ○ | ✓ vijf `_CPC`-domeinen, 2600 MHz | ✓ p-state-tune E 5/8 = 2172 MHz, P 6/20 = 2352 MHz (M7) | – |
| Console op het glas | ✓ ramfb | ✓ via flip op de eerste kaart, ✗ sinds de herflash (firmware weigert) | ✓ 32 bpp | ✓ HDMI (geen EDID) | ○ GOP | ✓ GOP 1920x1080 | – | – |
| USB xHCI (HID, display-app) | ✓ qemu-xhci | ○ 2 xHCI's up, niets ingeplugd | ○ VL805 koud: fix d0b6bd3 (SCB0_SIZE, notify, twee pogingen), koude boot nodig | ○ 2 DWC3 up, niets ingeplugd | ○ | ✓ 10 xHCI's up, de Blu-ray-drive over USB-BOT leest de disc (Lumen) | – | – |
| Opslag (hopfs, volumes, OP_SYNC) | ✓ virtio-blk | – (bewust geen NVMe) | – | – (stateless, alles in het geheugen) | ○ NVMe | ✓ NVMe Lexar 4 TB, hopfs hersteld (generatie 3456) | ✓ ANS NVMe 414 GB, hopfs hersteld; de ANS asynchroon met read-ahead (M22): rauw 4952 / 1925 (M23), door de app 1270 / 1690 (M24) | – |
| Console op 5555 | ✓ | ✓ | ✓ | ✓ | ○ | ✓ | ✓ (hopos.replay=45) | ○ |
| Hardwaredecoder (media-smaak) | – | – | – | – | – | ✓ Linlon V8, 85,7 fps 4K P010 via de grant; nu tijdelijk weg (gui-flip H) tot de koude boot | – | – |
| Kaart of stick klaar in `target/` | – | ✓ 22:37 (I) | ✓ 22:37 (I) | ✓ 22:38 (I, gepatchte Hop) | ✓ 18:12 | ✓ 22:38 stempel I (kern-fix bundelpartitie, verse Hop, efirng) | ✓ D4b geïnstalleerd (pstate=off, zonder de fixes van 01-10); art/hopos-apple.flip = M24 (cfg/m4-meet.cfg, replay=0); nieuw image met main gewenst | ✗ donor-FIP |

## De nodes, één voor één

Alleen wat nog moet; wat af is staat in de tabel hierboven.

### Raspberry Pi 5 (pi5-1, 192.168.1.207)

- [ ] De warme flip sterft na de landing (stempel F, twee keer; 02-10 ook
      op OP1 vanaf main: de node pingt niet meer na de POST, geen landing op
      5555, de knop is nodig): koud terug
      van de kaart met "jump landed and the handover was consumed, died
      later in the new kernel's boot". De kaart-kern is te oud voor de
      zwarte doos; eerst de nieuwe kaart flashen, dan flippen en de doos
      lezen (`HOPOS_FLIP_BLACKBOX`).
- [ ] Het glas: sinds de herflash weigert de firmware elke framebuffer
      (0x80000001), ook koud met 5 s geduld. Hangt het scherm eraan en
      stond het aan bij de power-on?
- [ ] De echte watchdogtoets (kabel eruit of Hop stoppen, reset binnen
      12 s), HID en de display-app.
- [ ] De koude flip weigert zodra er een app-core draaide (CPU_OFF komt op
      de Pi 5 niet terug).

### Raspberry Pi 4 (pi4-1, 192.168.1.40)

- [ ] De VL805 koud: fix d0b6bd3 (RC met SCB0_SIZE, endpoint dicht tot de
      VideoCore de firmware meldt, twee pogingen, SError-diagnose) is
      alleen op QEMU getoetst; een koude boot van de nieuwe kaart moet
      `usb: vl805 firmware 0x... loaded` geven.
- [ ] De echte watchdogtoets, HID en de display-app.

### Radxa Zero 3E (radxa-1, 192.168.1.241)

- [ ] De TSADC geeft geen geldige code (`HOPOS_TSADC_NONE`), zoals in Go op
      06-08; de init is die van Linux. Waarom converteert hij niet.
- [ ] De echte watchdogtoets (DW-WDT 89 s).
- [ ] De config zit in `hopos.ird`: alleen te wijzigen met `CFG=` of in de
      APPEND-regel. De koude flip weigert (stateless: warm flippen of de
      kaart herstarten). De kaart in `target/` is van 14:41; de node draait
      warm op F.
- [ ] **De klok staat op 816 MHz** (de firmware; de rk3566 kan 1800, en per
      MHz doet hij hetzelfde als de Pi 4): hoger vraagt vdd_cpu tot 1,15 V
      via de RK8600 op i2c0 plus de SCMI-klok via TF-A. Spanning op het
      board, dus bewust niet blind gedaan; verwacht tot 2x app naar app.
      De host-ringen zijn hier nog Maintained (geen schijf om het te meten).

### Ampere Altra (altra-1)

Stick: `target/uefi-esp-altra/` (18:12), naar een FAT32-stick met
`hopos.cfg` naast `EFI/`.

- [ ] Eerste boot: de EFI-stub, ACPI, `HOPOS_WD_ARMED` (SBSA), igb gepold
      (bewust), NVMe, `HOP_UP`, welcome, 5555. Geen guard-pagina onder de
      stack; de NVMe pollt.

### Orion O6N (o6n-1, 192.168.1.205)

Stick G (21:29): gui plus media, verse Hop; `hopos.efirng=1` staat klaar
voor de volgende stick (nog niet erop).

- [ ] **Na `DELETE` van Lumen (10 cores, 16 GiB, codec- en USB-devices)
      weigert de kern elke plaatsing** ("unplaceable" binnen seconden,
      Hop ziet 10 vrije cores), ook de flipbundel; alleen een koude boot
      hielp (21:12 tot 21:34). De kooi, partitie of devices komen niet
      vrij. Reproduceren op QEMU met een job met devices.
- [ ] **De console-listener op 5555 neemt na een meetronde geen
      verbindingen meer aan** (`nc -z` faalt; veel `nc | head` van de
      agents). Lezersplaatsen lekken bij abrupt sluitende clients. Zelfde
      beeld op de Radxa na zijn koude herstart van ~21:50 (5555 dicht, Hop
      zonder jobs); open na de flip naar H.
- [ ] **Hop brengt na een koude boot een oude taak terug uit zijn
      hopfs-staat** (vitals, 10240 shares, 512 MiB) zonder leader-job:
      `/v1/status` zegt `jobs:0`, `DELETE /v1/jobs/vitals` raakt hem niet,
      alleen `POST :8080/stop-task/{id}` (21:37, stempel G).
- [ ] **vitals in slot 3 stopt direct na `HOPOS_APP_MMU`** (4 cores vanaf
      core 2, `part 0xbfe00000+0x10000000`, over de grens van 3 GiB; tien
      keer, geen app-regel, geen fault-regel); als eerste geplaatst (slot 2)
      draait hij, bench in slot 3 (`part 0x7f5600000`) ook (21:40, G).
- [ ] Lumen terugzetten na elke koude boot (spec in
      scratchpad/o6n-job-lumen.json; welcome erbij kost Lumen een core) en
      dan de mediaketen: MMC, de HEVC-encoder, WebDAV. De VPU had één
      herstelcyclus nodig ("incomplete power state"); waarom.
- [ ] `hopos.codecdemo`, de kernmeting, naast de 85,7 fps van apps/decode.
- [ ] De display-app en HID op de xHCI's; hop-gui als job.
- [ ] cloudflared-lean (6bbcbfa) als job met een echt token: de
      productieproef.

### Mac mini M4 (m4-1, 192.168.1.122)

Image: `image/apple-m4.sh` met `CFG=` (cfg ingebakken op 0xF000; nu
`hopos.pstate=off`, `hopos.replay=45`, `hopos.cages=on`, insecure) en
`EMBED=` (Hop ingebakken); install vanuit Recovery met `install.sh go` op
de stick (`hopos-m4.img`, een veelvoud van 16 KiB). Console: USB-C-
debugkabel, de kis-poort komt alleen na een verse plug aan de Mac-kant
gevolgd door `sudo macvdmtool reboot debugusb` (1 s); `debugusb` zonder
herstart en `reboot serial` helpen niet. Lezer `scratchpad/m4-watch.sh`
op `/dev/cu.kis-100000-ch-0`. Sinds 01-10 ook op het LAN: 5555, 8080,
9080, 10100 (5555 draagt de vroege bootregels niet).

- [ ] **Hop en zijn jobs over een flip**: een warme flip houdt ze (M13 →
      M14 → M15 getoetst, 3 van 3 bewoners, vitals meet door), al meldt de
      landing nog "0 B agent state": Hop houdt ze in zijn eigen geheugen. Het
      gat zit bij een koude boot: dan vergeet de leader zijn jobs (de agent
      herstart bench en pull uit zijn eigen staat, de leader zegt `jobs: 0`).
      Hop-repo, niet klein. Ook: Hop blijft een plaatsing zonder capaciteit
      opnieuw proberen (een vloed `HOP_JOB_FAILED`; sinds de retire
      onschuldig, maar ruis).
- [ ] **De 4 KiB-schrijfjes van de ene app naast 1 MiB-calls van een andere
      app blijven rond 19 tot 20** (in m4-scale, twee apps die allebei
      schrijven en lezen, nu 81; in m4-mix onder een pure bulklezer nog 19) (van 82 alleen; M24). Niet de schijf: met gaten
      lezen (geen schijfblok) zakt het ook. De coöperatieve OS-core: het
      transport van de bulk-app werkt in brokken van 1 MiB (`tcp_write`,
      150 tot 180 us per poll) en de calls van de andere wachten daar
      telkens achter. Fix: het transport eerlijk maken (kleinere brokken met
      een yield in de verbindingstaak en leannet); netklus, 50 tot 150
      regels, ongetoetst. Voor SQLite naast een bulk-lezer is dit het punt.
- [ ] **De OS-core is nu de grens voor veel kleine calls** (M32, 01-10): met
      de wachtrij naar de schijf (de actor met zestien calls in de lucht,
      de ANS op zestien tags) lezen vier apps samen 25.000 en acht apps
      36.000 willekeurige 4 KiB-blokken per seconde (was 9.300 voor
      allemaal samen); de schijf doet er 175.000 met zestien tegelijk. Bij
      acht apps staat de OS-core op busy_ms 999, ~27 µs per call. Verder
      vraagt een goedkoper callpad of I/O buiten de OS-core. Bewuste keuzes:
      één call per slot tegelijk (geen parallellisme binnen één app), een
      sync achter een lopende commit en remove/truncate op een lege pool
      houden de calls erachter op; de commit loopt nu naast de I/O en geeft
      soms één opdracht van 3 tot 46 ms (slowest_us). Eerst meten met een
      echte database van 100 GB of meer blijft staan.
- [ ] De hop-repo bouwt niet meer tegen main: agentd-hopos struikelt over
      de leanhttp-traits van TcpConn (de queue-agent, 01-10). Voor een nieuwe
      hop-m4.elf moet dat eerst.
- [ ] Het geïnstalleerde image is nog D4b (pstate=off, zonder de core-start
      en de ANS-fixes): na een koude boot staat de M4 op D4b en moet er
      geflipt worden (`art/hopos-apple.flip` = M15). Een nieuw image met
      main en een cfg zonder `hopos.pstate=off` via Recovery is de nette weg.
- [ ] De koude flip werkt er niet (PSCI CPU_OFF zonder EL3); na een
      verhuizing met een rode voorproef spint de oude core.

### LicheeRV Nano (RISC-V)

- [ ] Donor-FIP aanwijzen (docs/boards-riscv.md: `fip.bin` uit sipeed
      release 20260114) en de kaart bouwen met `CFG=` en `APP=`.
- [ ] Eerste boot: `CLINT: mtimecmp writable`, de DW-WDT, dwmac 0x1037, de
      C906L via de reset-ingang, appspike.
- [ ] Geen kick van app naar kern, geen SMP, geen flip op RISC-V; geen RNG.

### De console op 5555

- [ ] Go's vraagvenster (`printf 'stats\n' | nc node 5555`, `disc`) is niet
      geport: alleen de stroom.

### Nog geen node: hoplb, hopdns, hopprom, hop-gui, hoplockserver, replica

Alle plugins staan in Rust op `v3.0.0` en zijn op QEMU bewezen; op een echte
node heeft er nog geen gedraaid.

- [ ] hop-gui als job op de Pi 5 en het dashboard in de browser.
- [ ] hoplb naar welcome op `Host:`; hopdns die `welcome.hop.local` beantwoordt
      (met `DNSPORT` op QEMU, op het LAN gewoon poort 5353).
- [ ] hopprom op `/metrics`; hoplockserver op een volume als lock van een
      cluster van twee nodes.
- [ ] Replica: `sqlite-persist` op een node met schijf; de database-eigenaar
      en de lease-heartbeat van de hostapp; een S3-weg vanuit het slot of een
      Store-adapter op de store-ops.

## Overal

- [ ] **Op ijzer te toetsen na de opruiming van 1 en 2 oktober** (zo'n 5300
      regels minder in acht groepen, alles host en QEMU groen; de nodes
      zijn niet aangeraakt). Gedaan 02-10 ochtend op de Pi 4 (OP1: adoptie
      2 van 2, watchdog één keer, vitals cpu, app naar app 450) en de Radxa
      (OP1: adoptie 2 van 2, het glas 1080p60 zonder edid-regel, app naar
      app 269) en de O6N (OP1: twaalf cores, Hop in de OS-core-rotatie,
      app-cores 11, Lumen en bench overgenomen, de Lexar via board_uefi::pcie
      op 1 MiB); de Pi 5 stierf op de flip (zie daar). Nog te doen: M4 (in
      gebruik door een andere sessie met Spin), Altra, LicheeRV. Eén flip per board van main en kijken naar:
      M4: start_one en de vectoringang 8 (cpu), de flip-boot-watchdog
      (één keer wapenen, alleen HOPOS_BOOT_GUARD), core-class big op een
      P-core, plaatsing en flip met de ene DevMem, de zwarte doos, mpidr uit
      cpu, aic op afgeleide tabeladressen, ANS via SART, rtkit en smc, tg3
      gepold zonder diag-regel. O6N: NVMe via board_uefi::pcie, xhci met de
      nieuwe helpers en 64-bit registers (toetsenbord, Blu-ray), rtl8126,
      de acpi-regel en de SPCR-console, de optical-owner (read_at zonder
      size), Lumen. Pi 4: heap en GIC via cpu, xhci op de VL805, vcmail,
      genet, device_enabled via Fdt::enabled, het glas via conport::tee.
      Pi 5: xhci, RP1 en PLL via brcmpcie, gem. Radxa: het glas zonder
      EDID (verwacht HOPOS_DISPLAY_UP zonder edid-regel, beeld op de
      monitor), dwc3-registertabel, dwmac4, de riscv-achtige
      REGIME-operanden niet (dat is de LicheeRV). Altra: NVMe en igb via
      pcie::first_in. LicheeRV: de riscv-switcher met REGIME_*-operanden,
      dwmac check en de diag-regel.

- [ ] **De boot-stack van 256 KB heeft 43 KB marge** (02-10, na de
      display-regressie): setup draagt alle start-functies inline, en de
      grootste tijdelijke waarden zijn de future van gui::usb::run (123 KB:
      Manager 61 KB plus de bring_up-future 59 KB, twee kopieën) en het
      FsActor-blok in storage.rs (85 KB). Die op de heap zetten zoals de
      Lifecycle nu, en een wachtpost in qemu-test.sh (rood als stack_kb bij
      HOPOS_TICK 1 boven 224 KB komt), anders breekt de volgende opruiming
      dit weer stil.

De leesreview van 01-10 (wat weg kan, wat simpeler kan, wat goed is, met
de fouten die erbij gevonden zijn) staat in docs/review-2026-10-01.md.

- [ ] Hop bouwt pas zonder patch na een hop-os-tag en het ophogen van de
      drie tags in de hop-repo (agentd-hopos, hopos-runner, hop-http); tot
      dan `HOP_REV=worktree`. De verse Hop (applib::rand, de nieuwe MMU)
      staat alleen op stick G.
- [ ] Hop's downloader weigert chunked transfer ("serve it with a
      Content-Length"): example.com chunkt, een CDN ook. Of chunked lezen,
      of het luid in de docs van de jobspec.
- [ ] De leader-API van Hop staat stil tijdens een download: de dispatch
      doet één aanroep tegelijk en de download zit erin (POST en DELETE
      gaven HTTP 000 na 10 s). De download hoort in een eigen taak.
- [ ] Na een flip meldt Hop één keer `NEXT_STORE failed: system call timed
      out`: de lange wacht van de store-taak liep over de flip heen; hij
      herstelt, maar de regel hoort er niet te zijn.
- [ ] De device-op (19): async BOT, MMC en `deviceabi` hebben geen eigen
      hosttests en geen QEMU-proef.
- [ ] CPU-meting per slot (Go's usage.go) en `cpu_percent` in Hop.
- [ ] De schijf-interruptlijn buiten QEMU virt (UEFI, rk3566, RISC-V, NVMe).
- [ ] Guard-pagina op UEFI en RISC-V; de verhuisde kern op een heap-stack
      van 64 KB zonder guard.
- [ ] hoplb: WebSocket (een 101 wordt 502), geen verbindingspool.
- [ ] hopdns: DNS over TCP, split horizon, CNAME's in de bewoner.
- [ ] Hop op de host: SIGTERM (std heeft geen signaal-API: beslissing),
      overdracht van de agent-staat; op HopOS: de S3-lock alleen met
      hosttests, twee HopOS-nodes naast elkaar alleen op ijzer.
- [ ] applib: `leave_group` vraagt lean; de timebase van 10 MHz voor
      RISC-V komt van de control-page (staat), de tellerfrequentie.
- [ ] lean: de IPv6-baan van leannet, `Stack::leave_group`.
- [ ] Replica op GitHub (`xinix00/replica`): nog geen remote.

### Prestaties: waar v3 onder de Go-lat zit (vitals 30-09)

Stand 01-10 avond, de eerste versie: app naar app M4 4450 (E) en 6100 (P),
O6N 1500 tot 1600, Pi 4 407 tot 435 (de A72 vol), Radxa 257 tot 262 (816
MHz, vol); NVMe door de app op de M4 1270 schrijven en 1690 lezen, rauw op
de Go-lat; Replica (SQLite op HopFS) 0,2 s per proef. Alles boven Go
behalve schrijven door de app op de M4 (1270 tegen 1600) en willekeurig
lezen (12.000 per seconde, de wachtrij is het weekendpunt). De punten
hieronder zijn het logboek van 30-09 en de ochtend van 01-10; wat open is,
staat per node hierboven.

De vier rapporten staan in docs/measurements.md (kolommen v3). Open:

- [ ] **O6N schrijven: ~700 MB/s in de middag tegen 800 tot 1188 in de
      ochtend**, met dezelfde kerncode (de heap maakt geen verschil, zie
      boven), vitals steeds in slot 2. Verschil in de meting: de ochtend
      was een koude boot op de S/X-kern, de middag warme flips (gen 4 tot
      6) na de Lumen-sessie van een andere agent. Nog uitzoeken: koude boot
      op main en opnieuw meten; de NVMe-staat (de rogue-sessie schreef er
      Lumen-beelden heen) en de plaatsing van de core tellen mee.
- [ ] **Na een flip zaait de kern uit jitter, niet uit de TRNG van de
      firmware:** de O6N meldt na de Z-flip `HOPOS_RNG_SLOTS source=jitter`
      waar de koude boot `source=hardware` had (efi-rng is een boot
      service, en die is weg). Klein: de feitenpagina kan 64 bytes zaad van
      de koude boot meedragen, of de oude kern geeft zijn DRBG-stand door
      in de handoff.
- [ ] **Storm door de NAT (hairpin) stokt 1 s per ronde** op de Pi 4 (H):
      p50 2,5 ms, p99 1002 ms, 96 conn/s, zonder `HOPOS_MASQ_SLOT_FULL`.
      Eén SYN per ronde valt (RTO 1 s); ook node naar node (Pi 4 naar Pi 5,
      Radxa naar Pi 4, run 3). Verdachte: leannet's listen-backlog van 8
      (`TCP_BACKLOG` in leannet/src/stack.rs): nu de dial snel is, komen
      200 SYN's sneller binnen dan accept ze haalt, en een SYN op een volle
      backlog valt. Een backlog van 64 in lean (tag) is de lean fix; Go's
      O6N-storm door de NAT haalde 844 conn/s, p99 19,6 ms. Node naar
      node over de draad (vitals storm, 200 verbindingen, 8 werkers, 30-09)
      is bimodaal: ~600 tot 700 conn/s met p50 ~10 ms, of 1400 tot 5000
      met p50 1 tot 4 ms, zonder vaste richting of stempel; cyclus (1
      werker) p50 ~5 tot 7 ms of ~2,4 ms (Go 1,1 tot 4,1). De 1-s-SYN op de
      O6N viel wel samen met `HOPOS_MASQ_SLOT_FULL` (13x; een ronde storm,
      cyclus en rtt is 500 uitgaande flows uit één slot).
- [ ] **App-opslag O6N** (1 MiB-calls): na de deur (H) schrijven 414, lezen
      85 MB/s (Go 557 tot 625 / 727 tot 797). Oorzaak (agent, afgeleid uit
      de code en Go's meting "ongecachet 59 MB/s"): het datablok van de
      NVMe lag Normal-NC en `poll_op` kopieert elke MiB met 131.072
      vluchtige loads uit ongecachet geheugen (~9 ms per MiB). Fix: de stub
      mapt `BLK_DATA` Normal-WB (de driver doet al push en pull), op de
      J-stick. Bewijs na de koude boot: `hopos.nvmebench=1` (rauw lezen van
      ~100 naar >1000 MB/s) en vitals `test=disk`. Het transport kern naar
      app (300 MB/s bij gaten lezen) is de volgende trap. Herhaald op H met
      vitals op 4 cores en bench ernaast (30-09, drie runs): schrijven 87,7
      tot 88,6, lezen 87,7 tot 88,3 MB/s, 4 KiB-schrijven 32 tot 33 MB/s:
      de 414 kwam niet terug. Rauw NVMe, hopfs en sequentieel in
      docs/measurements.md: één boot met hopos.nvmebench=1 nodig (stick-cfg).
- [ ] **NAT-tabel vol**: 512 flows per slot (`HOPOS_MASQ_SLOT_FULL`), dan
      valt een SYN en kost 1 s. Oorzaak: een inbound RST liet de flow 300 s
      staan zonder hem gesloten te tellen. Fix a102e51 (RST = gesloten in
      beide richtingen, sluit-TTL en recycler pakken hem); nog te flippen
      en de storm te herhalen.
- [ ] vitals: de standaard-rx-URL (cachefly) faalt zonder DNS in de env
      ("CONNECT is not supported"); rx-duren vallen op stappen van 100 ms.
- [ ] De Mac hangt op Wi-Fi en macOS laat netmeter niet op het LAN
      ("Lokaal netwerk"-recht): alle host-getallen zijn Wi-Fi; daarom node
      naar node over de draad gemeten (30-09, vitals rx van `/blob`, 256
      MB, drie runs; tabel in docs/measurements.md).
- [ ] **Radxa over de draad ~21 MB/s in beide richtingen** (in 20,1 tot
      22,1, uit 20,8 tot 21,6, tegen O6N, Pi 4 en Pi 5 gelijk, met 4
      verbindingen ook; stempel F): Go 55,8 tot 56,6 in en 98,8 tot 99,6
      uit, dus 2,6x en 4,7x onder de lat.
- [ ] **Pi 5 uit ~43 MB/s** (42,9 tot 43,3 naar de O6N, 41,3 tot 42,5 naar
      de Pi 4; kaart-kern dev): Go 41,6 tot 50,6, dus op de onderkant. De
      Pi 5 in (76 tot 93, Go 57 tot 70), de Pi 4 in (41 tot 44, Go 6,6) en
      uit (76 tot 93, Go 42,3) zitten boven de lat. De O6N haalt in >= 78
      en uit >= 85 (Go 111 tot 118): geen peer is snel genoeg om zijn
      plafond te zien; drie ontvangers tegelijk samen ~90.

- [ ] **Hop: een rolling update met een vaste poort op één node slaagt
      nooit** (O6N 01-10): een `POST /v1/jobs` voor een job die Hop uit zijn
      bewaarde staat had hersteld, is een update (rolling); de nieuwe taak
      wil :80 terwijl de oude hem houdt ("port 80 is taken by slot 2"), en
      Hop probeert elke paar seconden opnieuw. Go had hetzelfde model; de
      eerlijke fix is in Hop: bij een poortbotsing op dezelfde node de oude
      eerst stoppen (recreate), of de botsing als "wacht" tellen in plaats
      van als nieuwe start. Omweg: DELETE en opnieuw POSTen.

### Prestatietests (vitals)

`apps/vitals` plaatsen en `test=all` draaien, de markers in de tabel Vitals
van `docs/measurements.md` (het commando staat daar en in de README).

- [ ] Pi 5: vitals gedraaid, kolom in docs/measurements.md gevuld.
- [ ] Radxa: vitals gedraaid, kolom in docs/measurements.md gevuld.
- [ ] De netmeter-doorvoer (`netmeter NODE:80 --repeat 3`, bench in een
      slot) per board in de tabellen Netwerk en Latentie van
      docs/measurements.md; alleen de QEMU-kolom staat.

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
