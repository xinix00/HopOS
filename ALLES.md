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
| Kern-flip, warm | ✓ | ✗ sterft na de landing (F en H, 3x); de nieuwe kaart (H, met de zwarte doos) ligt in target/ | ✓ (6x, gen 4 op I) | ✓ (5x, gen 3 op I) | ○ | ✓ gen 2 op H (gui-bundel); I geweigerd door de H-kern (bundelpartitie 8 KiB te krap, fix de61b4b zit in de I-stick): koude boot | – (geen CPU_OFF) | – |
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
| Kaart of stick klaar in `target/` | – | ✓ 22:37 (I) | ✓ 22:37 (I) | ✓ 22:38 (I, gepatchte Hop) | ✓ 18:12 | ✓ 22:38 stempel I (kern-fix bundelpartitie, verse Hop, efirng) | ✓ 20:45 (Hop ingebakken) | ✗ donor-FIP |

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

- [x] **SError na de p-state-tune** (01-10): de schrijf naar P-cluster
      +0x48400 (4442f3d) én de CL_PSTATE-schrijf zelf; met
      `hopos.pstate=off` geen enkele SError meer, slaapt 3250/s in plaats
      van 131.000. De tune blijft uit tot hij op dit silicium begrepen is.
- [x] **Doof na DHCP** (01-10, 4be8b60): de tg3 nam geen unicast aan.
      De chipregel in de tik (`HOPOS_NIC_DIAG`) zei het: `rx ucast=0`
      naast `bcast=308` per vijf seconden, geen filter-drops, ring en
      status-blok gezond, en de terugleesregel `mac0=0x0010/0x18000000`:
      MAC_ADDR_0 hield Broadcom's default. De bootcode zet die terug ná
      onze `set_mac` (Rust schreef het adres in new en init, vóór de link;
      Go pas in Init, ná LinkUp). Fix: het adres nog eens aan het eind van
      `link_up`, in alle vier de filters zoals Linux. Bewezen zonder
      vangnet (11:15, D5 via een flip): `mac0=0x1cf6/0x4c54fa90`,
      `rx_mode=0x1000002`, `rx ucast` loopt. Ping, 5555, 8080, 10100, Hop
      leider, de node-stack ziet TCP.
      De "connection refused" van Hop op de system-API was hetzelfde
      filter: applib meldt een time-out als refused.
- [x] **Flippen op de M4 met adoptie** (01-10 12:14, agent, stempels M3 tot
      M7): de flip landde maar adopteerde niet, omdat de config op de M4 ín
      het image zit (venster 0xF000) en een bundel een leeg venster over het
      image legde: de geflipte kern bootte zonder `hopos.pstate=off`, de
      tune gaf de SError, de OS-core-preflight faalde, geen plan, geen
      slots, na twee minuten de guard. Fix: de draaiende kern draagt zijn
      config-venster over in het gestagede beeld (`fwinfo::carry_config`,
      `HOPOS_FLIP_CFG`), en `flip-bundle.sh apple` kan een `CFG=` meebakken
      voor de overgang vanaf een kern zonder die code. Bewijs: twee warme
      flips op rij (gen 1 → 2 → 3), `HOPOS_FLIP_ADOPT 3 of 3`,
      `HOPOS_FLIP_NAT restored=4 of=4`, `HOPOS_HOP_RESUMED`,
      `HOPOS_FLIP_SETTLED`, ~3.100 slaapjes/s, geen SError. Ook:
      `conport::here()` direct na `discover`, anders kwamen de landing, de
      bootregel en de zwarte doos nooit op 5555. De ANS van de oude kern
      geeft de nieuwe geen HELLO; de nieuwe reset het domein en de schijf
      komt op met de hopfs-generatie mee (zonder SError).
