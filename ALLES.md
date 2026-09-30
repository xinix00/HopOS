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
| Kaart of stick klaar in `target/` | – | ✓ 17:47 | ✓ 17:47 | ✓ 14:41 (zonder 5555) | ✓ 18:12 | ✓ 18:12 | ✓ 14:41 | ✗ donor-FIP |

## De nodes, één voor één

### Raspberry Pi 5 (pi5-1) — de eerste groene node

Gezien op 30-09: bunny, DTB, 2040 MB, GIC-400, de RP1-NIC op 1000 Mbps met
MSI-X via de MIP, DHCP, `HOPOS_OS_SELFTEST ok`, Hop met de MMU aan als
leader, welcome geplaatst op core 1 en de pagina door de DNAT bij de Mac,
twee kern-flips op ijzer (generatie 2 en 3, Hop en welcome meegenomen), en
na de tweede flip (alpha.17: de NAT ARP't naar de gateway) SNTP gelukt
(`HOP_CLOCK_SYNCED`, stratum 2, 7,8 ms) en TCP naar buiten het LAN. De derde
flip (generatie 4, de gui-smaak: `GUI=1 sh image/flip-bundle.sh rpi5`) zette
de console op het glas (`HOPOS_FB_CONSOLE`, 1920x1080, 120x67 cellen; de
bunny en de meetregels gezien op de HDMI) en bracht beide RP1-xHCI's op
(`HOPOS_USB_UP`). Daarna, tot generatie 9: de wandklok mee over de flip,
de temperatuur in de tik, de NAT die de gateway-MAC niet meer van buren
leert (18 van 18 connects naar example.com, `natin` telt de antwoorden),
de RNG200 (`HOPOS_RNG200_UP`) en de PM-watchdog gewapend en geaaid.

- [ ] Hardware-RNG voor de slots (zie Overal). De RNG200 van de SoC zaait
      sinds generatie 8 de kern-DRBG (`HOPOS_RNG200_UP`, gezien 30-09); de
      slots hebben er nog niets aan.
- [ ] Het glas toont pas de echte tijd als Hop zijn uurlijkse SNTP doet:
      de wandklok gaat sinds generatie 6 mee over de flip
      (`HOPOS_CLOCK_CARRIED`, bewezen 30-09), maar de generaties 4 en 5
      begonnen nog op de vaste boot-klok en die offset is wat 6 erfde.
      Verdwijnt vanzelf na de volgende `HOPOS_CLOCK_SET`.
- [ ] De PM-watchdog staat gewapend sinds generatie 9 (`HOPOS_WD_ARMED`
      12 s, `HOPOS_CANARY_LIVE`, en de node leeft daarna gewoon door: de
      aaien werken). De echte toets is nog niet gedaan: de kabel eruit of
      Hop stoppen, en dan een reset binnen ~12 s (dat boot de kale kaart).
- [ ] Dvfs via de mailbox: geport, maar de kaart heeft geen `arm_freq_min`
      (generatie 8: `HOPOS_CLOCK_NONE`, "one ARM clock"). Komt met de nieuwe
      kaart: `HOPOS_CLOCK_UP`, `HOPOS_CLOCK_EDGE` naar 800 MHz na 30 s rust.
- [ ] **Een flip vanuit de koud gebootte kaart-kern doodt het pad RP1 →
      host** (zeven keer op 30-09: J tot O): na de landing zijn de
      GEM-registers bereikbaar en ziet hij de link, maar hij haalt zijn
      descriptors niet uit DRAM (`rxqbase` blijft op de basis, `rxstatus
      0x4` overrun, de descriptors onaangeroerd ook na cache-invalidatie)
      en zijn MSI komt evenmin aan (`irq(nic=0)`); DHCP "no server
      answered", de boot-guard verloopt, de watchdog reset naar de kaart
      en die boot gewoon. Uitgesloten: de GIC (veeg), de klok (NIC-init op
      1500 MHz, en de flank 10 s uitgesteld), een IACK die de RP1 nog
      verwachtte. Uit de kale alpha.17-kern slaagden acht flips, en de Pi 4
      (GENET op de SoC) flipt vanuit zijn koude gui-kern gewoon. Wat de
      koude gui-kern anders achterlaat: twee draaiende xHCI's in de RP1
      (DMA-masters op dezelfde PCIe-link) tijdens de link-reset van de
      nieuwe kern. Sinds de commit na proef O halteert de vertrekkende
      kern zijn xHCI's vlak vóór de sprong (`gui::quiesce_usb`,
      `HOPOS_USB_HALTED`) en zet hij de klok vol; dat zit pas in de kaart
      na een herflash. Proef: de nieuwe kaart koud booten (de dump op 5 en
      30 s is dan de referentie, `HOPOS_RP1_DIAG`) en dan flippen.
