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
| Boot, EL2, kooi, zelftest | ✓ | ✓ | ✓ (zelftest koud: SError bij de VL805) | ✓ | ○ | ✓ VHE (kick via de SGI komt niet aan, de timer vangt hem) | ✓ kmutil-boot, EL2; ✗ kooi: SError-storm na de AIC, preflight rood | ○ |
| Hop als bewoner, welcome door de DNAT | ✓ | ✓ | ✓ | ✓ | ○ | ✓ | ○ ingebakken (20:45), nog niet gezien | ○ |
| Kern-flip, warm | ✓ | ✓ (ook uit de koude gui-kern, sinds de xHCI-stop vóór de sprong) | ✓ (3x) | ✓ (2x) | ○ | ○ | – (geen CPU_OFF) | – |
| NIC met interrupt | ✓ | ✓ MSI-X via de MIP | – (GENET gepold, zoals Go) | ✓ SPI 64 | – (igb gepold, bewust) | ✓ RTL8125B, MSI-X via de ITS (LPI 8192) | ✓ tg3 link + DHCP (gepold); ✗ doof daarna | ○ (dwmac) |
| Off-link door de NAT, SNTP | ✓ | ✓ | ✓ | ○ | ○ | ○ | ○ | ○ |
| Watchdog gewapend en geaaid | – | ✓ PM (12 s) | ✓ PM | ✓ DW-WDT (89 s) | ○ SBSA | ✓ SBSA (8,5 s) | ○ | ○ DW-WDT |
| Hardware-RNG voor de kern | ✗ (jitter) | ✓ RNG200 | ✓ RNG200 | ✓ rk3568-rng | ○ SMCCC-TRNG of rndr (fc5348f) | ○ rndr (fc5348f, nog te flippen) | ○ | ✗ (niets) |
| Hardware-RNG voor de slots (CTRL_RNG_SEED, fc5348f) | ○ jitter | ○ | ○ | ○ | ○ | ○ | ○ | ○ |
| Temperatuur in de tik | – | ✓ mailbox | ✓ mailbox | ✗ TSADC converteert niet (Go ook niet) | ○ SMpro | ✓ SCMI (39 C in de agentlijst) | ○ | – |
| Klokbeleid (dvfs) | – | ✓ 1500/800 | ✓ 1500/600 | – | ○ | ✓ vijf `_CPC`-domeinen, 2600 MHz | – | – |
| Console op het glas | ✓ ramfb | ✓ via flip op de eerste kaart, ✗ sinds de herflash (firmware weigert) | ✓ 32 bpp | ✓ HDMI (geen EDID) | ○ GOP | ✓ GOP 1920x1080 | – | – |
| USB xHCI (HID, display-app) | ✓ qemu-xhci | ○ 2 xHCI's up, niets ingeplugd | ✗ VL805 koud: versie 0, HCRST | ○ 2 DWC3 up, niets ingeplugd | ○ | ✓ 10 xHCI's up, de Blu-ray-drive over USB-BOT leest de disc (Lumen) | – | – |
| Opslag (hopfs, volumes, OP_SYNC) | ✓ virtio-blk | – (bewust geen NVMe) | – | – (stateless, alles in het geheugen) | ○ NVMe | ✓ NVMe Lexar 4 TB, hopfs hersteld (generatie 3456) | ✓ ANS NVMe, hopfs hersteld (395 GB) | – |
| Console op 5555 | ✓ | ✓ | ✓ | ✓ | ○ | ✓ | ○ luistert, onbereikbaar (doof na DHCP) | ○ |
| Hardwaredecoder (media-smaak) | – | – | – | – | – | ✓ Linlon V8, 85,7 fps 4K P010 via de grant | – | – |
| Kaart of stick klaar in `target/` | – | ✓ 17:47 | ✓ 17:47 | ✓ 14:41 (zonder 5555) | ✓ 18:12 | ✓ 18:12 | ✓ 20:45 (Hop ingebakken) | ✗ donor-FIP |

## De nodes, één voor één

Alleen wat nog moet; wat af is staat in de tabel hierboven.

### Raspberry Pi 5 (pi5-1, 192.168.1.207)

- [ ] Na de flip vanuit de verse kaart (18:22) leefde de RP1 (DHCP in 29 ms,
      `HOPOS_FLIP_SETTLED`, welcome 200), maar 5555 en 10100 (de node-stack)
      antwoorden daarna niet meer terwijl de DNAT-poorten werken; nakijken
      op de volgende flip, met de dump.
- [ ] Het glas: sinds de herflash weigert de firmware elke framebuffer
      (0x80000001), ook koud met 5 s geduld. Hangt het scherm eraan en
      stond het aan bij de power-on?
