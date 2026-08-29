# TODO

## tamago-PR: RAM boven 512GB (Apple silicon) — fork in gebruik, upstream indienen

**Waarom:** Apple legt DRAM op 1TiB (0x100_0000_0000). tamago's arm64 `InitMMU`
bouwt een vlakke 39-bit-wereld (één L1-tabel = 512GB, T0SZ=25, IPS 40-bit) —
daarbuiten kan de runtime de MMU niet eens aanzetten. Gepatcht in de fork
`~/Git/tamago`, branch `hopos-highram` (commit 8f88bdb, ~80 regels): ligt
RamStart boven 512GB, dan een L0-root (+0xA000) met een lage-MMIO-L1 (+0x9000,
inclusief de nullpointer-val) en de bestaande L1 (+0x4000) voor de 512GB-regio
met het RAM; TCR naar 48-bit VA, IPS uit `ID_AA64MMFR0.PARange`. Het vlakke pad
is byte-gelijk (rk3566-agent bouwt er ongewijzigd tegen).

**Nu:** alleen `image/apple-m4.sh` bouwt ertegen, via `image/apple/go.work`
(replace naar `../../../tamago`); alle andere builds blijven GOWORK=off op de
upstream-tag in `metal/go.mod`. Besluit Derek 28-08: fork is prima, PR volgt.

- [ ] PR indienen bij usbarmory/tamago (stijl: docs/upstream-pr-stijl.md — kort,
      geen em-dashes; het commitbericht in de fork is al Engels en to the point)
- [ ] na merge/tag: `metal/go.mod` bumpen en `image/apple/go.work` weghalen;
      wordt hij afgewezen: xinix00/tamago-fork mét tag (geen pad-replace), zoals
      de netstack destijds
- [ ] tegelijk meenemen in de PR-overweging: `TCR_EPD1` (upstream master zet hem
      al) zodat een verdwaald hoog adres een nette translation fault geeft
- [ ] `tools/test.sh`: de apple-smaak in de gate — kan pas zonder go.work zodra
      de fork een tag heeft (nu bouwt alleen `image/apple-m4.sh` ertegen)

## Apple M4 — volgende treden (stand 29-08, zie docs/archief/apple-m4.md)

Doel CPU → idling → netwerk is gehaald: de node boot HopOS, krijgt DHCP, zet
zijn klok via SNTP, haalt de welcome-app over HTTPS van GitHub en draait hem in
de kooi (slot 1, 64MB partitie). Gate groen, niets gecommit.

- [x] agent booten, NumSlots=9, stage-2 met VM=1 op ijzer bewezen
- [x] idling: 3.277.000 → 954 wekmomenten/s bij 100% slaap (`idle.UseTimerSleep`)
- [x] NIC-pad: PERST-GPIO + LTSSM, bridge-venster, BAR's, DART-bypass, RID2SID
- [x] `driver/nic/tg3`: reset, MAC uit de ADT, MDIO/PHY, 1000 Mb/s, ringen, DMA
      beide richtingen, `ProbeNIC` + DHCP-lease in `board/apple/hop/net.go`
- [x] **NVMe lezen.** `driver/rtkit` (de coprocessor-mailbox) + `driver/nvme/apple.go`
      (het ANS-pad in dezelfde controller). Leest de GPT van de interne SSD en
      geeft de coprocessor daarna netjes terug.
- [ ] **NVMe: `pmgr_reset`.** Valt de coprocessor om of gaat de node hard uit,
      dan blijft de ANS dicht tot iets zijn power-domein reset. Nu is de enige
      weg terug een m1n1-sessie (`nvme_init` + `nvme_shutdown`). Referentie:
      m1n1 src/pmgr.c.
- [ ] **NVMe schrijven.** De interne SSD is geen gewone PCIe-NVMe maar ANS2
      achter RTKit: `storage: nvme: no device on bus 0` is de juiste uitkomst,
      geen fout. Referentie is m1n1 (`src/nvme.c` 588, `rtkit.c` 725, `dart.c`
      765, `sart.c` 325, `asc.c` 131 regels C) — en die kan alleen LEZEN;
      schrijven is dezelfde submit-weg met de NVMe-write-opdracht. Eerst de
      vraag beantwoorden wáár we mogen schrijven — en die is inmiddels gemeten:
      de schijf zit vol (iBootSystemContainer 500MB + APFS 471GB + RecoveryOS
      5GB, nul vrij). Er moet dus eerst vanuit macOS ruimte gemaakt worden
      (`diskutil apfs resizeContainer`); pas dan is er een LBA-bereik van ons.
- [ ] platform-config in het param-blok (hopos.node/cluster/apikey), serial uit
      de ADT; watchdog (0x3882b0000, reset-scope meten vóór wapenen)
- [ ] productie-boot zonder laptop: relocatie-stub + image als payload achter
      m1n1.bin (kmutil); tot die tijd is de loader de "U-Boot"
- [ ] console-flakiness: Chrome (WebUSB) houdt het Debug-USB-device vast → geen
      kis-poorten; Chrome sluiten of WebUSB-toestemming intrekken
- [ ] raadsel: een EL1-stage-1-fault landde op EL2 (EC 0x24, VM=0) — begrijpen
      vóór apps in kooien draaien (raakt de fault-routing van de switch)
- [ ] inkomend TCP nog niet getoetst: de laptop zit op Wi-Fi en de mini aan de
      draad, en pakketten van de laptop bereiken de chip niet eens
      (`rx ucast=0`). Toetsen vanaf een bedrade host op hetzelfde segment.

## Die-temp stond stil: `auto_cycle` ontbrak (11-08 GEFIXT, ongecommit)

**Symptoom** (Dereks observatie): `board: die temp 40.5C`, tien keer achter elkaar
exact hetzelfde terwijl de node downloadde. Per boot wél een andere waarde
(52,1 / 48,9 / 40,5), binnen een boot nooit beweging. Eén LSB is 0,35°C, dus
stilstand op die schaal is geen ruisonderdrukking maar een bevroren register.

**Oorzaak:** `metal/board/licheerv/temp.go` schreef `reg_tempsen_auto_cycle`
(0x030E0064, bits [23:0]) niet. Zonder periode doet de CV181x-TEMPSEN één
conversie bij `enable` en staat het resultaatregister daarna stil.

**Referentie-eerst nagekeken** tegen `linux_5.10/drivers/thermal/cv181x_thermal.c`
uit `sipeed/LicheeRV-Nano-Build` (de vendor-tree van precies dit bordje). Al het
andere klopte al: chopsel 3, accsel 2, cyc_clkdiv 0x31, `sel=0x1`, `en=1` en de
formule `result*1000*716/2048-273000` zijn identiek. Alleen die ene schrijving
ontbrak.

**Fix:** `write32(tempAuto, read32(tempAuto)&^0xffffff|0x100000)` vóór de enable,
prediv ongemoeid zoals de vendor. Op de 0,5MHz cycle-clock (T=2µs) is dat een
nieuwe meting per ~2s. Bouwt riscv64 + gate groen.

- [ ] **op ijzer verifiëren**: de waarde moet nu bewegen tussen twee prints, en
      onder last omhoog kruipen. Dat is ook de reden dat dit telt: de
      thermiek-metingen van gisteren (37-39°C tijdens netmeter, 48,9°C tijdens de
      log-storm) waren dus allemaal één meting per boot, niet een verloop.

## BESLUIT 11-08: eigen netstack in lean — de lneto-fix-marathon is gestopt