- [ ] Koude boot van de kaart van 17:47 (gezien 21:50): `HOPOS_WD_ARMED` en
      `HOPOS_CANARY_LIVE` koud, `HOPOS_CONPORT_UP`, RNG200, dvfs, USB,
      Hop; de referentiedump (`HOPOS_RP1_DIAG`) toont `txstatus 0x21` en
      een lopende ringpointer, terwijl elke mislukte flip `txstatus 0x0`
      had: na een flip zond de GEM nooit iets uit. Het glas blijft ook koud
      geweigerd (0x80000001, 5 s geduld).
- [ ] De koude boot van de kaart van 14:32 (gezien 16:40): `HOPOS_RNG200_UP`,
      `HOPOS_CLOCK_UP` met de val naar 800 MHz na 30 s rust, USB, Hop met
      "no disk on this node", SNTP. Maar géén glas: `fb: mailbox
      framebuffer: vcmail: firmware refused (0x80000001)`, en ook elke
      flip daarna weigert, óók met 5 s geduld, terwijl op de vorige kaart
      elke flip het glas kreeg. Hangt het scherm nog aan de Pi 5 en stond
      het aan bij de power-on? De nieuwe kaart (na 19:30) en een koude boot
      met het scherm aan: `HOPOS_FB_CONSOLE`, `HOPOS_WD_ARMED`.
- [ ] USB: de xHCI's staan, maar nog geen HID gezien (niets ingeplugd) en
      geen display-app.
- [ ] "saved agent state not restored: store i/o failed" hoort "geen schijf"
      te zeggen. Geen NVMe op de Pi 5: bewust niet.
- [ ] De koude flip weigert zodra er ooit een app-core draaide (CPU_OFF komt
      op de Pi 5 niet terug).

### Raspberry Pi 4 (pi4-1)

Geboot op 30-09 met de kale kern (alpha.17, `image/rpi4.sh` met
`EXTRA="hopos.insecure=1 hopos.node=pi4-1"`): Hop als leider op
192.168.1.40 en welcome via de DNAT (HTTP 200), zonder seriële console aan
de Mac.

- [ ] De USB-UART aan de Mac, dan de bootregels lezen: `HOPOS_BOOT`, genet
      op de level-SPI met de ack, `HOPOS_OS_SELFTEST ok` (de kick op de
      GIC-400), `HOP_UP`.
- [ ] De gui-smaak is er warm in geflipt (`GUI=1 sh image/flip-bundle.sh
      rpi4`, generatie 2): de agentlijst kreeg meteen `temp_milli_c` (48 tot
      50 C) en welcome bleef 200. Zonder console onbewezen: het glas
      (`HOPOS_FB_CONSOLE`) en VL805-USB met de firmware-handshake via vcmail.
Generatie 4 (bundel L, 30-09 ~19:15), gelezen via `nc 192.168.1.40 5555`:
`HOPOS_RNG200_UP` (BCM2711), de console op het glas (VideoCore
1920x1080, 32 bpp, `HOPOS_FB_CONSOLE`), dvfs `HOPOS_CLOCK_UP` met
600/1500 MHz (de Pi 4-firmware heeft zelf een vloer), VL805 geladen door
de VideoCore, de PM-watchdog gewapend, en drie warme flips op rij (J, K, L)
zonder de NIC-dood van de Pi 5 (de GENET wordt gepold).

- [ ] Koude boot van de kaart van 17:47 (gezien 21:55 op 5555): RNG200,
      het glas, dvfs 600/1500, `HOPOS_WD_ARMED` en `HOPOS_CANARY_LIVE`,
      `HOPOS_CONPORT_UP`, Hop. Maar de VL805 faalt koud: "firmware loaded
      by the VideoCore, version now 0x0", dan "timeout on HCRST clear",
      `HOPOS_USB_NONE`; en de eerste EL1-beurt van de zelftest krijgt een
      SError (`HOPOS_OS_SELFTEST_FAIL timer=(Fault, vec 11, INTID 1023)`),
      wat past bij een PCIe-toegang op een xHCI zonder firmware. Op de
      flips J tot L was de zelftest wel ok. Go's handshake via vcmail
      nakijken (OLD/metal/board/rpi4): de mailbox-notify hoort een versie
      te geven, geen 0.