- [ ] De echte watchdogtoets: de kabel eruit of Hop stoppen, reset binnen
      12 s.
- [ ] USB: HID en de display-app (niets ingeplugd).
- [ ] De koude flip weigert zodra er een app-core draaide (CPU_OFF komt op
      de Pi 5 niet terug).

### Raspberry Pi 4 (pi4-1, 192.168.1.40)

- [ ] De VL805 faalt koud: "firmware loaded by the VideoCore, version now
      0x0", "timeout on HCRST clear", `HOPOS_USB_NONE`, en de eerste
      EL1-beurt krijgt een SError (`HOPOS_OS_SELFTEST_FAIL vec 11`). Go's
      handshake via vcmail nakijken (OLD/metal/board/rpi4): de notify hoort
      een versie te geven, geen 0. Op de warme flips was de zelftest ok.
- [ ] De echte watchdogtoets, HID en de display-app.

### Radxa Zero 3E (radxa-1, 192.168.1.241)

- [ ] De TSADC geeft geen geldige code (raw cpu 0 gpu 0, `HOPOS_TSADC_NONE`),
      zoals in Go op 06-08; de init is die van Linux. Waarom converteert hij
      niet.
- [ ] De echte watchdogtoets (DW-WDT 89 s): Hop stoppen of de kabel eruit.
- [ ] EDID: "no answer on the DDC", "sink attached: false". Hangt er een
      scherm aan?
- [ ] De config zit in `hopos.ird`: alleen te wijzigen met `CFG=` of in de
      APPEND-regel.
- [ ] De koude flip weigert (geen staging op de kaart; stateless, dus warm
      flippen of de kaart herstarten).
- [ ] De kaart in `target/` opnieuw flashen zodra de build van 19:05 (5555,
      DW-WDT, TRNG, DWC3) er staat.

### Ampere Altra (altra-1)

Stick: `target/uefi-esp-altra/` (18:12, main fc66084), naar een FAT32-stick
met `hopos.cfg` naast `EFI/`.

- [ ] Eerste boot: de EFI-stub, ACPI, `HOPOS_WD_ARMED` (SBSA), igb gepold
      (bewust), NVMe, `HOP_UP`, welcome, 5555.
- [ ] Geen guard-pagina onder de stack op de UEFI-boards.
- [ ] De schijf-interruptlijn: NVMe pollt.

### Orion O6N (o6n-1, 192.168.1.205)

- [ ] De zelftest-kick: de SGI van de OS-core naar zichzelf komt op de
      GICv3 niet aan (`kick=(Timer, 100000 us, try 2)`,
      `HOPOS_OS_SELFTEST_FAIL`); de timer vangt hem.