Aanleiding: de review hieronder (29 punten) plus de optelsom. Wij gebruiken
~13k regels lneto+go-net via twee forks, terwijl het moeilijke deel (TCP
sluiten, retransmit, de pool) daar na jaren nog niet af is en onze fixes er
alleen via PR-diplomatie of de refork-dans in komen. Besluit (Derek): stoppen
met repareren, zelf bouwen in `xinix00/lean` ("leannet"), met gVisor én lneto
als playbook. Eisen: mag performen (128-core Altra moet hard kunnen
downloaden), en het geheugenmodel is één knop — "hier heb je 2MB (klein board)
of 40MB (server), deel het zelf in" — dus budget-gestuurd, groeien bij gebruik,
nooit vooraf claimen per listener/dial.

- [x] ontwerp-dossier + testplan (docs/leannet-ontwerp.md; de 29 hieronder
      zíjn het testplan)
- [x] **leannet GEBOUWD 11-08** (~4,3k regels in lean/leannet, ongecommit):
      frames, budget-pot, groeiende rings, TCP-machine (FIN-familie als tests,
      WS, zero-window-probe), ARP (dedup=map-sleutel, poisoning-regel, passief
      leren zonder MAC-wissel), ICMP-echo, UDP, stack-laag (TX-pomp slaapt tot
      vroegste deadline, geen tick), socket-laag (SocketFunc, echte deadlines,
      efemeer ≥49152). Suite groen incl. -race; tamago riscv64+arm64
- [x] **A/B-naad GEBOUWD 11-08** (hop-os, ongecommit): `-tags leannet` in
      hopnet (stack_lneto.go/stack_leannet.go), appnet (up_leannet.go) en
      netmeter; budget = raam/8 (kern) resp. partitie/8 (app), niemand kiest
      nog buffer-getallen. Gate groen, beide kanten bouwen op beide bogen
- [x] **QEMU-smoke 11-08 GROEN op leannet**: agent+leader 200, SNTP zet de
      klok, 200× churn schoon, **8 vastgehouden verbindingen + verse = 200**
      (de 5555-dood kan niet meer bestaan), demo-keten álle 20 markers OK
      (slots/isolatie/swarm/outbound/poorten), kern 7,3MB (−0,5MB)
- [x] **restant 29-scenario's afgerond 11-08 laat**: alle 29 verantwoord (tabel
      hieronder), plus een coverage-ronde 84,2% → 91,0% met de
      gVisor-klassiekers (sequence-wraparound, RFC 5961 blind-RST en
      mid-connection SYN, venster-krimp, tiny-MSS) en een rommeltest per laag.
      Die ronde vond twee echte bugs: een verbinding gaf nooit op (SYN-flood
      hield zijn floor eeuwig) en een backlog-weigering stuurde stil geen RST
- [x] **lneto ERUIT 12-08** (Derek): geen build-tags meer, leannet is de enige
      stack. `metal/net/netdev` is het nieuwe device-contract (twee methodes,
      importeert niets) zodat boards/drivers/switch aan geen enkele stack
      hangen; `nodefaultstack` uit alle 11 buildscripts; go-net, lneto, gvisor
      en 3 transitieve deps uit go.mod; indeling.md + importcheck bijgewerkt.
      Gate groen, QEMU-smoke groen (agent+leader 200, SNTP, 100× churn)
- [ ] **netmeter-A/B**: doorvoercijfers naast de lneto-getallen (lat: 8,84 MB/s
      LicheeRV-referentie) + de OOM-reproductie (ramSize=64MB + SSE) die op
      leannet moet overleven. Daarna: kan de GOGC-25-pleister in cpu/memlimit
      terug naar Go's ~10%-richtlijn?
- [x] **op ijzer LicheeRV 12-08 — de node-test is GROEN**: app-wissel
      welcome→vitals→welcome via GitHub-TLS in ~9s (DNS+TLS+masquerade door
      leannet), image-stream 1,8s, vitals-storm 375 conn/s (p99 30ms),
      **SSE-soak 7 min = 0 gefaalde polls** (oude stack: dood na 151-250s),
      die-temp beweegt (46→55°C onder last — auto_cycle bewezen), 0 panics.
      App-pad RX 4,79 MB/s (app→ringen→switch→masq; níét de node-pad-referentie)
- [ ] **op ijzer, rest**: netmeter-kaart voor het kale node-pad (vs 8,84 MB/s)
      + Radxa