### Radxa Zero 3E (radxa-1)

Geboot op 30-09 (16:40) van de kaart van 14:41 (gui-smaak, Hop): Hop als
leider radxa-1 op 192.168.1.241, welcome geplaatst en via de DNAT HTTP 200.
Zonder seriële console aan de Mac; de kaartbouw: `GUI=1 CFG=<radxa.cfg>
APP=hop sh image/radxa-zero3.sh`, console 1500000 8N1 op de header.

Generatie 3 (bundel L, de eerste warme flip op de Radxa, 30-09 ~19:20):
`HOPOS_FLIP_SETTLED`, dwmac4 1000 Mbps met DHCP in 9 ms, de beeldketen
staat (`display: 1920x1080p60 on HDMI`, `HOPOS_FB_CONSOLE`), de console op
`nc 192.168.1.241 5555` (`HOPOS_CONPORT_UP`), usbhost30 als xHCI up.

- [ ] EDID: "no answer on the DDC at byte 0, driving 1920x1080p60 blind,
      sink attached: false". Hangt er een scherm aan? Zo ja, dan de DDC.
- [ ] usbdrd30: "GSNPSID names no DWC3 core (0 = not clocked)": de tweede
      DWC3 krijgt zijn klok niet.
- [ ] De DW-WDT: gemeten (89478 ms op TOP 15) maar "NOT armed (no petting
      policy in v3 yet)": aansluiten op `hopos/src/watchdog.rs` zoals de
      PM-watchdog van de Pi.
- [ ] Geen RNG (`HOPOS_RNG_INSECURE`), geen dvfs. Geen SD-driver: bewust,
      de Radxa's zijn stateless (Derek, 30-09).
- [ ] De config zit in `hopos.ird`: na het flashen alleen te wijzigen met
      `CFG=` of in de APPEND-regel.
- [ ] De koude flip weigert (geen staging van Hop op de kaart; stateless,
      dus alleen een warme flip of een herstart van de kaart).

### Ampere Altra (altra-1)

Nog niet geboot. Stick gebouwd 30-09 14:41 in de gui-smaak met Hop:
`target/uefi-esp-altra/` (`GUI=1 BOARD=altra CFG=<altra.cfg>
APP=<agentd-hopos> ROLE=hop sh image/uefi-run.sh`), naar een FAT32-stick met
`hopos.cfg` naast `EFI/`.

- [ ] Eerste boot: de EFI-stub, ACPI, `HOPOS_WD_ARMED` (de eerste echte proef
      van de SBSA-watchdog), igb gepold (bewust: L83), NVMe, `HOP_UP`.
- [ ] Geen guard-pagina onder de stack op de UEFI-boards (een geflipte kern
      hergebruikt de map van de feitenpagina).
- [ ] De schijf-interruptlijn: NVMe pollt (zonder de core vast te houden).

### Orion O6N

Geboot op 30-09 (18:31) in één keer van de media-stick (gui plus media,
main fc66084, `BOOTAA64.EFI` naast de hernoemde Go-build): `HOPOS_UEFI_VHE`,
de config van de ESP, de GOP-console op het glas, de NVMe (Lexar NM790
4 TB) met hopfs hersteld uit de Go-tijd (generatie 3456), de ITS met LPI's,
dvfs met vijf `_CPC`-domeinen naar 2600 MHz, de RTL8125B met MSI-X via de
ITS, tien xHCI's, de SBSA-watchdog gewapend (8,5 s), de codec-firmware (16
blobs van het volume), de VPU via SCMI (na één herstelcyclus,
`HOPOS_VPU_RECOVER`) en de Linlon V8 up (4 cores, arena 768 MB,
`HOPOS_CODEC_UP`), Hop als leider o6n-1 op 192.168.1.205, welcome op een
app-core met HTTP 200, de console op 5555.

- [ ] De zelftest-kick: `kick=(Timer, 100000 us, try 2)`, dus
      `HOPOS_OS_SELFTEST_FAIL`: de SGI van de OS-core naar zichzelf komt op
      de GICv3 van de O6N niet aan en de timer vangt hem. De rest werkt.
