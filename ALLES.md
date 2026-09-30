# ALLES

De ene lijst van wat er nog moet, sinds de eerste boot op ijzer (30-09-2026).
Per node wat er open staat, daaronder wat overal geldt. Wat af is gaat eruit,
niet doorgestreept. Afspraak: één lijst, hier; `docs/README.md` wijst hierheen.

## De nodes, één voor één

### Raspberry Pi 5 (pi5-1) — de eerste groene node

Gezien op 30-09: bunny, DTB, 2040 MB, GIC-400, de RP1-NIC op 1000 Mbps met
MSI-X via de MIP, DHCP, `HOPOS_OS_SELFTEST ok`, Hop met de MMU aan als
leader, welcome geplaatst op core 1 en de pagina door de DNAT bij de Mac,
en de eerste kern-flip op ijzer: generatie 2, Hop en welcome meegenomen.

- [ ] Off-link verkeer door de NAT: een download van buiten het LAN faalt
      (`connect neverssl.com: deadline exceeded`), SNTP faalt (`udp: no
      answer`). De NAT leert de gateway-MAC alleen van een binnenkomend
      off-subnet frame. Fix: de gateway uit de lease actief ARP'en.
- [ ] Hardware-RNG voor de slots (zie Overal).
- [ ] Watchdog, thermiek en klok via de mailbox (nu alleen haken:
      `HOPOS_WD_NONE`, `HOPOS_CLOCK_NONE`, `temp=-`).
- [ ] De console op het glas: het Pi-image bouwt kaal; `image/rpi5.sh` heeft
      geen `GUI=1`.
- [ ] USB: RP1 usb0/usb1 zijn bedraad maar nooit gezien; HID en de
      display-app.
- [ ] NVMe op de Pi 5 (geen schijf: Hop bewaart geen staat over een herstart).
- [ ] "saved agent state not restored: store i/o failed" hoort "geen schijf"
      te zeggen.
- [ ] De koude flip weigert zodra er ooit een app-core draaide (CPU_OFF komt
      op de Pi 5 niet terug).

### Raspberry Pi 4 (pi4-1)

Nog niet geboot. Image: `image/rpi4.sh` (`EXTRA="hopos.insecure=1 hopos.node=pi4-1"`).

- [ ] Eerste boot: bunny, `HOPOS_BOOT`, genet op de level-SPI met de ack,
      `HOPOS_OS_SELFTEST ok` (de kick op de GIC-400), `HOP_UP`, welcome.
- [ ] VL805-USB met de firmware-handshake via vcmail (nooit gezien).
- [ ] Zelfde punten als de Pi 5: off-link NAT, RNG, mailbox-watchdog, glas.

### Radxa Zero 3E (radxa-1)

Nog niet geboot. Image: `image/radxa-zero3.sh` (`CFG=`, `APP=hop`); console
1500000 8N1 op de header.

- [ ] Eerste boot: `Retrieving /hopos.ird`, de rol uit de initrd,
      `HOPOS_BOOT`, dwmac4 met de MDIO-PHY, `HOP_UP`, welcome.
- [ ] Geen SD-driver: geen schijf, geen staat over een herstart.
- [ ] De beeldketen (PD_VO, VOP2, DW-HDMI, EDID over DDC) is ongemeten;
      DWC3-USB nooit gezien.
- [ ] De config zit in `hopos.ird`: na het flashen alleen te wijzigen met
      `CFG=` of in de APPEND-regel.
- [ ] De koude flip weigert (geen staging van Hop op de kaart).

### Ampere Altra (altra-1)

Nog niet geboot. Stick: `target/uefi-esp-altra/` (`CFG=`, `APP=<agentd-hopos> ROLE=hop`).

- [ ] Eerste boot: de EFI-stub, ACPI, `HOPOS_WD_ARMED` (de eerste echte proef
      van de SBSA-watchdog), igb gepold (bewust: L83), NVMe, `HOP_UP`.
- [ ] Geen guard-pagina onder de stack op de UEFI-boards (een geflipte kern
      hergebruikt de map van de feitenpagina).
- [ ] De schijf-interruptlijn: NVMe pollt (zonder de core vast te houden).

### Orion O6N (vanavond)

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
      SMCCC-TRNG (cpu/src/trng.rs), maar geen boards zonder die twee (de
      Pi's: het RNG-blok van de SoC via een driver; de Radxa; de LicheeRV
      heeft niets). Nodig: (1) een TRNG-driver per board dat er een heeft,
      (2) de kern zaait elk slot bij de start met 32 bytes uit zijn DRBG op
      de control-page (additief ABI-blok) en ververst ze op verzoek, (3)
      applib gebruikt dat zaad voor TLS, DNS en de ISS, en meldt luid als het
      ontbreekt.
- [ ] Off-link verkeer door de NAT (zie Pi 5): één fix voor elk board.
- [ ] SNTP: pas na de NAT-fix te beoordelen; de wandklok staat vast tot dan,
      en https-downloads en de cluster-join wachten erop.
- [ ] De leader-API van Hop staat stil tijdens een download: de dispatch
      doet één aanroep tegelijk en de download zit erin (POST en DELETE
      gaven HTTP 000 na 10 s). De download hoort in een eigen taak.
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