- [x] **De p-state-tune zonder SError** (01-10, M5 tot M7): de tune loopt nu
      stap voor stap in `start_interrupts` met na elke schrijf een
      SError-toets (`HOPOS_APPLE_PSTATE_SERROR`). De bron was de schrijf
      naar P-cluster +0x440f8 (het t8122-recept van m1n1; de t8132 kent
      m1n1 niet); het E-cluster-recept is schoon. Zonder die schrijf: E
      5/8 = 2172 MHz, P 6/20 = 2352 MHz, geen SError, zelftest ok,
      settled. Een hangende SError is per core en overleeft een flip; alleen
      een koude reset wist hem (een bundel met `hopos.cages=off` kan niet
      adopteren en laat de guard de M4 binnen twee minuten koud herstarten).
      Het geïnstalleerde image is nog D4b (pstate=off); de tune reist met
      elke flip mee via de config-overdracht.
- [ ] **De M4 app naar app: 52 MB/s** (M7, tune aan; 47 met pstate=off),
      tegen 461 op de Pi 4 en 1500+ op de O6N: niet klokgebonden (2,4x
      hogere E-klok gaf 10%). De pool is op Apple Device gemapt
      (`dram_attr`), dus de ringbelofte staat daar uit en elke kopie uit en
      naar een ring loopt per 8 bytes vluchtig; dat is de eerste verdachte.
- [ ] **Hop en de slots lopen op de M4 uiteen na flips**: de kern nam 3
      bewoners over terwijl Hop `jobs: []` meldde (Hop herstelt zijn staat
      niet: "saved agent state not restored: store i/o failed"), een DELETE
      van bench stopte slot 2, en een vitals-POST faalt daarna met "slot 5:
      port 8090 is taken by slot 4" (`HOP_JOB_FAILED`). De wezenveger
      (`sweep_strays`) draait alleen na een restore.
- [ ] De koude flip werkt er niet (PSCI CPU_OFF zonder EL3); na een
      verhuizing met een rode voorproef spint de oude core.
