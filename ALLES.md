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
| Boot, EL2, kooi, zelftest | ✓ | ✓ | ✓ (koud: SError bij de VL805; fix d0b6bd3 wacht op een koude boot) | ✓ | ○ | ✓ VHE, kick via SGI 1 (`kick=(Ipi, 0 us)`, stempel G) | ✓ kmutil-boot, EL2; ✗ kooi: SError-storm na de AIC, preflight rood | ○ |
| Hop als bewoner, welcome door de DNAT | ✓ | ✓ | ✓ | ✓ | ○ | ✓ | ○ ingebakken (20:45), nog niet gezien | ○ |
| Kern-flip, warm | ✓ | ✗ sterft na de landing (F en H, 3x); de nieuwe kaart (H, met de zwarte doos) ligt in target/ | ✓ (5x, gen 3 op H) | ✓ (4x, gen 2 op H) | ○ | ✓ gen 2 op H (gui-bundel; de media-bundel wacht op de kern-fix de61b4b via de volgende stick) | – (geen CPU_OFF) | – |
| NIC met interrupt | ✓ | ✓ MSI-X via de MIP | – (GENET gepold, zoals Go) | ✓ SPI 64 | – (igb gepold, bewust) | ✓ RTL8125B, MSI-X via de ITS (LPI 8192) | ✓ tg3 link + DHCP (gepold); ✗ doof daarna | ○ (dwmac) |
| Off-link door de NAT, SNTP | ✓ | ✓ | ✓ | ○ | ○ | ○ | ○ | ○ |
| Watchdog gewapend en geaaid | – | ✓ PM (12 s) | ✓ PM | ✓ DW-WDT (89 s) | ○ SBSA | ✓ SBSA (8,5 s) | ○ | ○ DW-WDT |
| Hardware-RNG voor de kern | ✗ (jitter) | ✓ RNG200 | ✓ RNG200 | ✓ rk3568-rng | ○ SMCCC-TRNG of rndr (fc5348f) | ○ efi-rng: geen FEAT_RNG of SMCCC-TRNG, wel het EFI_RNG_PROTOCOL van de firmware (`hopos.efirng=1`, volgende stick) | ○ | ✗ (niets) |
| Hardware-RNG voor de slots (CTRL_RNG_SEED, fc5348f) | ○ jitter | ○ (kaart-kern van vóór fc5348f) | ✓ rng200 (F) | ✓ rk3568-rng (F) | ○ | ○ jitter (G); efi-rng met de volgende stick | ○ | ○ |
| Temperatuur in de tik | – | ✓ mailbox | ✓ mailbox | ✗ TSADC converteert niet (Go ook niet) | ○ SMpro | ✓ SCMI (39 C in de agentlijst) | ○ | – |
| Klokbeleid (dvfs) | – | ✓ 1500/800 | ✓ 1500/600 | – | ○ | ✓ vijf `_CPC`-domeinen, 2600 MHz | – | – |
| Console op het glas | ✓ ramfb | ✓ via flip op de eerste kaart, ✗ sinds de herflash (firmware weigert) | ✓ 32 bpp | ✓ HDMI (geen EDID) | ○ GOP | ✓ GOP 1920x1080 | – | – |
| USB xHCI (HID, display-app) | ✓ qemu-xhci | ○ 2 xHCI's up, niets ingeplugd | ○ VL805 koud: fix d0b6bd3 (SCB0_SIZE, notify, twee pogingen), koude boot nodig | ○ 2 DWC3 up, niets ingeplugd | ○ | ✓ 10 xHCI's up, de Blu-ray-drive over USB-BOT leest de disc (Lumen) | – | – |
| Opslag (hopfs, volumes, OP_SYNC) | ✓ virtio-blk | – (bewust geen NVMe) | – | – (stateless, alles in het geheugen) | ○ NVMe | ✓ NVMe Lexar 4 TB, hopfs hersteld (generatie 3456) | ✓ ANS NVMe, hopfs hersteld (395 GB) | – |
| Console op 5555 | ✓ | ✓ | ✓ | ✓ | ○ | ✓ | ○ luistert, onbereikbaar (doof na DHCP) | ○ |
| Hardwaredecoder (media-smaak) | – | – | – | – | – | ✓ Linlon V8, 85,7 fps 4K P010 via de grant; nu tijdelijk weg (gui-flip H) tot de koude boot | – | – |
| Kaart of stick klaar in `target/` | – | ✓ 17:47 | ✓ 17:47 | ✓ 14:41 (zonder 5555) | ✓ 18:12 | ✓ 21:29 stempel G (nieuwe Hop) | ✓ 20:45 (Hop ingebakken) | ✗ donor-FIP |

## De nodes, één voor één

Alleen wat nog moet; wat af is staat in de tabel hierboven.

