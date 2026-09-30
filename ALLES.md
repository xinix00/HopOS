# ALLES

De ene lijst van wat er nog moet, sinds de eerste boot op ijzer (30-09-2026).
Per node wat er open staat, daaronder wat overal geldt. Wat af is gaat eruit,
niet doorgestreept. Afspraak: één lijst, hier; `docs/README.md` wijst hierheen.

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
- [ ] De koude boot van de kaart van 14:32 (gezien 16:40): `HOPOS_RNG200_UP`,
      `HOPOS_CLOCK_UP` met de val naar 800 MHz na 30 s rust, USB, Hop met
      "no disk on this node", SNTP. Maar géén glas: `fb: mailbox
      framebuffer: vcmail: firmware refused (0x80000001)`, terwijl elke
      geflipte kern de zelfde vraag minuten later wel kreeg. De ontdekking
      wacht nu tot 5 s op de firmware; en de kaart droeg nog de oude
      watchdogproef. Nieuwe kaart nodig (na 17:00 gebouwd) en een koude
      boot om `HOPOS_FB_CONSOLE` en `HOPOS_WD_ARMED` koud te zien.
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
- [ ] Koud geboot van de kaart van 14:33 (gezien 16:40, welcome weg):
      zonder console onbewezen wat de kern zei; die kaart draagt nog de
      oude watchdogproef en niet het geduld voor de framebuffer. Nieuwe
      kaart (na 17:00) en een koude boot; `arm_freq_min` staat nog niet in
      het Pi 4-recept, dus dvfs zegt daar "one ARM clock".

### Radxa Zero 3E (radxa-1)

Geboot op 30-09 (16:40) van de kaart van 14:41 (gui-smaak, Hop): Hop als
leider radxa-1 op 192.168.1.241, welcome geplaatst en via de DNAT HTTP 200.
Zonder seriële console aan de Mac; de kaartbouw: `GUI=1 CFG=<radxa.cfg>
APP=hop sh image/radxa-zero3.sh`, console 1500000 8N1 op de header.

- [ ] De bootregels lezen met de UART aan de Mac: `Retrieving /hopos.ird`,
      de rol uit de initrd, `HOPOS_BOOT`, dwmac4 met de MDIO-PHY, `HOP_UP`,
      en wat de beeldketen (PD_VO, VOP2, DW-HDMI) zei.
- [ ] Geen SD-driver: geen schijf, geen staat over een herstart.
- [ ] De beeldketen (PD_VO, VOP2, DW-HDMI, EDID over DDC) is ongemeten;
      DWC3-USB nooit gezien.
- [ ] De config zit in `hopos.ird`: na het flashen alleen te wijzigen met
      `CFG=` of in de APPEND-regel.
- [ ] De koude flip weigert (geen staging van Hop op de kaart).

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

### Orion O6N (vanavond)

Stick gebouwd 30-09 14:41 in de gui-smaak met Hop: `target/uefi-esp-o6n/`
(node o6n-1, `hopos.cfg` naast `EFI/`).

- [ ] De VHE-kern op de A720: op QEMU met het Neoverse-model bewezen, op
      silicium niet. Verwacht `HOPOS_UEFI_VHE`, `HOPOS_OS_SELFTEST ok`.
- [ ] De console na de exit: de SPCR wijst naar een UART die de SCP dicht
      houdt (Go: de vroege UART op 0x040d0000).
- [ ] MSI-X op de RTL8125 via de Cix-IORT; anders `hopos.nicirq=intx`.
- [ ] Het kernvenster op 0x8800_0000 (`window-8000`) is nieuw op ijzer.
- [ ] De VPU: arena, firmware uit hopfs, `hopos.codecdemo`, apps/decode;
      de meting 24 fps 4K P010 (Go: 27,25 fps).
- [ ] De tien xHCI's uit de DSDT, de display-app, de optische drive over
      USB-BOT en MMC (de device-op is gebouwd, nergens getoetst).
- [ ] `HOPOS_CLOCK_UP` met vijf `_CPC`-domeinen, de thermiek via SCMI.

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