- [ ] Het diagnose-image van 06:45 (met `self_test` ná de init) bootte
      niet onder iBoot en viel in Recovery; A, D2 en D3 bootten. Oorzaak
      niet gevonden; `self_test` schrijft in de send-RCB en hoort vóór de
      init.
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
      hier waarschijnlijk ook achter zitten. Draadronde 30-09 (bench, drie
      runs): pull in de node O6N (H) 141,5 tot 143,7, Pi 4 (H) 23,6 tot
      26,7, Radxa (I) 19,9, Pi 5 (dev, zonder deur) 6,6 MB/s (Go M4 769).
      **App naar app 01-10: Pi 4 100,6 → 271,8 MB/s** (lean v3.1.3, bench
      met de nieuwe applib, K-kern). De oorzaak zat in leannet, niet in de
      kern: de puller meldde 51200 segmenten van precies 16 KiB voor 800 MB
      (`BENCH_PULL_STATS`, nieuw), één venster per rondreis van ~160 µs:
      de ontvangstring bleef op de vloer van 16 KiB omdat de groei een vol
      segment (MSS 65495 op het slot-LAN) of een volle ring eiste, en een
      snelle lezer de ring leeg hield. v3.1.2 ("segment vult de lokale vrije
      ruimte") hielp niet (100,6 bleef 100,6); v3.1.3 groeit als de zender
      de geadverteerde rand bereikt (`rcv_nxt == adv_edge`), en telt groei en
      weigering (`tcp_rx_grown`, `tcp_rx_grow_refused`). Nu: 15392
      segmenten van gemiddeld 54 KB, de ring groeide vijf keer tot 480 KiB,
      de OS-core is weer idle tijdens de pull (door 2300/s, idle 2100/s) en
      de **puller zelf is de rem** (slot 3 op 83% bezet: checksum, de
      kopie uit de ABI-ring met `dc civac` per regel plus vluchtige
      8-byte-loads, de kopie naar de app, en bench's bytevergelijking).
      Let op: de kern en applib pinden leannet nog op v3.0.0 terwijl de apps
      v3.1.1 hadden (één lock-regel verraadde het); nu overal v3.1.3.
      Ping app naar app over de draad: O6N naar Pi 5 p50 200 tot 227 µs,
      Pi 5 naar O6N 206 tot 238, Radxa (I) naar O6N 287 tot 289, maar de
      **Pi 4 (H) naar O6N 1212 tot 1213 µs** (koud 1,3 ms; in de node 48
      µs): de Pi 4 (GENET gepold, `nic=0` in de tik) wacht op het draadpad
      nog ~1 ms. De Pi 5 op de kaart-kern (zonder deur) wekt koud in
      1239 µs; na de deur koud 51 tot 144 µs.
      **Datapad 01-10 (d476c80, 7599f58, fb04980; stempel X op de Pi 4 en
      de O6N): Pi 4 app naar app 272 → 418 tot 463 MB/s, O6N 259 → 1495
      tot 1711; O6N schrijven 290 tot 305 → 992 tot 1188, lezen 158 tot
      168 → 472 tot 590 (vitals in slot 2) en 373 tot 376 (slot 4).** Vier
      oorzaken, in volgorde van opbrengst: (1) `compiler_builtins` kopieert
      op `+strict-align` per byte bij ongelijke uitlijning en vergelijkt
      per byte, en op het framepad is bijna elke kopie ongelijk (de payload
      op +54, de stroompositie willekeurig): eigen memcpy en memcmp met
      ongealigneerde ldp/stp, alleen op Normal geheugen (dev/src/mem.rs;
      Pi 4 261 → 365, O6N 259 → 545). (2) De ring deed `dc civac` per
      record terwijl beide kanten write-back inner shareable mappen, en de
      civac gooit de regels uit de gedeelde L2 zodat de kopie uit DRAM
      komt: nu een belofte per kant in zijn eigen regel van de ringkop,
      de kern op elk arm64-board behalve Apple (O6N 545 → 1050). (3) Frames
      in plaats in de ring lezen en bouwen (1150 → 1430). (4) Het venster
      per verbinding van een halve ring naar de ring min twee frames (→
      1650 tot 1750). NVMe: het datablok met memcpy en tot 16 opdrachten
      per verzoek achter één doorbell (MDTS 512 KiB: een MiB was twee
      seriële opdrachten). Niet gehouden: de switch met één kopie (Pi 4
      360 tegen 420, trager), een kleinere RX_BATCH, een yield per read.
      **De lat van 2000 MB/s is niet gehaald.** De rem van één stroom is de
      rondgang van zijn venster (de zender wacht op venster, de ontvanger
      wekt ~1 keer per venster, alle cores ~50% idle); twee stromen samen
      halen 2380 MB/s, en zonder de bytevergelijking van bench 1532 tot
      1611: elke kopie minder per byte bij de ontvanger telt. Bij opslag
      loopt alles serieel per call (hopfs ~800 µs + send ~320 µs + de app
      ~600 µs per MiB). Volgende kandidaten: een groter venster (grotere
      frame-ringen, de ABI-staart van 2 MB), een leesweg in plaats in
      leannet, DMA rechtstreeks in de kernbuffer, en voor opslag de echte
      hefboom: een fs-grant naar het voorbeeld van `codec_grant` (NVMe-PRP
      over het RAM van de app, geen TCP). Of de PCIe van de O6N
      I/O-coherent is (`_CCA` in de DSDT) bepaalt of de dc civac/cvac per
      MiB weg mag. Lean hoefde niet mee: de checksums op de slot-link
      stonden al uit. Les van de Radxa (01-10, stempel AA): de ringbelofte
      van de kern moet volgen wat het board werkelijk mapt; de Radxa-kern
      mapt alles boven 0x0880_0000 Device (de tamago-keuze), en met de
      belofte las hij langs de cache van de app heen (corrupte ringen in
      slot 3). Nu Maintained op Apple én de Radxa (AB); app naar app daar
      29,8 MB/s, als vóór de belofte.
- [x] **De kernheap lekte onder gemengde allocaties** (01-10, crate `heap`):
      de kern had een bump-allocator die alleen het laatste blok terugnam;
      metadata-, netwerk- en I/O-allocaties door elkaar lieten vrijgegeven
      geheugen bezet (O6N tijdens Lumen: `OP_SYNC out of memory 483328
      bytes`, daarna viel ook 5555 uit). Nu dezelfde allocator met vrije
      lijsten en grenslabels als de apps, in een eigen crate; alleen de
      OS-core alloceert (`KernelCore::id()` is 0). Door een andere sessie
      voor de prestaties geschreven (Derek: drie keer zo snel); docs/heap.md.
      A/B op de O6N (01-10 12:25, vitals in slot 2, warme flips, drie tot
      vier runs): met heap schrijven 708, lezen 489 tot 491, write_4k ~50,
      floor_p50 59 tot 60 µs; zonder heap 690 tot 712, 491 tot 494, 52 tot
      53, 55 tot 64. Gelijk: de heap kost het I/O-pad niets. App naar app
      met heap 1504 / 1582 / 1514 MB/s (X: 1495 tot 1711).
- [ ] **O6N schrijven: ~700 MB/s in de middag tegen 800 tot 1188 in de
      ochtend**, met dezelfde kerncode (de heap maakt geen verschil, zie
      boven), vitals steeds in slot 2. Verschil in de meting: de ochtend
      was een koude boot op de S/X-kern, de middag warme flips (gen 4 tot
      6) na de Lumen-sessie van een andere agent. Nog uitzoeken: koude boot
      op main en opnieuw meten; de NVMe-staat (de rogue-sessie schreef er
      Lumen-beelden heen) en de plaatsing van de core tellen mee.
- [x] **Het flipvenster van de O6N was de lopende kern: 2204 KiB.**
      `image::limit()` op UEFI was het einde van het lopende beeld, en een
      bundel is pas welkom als zijn beeld tot het einde van .stack daarin
      past ("length 2269184 exceeds 2256896"). HEAD 4442f3d was al 2208
      KiB en paste niet; fb04980 past precies (`#[inline(never)]` op de
      mem-symbolen: LTO plakte de lus in elke aanroeper, +12 KiB; de IoPace
      van de system-API eruit, de versie met IoPace staat in de scratchpad).
      Fix 50989f6: de grens is de allocatie van de koude boot uit de
      feitenpagina (`facts::image_window`, de SizeOfImage van de stick-kern,
      overleeft elke flip), en hopos/efi.ld legt 2 MiB speling in
      SizeOfImage zonder sectie of segment (de firmware geeft ze mee, het
      platte beeld van een bundel telt ze niet). Pas werkzaam na een koude
      boot met zo'n kern op de stick. Bewezen 01-10 08:50: de O6N koud op
      de Y-kern (50989f6: "image 0x7fa3c5000+0x427000"), daarna de
      Z-bundel geflipt in tien seconden (gen 2, drie bewoners
      overgenomen, Hop niet herstart), en de geflipte kern meldt hetzelfde
      venster uit de feitenpagina. Ook: `hopos.nvmebench=1` moet in de
      cfg op de stick, een flipbundel draagt geen cfg.
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
- [ ] **Idle**: fix 262ea1f (uitstelbare timers, WFE-lus, tweede core
      telt mee). Op ijzer: app-core Pi 4 1011 → 29 wekken/s, O6N 3300 → 52
      (4 cores, nieuwe applib op de H-kern). Met cf21ac5 (CtrlIdle na elke
      WFE) klokt de Pi 4 met een stille tweecore-vitals terug: `dvfs:
      clock 600 MHz (quiet), busy none`, 21,6 wekken/s. De OS-core op de
      Pi 4 telt nog ~850 `sleeps=`/s: de event-stream van 1,2 ms in de
      WFE-lus (geen executor-rondes meer, wel de teller). Klaar op de Pi 4
      en de O6N (app-kant); de Radxa en de Pi 5 na hun kaart.
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

- [x] **Een volle node zei "slot 5 out of range 1..4"** in plaats van
      "full" (Pi 4, 01-10): de system-API kreeg het ABI-plafond (128) als
      slotgrens en zocht door tot slot 5; Hop las er geen capaciteitstekort
      in en vroeg elke paar seconden opnieuw, 128 statuscalls per vraag door
      de lifecycle-mailbox. Fix 1c86bec: het slotaantal van het board
      (`slot_count`), "table full (4 entries)" is voor Hop `NoCapacity`.
- [x] **Wezen na een herstart van Hop**: twee uitgemeten benches in slot 3
      en 4 van de Pi 4 die Hop niet kende (de jobs waren rond zijn herstart
      verwijderd) hielden elke plaatsing en elke flip tegen tot een koude
      flip. Hop 196b1d3: na `restore` stopt `sweep_strays` elke bewoner
      boven slot 1 die niet in de bewaarde staat staat (`HOP_STRAY_STOPPED`).
- [x] **"actor mailbox full" bij een flip** was de weigering van dezelfde
      bundel (`refuse("same bundle", Busy)`); heet nu "version X, want X"
      (1c86bec). Een koude flip naar dezelfde bundel blijft geweigerd: wie
      alles wil herstarten, bouwt een nieuwe stempel.
- [ ] **O6N: slot 3 sterft stil** (01-10, H-kern gen 2): elke app in slot 3
      (vitals 30-09, pull 01-10) drukt `HOPOS_APP_MMU` en niets meer; de
      kern meldt geen fault, hopfs zegt "slot 3 stopped", Hop plaatst
      opnieuw, elke 10 s. Slot 2 (part 0x7f5600000) werkt; slot 3 ligt op
      part 0xfbe00000+0x4000000, ctrl 0xffc00000, vlak onder 4 GB. Verdacht:
      die regio is in de UEFI-kaart geen gewoon RAM (of ligt onder een
      PCIe/MMIO-venster). Te doen bij de koude boot met de J-stick: de
      pool-regel (`slots: pool N MB in K regions`) en de UEFI-geheugenkaart
      lezen; tot dan geen app naar app op de O6N (bench serve pakt 2, pull 3).
      **Koude boot met de K-stick (01-10 05:47): niet meer gezien.** Pool
      32218 MB in 11 regio's (grootste 28462), slot 3 leeft (pull 259 MB/s
      erin), slot 4 (vitals) op part 0xc3e00000, ook onder 4 GB. Het was
      dus de H-kern na de "unplaceable"-nacht, niet de regio. Blijft: de
      kern drukt zijn regio's niet af; één regel per regio bij de boot zou
      zo'n raadsel in één oogopslag beslechten.
- [ ] **O6N op de K-kern (koude boot 01-10)**: efi-rng bewezen
      (`HOPOS_RNG_EFI_UP`, de slots zaaien uit efi-rng), app naar app 259
      MB/s (was 142), vitals disk schrijven 290 tot 305, lezen 158 tot 168
      MB/s (was 85 tot 88, Go 727): het write-back-datablok telt, maar het
      leespad kern naar app (1 MiB-calls, vloer 76 µs) heeft een tweede rem
      die niet het venster van de app is (vitals op lean v3.1.3 leest even
      snel als op v3.1.2). Verdacht: de kopieën en checksums per MiB over
      de system-verbinding (NVMe naar BLK_DATA, poll_op naar de kern, de
      kern naar de slot-ring met `dc cvac` per regel, leannet in de app).
      rtt app naar kern 94 µs (H: 51 tot 53).
- [ ] **Hop: een rolling update met een vaste poort op één node slaagt
      nooit** (O6N 01-10): een `POST /v1/jobs` voor een job die Hop uit zijn
      bewaarde staat had hersteld, is een update (rolling); de nieuwe taak
      wil :80 terwijl de oude hem houdt ("port 80 is taken by slot 2"), en
      Hop probeert elke paar seconden opnieuw. Go had hetzelfde model; de
      eerlijke fix is in Hop: bij een poortbotsing op dezelfde node de oude
      eerst stoppen (recreate), of de botsing als "wacht" tellen in plaats
      van als nieuwe start. Omweg: DELETE en opnieuw POSTen.
- [ ] **De puller als rem na v3.1.3** (Pi 4 272 MB/s, slot 3 op 83%):
      kandidaten in volgorde van gewicht: de kopie uit de ABI-ring (per
      record `dc civac` per 64 B plus vluchtige 8-byte-loads: op ARM met
      beide kanten write-back is het cache-onderhoud loos werk, Go had
      `Push`/`Pull` daar als no-op), de softwarechecksum, de tweede kopie
      naar de app. Go M4 769 is een andere machine; de O6N na de koude boot
      is de eerlijke vergelijking (was 142).

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