### Raspberry Pi 5 (pi5-1, 192.168.1.207)

- [ ] De warme flip sterft na de landing (stempel F, twee keer): koud terug
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
- [ ] De echte watchdogtoets (DW-WDT 89 s); EDID ("no answer on the DDC").
- [ ] De config zit in `hopos.ird`: alleen te wijzigen met `CFG=` of in de
      APPEND-regel. De koude flip weigert (stateless: warm flippen of de
      kaart herstarten). De kaart in `target/` is van 14:41; de node draait
      warm op F.

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
      agents). Lezersplaatsen lekken bij abrupt sluitende clients.
- [ ] Lumen terugzetten na elke koude boot (spec in
      scratchpad/o6n-job-lumen.json; welcome erbij kost Lumen een core) en
      dan de mediaketen: MMC, de HEVC-encoder, WebDAV. De VPU had één
      herstelcyclus nodig ("incomplete power state"); waarom.
- [ ] `hopos.codecdemo`, de kernmeting, naast de 85,7 fps van apps/decode.
- [ ] De display-app en HID op de xHCI's; hop-gui als job.
- [ ] cloudflared-lean (6bbcbfa) als job met een echt token: de
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

### Prestaties: waar v3 onder de Go-lat zit (vitals 30-09)

De vier rapporten staan in docs/measurements.md (kolommen v3). Open:

- [ ] **1 ms per switch-oversteek** bij connect en close: rtt app naar de
      kern 1068 tot 1980 µs (Go 156 tot 201), naar Hop 2 ms; een open
      verbinding is snel (Stat 60 µs, ping app naar app 78 µs). De tik:
      `sw(timer=)` loopt mee met elke handshake. Fix dbc522f (de deur van
      de switch om de slaper): QEMU rtt 2238 → 115 µs, pull 6,9 → 62,9
      MB/s; op ijzer (stempel H) Pi 4 rtt 1476 → 133 µs, Radxa 1984 → 365
      µs, O6N 1068 → 53 µs (Go 156 tot 201: nu sneller dan Go). Pull app
      naar app op de Pi 4 (H): 25,9 MB/s (Radxa vóór de deur 6,5; Go op de
      Pi 5 400 tot 542): het datapad app naar app is nog 15x onder Go.
      Agent bezig met de idle-wekken (app-pomp 1 kHz, OS-core 900/s), die
      hier waarschijnlijk ook achter zitten.
- [ ] **Storm door de NAT (hairpin) stokt 1 s per ronde** op de Pi 4 (H):
      p50 2,5 ms, p99 1002 ms, 96 conn/s, zonder `HOPOS_MASQ_SLOT_FULL`.
      Eén SYN per ronde valt (RTO 1 s); ook node naar node (Pi 4 naar Pi 5,
      Radxa naar Pi 4, run 3). Verdachte: leannet's listen-backlog van 8
      (`TCP_BACKLOG` in leannet/src/stack.rs): nu de dial snel is, komen
      200 SYN's sneller binnen dan accept ze haalt, en een SYN op een volle
      backlog valt. Een backlog van 64 in lean (tag) is de lean fix; Go's
      O6N-storm door de NAT haalde 844 conn/s, p99 19,6 ms.
- [ ] **App-opslag O6N** (1 MiB-calls): na de deur (H) schrijven 414, lezen
      85 MB/s (Go 557 tot 625 / 727 tot 797); het leespad is de rest (was
      93, dus de deur hielp lezen niet). Agent bezig (system-API fs-ops,
      hopfs, NVMe-voltooiing).
- [ ] **NAT-tabel vol**: 512 flows per slot (`HOPOS_MASQ_SLOT_FULL`), dan
      valt een SYN en kost 1 s. Oorzaak: een inbound RST liet de flow 300 s
      staan zonder hem gesloten te tellen. Fix a102e51 (RST = gesloten in
      beide richtingen, sluit-TTL en recycler pakken hem); nog te flippen
      en de storm te herhalen.
- [ ] **Idle**: app-core 1000 tot 3000 wekken/s (poll-ronde van de
      app-pomp), OS-core ~900/s (Go ~100, op de interrupt); een stilstaande
      tweecore-app houdt de Pi's op 1500 MHz ("busy slot 2, 544 permille
      idle").
- [ ] vitals: de standaard-rx-URL (cachefly) faalt zonder DNS in de env
      ("CONNECT is not supported"); rx-duren vallen op stappen van 100 ms.
- [ ] De Mac hangt op Wi-Fi en macOS laat netmeter niet op het LAN
      ("Lokaal netwerk"-recht): alle host-getallen zijn Wi-Fi; node naar
      node over de draad is gemeten (Pi 4 44 tot 49 MB/s van de O6N).

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