- [ ] De VPU had één herstelcyclus nodig ("incomplete power state
      pgctrl=0x7cef000"); daarna goed.
- [ ] De console na de exit: de SPCR-UART blijft van de SCP; 5555 is de
      console (werkt).
- [ ] De meting is gedaan: apps/decode via Hop met een 4K-clip van 240
      beelden geeft `HOPOS_DECODE fps=85.7 MBps=2134` (2797 ms; de lat is
      24 fps, Go 27,25 met Lumen). Eerst faultte de decoder na 14 beelden:
      decode knipte zijn happen van 1 MB midden in een NAL-eenheid (de fout
      van 22-09 in docs/media.md); nu knipt hij op de laatste startcode en
      neemt hij de rest mee. Nog te doen: `hopos.codecdemo` (het
      meetinstrument van de kern zelf) en Lumen.
- [ ] De display-app en HID op de xHCI's (op XHC4 poort 2 zit een
      super-speed apparaat), de optische drive over USB-BOT en MMC (de
      device-op is gebouwd, nergens getoetst).
- [ ] De Go-config droeg init-jobs (display, launcher, apps van
      hop-os-surf): Go-ELF's, niet overgenomen; hop-gui als job op v3.
- [ ] De productieproef (Derek, 30-09): Lumen, de mediaserver
      (hop-app-lumen, de Rust-port in `rust/` draait op QEMU: portaal,
      WebDAV, beheer; de mediaketen met de disc, de hardwaredecoder en de
      HEVC-encoder op NVMe wacht op de O6N), en cloudflare-lean (een
      Go-wrapper om lean, nog te porten; "zal meevallen"). Beide als job op
      de O6N via Hop.

### Mac mini M4 (vanavond)

Image gebouwd 30-09 14:41 met Hop en de config ingebakken (node m4-1):
`target/apple-m4/hopos-apple.img`; laden met `image/apple/boot-cycle.sh`.

- [ ] Nooit gestart. Onder m1n1: `hopos x…`, de bunny, de ADT met 6 E + 4 P
      cores, de watchdogs stil, `cores: via m1n1's spin-table`, `HOPOS_BOOT`,
      de ANS met de GPT, het AIC-doel, tg3 tot `HOPOS_NIC_UP`, DHCP,
      `HOPOS_APPLE_PREFLIGHT ok`, `HOPOS_CAGE_UP`, `HOPOS_OS_SELFTEST ok`.
- [ ] De koude flip werkt er niet (PSCI CPU_OFF zonder EL3).
- [ ] Na een verhuizing met een rode voorproef spint de oude core.
- [ ] Installeren met kmutil (1TR), daarna `cores: ours`.

### LicheeRV Nano (RISC-V)

- [ ] Donor-FIP aanwijzen (docs/boards-riscv.md: `fip.bin` uit sipeed
      release 20260114) en de kaart bouwen met `CFG=` en `APP=`.
- [ ] Eerste boot: `CLINT: mtimecmp writable`, `HOPOS_RNG_INSECURE`, de
      DW-WDT, dwmac 0x1037, de C906L via de reset-ingang, appspike.
- [ ] Geen kick van app naar kern, geen SMP, geen flip op RISC-V; de C906L
      spint; geen SD.

### De console op 5555

Sinds 30-09 (commit na batch K): `nc NODE 5555` geeft de bewaarde console
(256 KiB) en leest live mee, aan bij `hopos.insecure=1` of `hopos.console=1`.
Gezien op de Pi 4 en de Radxa; de Pi 5 pas na een geslaagde flip of de
nieuwe kaart.

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
      (`HOP_TLS_ENTROPY_WEAK`). De kern kent `rndr` (FEAT_RNG) en de
      SMCCC-TRNG (cpu/src/trng.rs), en sinds 30-09 de RNG200 van de Pi's
      (driver/rng200, `HOPOS_RNG200_UP` op de Pi 5); de Radxa en de LicheeRV
      hebben nog niets. Nodig: (1) een TRNG-driver voor de Radxa,
      (2) de kern zaait elk slot bij de start met 32 bytes uit zijn DRBG op
      de control-page (additief ABI-blok) en ververst ze op verzoek, (3)
      applib gebruikt dat zaad voor TLS, DNS en de ISS, en meldt luid als het
      ontbreekt.
- [ ] Hop's downloader weigert chunked transfer ("serve it with a
      Content-Length"): example.com chunkt, een CDN ook. Of chunked lezen,
      of het luid in de docs van de jobspec.
- [ ] De leader-API van Hop staat stil tijdens een download: de dispatch
      doet één aanroep tegelijk en de download zit erin (POST en DELETE
      gaven HTTP 000 na 10 s; twee keer gezien op de Pi 5). De download
      hoort in een eigen taak.
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
