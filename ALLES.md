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
| Boot, EL2, kooi, zelftest | ✓ | ✓ | ✓ (zelftest koud: SError bij de VL805) | ✓ | ○ | ✓ VHE (kick via de SGI komt niet aan, de timer vangt hem) | ○ | ○ |
| Hop als bewoner, welcome door de DNAT | ✓ | ✓ | ✓ | ✓ | ○ | ✓ | ○ | ○ |
| Kern-flip, warm | ✓ | ✓ uit de kale kern, ✗ uit de koude gui-kern (RP1) | ✓ (3x) | ✓ (1x) | ○ | ○ | – (geen CPU_OFF) | – |
| NIC met interrupt | ✓ | ✓ MSI-X via de MIP | – (GENET gepold, zoals Go) | ✓ SPI 64 | – (igb gepold, bewust) | ✓ RTL8125B, MSI-X via de ITS (LPI 8192) | ○ (tg3, AIC) | ○ (dwmac) |
| Off-link door de NAT, SNTP | ✓ | ✓ | ✓ | ○ | ○ | ○ | ○ | ○ |
| Watchdog gewapend en geaaid | – | ✓ PM (12 s) | ✓ PM | ✗ DW-WDT gemeten, aai komt | ○ SBSA | ✓ SBSA (8,5 s) | ○ | ○ DW-WDT |
| Hardware-RNG voor de kern | ✗ (jitter) | ✓ RNG200 | ✓ RNG200 | ✗ TRNG komt | ○ SMCCC-TRNG of rndr | ○ rndr | ○ | ✗ (niets) |
| Hardware-RNG voor de slots | ✗ | ✗ | ✗ | ✗ | ✗ | ✗ | ✗ | ✗ |
| Temperatuur in de tik | – | ✓ mailbox | ✓ mailbox | ✗ TSADC komt | ○ SMpro | ✓ SCMI (39 C in de agentlijst) | ○ | – |
| Klokbeleid (dvfs) | – | ✓ 1500/800 | ✓ 1500/600 | – | ○ | ✓ vijf `_CPC`-domeinen, 2600 MHz | – | – |
| Console op het glas | ✓ ramfb | ✓ via flip op de eerste kaart, ✗ sinds de herflash (firmware weigert) | ✓ 32 bpp | ✓ HDMI (geen EDID) | ○ GOP | ✓ GOP 1920x1080 | – | – |
| USB xHCI (HID, display-app) | ✓ qemu-xhci | ○ 2 xHCI's up, niets ingeplugd | ✗ VL805 koud: versie 0, HCRST | ○ 1 van 2 DWC3 | ○ | ○ 10 xHCI's up, één super-speed apparaat op XHC4 | – | – |
| Opslag (hopfs, volumes, OP_SYNC) | ✓ virtio-blk | – (bewust geen NVMe) | – | – (stateless, alles in het geheugen) | ○ NVMe | ✓ NVMe Lexar 4 TB, hopfs hersteld (generatie 3456) | ○ ANS | – |
| Console op 5555 | ✓ | ✓ | ✓ | ✓ | ○ | ✓ | ○ | ○ |
| Hardwaredecoder (media-smaak) | – | – | – | – | – | ✓ Linlon V8, 85,7 fps 4K P010 via de grant | – | – |
| Kaart of stick klaar in `target/` | – | ✓ 17:47 | ✓ 17:47 | ✓ 14:41 (zonder 5555) | ✓ 18:12 | ✓ 18:12 | ✓ 14:41 | ✗ donor-FIP |

## De nodes, één voor één

Alleen wat nog moet; wat af is staat in de tabel hierboven.

### Raspberry Pi 5 (pi5-1, 192.168.1.207)

- [ ] **Een flip vanuit de koud gebootte gui-kern doodt het pad RP1 → host**
      (zeven keer op 30-09): na de landing haalt de GEM zijn descriptors niet
      uit DRAM en zendt hij niets (`txstatus 0x0`, `rxstatus 0x4`), de MSI
      komt niet aan, de watchdog reset naar de kaart. Uitgesloten: de GIC,
      de klok, een IACK. Verdenking: de twee draaiende xHCI's in de RP1
      tijdens de link-reset van de nieuwe kern; de vertrekkende kern
      halteert ze nu vóór de sprong (`HOPOS_USB_HALTED`), en dat zit pas in
      de kaart van 18:22 na een herflash. Proef: koud booten, dan één flip;
      de dump op 5 en 30 s (`HOPOS_RP1_DIAG`) toont het verschil.
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

- [ ] De DW-WDT aaien, de TRNG, de TSADC en de tweede DWC3 (usbdrd30 "not
      clocked"): de agent is bezig (30-09), daarna een warme flip en 5555.
- [ ] EDID: "no answer on the DDC", "sink attached: false". Hangt er een
      scherm aan?
- [ ] De config zit in `hopos.ird`: alleen te wijzigen met `CFG=` of in de
      APPEND-regel.
- [ ] De koude flip weigert (geen staging op de kaart; stateless, dus warm
      flippen of de kaart herstarten).
- [ ] De kaart in `target/` is van 14:41, zonder 5555: opnieuw bouwen na de
      agent.

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
- [ ] Lumen draait (poort 8098, 8 cores, de volumes van de NVMe); nu de
      mediaketen zelf: een disc in de drive (op XHC4 poort 2 zit een
      super-speed apparaat), de optische drive over USB-BOT en MMC (nergens
      getoetst), de HEVC-encoder, WebDAV.
- [ ] De display-app en HID op de xHCI's; hop-gui als job (de init-jobs
      van de Go-config zijn Go-ELF's, niet overgenomen).
- [ ] cloudflare-lean porten (een Go-wrapper om lean) en als job erbij: de
      productieproef.

### Mac mini M4

Image: `target/apple-m4/hopos-apple.img` (14:41, config ingebakken), laden
met `image/apple/boot-cycle.sh`.

- [ ] Nooit gestart: onder m1n1 de bunny, de ADT met 6 E + 4 P cores, de
      watchdogs stil, `cores: via m1n1's spin-table`, de ANS met de GPT, het
      AIC-doel, tg3, DHCP, `HOPOS_APPLE_PREFLIGHT ok`, `HOPOS_CAGE_UP`,
      `HOPOS_OS_SELFTEST ok`.
- [ ] De koude flip werkt er niet (PSCI CPU_OFF zonder EL3); na een
      verhuizing met een rode voorproef spint de oude core.
- [ ] Installeren met kmutil (1TR), daarna `cores: ours`.

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

- [ ] **Hardware-RNG voor de slots. Essentieel.** Een app heeft geen
      entropiebron: TLS en de ISS van leannet komen uit timer-jitter
      (`HOP_TLS_ENTROPY_WEAK`). De kern zaait zichzelf al (rndr, SMCCC-TRNG,
      de RNG200 van de Pi's; de Radxa-TRNG komt van de agent). Nodig: de
      kern zaait elk slot bij de start met 32 bytes uit zijn DRBG op de
      control-page (additief ABI-blok) en ververst ze op verzoek; applib
      gebruikt dat zaad voor TLS, DNS en de ISS, en meldt luid als het
      ontbreekt.
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
