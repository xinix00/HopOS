# Kern-flip: HopOS onder zichzelf updaten (live, zonder de apps te raken)

Status: **arm64 op IJZER bewezen — Mac mini M4, 02-09: een kern gewisseld
onder een draaiende app, node bleef stabiel (Derek: "live patching is een
ding"). QEMU-regressie sinds 31-08/01-09; riscv64 gebouwd, ijzer open.**

Aan/uit per node: **`hopos.flip.enable`** in de platform-config, standaard AAN
in onze eigen images. Dat is de énige plek waar de flip een node raakt die er
nooit gebruik van maakt — hij bepaalt of de switch-code die een app-core
uitvoert naar de plan-regio verhuist of in het kern-image blijft staan. Op 0
gedraagt de node zich byte-voor-byte als vóór deze feature bestond, met
dezelfde binary: bewust een runtime-vlag en geen build-tag, want twee
build-smaken betekent dat wat je test niet is wat draait (die drift kostte deze
boom al eens vijf boot-cycli, zie `dev.RealCacheOps`).

Kosten van al het overige, gemeten: **78 KB image (1,43%)** aan code die inert
blijft tot er echt geflipt wordt.

Een app draait door twee kernwissels heen zonder te merken dat het OS onder hem
vervangen werd (`HOPOS_FLIP_ADOPT_OK`, heartbeat loopt door, status blijft
READY), en daarna draait de volle demo-suite op de geflipte kern.

**Hardware-stand.** Alle zeven board-smaken leveren een geldige flip-bundel —
en dat is geen formaliteit: de bundel ontstaat uit een diff van twee builds op
verschillende linkadressen, en die faalt hard zodra ook maar één woord geen
zuivere linkbasis-delta draagt. Dat hij overal slaagt, bewijst dat elke kern
herbaseerbaar is:

| board | arch | payload | relocs | flip-kant |
|---|---|---|---|---|
| QEMU virt | arm64 | `0x40000000` | 23.806 | **bewezen** (regressie) |
| Pi 5 | arm64 | `0x80000` | 23.832 | gebouwd |
| Pi 4 | arm64 | `0x80000` | 23.826 | gebouwd |
| Radxa Zero 3E | arm64 | `0x2200000` | 23.769 | gebouwd |
| UEFI/Altra | arm64 | `0x50000000` | 23.868 | gebouwd |
| Mac mini M4 | arm64 | `0x10100000000` | 23.867 | gebouwd |
| LicheeRV Nano | **riscv64** | `0x84000000` | 22.946 | gebouwd |

Bouwen: `image/flip-bundle.sh <board>` — dat print meteen de `hopos.flip.sha256`
die erbij hoort. Een flip wordt AANGEVRAAGD — POST /flip {"url","sha256"} op
de agent-API is de enige trigger, achter dezelfde HMAC als job-dispatch (wie
een kern mag aanleveren moet bewijzen wat een job-gever bewijst); de som
reist mee in
zijn platform-config; de agent-main doet de adoptie zelf, dus dit werkt op elk
board en niet alleen op de QEMU-bank.

**RISC-V is gebouwd maar niet op ijzer geverifieerd**, en de eerste boot ván de
LicheeRV ís die verificatie: de M-mode-switcher (`mentry`..`mmodeEnd`, 1384
bytes) verhuist daar nu als kopie naar de plan-regio, net als de EL2-blobs op
ARM. Dat is een gedragsverandering op een pad dat élke app-start raakt. Twee
metingen die het onderbouwen: de blob is volledig positie-onafhankelijk
(objdump 01-09: `parkenter` laadt `mentry` als AUIPC+ADDI, en `mrotate` via een
PC-relatieve JAL — dus de kopie kost geen enkele patch), en hij past ruim
(1448 bytes descriptor+blob in 24 KB vrije plan-ruimte).

Besluit Derek 31-08: **lenen** — de nieuwe kern wordt in een uit de app-pool
geleende regio geplaatst, de oude kern springt erin, en de nieuwe kern geeft
het oude venster aan de pool terug. Het artifact is **gewoon de kern-ELF**
(zelfde download- en plaatsingsweg als een app), plus een reloc-staart.

## Waarom dit kan

De architectuur heeft het zware werk al gedaan:

- **Alle app-staat woont in de partitie** (ABI-staart: control page, ringen,
  koppen) — die overleeft een kern-wissel per definitie.
- **Apps draaien door zonder HOP**: yields/parks worden op de app-core zelf
  afgehandeld, de heartbeat is app-zijdig, hop-ABI-RPC's hebben een timeout.
  De enige koppeling is `hopswitch.loop()` (frames), en dat gat overbruggen
  TCP-peers met retransmits. Gat-budget: seconden.
- **Netboot is 01-09 gesloopt** — pakket en al, inclusief het sign-gereedschap.
  Het deed hetzelfde als de flip maar primitiever, en twee mechanismen voor één
  ding is er één te veel. De fetch is nu een kleine sha256-gecontroleerde GET
  in kernflip zelf. (`apple.Chainload` blijft wél, maar voor iets anders: dat
  is de m1n1-proxykabel voor een bord dat nog niets kan.)
- **De reloc-machinerie bestaat** (mkkernel `writePEReloc`, UEFI-bewezen):
  dezelfde build op twee linkadressen, diff over 8-byte-woorden = de tabel.
- **Het vangnet is gratis**: faalt de flip, dan haalt de canary fase 1 nooit
  en reset de watchdog de node — precies de bestaande update-weg.

## Verhouding tot bestaande besluiten

- **Netboot-kader (30-08)** blijft staan: het image komt signed binnen, per
  node gewild (config/opdracht aan díe node), nooit vloot-breed automatisch.
  De flip verandert alleen wat er ná verificatie gebeurt: springen i.p.v.
  rebooten.
- **"HOP-leven = node-leven / geen reconcile"** wordt bewust en begrensd
  versoepeld: de adoptie reconstrueert kern-boekhouding uit (a) een expliciet,
  geversioneerd handoff-blob en (b) de waarheid die al in DRAM staat
  (ctx-blokken, control pages, ring-koppen). Liveness wordt niet aangenomen
  maar gemeten (heartbeat moet lopen), anders wordt het slot opgeruimd — een
  mislukte adoptie degradeert dus naar het bestaande gedrag.

## De vorm

### Artifact: kern-ELF + `HOPRELO1`-staart

- De release bouwt de kern **twee keer** (canoniek linkadres + één
  schaduwadres, zonder `-s`, met `-buildid=`), volgens de bestaande
  diff-methode: elk 8-byte-woord dat verschilt moet exact de linkbasis-delta
  dragen, anders faalt de build hard.
- Uitvoer: de canonieke ELF **onaangeroerd**, met daarachter een staart:
  `HOPRELO1 | flipABI | linkLoad | flatSize | entry | count | off[]…`
  (offsets t.o.v. linkLoad, in de platte beeld-ruimte).
- Verificatie: de **sha256 van het hele bestand**, die in de platform-config
  van de node staat (`hopos.flip.sha256`). Geen handtekening — de sleutel zou
  in dezelfde repo wonen als de release, dus die dekt geen aanval die deze som
  niet al dekt. Het anker is het bootmedium dat jij schrijft; dezelfde keten
  als de `SHA256SUMS` die bij een release gepubliceerd worden.

### De flip (in de vertrekkende kern, `kern/kernflip`)

1. Fetch + sha256-controle (`fetchBundle`) — of bytes uit een andere gewilde bron.
2. Staart parsen, flip-ABI toetsen (weigeren bij mismatch).
3. **Venster lenen** uit de partitie-pool (maat = het kern-venster van dit
   board), 2MB-korrel; `coopCleanInv` erover (stale lines van een vorige
   huurder).
4. ELF-segmenten plaatsen op `venster + (Paddr − linkLoad)` (zelfde rekensom
   als place), BSS nullen, via de dev-accessors (device-schrijfweg → geen
   cache-contract).
5. Reloc-pass: per offset `w += (venster − linkLoad)`.
6. `runtime/goos.RamStart/RamSize` patchen naar (venster, venstermaat) —
   zelfde symbolen als bij een app.
7. Handoff-blob schrijven (zie onder) + pointer op de boot-scratch-pagina.
8. Springen: op arm64 een HVC #2 naar de handler op de revoke-vectoren
   (EL1-MMU/caches uit, dan `br entry` op EL2 met de DTB-pointer in x0); op
   riscv64 een kale `JALR` in machine mode (`mie` uit, `fence` + `fence.i`).
   De nieuwe kern doorloopt daarna zijn volledige cpuinit→main-pad, alsof de
   firmware hem daar had afgeleverd.

### De conntrack gaat mee

Alles op deze node loopt door de NAT, dus een kernwissel die de apps spaart
maar hun verbindingen laat vallen is maar half werk. De conntrack gaat daarom
als eigen blok in het blob mee — en dat kan lean, om twee redenen die
allebei uit het bestaande ontwerp komen:

- **Eén schrijver.** Alle NAT-staat leeft onder het switch-mutex, met de
  switch-lus op de HOP-core als enige muteerder. Een snapshot is dus een
  gewone lees-ronde: geen quiesce, geen barrière, geen tweede waarheid.
- **Geen index nodig.** Een flow beschrijft zichzelf volledig, en béide
  lookup-sleutels zijn eruit af te leiden (`fkey` uit slot+peer, `rkey` uit
  node-poort+peer). De nieuwe kern bouwt de maps dus terug met precies de
  velden waarmee `flowFor` ze ooit aanlegde — 24 bytes per flow.

De volle tabel (4096 flows) plus zestien bewoners is 99.736 bytes; de
handoff-staart is daarom 256 KB (0,1% van een kern-venster). Een node die
zijn conntrack vol heeft is precies de node die de overdracht het hardst
nodig heeft, dus daar hoort geen krappe grens te zitten.

Twee dingen die de correctheid gratis regelen: `allocPort` toetst elke
kandidaat al tegen `flowsRev` én de gepubliceerde poorten, dus herstelde flows
blokkeren hun eigen node-poort vanzelf; en `RestoreNAT` draait ná de
slot-adoptie en slaat elke flow over waarvan het slot geen switch-poort meer
heeft — een verbinding van een app die de wissel niet haalde, blijft niet
hangen.

### De agent-state gaat mee (JSON)

De kern kan de apps overdragen, maar HOP zelf herstart als gewoon proces in die
kern — met een lege administratie. Zonder overdracht kent de nieuwe agent zijn
eigen doorlopende taken dus niet meer: hij wil ze plaatsen en stuit dan op
slots die ze al bezetten (`claimSlot` weigert met "still live"). De apps
overleven de flip dan technisch en worden operationeel wezen.

De oplossing is de eenvoudigste die er is en hij lag klaar: `agentState` is
`{jobs, tasks}`, en `types.Job`/`types.Task` dragen hun JSON-tags al (ze gaan
over de HTTP-API en naar de gecommitte clusterstaat). Eén `json.Marshal` dus —
`agent.Snapshot()` / `agent.Restore()` in `xinix00/hop`, elk één op op de
state-loop en daarmee atomair zonder lock. HopOS reikt ze aan elkaar via
`agentboot.Options.RestoreState` en `OnSnapshot`.

Meevaller: `types.Task.Pid` draagt op HopOS het **slotnummer**
(`internal/runner/hopos.go`), dus de task→slot-koppeling zat al in het formaat.

Waarom in het blob en niet uit S3: een standalone node (alleen `hopos.init[]`,
geen lock-backend) heeft geen gecommitte staat om uit te herstellen. Die zou
zijn hele taakadministratie kwijt zijn terwijl de taken doordraaien. Zo hopt
hij gewoon mee — de flip is onafhankelijk van de opslag-backend, net als de
apps zelf.

### Wanneer springt hij?

Op aanvraag, en alleen op aanvraag: `POST /flip` op de agent-API, met de URL
van de bundel en zijn sha256. Achter dezelfde HMAC als job-dispatch — wie een
kern mag aanleveren moet bewijzen wat een job-gever bewijst, want een kern van
het net is code met alle rechten op die machine. De node haalt hem één keer op,
toetst de som tegen wat de aanvrager zei, en springt.

Dit is de ENIGE trigger (Derek, 01-09). Een console-commando en een
config-gedreven variant (`hopos.flip=<url>` bij boot) zijn er allebei geweest
en zijn gesloopt: drie deuren naar hetzelfde gevaarlijke pad is er twee te
veel, en de twee die weg zijn waren precies de deuren die niemand bewaakte.
Niet terugbrengen.

Een lus kan het niet worden: de vorige kern schrijft de som van zijn bundel in
het blob, dus dezelfde bundel nog eens aanbieden is een no-op. En kán deze node
niet flippen (`hopos.flip.enable=0`), dan zegt het endpoint eerlijk 501 in
plaats van te doen alsof.

Wat het meetinstrument was: flippen op een node die zijn werk al doet, niet op
een lege. Alleen dat eerste bewijst iets — en dat is precies wat op de M4 is
gebeurd.

### Handoff + adoptie (de nieuwe kern)

- Vaste plek: de boot-scratch-pagina draagt op een vast offset een pointer +
  magic naar het blob (dat in de staart van het geleende venster ligt, buiten
  de RAM-declaratie).
- De nieuwe kern **consumeert** het blob als allereerste (magic wissen), zodat
  een watchdog-reboot nooit een stale blob adopteert.
- Inhoud (blob-versie 3): het venster van de vorige kern, het eigen venster,
  de generatieteller, de som van de bundel waar deze kern uit kwam (die
  voorkomt dat een herhaald flip-commando een lus wordt), en per bezet slot
  {slot, partitie, core, gepubliceerde poorten, job-naam}. Bewust NIET:
  DHCP-lease en klok-offset — die herstelt de nieuwe kern zelf, en een fout
  overgenomen lease is erger dan een nieuwe DISCOVER.
- Adoptie: per geclaimd slot eerst **liveness meten** (CtrlHeartbeat moet
  binnen ~200ms lopen); dood → gewoon opruimen. Levend → partitie-boekhouding
  herstellen, ringen met `ring.Open` (niet `Init`) heropenen, servicer
  herstarten, switch-poort `Attach`.
- De verse-boot-paden die nu alles wissen (sched-blokken, `CtxState`,
  `armSlot`'s ring-init + control-page-veeg) krijgen een adoptie-smaak die
  leest i.p.v. wist.

### Stap 0 (randvoorwaarde): switch-code de plan-regio in

App-cores voerden kern-image-code uit: op arm64 `el2entry`
(cpu/el2/switch.s), `s2tramp` (el2.s) en `smpEL2Tramp` (smp.s); op riscv64 het
aaneengesloten stuk `mentry`..`mmodeEnd` (cpu/mmode/switch.s, incl. de rotatie
en de parkeerlus). Alle vier zijn volledig positie-onafhankelijk — op ARM
SP/TPIDR-relatief met de data uit sched-blok en control-page, op RISC-V met
AUIPC/JAL voor de interne sprongen (gemeten 01-09).
Ze verhuizen als kopie naar de vrije ruimte in het slot-0-blok van de
plan-regio (na de sched-blokken, +0xA000); de thunks, `CtxBootPC`,
`CtrlSMPTramp` en (rv64) `stubTrapPC`/`parkenter` wijzen naar de kopie.
Daarmee voert een app-core **nooit meer kern-image-bytes** uit en is het oude
venster na adoptie veilig vrij te geven. De kopie krijgt een lengte/hash-woord
zodat een adopterende kern kan toetsen dat de zittende versie de zijne is
(mismatch = flip weigeren → gewone reboot-update).

## Volgorde + acceptatie

1. ✅ **Blob-verhuizing (arm64)** — de drie EL2-blobs (el2entry, s2tramp,
   smpEL2Tramp) staan als kopie in de plan-regio, met een `HOPSWTC1`-descriptor
   (lengte + FNV-som + entry-offsets). De volle QEMU-demo bleef 20/20 groen.
2. ✅ **mkkernel `-elfreloc`** — bundel = de kern-ELF onaangeroerd + een
   `HOPRELO1`-staart met de relocatietabel uit de bestaande dubbel-build-diff.
   Gemeten: 13,0 MB ELF + 29.839 reloc-entries ≈ 120 KB staart.
3. ✅ **Koude flip (QEMU)** — `image/qemu-run.sh flip`: kern A haalt de bundel,
   leent 242 MB uit de pool, herbaseert en springt. Kern B meldt
   `HOPOS_FLIP_BOOT` en draait de volle demo af.
4. ✅ **Handoff + adoptie (QEMU)** — een app met gepubliceerde poort draait
   dwars door de flip: `HOPOS_FLIP_ADOPT_OK`, status blijft READY, de heartbeat
   loopt gewoon door (17 → 29 en 45 → 57 over de twee sprongen) en de
   servicer/switch/NAT komen terug om hem heen. Daarna draait de volle
   demo-suite (24 markers) op de geflipte kern.

   De regressie flipt **twee** keer, en dat is geen luxe: pas bij de tweede
   flip is het achtergelaten venster échte pool-grond. Precies dáár zat een
   bug — de eerste opzet gaf dat venster expliciet terug aan de vrije lijst
   terwijl `poolInit` het er al in had staan (elke kern knipt alleen zijn
   **eigen** venster uit de board-pool). Twee overlappende regio's in de vrije
   lijst → twee slots kregen dezelfde partitie → ring-corruptie
   (`head=0x205090402020201` waar een ringkop hoorde) en de swarm viel om.
   Weggehaald: teruggeven ís impliciet. Host-tests bewaken het nu
   (`TestPartAdoptClaimtEnGeeftNietOpnieuwUit`, `TestBorrowKernWindow`).
5. ✅ **riscv64 gebouwd** — de M-mode-switcher verhuist naar de plan-regio, de
   chainload is een kale JALR (HOP draait daar zelf in M-mode zonder MMU, dus
   er is geen vertaalregime af te breken — alleen `mie` uit, `fence` +
   `fence.i`), en de bundel bouwt. Niet op ijzer: er is geen riscv64-QEMU-bank
   in de boom, dus de LicheeRV ís de meetbank.
6. ✅ **Agent-node (QEMU)** — `qemu-run.sh agent` met een getekende bundel en
   `hopos.flip.after`: een node kreeg via de leader-API een echte job, en flipte
   daarna met die taak levend. Uitkomst: `1 of 1 resident(s) survived`,
   `agent state restored from the previous kernel` (603 B), en **geen enkele
   herplaatsing** in het log erna — de agent herkende zijn eigen taak.
7. ⬜ **Ijzer** — twee kandidaten, om verschillende redenen:
   - **LicheeRV** verifieert de riscv64-verhuizing (de eerste boot bewijst hem
     al: komt een app op, dan draait de switcher uit de plan-regio);
   - **M4** verifieert de flip zelf op echt silicium — Chainload, SelfImage en
     de node-watchdog liggen er al, en het is het board waar een reis naar de
     machine het duurst is.

## Gedeelde naad, twee architecturen

De flip zelf (`flip.go`) is gedeeld; per architectuur staat er alleen wat er
écht verschilt (`arch_arm64.go` / `arch_riscv64.go`):

| vraag | arm64 | riscv64 |
|---|---|---|
| welke code voert een app-core uit? | `el2.BlobSymbols` (3 blobs) | `mmode.BlobSymbols` (1 blob) |
| wie kopieert hem naar de plan-regio? | `kern/stage2` | `kern/slots` (cage_riscv64) |
| hoe springt de kern? | HVC #2 → handler op de revoke-vectoren (EL1-MMU uit, EL2-entry) | `JALR` in M-mode (`mie` uit, `fence`+`fence.i`) |
| wat krijgt de nieuwe kern mee? | DTB-pointer in x0 | niets (de FSBL geeft er ook geen) |
| extra node-cores? | `smp.NodeStarted()` | altijd 0 (één HOP-hart) |

## Wat er staat (arm64)

| onderdeel | plek |
|---|---|
| NAT-overdracht | `net/hopswitch/handoff.go` (`SnapshotNAT`/`RestoreNAT`) |
| bundel bouwen | `image/mkkernel` (`-elfreloc`, `-flipabi`) |
| bundel parsen | `metal/kern/kernflip/bundle.go` |
| plaatsen + springen | `metal/kern/kernflip/flip_arm64.go` |
| handoff-blob | `metal/kern/kernflip/handoff.go` (v2, met generatie-teller) |
| adoptie-stand | `metal/kern/kernflip/adopted.go` |
| slot-overdracht | `metal/kern/slots/adopt.go` |
| venster lenen | `slots.BorrowKernWindow` / `ReturnKernWindow` (partmem.go) |
| switch-code-kopie | `stage2.installSwitchCode` (arm64) / `cage_riscv64.installSwitchCode`, beide op `layout.SwitchCodePA` |
| chainload-sprong | `stage2.ChainloadEL2` (arm64) / `slots.ChainloadM` (riscv64) |
| regressie | `image/qemu-run.sh flip` + `cmd/hopos-embed/flip_virt.go` |
| bundel per board | `image/flip-bundle.sh <board>` |
| productie-bedrading | hop `internal/agent` POST /flip → `agentboot.Options.OnFlip` → `cmd/hopos/main.go` (flipRequest) |
| arch-naad | `kern/kernflip/arch_arm64.go` / `arch_riscv64.go` |
| agent-state | `xinix00/hop`: `internal/agent/handoff.go` + `agentboot.Options` |
| agent-haak | `kern/kernflip/agentstate.go` (`UseAgentState`) |

## Begrenzingen v1 (bewust, en allemaal afgedwongen)

De flip weigert liever dan half te slagen — een geweigerde flip kost niets, de
node draait door op de zittende kern. Wat hij weigert:

- **Node-SMP actief** (`smp.NodeStarted() > 0`): de extra node-cores draaien
  goroutines van de vertrekkende kern. Flip vereist `hopos.cores=1`; quiesce is
  v2.
- **Een SMP-app** (cores > 1). Bij het narekenen bleek de boekhouding te
  passen — `Cores` staat in het slot-record, de secundaire cores draaien
  app-code op hun eigen mailbox in de plan-regio, en `CtrlSMPTramp` wijst sinds
  de blob-verhuizing naar de plan-kopie. Maar er is nooit een SMP-app dóór een
  flip gehaald, en een onbewezen aanname hoort niet in dit pad.
- ~~Een task met gemounte volumes~~ — **mag sinds 01-09**, en de oude reden was
  verkeerd geredeneerd. Klopt: hopfs overleeft de flip niet. Maar hij overleeft
  een **reboot** evenmin — hij is bewust vluchtig (`kern/hopfs`: "géén
  persistentie … bij boot is alles per definitie leeg", de bron is S3). De flip
  maakt het dus niet erger dan de bestaande update-weg; hij maakt het alleen
  zichtbaar, omdat de app blijft leven terwijl zijn volume leeg wordt. Wat een
  geadopteerde app nodig heeft is niet zijn oude inhoud maar zijn mount-PUNTEN,
  anders schrijft hij vanaf nu in het niets. Die gaan mee, en de adoptie maakt
  de dirs opnieuw aan — net als `armSlot` bij een gewone start.
- **Een nieuwe kern met andere switch-code**, zolang er bewoners leven: de som
  over de drie EL2-blobs moet gelijk zijn aan die van de zittende kopie, want
  daar staan geyielde cores letterlijk in. Ongelijk = geen flip; met een lege
  node mag het wel.

Wat er wél verloren gaat en niet geweigerd wordt:

- **De neighbor-cache**: de switch leert zijn L2-next-hops binnen één
  ARP-ronde terug, en een verkeerd overgenomen next-hop is erger dan een korte
  leerpauze. De gateway-MAC gaat wél mee (zeven bytes, en zonder hem sneuvelen
  de eerste uitgaande frames terwijl er geARP't wordt).
- ~~Agent-taskstate~~ — **gaat mee sinds 01-09**, als JSON. Zie hieronder.
- **M4**: RVBAR is vergrendeld op het oorspronkelijke bootobject — het
  koude-start-stubje moet daar bereikbaar blijven; na de flip draaien alle
  cores al (geadopteerd), dus dit raakt alleen het pad na een echte reboot.

## Waarom lenen (en niet ping-pong of copy-over-self)

- De oude kern blijft leven tot aan de sprong: plaatsen/verifiëren/blob
  schrijven gebeurt met een dráaiende node; een mislukte plaatsing kost niets.
- Geen purgatory: bestemming en uitvoerende code overlappen nooit.
- Nul permanente RAM-kosten: het geleende venster is gewoon een
  pool-allocatie; het oude venster gaat terug de pool in (minus de vaste
  DMA-carve-outs van het board, die blijven gereserveerd).
- De pool-trim gebeurt generiek: elke kern haalt bij boot zijn **eigen**
  `runtime.MemRegion()` uit de pool (dat dekt zowel de eerste boot op het
  board-venster als elke latere flip-positie).