- [ ] De VPU had één herstelcyclus nodig ("incomplete power state
      pgctrl=0x7cef000"); waarom.
- [ ] `hopos.codecdemo`, de kernmeting, naast de 85,7 fps van apps/decode.
- [ ] Lumen draait (poort 8098, 8 cores, de volumes van de NVMe) en leest
      de Blu-ray in de drive over USB-BOT (18:40); nu de rest van de
      mediaketen: MMC, de HEVC-encoder, WebDAV.
- [ ] De display-app en HID op de xHCI's; hop-gui als job (de init-jobs
      van de Go-config zijn Go-ELF's, niet overgenomen).
- [ ] cloudflare-lean porten (een Go-wrapper om lean) en als job erbij: de
      productieproef.

### Mac mini M4 (m4-1, 192.168.1.122)

Image: `target/apple-m4/hopos-apple.img` (30-09 20:45, 2588672 bytes, Hop
ingebakken via `EMBED=`, config met `hopos.replay=45` en `hopos.cages=on`);
nog niet geïnstalleerd. Op de stick staat de 20:25-build (SError-drain,
zonder Hop). Console: USB-C-debugkabel, `sudo macvdmtool debugusb`
(zonder reboot; na elke herstart van de M4 de kabel aan de Mac-kant even
uit en in, anders enumereert de kis-poort niet), lezer
`scratchpad/m4-watch.sh` op `/dev/cu.kis-100000-ch-0`. De kern herhaalt
elke 45 s zijn eerste 16 KiB console (`HOPOS_CONSOLE_REPLAY`).

- [ ] v3 boot volledig onder kmutil (30-09 20:07): bunny, ADT 6 E + 4 P,
      cores ours, NVMe AP0512Z met hopfs (395 GB), AIC, apcie 2 van 3
      poorten, tg3 LINK UP 1000, DHCP 192.168.1.122 in 1 ms, 10100 en
      5555 luisteren. Maar:
- [ ] **SError-storm vanaf de AIC-start**: ESR 0xbe000000 (vector 11), niet
      vóór de AIC, wel na elke stap daarna en continu; de EL1-beurt van de
      preflight sterft er meteen aan (`HOPOS_APPLE_PREFLIGHT_FAULT`), de
      kern weigert de kooien, geen Hop. Go: zo'n SError is een L2C_ERR
      (stille verboden schrijf, adres in L2C_ERR_ADR). De 20:45-build leest
      en wist m1n1's L2C_ERR_STS/ADR/INF bij elke drain
      (`HOPOS_APPLE_SERROR ... l2c adr=`) en forceert de kooien
      (`hopos.cages=on`; een bewoner draait op EL1 met PSTATE.A dicht, dus
      Hop overleeft de storm). Eerst kijken welk adres het is (de AIC-
      bring-up schrijft iets dat dit silicium weigert; vergelijk
      OLD/metal/board/apple en driver/aic met m1n1's aic.c voor de t8132).
- [ ] **Doof na DHCP**: de lease komt in 1 ms, daarna antwoordt de node
      niet meer (geen ARP, ping, 5555, 10100 vanaf de Mac). tg3 RX-pad
      (bijvullen van de producer-ring, of DMA die stilvalt door de
      L2C-fout). Een tg3-diagnoseregel in de tik (`counters()`,
      `rcb_dump()`, `irq_diag()` bestaan in driver/nic/tg3) is de volgende
      stap; de pomp bezit de NIC, de tik niet.
- [ ] Onder kmutil geen stage van een loader: de kern neemt nu de
      ingebakken Hop (`board/apple/build.rs`, `HOPOS_EMBED`,
      `slots::staged_image` valt erop terug). Nog niet op ijzer gezien.
- [ ] Flippen op de M4: geen PSCI, dus `send_off` (CPU_OFF) bestaat niet;
      flip.rs kent het board (`board-apple` in de koude weg) maar de warme
      flip met een geparkeerde app-core is hier nooit gedaan. Pas zinvol
      als Hop woont.
- [ ] De koude flip werkt er niet (PSCI CPU_OFF zonder EL3); na een
      verhuizing met een rode voorproef spint de oude core.
- [ ] Na de eerste zichtbare boot: `cores: ours` (zonder m1n1's spin-table).

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

- [ ] **Hardware-RNG voor de slots: gebouwd (fc5348f), nog op ijzer zien.**
      Het zaad staat op de control page (CTRL_RNG_SEED/GEN/SOURCE, elke
      seconde vers), applib::rand mengt het met jitter, Hop meldt
      `HOP_TLS_ENTROPY_HW`. Per board na een flip `HOPOS_RNG_SLOTS
      source=hardware` en in slot 1 `HOPOS_APP_RNG source=hardware` zien;
      dan de tabel op ✓. Hop bouwt pas zonder patch na een hop-os-tag en
      het ophogen van de drie tags in de hop-repo (agentd-hopos,
      hopos-runner, hop-http); tot dan `HOP_REV=worktree`.
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
- [ ] Apple ANS nog synchroon in `start`.
- [ ] hoplb: WebSocket (een 101 wordt 502), geen verbindingspool.
- [ ] hopdns: DNS over TCP, split horizon, CNAME's in de bewoner.
- [ ] Hop op de host: SIGTERM (std heeft geen signaal-API: beslissing),
      overdracht van de agent-staat; op HopOS: de S3-lock alleen met
      hosttests, twee HopOS-nodes naast elkaar alleen op ijzer.
- [ ] applib: `leave_group` vraagt lean; de timebase van 10 MHz voor
      RISC-V komt van de control-page (staat), de tellerfrequentie.
- [ ] lean: de IPv6-baan van leannet, `Stack::leave_group`.
- [ ] Replica op GitHub (`xinix00/replica`): nog geen remote.

### Prestatietests (vitals)

`apps/vitals` plaatsen en `test=all` draaien, de markers in de tabel Vitals
van `docs/measurements.md` (het commando staat daar en in de README).

- [ ] Pi 5: vitals gedraaid, kolom in docs/measurements.md gevuld.
- [ ] Pi 4: vitals gedraaid, kolom in docs/measurements.md gevuld.
- [ ] Radxa: vitals gedraaid, kolom in docs/measurements.md gevuld.
- [ ] O6N: vitals gedraaid, kolom in docs/measurements.md gevuld.
- [ ] M4: vitals gedraaid, kolom in docs/measurements.md gevuld.
- [ ] De netmeter-doorvoer (`netmeter NODE:80 --repeat 3`, bench in een
      slot) per board in de tabellen Netwerk en Latentie van
      docs/measurements.md; alleen de QEMU-kolom staat.