- [x] **watchdog-beleid ÉÉN keer voor alle boards (12-08, Dereks vraag
      "waarom is dit niet gelijk bij elk board?!")**: cmd/hopos/watchdog.go
      draagt HET beleid, per board resten twee hardware-regels (Arm/Pet +
      cadans). Er waren vier uiteengegroeide kopieën, met twee filosofieën:
      Pi/UEFI wapenden vroeg maar aaiden BLIND (doofheid onzichtbaar),
      LicheeRV/Radxa aaiden op bewijs maar wapenden laat. Het ene beleid
      combineert beide: boot-guard (vroeg wapenen, blind aaien tot het eerste
      levensteken — bevroren boot reset, netloze bring-up blijft staan),
      daarna alleen aaien op bewezen leven (02-08-les, nu óók op de Pi's).
      hopos.wd=off overal dezelfde knop. Alle 5 boards bouwen, gate groen,
      QEMU toont de eerlijke nil-regel ("wires no hardware watchdog")
- [x] **watchdog-canary GEFIXT 12-08** (was: twee boots stil NIET gewapend):
      de probe dialt nu het EIGEN externe IP:8080 i.p.v. 10.100.0.1 — dat
      adres is vanuit de kern onbezorgbaar (GwFromHost is per ontwerp slot≥1).
      De voor de hand liggende hairpin-fix is bewust afgewezen: die routeert
      via de gáteway, en een watchdog aan de gateway = reboot-loop bij een
      dode router op een kerngezonde node. Het eigen-IP-pad is exact de route
      waarmee de leader vanochtend jobs dispatchte (09:17, zelfde poort), dus
      op ijzer al bewezen. Beide boards (licheerv + rk3566); de Radxa-wachtlus
      kreeg ook de luide "NOT armed"-melding die de LicheeRV al had.
      VERIFICATIE volgende boot: "watchdog: ... armed" op de console
- [ ] v1-beperking bewust: géén congestion control — waarom en waar cwnd later
      inhaakt staat in `~/Git/lean/leannet/DESIGN.md`
- [ ] lean taggen (v0.2.0) → dev-replace uit metal/go.mod, require bumpen.
      NIET committen zolang die replace er staat
- [ ] PR's #178-#183 + go-net#5/#6 blijven open als bijdrage aan upstream —
      geen onderhoudsplicht meer; tools/refork-netstack.sh heeft die status
      als notitie bovenaan. Ronde 2 geparkeerd

De 29 bevindingen blijven hieronder staan als **testplan voor leannet**: elk
punt wordt daar een regressietest vanaf dag één. Volledig rapport met
faalscenario's: `~/Git/lneto/BEVINDINGEN.md`.

**Dekkingsstand in leannet, 11-08 laat — alle 29 verantwoord.** Drie
categorieën: TEST = regressietest in lean/leannet, CONSTRUCTIE = de
faalvorm kan in dit ontwerp niet bestaan (met de reden), N.V.T. = het
mechanisme bestaat niet in leannet.

🔴 Hoog:

- [x] 1 verloren kale FIN — TEST `TestTCPLostBareFIN` (SYN/FIN zitten in de
      sequence-boekhouding; retransmit = her-lezing)
- [x] 2 valse FIN-WAIT-2 — TEST `TestTCPRTOInFinWait1KeepsFIN` +
      `TestTCPPartialAckHoldsFinWait1` (overgang alleen op ack == finSeq+1)
- [x] 3 LAST-ACK op élke ACK — TEST `TestTCPLastAckIgnoresStaleACK`
- [x] 4 fout als verbinding — TEST `TestStackSocketShapes` (mislukte dial =
      (nil, err); getypeerde returns tot aan de rand)
- [x] 5 t.Logf na testeinde — CONSTRUCTIE: geen enkele testgoroutine logt;
      resultaat loopt via kanalen (srvDone-patroon)

🟠 Midden:

- [x] 6 reaping alleen bij idle listener — TEST `TestTCPHalfOpenGivesUp` +
      `TestTCPDeadPeerGivesUp` + `TestTCPZeroWindowPeerStaysAlive`: de
      opgeef-grens zit ín de machine (retries, reset door élke geldige ACK),
      dus hij werkt onafhankelijk van Accept-gedrag; geen aparte reaper
- [x] 7 tweede Reset panict — N.V.T.: geen Reset-API, geen vaste
      passive-regio (ARP-tabel is een map)
- [x] 8 ARP-poisoning — TEST (arp_test.go): reply alleen aan ons gericht
      lost op; gratuitous ververst hoogstens; passief leren wijzigt nooit
      een MAC
- [x] 9 RTO op wandklok — CONSTRUCTIE: stack-klok = time.Since(t0)
      (monotoon); machine krijgt now geïnjecteerd, alle tests draaien erop
- [x] 10 dial-pad dupliceert config — CONSTRUCTIE: één newConnLocked voor
      dial én accept, dezelfde pot/maxBuf/klok
- [x] 11 CacheRemove zonder mutex — CONSTRUCTIE: één stack-mutex over alle
      machine-staat, -race-run groen
- [x] 12 dubbele ARP-entries — TEST (arp_test.go dedup): map-sleutel ís de
      dedup; NDP bestaat niet (geen IPv6)
- [x] 13 data voorbij eigen FIN — TEST `TestTCPWriteAfterClose` (finSeq
      klinkt vast bij close, write erna = ErrClosed)
- [x] 14 poortbotsing zonder re-pick — TEST `TestStackEphemeralSkipsOccupied`
      (sequentieel + levende poorten overslaan)

🟡 Laag:

- [x] 15 fast retransmit dood in sluitstaten — TEST
      `TestTCPFastRetransmitInFinWait1` (dup-ACK-telling kent geen staat-gate)
- [x] 16 DHCP-lease weggegooid — N.V.T. voor leannet (DHCP = leandhcp,
      buiten de stack); klasse-les blijft voor leandhcp staan
- [x] 17 accept spint pool-scan — TEST `TestStackIdleCostsNothing`: Accept
      blokkeert op een kanaal, pomp slaapt zonder deadline bij een idle stack
- [x] 18 stale WS na RST in SYN-RCVD — TEST `TestTCPRSTKillsEmbryo`: RST
      sloopt het embryo, een nieuwe SYN krijgt een verse machine
- [x] 19 WS-optie valt weg — CONSTRUCTIE: SYN-opties hebben altijd hun 8
      bytes (SYN draagt nooit payload); wsOn alleen bij wederzijds aanbod
- [x] 20 callback-vlag nooit gewist — CONSTRUCTIE: poll-model, er bestaan
      geen callbacks (gedocumenteerd in arp.go)
- [x] 21 seed buiten subnet werkt half — TEST `TestStackSeedNeighborSubnetRule`
      (luid geweigerd)
- [x] 22 selfdial-log-na-einde — CONSTRUCTIE: zie 5
- [x] 23 handgerolde itoa — N.V.T.: bestaat hier niet
- [x] 24 teststeiger 4× — CONSTRUCTIE: één tcpWire (machine) + één
      newStackPair (integratie), mangle via drop-functies
- [x] 25 dubbele ring-rekenkunde — CONSTRUCTIE: één ring-implementatie,
      leeg-detectie op één plek, uitputtend getest (TestRingExhaustive)
- [x] 26/27/28/29 dode code — N.V.T.: de betreffende constructies bestaan
      niet in leannet

Bonus-regressies zonder lneto-nummer: budget-hygiëne na close (echo-test),
pot-leeg = luide RST, PassivePeers-equivalent (server nul ARP-queries),
deadline = échte net.Error-timeout, ICMP-echo, 5555-klasse op QEMU (8
vastgehouden verbindingen + verse = 200).

## leantls gemeten (12-08): 0,40 MB in ketenmodus — maar net/http kost 3,2 MB

Dereks vraag: wat levert leantls ons op? Gemeten met ÉÉN main per variant,
riscv64, licheerv-board als vloer, `-w -T 0x84010000` (scratchpad
`tlsmeasure/`). Alle getallen uit onze eigen boom:

| variant | MB | wat |
|---|---|---|
| v0 | 2,091 | vloer: board + fmt + `net.Dial` |
| v8 | **2,384** | leanhttp (client+server) + ed25519-signatuur, GEEN TLS |
| v4 | 2,475 | leantls GEPIND + handgeschreven HTTP |
| v3 | 3,635 | leantls + x509verify + CA-bundel (ketenmodus) |
| v2 | 4,298 | crypto/tls + handgeschreven HTTP |
| v5 | 5,367 | net/http-server + leantls-keten-client |
| v7 | 5,619 | net/http + plain http + ed25519 (dus zónder https!) |
| v6 | **5,768** | wat de kern VANDAAG doet: net/http + crypto/tls + roots |

**De vergelijking die telt is v6 → v3: 2,13 MB** (Dereks punt, en hij is juist).
v3 kan functioneel álles wat de kern vandaag doet — https naar github.com met
echte certificaatvalidatie — en is 2,13 MB lichter. Mijn eerste conclusie
("0,40 MB") mat v6 → v5, waarin ik net/http liet staan; dat was een aanname,
geen eis.

Die 2,13 MB valt in twee stukken, en dat is de eigenlijke keuze:

| stap | winst | risico |
|---|---|---|
| net/http → leanhttp (crypto/tls houden) = v6→v2 | **1,47 MB** | geen eigen crypto; stdlib-TLS blijft |
| daarna crypto/tls → leantls-keten = v2→v3 | **0,66 MB** | eigen TLS-implementatie in het artifact-pad |

**Waarom net/http zo zwaar is: het linkt crypto/tls onvoorwaardelijk.** v7 laat
dat zien — gooi https er helemaal uit en verifieer alleen een ed25519-signatuur,
en de binary zakt maar 0,15 MB (5,768 → 5,619). Zolang de agent-API op net/http
staat is TLS gratis aanwezig en kan geen enkele TLS-keuze iets opleveren. Dat is
ook waarom de leanhttp-stap eerst moet: hij maakt de TLS-keuze pas betekenisvol.

**En v8 (2,384, signed HTTP zonder TLS) is NIET haalbaar met GitHub als bron.**
Gemeten 12-08: `http://github.com/...` geeft 301 naar https, en de
release-asset-URL erachter draagt `spr=https` in zijn eigen signature. Plain
HTTP kan dus alleen vanaf een bron die wij zelf serveren. Daarmee is v3 de
laagst haalbare configuratie voor de huidige artifact-bron.

**Haalbaarheid van de leanhttp-stap** (nagekeken 12-08 in hop
v0.20.13-testing.4): `internal/api` + `pkg/httputil` gebruiken `http.Request` en
`http.ResponseWriter` (±20×), `http.NewServeMux`, `http.Server`,
`http.HandlerFunc` en `http.Flusher` voor de SSE-stroom. leanhttp's
ResponseWriter heeft `Flush` én `Hijack` IN het contract (geen optionele
type-assertie), en `Request.Done()` voor streamende handlers — dat past strakker
dan wat er nu staat. Wat ontbreekt is de mux: leanhttp routeert niet, dat doet de
handler op `r.Path`. Mechanisch werk, maar het raakt de vitale agent/leader-API.

**Waar leantls dan wél voor is:** gepind kost TLS 0,38 MB (v0→v4) waar
crypto/tls 2,2 MB kost (v0→v2) — factor 5,8. In een wereld zonder net/http is
dat de enige manier om versleuteld met je eigen leader te praten zonder een
kwart van je vrije heap op een 64MB-node op te geven. Dus: bewaren, niet nu
inzetten, en pas inzetten wanneer er een eigen-server-pad is
(hoplockserver-over-netwerk, federatie agent↔leader over WAN).

- [ ] besluit: net/http → leanhttp in hop's API-laag (1,47 MB, geen eigen
      crypto). Vergt de mux zelf + ±20 handler-signaturen in de hop-module;
      raakt de vitale agent/leader-API, dus stap voor stap met de QEMU-gate en
      een ijzer-verificatie erachter
- [ ] daarna: crypto/tls → leantls-ketenmodus in het artifact-pad (+0,66 MB).
      Alleen zinvol ná de leanhttp-stap — vóór die stap levert het 0,40 MB
- [ ] artifact-signatuur op de node verifiëren: de release-keten ondertekent al
      (`ssh-keygen -Y sign` over SHA256SUMS, `tools/release_key.pub` in git),
      maar geen enkele regel in `metal/` controleert dat. Dat is een
      veiligheidswinst los van de bytes: je vertrouwt dan de bytes i.p.v. de
      pijp, en een gecompromitteerde CA of CDN kan je geen image meer opdringen

## Upstream-PR's netstack: in de gaten houden tot merge

Ingediend 09-08 (details en exit-checklist: `docs/netstack-upstream.md`):

- [ ] [soypat/lneto#178](https://github.com/soypat/lneto/pull/178) — deadline-gedreven waits
- [ ] [soypat/lneto#179](https://github.com/soypat/lneto/pull/179) — window scaling (RFC 7323)
- [ ] [soypat/lneto#180](https://github.com/soypat/lneto/pull/180) — sequentiële efemere poorten
- [ ] [usbarmory/go-net#5](https://github.com/usbarmory/go-net/pull/5) — `nodefaultstack`-tag
- [ ] [soypat/lneto#181](https://github.com/soypat/lneto/pull/181) — ring-schrijfpositie (stille corruptie)
- [ ] [soypat/lneto#182](https://github.com/soypat/lneto/pull/182) — retransmissie na lokale close
- [ ] [soypat/lneto#183](https://github.com/soypat/lneto/pull/183) — retransmissie-timer aansluiten

Snel checken: `gh pr status --repo soypat/lneto` (en idem voor go-net).
Reviewvragen kunnen komen; de onderbouwing per PR staat in
`~/Git/netstack-prs.md`.

**Nog te PR'en — go-net, de UDP-fix (NIEUW 11-08, en de zwaarste van de drie
go-net-punten):**

- [ ] go-net `MaxActiveUDPPorts` configureerbaar + default 4 — stond hardcoded
      op 0 met "Unsupported as of yet", terwijl lneto UDP al lang kan
      (`x/xnet`'s Socket doet udp/udp4/udp6, `StackAsync.DialUDP`,
      `RegisterListenerUDP`). Met nul slots faalt élke UDP-socket met
      `ErrExhausted`, dus geen DNS en geen SNTP — en een node zonder klok
      verifieert geen enkel certificaat. Het zichtbare symptoom is dus
      `x509: certificate is not yet valid` op élke https-download, vier lagen
      van de oorzaak vandaan. Code klaar als hopos-commit **d3827f8**
      (fork-tag `v0.1.1-hopos.1`), bewezen: SNTP zet de klok, artifact van
      GitHub gestreamd, app HTTP 200

**Nog te PR'en — ronde 2** (klaar om te vuren, afgemaakt 09-08 avond; wacht
op de reacties op ronde 1; concept-teksten in `~/Git/netstack-prs.md`, elk
standalone groen vanaf upstream-main met een rood-bewezen test):

- [ ] lneto `arp-resolve-all-pending` — ARP-reply lost álle wachtende
      cache-entries op
- [ ] lneto `listener-pool-maintenance` — CheckTimeouts aangedreven uit de
      accept-lus (half-open-flood maakte een listener voorgoed doof)
- [ ] lneto `seed-neighbor` — SeedNeighbor: statische buren, nul ARP
- [ ] go-net `listener-pool-size` — pools op MaxListenerConns (16MB/listener-bug)
- [ ] go-net `passive-peers` — PassivePeers aan (listener-replies naar peer-MAC)
- [ ] go-net statische gw-MAC + SeedNeighbor-passthrough — **GEBLOKKEERD**
      tot lneto SeedNeighbor gemerged én getagd heeft (compileert eerder niet;
      code staat klaar als hopos-commit f191782)

**Mogelijke bijdragen later — geen migratieblokkers.** Dit zijn functies die
HopOS zelf al heeft en voor zijn concrete boot-/hardwarepad nodig heeft. Onze
implementatie blijft de default zolang lneto niet aantoonbaar minstens dezelfde
werking biedt op onze boards; upstreamen is hier een bijdrage, geen reden om
werkende HopOS-code te schrappen.

- [ ] lneto DHCPv4 renewal/rebinding — `StateRenewing` en `StateRebinding`
      bestaan, maar de client doorloopt alleen INIT→SELECTING→REQUESTING→BOUND;
      onze `leandhcp.Renew`/`KeepAlive` blijft dus leidend
- [ ] lneto DHCPv4 broadcast-flag — configureerbare BOOTP broadcast-bit voor
      DORA vóór een unicastfilter/IP-stack betrouwbaar actief is, met een test
      op het uitgezonden frame; onze raw-NIC bootclient zet deze expliciet
- [ ] lneto PHY 1000BASE-T autonegotiation — GBCR/GBSR meenemen in advertise
      en negotiated-link-selectie; HopOS gebruikt dit al voor zijn gigabit-PHY's
- [ ] lneto PHY RTL8211F RGMII-delayhelper — alleen voorstellen als upstream
      vendorhelpers wil dragen; onze gemeten `rgmii-id`-configuratie blijft in
      `metal/driver/nic/mdio` totdat een upstreamvariant op Radxa bewezen is
- [ ] lneto HTTP-clientfaçade — eventueel de clienthelft van `leanhttp`
      upstreamen (redirects, chunked response, deadlines en body lifecycle);
      groot/optioneel en geen reden om `leanhttp` uit SURF/Easy te halen

**10-08: de pad-replaces zijn ERUIT — netstack is nu een eigen fork.**

Een `replace` geldt alleen in de hoofdmodule, dus surf en de vitals/welcome-apps
losten `soypat/lneto` op naar UPSTREAM terwijl de node de gepatchte versie
draaide. Stil, want het compileert. Daarom nu echte forks met eigen module-pad:

| | fork | tag |
|---|---|---|
| lneto | `github.com/xinix00/lneto` | `v0.4.0-hopos.1` |
| go-net | `github.com/xinix00/go-net` | `v0.1.0-hopos.1` |

Per clone twee branches: `hopos` (upstream-pad, basis voor de PR's) en `fork`
(+ één mechanische pad-commit, hier taggen we). `tools/refork-netstack.sh`
regenereert `fork` uit `hopos`, byte-identiek aan de tags. Nieuwe tag moet
HOGER zijn dan élke upstream-tag.

- [ ] **satellieten bumpen zodra er een nieuwe metal-tag is** — dan komt de
      fork automatisch mee (zij importeren lneto/go-net niet direct, dus alleen
      `go get …/metal@vX && go mod tidy`): hop-os-surf (nu v1.9.2), vitals
      (v1.11.1), welcome, cloudflared, hoplb, hopdns, hopprom (v1.8.3)

**Als alles upstream geland is — de fork ERUIT:**

- [x] `metal/go.mod`: pad-replaces eruit (10-08, nu fork-tags)
- [ ] als de PR's geland zijn: de fork-requires terug naar `soypat/lneto` en
      `usbarmory/go-net` met echte upstream-versies, en de fork opheffen
- [ ] nálopen dat er nergens anders een pad-replace achterblijft (surf en hop
      horen er geen te hebben; chain-beta zet ze alleen tijdelijk en draait ze
      zelf terug — controleer met `grep -rn "=> /Users" */go.mod`)
- [ ] daarna: gate + QEMU-smoke, en de eerstvolgende release is weer een
      STABIELE (reproduceerbaar uit module-versies, geen chain-beta)

Volledige exit-checklist: `docs/netstack-upstream.md`. Tot die tijd: clones op
branch `hopos` laten staan — de replace volgt de werkboom.

## LicheeRV liep vast na ~250s: hij verdronk in zijn eigen logging (11-08 GEFIXT)

**Symptoom** (Derek, op een druk LAN): eindeloze regels `netstack (tx=false):
packet dropped`, en na 250-252s reageerde de node niet meer. Consistente timing.

**De keten, en die is zelfversterkend:**

1. een echt LAN is vol broadcast (mDNS, SSDP, IPv6 router advertisements, ARP);
   die frames zijn niet voor ons, dus dropt de stack ze — correct gedrag, en
   lneto logt het zelf op *debug*-niveau ("routine noise on a shared LAN")
2. `hopnet` printte élke melding onvoorwaardelijk
3. de console is een **blokkerende** busy-wait per byte
   (`board/licheerv/console.go`: `for LSR&THRE == 0 {}`) — op 115200 baud is dat
   87µs/byte, dus **3,5ms per logregel van 40 tekens**
4. bij ~300 regels/s is dat 104% van de tijd van het HOP-hart: er is geen tijd
   meer over
5. dus komt de RX-lus niet aan de beurt → de DWMAC-ring loopt over → **nog meer**
   drops → nog meer logregels → terug naar 3

Bevestiging uit Dereks eigen log: `board: die temp 48.9C`, waar hetzelfde bordje
tijdens de netmeter-metingen op 37-39°C zat. Tien graden erbij is precies een
hart dat staat te busy-waiten.

**Waarom we dit niet eerder zagen:** QEMU's slirp levert geen broadcast. Gemeten
in dezelfde soak: **0 drops** in QEMU tegenover honderden per seconde op ijzer.

**Fix: de handler is weg.** Eerst bouwde ik een rate-limiter met vensters en
tellers — 60 regels om ruis te *dempen* die we niet willen zien. Dat is de
verkeerde afslag (Derek, 11-08): `HandleStackErr` is optioneel, dus niet zetten
geeft geen ruis, geen busy-wait en geen code. Fouten meten doen we met
gereedschap dat daarvoor is (netmeter's `/diag` met de driver-tellers, vitals),
niet met een print in het datapad.

- [ ] **op ijzer verifiëren** met de volgende release: de drop-regels moeten
      wegvallen, de node moet voorbij de 250s blijven leven, en de die-temp hoort
      terug naar ~38°C
- [ ] overwegen: de console asynchroon maken (ring + flush-goroutine) i.p.v. een
      busy-wait per byte. Dan kost een logregel geen hart-tijd meer. Grotere
      verbouwing, en met de throttle niet meer urgent — maar de huidige console
      is wél een plek waar élke print rechtstreeks tijd van het OS afsnoept
- [ ] `appnet` en `netmeter` nalopen op dezelfde vraag: netmeter print zijn
      netstack-meldingen óók naar de console (naast de ring die `/report` leest),
      en dat is busy-wait tijdens een doorvoermeting — dus het vervuilt zijn
      eigen cijfers

## UDP stond op nul — klok, DNS en dus élke HTTPS lagen plat (11-08 GEFIXT)

**Symptoom** (gemeten op QEMU): geen enkel https-artifact kwam binnen.

    tls: failed to verify certificate: x509: certificate has expired or is not
    yet valid: current time 1970-01-01T00:17:12Z is before 2026-07-03

**Keten:** go-net zette `MaxActiveUDPPorts: 0` ("Unsupported as of yet") → élke
UDP-socket faalt met `resource exhausted` → SNTP faalt → de klok blijft op epoch
→ geen certificaat valideert → geen artifact-download, geen cloudflared. Vier
lagen tussen oorzaak en symptoom, en niets ervan luid: HOP's eigen auth is
HMAC-gebaseerd (klok-vrij) en de QEMU-tests haalden artifacts van `http://`.

**Waarom dit kon blijven zitten:** lneto ondersteunt UDP al lang —
`x/xnet`'s Socket doet `"udp"/"udp4"/"udp6"` met `udp.Conn`/`udp.PacketConn`, en
`StackAsync` heeft `DialUDP`/`RegisterListenerUDP`. Alleen go-net gaf het aantal
poorten niet door.

**Fix** (één regel plus een config-veld, go-net commit d3827f8 → tag
`v0.1.1-hopos.1`): `MaxActiveUDPPorts` is nu configureerbaar met default 4.
Expliciet gezet in `hopnet` (4: resolver + SNTP) en `appnet` (8: apps doen DNS,
en cloudflared praat QUIC — dat ís UDP). Een poort kost ~64 byte.

**Bewezen na de fix:** `clock: 2026-08-11T04:01:03Z (SNTP)`, daarna een
https-artifact van GitHub gestreamd (3 MB) en de app antwoordt met HTTP 200.

- [ ] **PR naar go-net** — dit is upstream-waardig en staat los van onze fork:
      het veld + default, met de keten in de tekst (commit d3827f8 op `hopos`)
- [ ] **alle app-images herbouwen**: de builds in de rolling-releases dragen de
      oude appnet (0 UDP-poorten), dus zij kunnen zelf geen DNS. welcome merkt
      dat niet (serveert alleen), maar **vitals' rx-test en cloudflared kunnen
      niet werken** tot ze opnieuw gebouwd zijn
- [ ] daarna cloudflared functioneel meten (tunnel opzetten); nu niet mogelijk

## Netstack-buffers: opbouwen i.p.v. vooraf claimen (idee 11-08, Derek)

**Waar het vandaan komt.** HOP ging OOM omdat `hopnet` één `TCPBufferSize` van
128KiB zet en `x/xnet` die **per verbinding, vooraf en vast** alloceert. Twee
plekken:

- `NewTCPPool`: élke `net.Listen` claimt `MaxListenerConns × 2 × TCPBufferSize`
  vooraf, dus 8 × 2 × 128KiB = **2MB per listener** die permanent vast staat,
  ook als er nul verbindingen zijn. HOP heeft drie listeners.
- `x/xnet/stack-go.go` (regels 125/151/180): élke uitgaande dial alloceert een
  verse `TxBuf`+`RxBuf` uit dezelfde config, dus **256KB per dial**, vrijgegeven
  pas door de GC.

Het geheugen schaalt daarmee met **geconfigureerde capaciteit** in plaats van met
**actief gebruik**. Op een raam van 46MB is dat het verschil tussen werken en
omvallen, en het dwong ons vandaag in een keuze die geen van beide kanten goed
is: buffers klein (kost doorvoer op élk board, ook die met geheugen genoeg) of
een ruimere GC-marge (kost heap). We kozen GOGC 25 als pleister.

### Hoe gVisor het doet — afgekeken 11-08, dit is de referentie

Bron: `gvisor.dev/gvisor/pkg/tcpip/transport/tcp` (staat nog in de module-cache).
Het architecturale verschil in één zin: **daar is de config een PLAFOND, hier is
hij een ALLOCATIE.** gVisor kent per protocol een range `{Min, Default, Max}` —
`MinBufferSize = 4KiB`, `Default = 1MiB`, `MaxBufferSize = 4MiB` — en een
endpoint start op Default en groeit naar Max. Niets wordt vooraf per slot geclaimd.

**Sendbuffer** (`endpoint.go`, `computeTCPSendBufferSize`): de buffer volgt het
congestievenster, want dat is precies wat er in flight kan zijn.

    numSeg     = max(InitialCwnd /* 10 */, SndCwnd)
    newSndBufSz = numSeg * MSS * 2          // factor 2 = pakket-overhead
    if newSndBufSz < curSndBufSz { houd de huidige }   // groeit alleen
    clamp op ss.Max

Start dus op ~10 × 1460 × 2 ≈ 29KB en groeit mee met cwnd. Auto-tuning gaat uit
zodra de gebruiker zelf `SO_SNDBUF` zet — een expliciete keuze wint.

**Receivebuffer** (`endpoint.go` ~1310): tuneert op wat de **applicatie**
daadwerkelijk uitleest in de laatste RTT, niet op wat er aankomt.

    alleen groeien als deze RTT méér gekopieerd is dan de vorige
    rcvWnd = prevRTTCopied*2 + 16*amss      // 2x voor verlies, 16 segmenten slack
    grow   = rcvWnd * (prevRTTCopied - prevCopied) / prevCopied
    rcvWnd += grow * 2                      // sender verdubbelt cwnd per RTT
    vloer  = amss * InitialCwnd * 2          // altijd 2x het initiële venster
    plafond = maxReceiveBufferSize()
    NOOIT krimpen — een kleiner venster zou data in flight afwijzen

De RTT meet de ontvanger zelf (`rcv.go`, `updateRTT`, minimum-observatie) als er
geen SRTT uit timestamps beschikbaar is. Dus: geen klok van buiten nodig, en geen
configuratie per verbinding.

**Wat we hiervan overnemen voor lneto:** de drie eigenschappen die het werk doen
zijn (1) config = range i.p.v. allocatie, (2) grow-only met een vloer en een
plafond, en (3) het signaal is *gebruik* — cwnd voor de send-kant, door de app
gekopieerde bytes voor de receive-kant. Punt 3 is de crux: op precies dezelfde
128KiB-config zou een health-check op zijn vloer blijven staan en een download
naar het plafond klimmen.

**Twee ontwerpen die de keuze weghalen** (niet exclusief, de tweede is de
goedkopere tussenstap):

1. **Rampen / auto-tunen.** Klein beginnen en het venster laten groeien zodra de
   peer het echt vult — een download krijgt zijn 128KiB, een health-check of een
   agent→leader-proxy blijft op een paar KB staan. Dit is wat gVisor deed, en het
   is exact de reden dat we dit vóór de flip nooit hebben gezien: daar was de
   buffer een gevolg van het gebruik, nu is hij een gevolg van de config.
2. **Één pool voor alles i.p.v. per verbinding.** Grote buffers uitgeven op
   aanvraag en teruggeven bij close. Dan kost N gelijktijdige bulk-transfers
   N × 128KiB, in plaats van (elke listener-slot + elke dial) × 128KiB. Past bij
   lneto's stijl, die al pools kent.

**Waarom dit de juiste plek is en niet nóg een knop.** Een tweede globale
instelling (inkomend/uitgaand, of "kern versus app") splitst op de verkeerde as:
bulk en control bestaan beide in beide richtingen, en per rol sizen zou
netstack-config in vreemde pakketten smokkelen. De as die telt is per socket, en
die kent alleen de socket zelf — dus hoort de beslissing daar, niet in een
config-getal. Dat maakt het ook het antwoord dat voor élke lneto-gebruiker
werkt, niet alleen voor ons.

**Wat het ons oplevert als het er is:** `hopnet` mag zijn 128KiB houden voor
downloads zonder dat listeners en proxy-dials meebetalen, de GOGC-25-pleiser in
`cpu/memlimit` mag terug naar Go's richtlijn van ~10%, en de vaste voet van HOP
(nu ~6MB alleen aan voorgeclaimde listener-pools) zakt mee.

- [ ] kandidaat-PR upstream, mét de QEMU-reproductie erbij (raam op 64MB +
      SSE-last op `/v1/events`): eerst de smalle bug (dial-buffers los van de
      listener-pool-config), daarna pooling of auto-tuning als het echte antwoord

## RAM-eis per app — gemeten 11-08 (QEMU virt, arm64)

**De regels van het geheugenmodel**, want daarmee is elk getal na te rekenen:

- `memory_limit` in de job-spec ís de partitiegrootte; HOP lijnt hem uit op 2 MB
  (5 MB → 6 MB) en de pool is de harde grens
- de app ziet **partitie − 2 MB**: `AbiTail` is de slot-ABI (control page,
  mailboxen, net-ringen). Gemeten bevestiging: 96 MB → vitals meldt `ram_mb 94`
- tijdens het **streamen** moeten het geplaatste beeld én de nog-niet-geplaatste
  staart samen passen — een grotere eis dan het beeld alleen
- daarbinnen zet `memlimit.Arm` de GC-limiet op
  `(venster − beeld) + Sys − max(budget/16, 4 MB)`

| app | beeld (gelinkt) | past vanaf | **werkt vanaf** | eigen gebruik |
|---|---|---|---|---|
| welcome | 3,31 MB | 6 MB | **18 MB** | — |
| vitals | 3,47 MB | 6 MB | **20 MB** | `sys 4 MB`, heap 0,9 MB, 0 GC |
| cloudflared | 31,32 MB | **38 MB** | nog onbekend | — |

Huidige job-specs staan royaal: cloudflared krijgt 190 MB, de voorbeelden 96 MB,
chain-beta 64 MB. Er is dus ruimte om te zakken, maar niet tot "past vanaf": het
verschil tussen 6 en 18 MB is de werkruimte die de Go-runtime nodig heeft.

**Wat opviel bij de ondergrens:** onder zijn minimum stopt een app met
`exit code=0` en `last=""` — een nette exit zonder één logregel. Het
post-mortem-veld dat juist hiervoor bestaat blijft leeg, dus de operator ziet
"gestopt" en niet "te weinig geheugen". Dat is een diagnose-gat.

**netmeter** is geen slot-app maar de monitor-payload (hij vervangt de agent), dus
hij heeft geen `memory_limit`: hij krijgt het hele HOP-venster. Op de LicheeRV is
dat 64 MB en hij meldde `sys 46 MB` — met 256 KiB TCP-buffers en een 8 MiB
serve-blob.

## LicheeRV: RX-jacht AFGEROND (10-08) — corruptie weg, doorvoer verdubbeld

**Eindstand op ijzer** (netmeter-image, Mac-sharing-net 192.168.99.2):

| | begin van de dag | eind |
|---|---|---|
| RX 16MiB | 4,22 MB/s en **sha FOUT** | **8,84 MB/s, sha correct** (5 runs: 1805-1818ms) |
| TX 8MiB | 9,45 MB/s bit-perfect | 9,48 MB/s bit-perfect |
| HTTPS 6,25MB | `tls: bad record MAC` | geslaagd, **byte-exact gelijk aan de GitHub-asset** |
| gedropte frames | 3-41 per download | **0** (RU-bit in DMA-status blijft nu weg) |
| allocaties per 16MiB | 641-910 mallocs, 0 GC | 67 mallocs, 0 GC |

RX zit nu op 93% van TX; de asymmetrie waarmee de jacht begon is praktisch weg.

**Er waren drie oorzaken, en ze lagen in drie verschillende lagen.**

1. **Stille corruptie (lneto, [#181](https://github.com/soypat/lneto/pull/181)).**
   `internal.Ring.onReadEnd` deed `Reset()` zodra de laatste leesbare byte weg
   was: schrijfpositie terug naar 0. Maar out-of-order TCP-segmenten worden met
   `PeekWrite` *achter* die positie gestaged, relatief eraan — `reassembly.go`
   bewaart bewust geen offset ("the ring write pointer advances in lockstep with
   rcv.NXT"). Leest de app de ring leeg terwijl er een gat open staat, dan
   commit `reassemble` willekeurige oude bufferinhoud: volle lengte, verkeerde
   bytes.
2. **Verlies was niet herstelbaar (lneto, [#182](https://github.com/soypat/lneto/pull/182)
   + [#183](https://github.com/soypat/lneto/pull/183)).** FIN-WAIT-1 weigerde
   élk datasegment (dus write-then-close kon zijn laatste segment nooit
   herstellen), en géén enkele verbinding in `x/xnet` had een
   retransmissie-timer. 512KiB-verliesstroomtest: 2/10 → 8/10 complete runs.
3. **De ring was te ondiep (ONS, `driver/nic/dwmac`).** 64 descriptors = ~8ms
   buffering, tegen een Go-scheduler-quantum van ~10ms. **Niet** omdat de lus te
   traag is: die doet 47µs/frame (9 driver + 38 stack) waar de draad 120µs per
   frame kost. Maar op één hart deelt de RX-lus de core met de goroutine die de
   download verwerkt (io.Copy + SHA256), en in één quantum komen er 83 frames
   binnen. Nu 128 descriptors (~15ms) en netDMASize 256→448KB — het maximum dat
   nog in de 1MB OS-staart past.

**Wat de weg wees, in deze volgorde:** de sha-vergelijking (corruptie), de
missed-frame-teller uit register 0x1020 (drops kwantificeren — de RU-bit in
DMA_STATUS is sticky en zegt alleen "ooit gebeurd"), en het tijdbudget per frame
(dat mijn eigen "de lus is te langzaam"-hypothese weerlegde). De poll-slaap is
gemeten en uitgesloten: 0/50/300µs geven binnen de ruis hetzelfde.

**Het instrument** (blijft staan, ook voor de Radxa — geen framebuffer, geen
UART-dongle nodig):

    PAYLOAD=netmeter image/licheerv-agent.sh   # -> out/hopos-licheerv-netmeter.img
    BLOB_MB=16 python3 metal/cmd/netmeter/hostsrv.py   # host-helft, :8099

| | |
|---|---|
| `/report` | alle meetregels + NIC-stand en tijdbudget per fase |
| `/diag` | ringen, DMA-status, missed-frame-tellers, memstats, die-temp |
| `/blob` | de TX-kant, van buitenaf te klokken |
| `/pull` | herhaal de 16MiB-download — een variant meten kost geen boot meer |
| `/set?rxsleep=N` · `/reset` | parameter verstellen · tellers nullen |

**Nog open in deze hoek:**

- [ ] de resterende 22% naar lijnsnelheid is CPU: 47µs/frame × ~8300 frames/s is
      al ~39% van één hart alleen voor de RX-lus. De 38µs stack-tijd is de
      grootste post — pas aan te pakken met een profiel per laag, niet met een
      gok
- [ ] `storm-conn` alloceert 206MB over 400 verse verbindingen (~516KB per
      verbinding) met 11 GC-cycli; werkt, maar op 64MB is dat het soort ding dat
      je wil weten
- [ ] agent-image (niet alleen netmeter) op ijzer met de diepere ring

## Display-app: read-deadline bijstellen

De switch stuurt bij slot-dood geen TCP-RST meer namens de dode app
(`hopswitch/rst.go` is 26-07 gesloopt — een dode peer merken is app-werk, want
stilte kan altijd van het netwerk komen en HOP heeft al twee lagen die dit
dekken: de health-check op de task en de app-eigen ping).

Gevolg: een verdwenen venster wacht weer op de eigen read-deadline van de
display-app in plaats van meteen te sluiten. Die stond op 30s.

**Te doen** (de display-app zit niet in deze repo, maar op de artifact-server):
zet de read-deadline op een paar keer het ping-interval. Met het huidige tempo
van ~10s pingen dus richting 12-15s; wil je het sneller, dan moet het
ping-interval mee omlaag (bijv. 3s pingen, ~8s deadline). Korter dan het
ping-interval sloopt gezonde verbindingen.

Zie `docs/technical/networking.md` → "Failure handling: noticing a dead peer is
the app's job".

## SURF-apps: `net/http` eruit, `apphttp` erin (~2,9MB per app)

`net/http` linkt onvoorwaardelijk `crypto/tls` mee. Gemeten 26-07 op `app/hello`
(tamago-build, `-w -T 0x50010000`):

| | image |
|---|---|
| `applib` alleen | 1,71 MB |
| + `appnet` (gVisor-netstack) | 4,70 MB |
| + `net/http` | 7,99 MB |
| + `apphttp` i.p.v. `net/http` | **5,06 MB** |

Die laatste sprong van 3,29MB is dus voor ~54% TLS/PKI (`crypto/tls`,
`x/crypto`, `x509`, `math/big`, `asn1`) — méér dan de hele netstack kost. Een app
die geen TLS nodig heeft, betaalt hem nu wel. `metal/app/applib/apphttp` is er
daarom: minimale HTTP/1.1-GET-client, geen TLS, 0 bytes `crypto/tls` in de
binary, en maar 0,36MB boven de netstack-vloer.

**Het repo-deel is KLAAR** — `apphttp` dekt inmiddels beide kanten:
`Serve`/`ListenAndServe` (één handler, Content-Length-antwoorden, keep-alive,
Flush→chunked, Hijack voor de KVM-WebSocket — zie de package-doc van
`serve.go`) en `Do` met methode + body, dus POST (`apphttp.go`, "de tweede
helft").

**Wat rest is per SURF-app** (eigen repo). Hieronder wat de env/poorten in
`docs/config.md` verraden — de apps zelf zitten hier niet, dus per app nog
nalopen wát er precies over HTTP gaat:

| App | Uit de config | Vermoedelijke actie |
|---|---|---|
| display | `ports: surf:7878, http:80` | serveert → wacht op `apphttp.Serve` |
| launcher | `HOP_ADDR` + serveert | POST't naar de agent → wacht op `Serve` + POST |
| taskman | `HOP_ADDR` (agent-API op `10.100.0.1:8080`) | client op plain http → `apphttp` |
| clock, calc | alleen `SURF_ADDR` | eerst checken of ze überhaupt HTTP doen |
| browser | alleen `SURF_ADDR` | **checken:** doet deze https? dan `net/http` houden |

Welke apps écht https doen weet jij; alleen díe houden `net/http` (de winst
vervalt daar, want TLS is dan geen dood gewicht maar functionaliteit).

**Niet migreren:** de `apploader` in deze repo. Die haalt artifacts óók van
https-URL's (GitHub-release-assets — x509-fout gemeten 20-07, `hop apply
https://…/app.elf`) en houdt dus `net/http` plus de x509-rootbundel. `app/hello`
blijft er ook op staan: dat is de documentatie van "gewoon Go, ongewijzigd"
(`docs/app.md`) en het is bovendien een server.

Dit is puur image-omvang, geen functionaliteit — niets hieraan is dringend.
Wat je inlevert t.o.v. `net/http`: geen https (faalt luid met een duidelijke
melding) en geen chunked transfer (`Content-Length` verplicht). Redirects worden
wél gevolgd.

Zie de package-doc van `metal/app/applib/apphttp` voor de volledige afweging.

## USB-HID op ijzer: drie boards wachten op één toetsaanslag

Gebouwd 06-08, gate groen op alle drie de smaken, **nul boards geverifieerd**.
De stack (`metal/gui/driver/usb/{xhci,dwc3,hid}` + `metal/gui/usbin`, alles
achter `-tags gui`) is compleet tot en met de interrupt-endpoint. HOP serveert
de events op `10.100.0.1:7879` en het adres reist mee in de fb-grant
(`INPUT_ADDR`); de display belt en leest — zie `stack/surfserve/hopinput.go` in
hop-os-surf.

Wat de eerste boot per board moet uitwijzen:

**Radxa (rk3566)** — de agent logt per DWC3-core één regel met GSNPSID/GCTL/
GUSB2PHYCFG/GUSB3PIPECTL, en daarna per controller de rauwe PORTSC's. Drie
uitkomsten, drie vervolgen:
- `id=00000000` of `ffffffff` → de core is niet geklokt. Dan is de CRU-gate aan
  de beurt, en die is **niet geverifieerd** — we hebben hem bewust niet gegokt
  (de TSADC van deze week is precies wat dat kost). Referentie eerst:
  `clk-rk3568.c` + `rk3568-cru.h`.
- Cores komen op, maar op geen enkele poort staat CCS → de fysieke connector
  van de Zero 3E hangt niet aan een DWC3 maar aan de losse EHCI/OHCI-companions
  (0xFD800000/0xFD840000, 0xFD880000/0xFD8C0000). Dán pas is een tweede driver
  te verantwoorden.
- CCS staat aan → doorlopen tot de attach-regel.

**Pi 5** — het goedkoopste pad: de PCIe-link naar de RP1 staat al voor de GEM.
Openstaand: of de RP1 zijn USB-blokken zelf uit reset en op klok zet. Zo niet,
dan is dat het RP1-CLOCKS/RESETS-blok en dus echt werk.

**Pi 4** — `driver/brcmpcie` kreeg een BCM2711-variant (RGR1_SW_INIT_1 voor
bridge+PERST, HARD_DEBUG op 0x4204, burst 128B, geen RESCAL/MDIO-PLL/UBUS-remap).
Die is **geschreven tegen de Linux-referentie en nooit uitgevoerd**. Eerste
meting: traint de link en meldt de VL805 zich als `1106:3483`?

Eén ding is bewust niet vooruitgebouwd: het inbound-window van de BCM2711 is
identiek gemapt (PCIe 0 → DRAM 0), dus `BusOff` is nul en de vraag "wat als een
window verschuift" doet zich hier niet voor. Zodra hij dat wél doet is dat een
meting, geen gok.

## De staging-botsing: twee allocators in één raam

**Symptoom** (Pi 5, 06-08, en al eerder op de Radxa): een app van ~5,8 MB in een
partitie van 32 MB weigert te plaatsen met `elf parse: bad magic number
'[0 0 0 0]'`, terwijl dezelfde app in 128 MB probleemloos start. Gemeten
drempel: 5,38 MB ✓, 5,77 MB ✗.

**Wat vaststaat.** `layout.StageAddr` legt de image tegen de BOVENKANT van het
app-RAM, en niemand vertelt de runtime dat die bovenkant bezet is:
`cpu/memlimit` rekent zijn muur op `RamStart + RamSize − RamStackOffset`, en
`ramStackOffset` is `0x100` — letterlijk de bovenkant. Daar bovenop stonden in
de apploader twee rekenfouten die precies het interessante geval oversloegen:

1. de aanscherping gold alleen `if lim < RAMSize/2`. Bij 30 MB app-RAM en een
   image van 5,5 MB was `lim` 22,5 MB en de drempel 15 MB — dus hij werd
   **niet** gezet, in exact het geval waarvoor hij bedoeld was;
2. `lim` rekende vanaf adres 0, terwijl `SetMemoryLimit` runtime-geheugen telt
   vanaf het heapfundament. Dat is de ~9 MB image+bss van de loader te veel.

Wat er dus overbleef was het voorlopige plafond `RAMSize/2` = 15 MB. Met een
heapfundament op ~9,4 MB mag `bloc` daarmee tot ~24,4 MB komen — en de
stagingbodem ligt op `30 − imgSize`. Botsing zodra `imgSize > 5,6 MB`, en de
gemeten drempel ligt tussen 5,38 en 5,77. Dat klopt tot op ~200 KB.

**Gerepareerd 06-08.** `memlimit.ArmBelow(top)` zet hetzelfde plafond met een
EXPLICIETE muur, en `applib.StageImage` roept hem aan met de stagingbodem —
de enige plek die dat adres kent. Te weinig ruimte over faalt daar nu luid
("image van N MB laat de runtime M MB over") in plaats van stil te corrumperen.
De twee foute regels in de apploader zijn weg.

**Wat NIET vaststaat, en nu gemeten wordt.** Dát het de heap is die eroverheen
loopt, is afgeleid en niet waargenomen. `StageImage` kijkt daarom na het
downloaden de ELF-magic terug: staat hij er niet meer, dan is de download níet
de dader (wij schreven hem net) en meldt de loader het mét zijn `MemStats`. Eén
boot zegt dan of `Sys` inderdaad tegen de stagingbodem aan staat.

**De knop voor grotere images in kleine partities** is het voorlopige plafond
`debug.SetMemoryLimit(RAMSize/2)` in `app/apploader/main.go`: wat de
TLS-handshake daar aan arena mag pakken, kan de image niet meer gebruiken.
Verlagen kan, maar niet blind — 8 MB is de ondergrens waar de keten het nog
doet (`minStageHeap`), en dit pad is de enige startroute van élke job.

**Wat twee stages NIET oplost** (Dereks vraag, 06-08): de loader en de image
moeten hoe dan ook tegelijk in de partitie staan, dus het plafond blijft
`partitie ≥ image + loader + werkruimte`. "De app over de loader heen gooien"
gebeurt trouwens al — `placeFromStaging` schrijft de segmenten op hun
linkadressen, dwars over waar de loader stond. Wil je een image van 20 MB in
32 MB, dan moet de loader de partitie uít (bijvoorbeeld een gedeelde
scratch-partitie voor fase 1, waarna HOP de segmenten naar de echte partitie
schrijft) — dat is een andere, grotere verbouwing.
