# Release-logboek

Leidend document: [afrondingsplan](release-afronding.md). Elke wijziging hieronder hoort bij een stap daarvan. **Afgetekend** betekent dat het genoemde bewijs bestaat; het zegt niets over nog open hardwareproeven. Oude resultaten blijven historisch bewijs wanneer de kandidaat verandert.

## Opdracht en huidige stand

Derek heeft op 6 september de twee stappen vóór hardware opgedragen: resterende codepunten sluiten (planstap 2) en één testbare kandidaat maken (planstap 3). Daarna plaatst Derek **2.2.0** voor de laatste hardware-ronde. Eerdere interpretatie dat beide nodes al de uiteindelijke 2.2 draaien is hiermee vervallen. In deze fase geen hardware-deploy of flash.

| Planstap | Status | Bewijs / restant |
| --- | --- | --- |
| 1. Framework tegen visie | Afgetekend voor de uitgevoerde codepaden en lokale regressies | [Contract](framework-contract.md), [reparaties](../reviews/2026-09-06-framework/fixes/README.md). Board-/drivergrenzen gaan naar stap 2; hardwarebewijs blijft apart. |
| 2. Codepunten sluiten | Afgetekend voor code/host | HOP v1.0.1 geïntegreerd; NIC-, opslag- en overige driverreviews uitgevoerd, concrete fixes getest. L04–L08. |
| 3. Kandidaat bouwen | Afgetekend | Definitieve bron-/bundlehashes, zeven builds, hostgates en relevante QEMU-proeven slagen. L09–L10. |
| 4. Hardware | In uitvoering; bevindingen open | H0–H6 op 2.2.0; deelbewijs L12–L13. Geen volledige hardware-aftekening. |
| 5–6. Eindaftekening | Open | Volgt na hardware en eventuele gerichte reparaties. |
| Vervolg: fysieke netwerk-IRQ | Gepland na de docs, voor een volgende puntrelease | Geen blokkade voor de huidige afronding; besluit L21. |

## Logboek 6 september 2026

### L01 — bestaand bewijs afgetekend (planstap 1)

- Frameworkeigendom, start/stop, geheugenwissen, ringgrenzen, SMP-context, wekpad en adoptie zijn gerepareerd. Hosttests en zeven kernelbuilds zijn uitgevoerd; exacte scope en logs staan in het reparatieverslag.
- QEMU lifecycle, geforceerde yield, gedeelde buur, SMP, hergebruik en opeenvolgende flips slagen op de vastgelegde snapshots.
- Netwerkacceptatie: twee blijvende TCP-verbindingen over drie flips, ieder 24 rondes / 864 bytes. Echte agentacceptatie: dezelfde appbootmarker met oplopende teller. [Bewijs](../reviews/2026-09-06-framework/acceptance/README.md).
- Log-adoptiepatch: oorspronkelijke code faalt op ontbrekende logroute; patch slaagt in hostregressie met race detector en echte QEMU-agent. Best-effort logs herstellen; verliesloze logging tijdens flip is niet beloofd. Nog niet geïntegreerd in productie-dependency.

### L02 — uitvoering vóór hardware gestart (planstappen 2–3)

Drie afgebakende driverdoorlopen: NICs en IRQ-bedrading; NVMe/PCIe/RTKit; overige drivers en optionele GUI. Parallel integreert de hoofdtaak de bewezen agentfix en bereidt de kandidaatbouw voor. Alleen concrete correctness-fouten worden gerepareerd. Geen nieuwe functies of algemene infrastructuur.

Nieuwe bevindingen, besluiten, gewijzigde bestanden en testuitkomsten worden hieronder toegevoegd vóór het betreffende punt wordt afgetekend.

### L03 — fysieke netwerk-IRQ uitgesteld (scopebesluit Derek)

Derek: “zullen we de IRQ voor alle boards wel even bewaren, dat is echt een test voor later”. Alleen QEMU heeft nu de netwerk-IRQ aangesloten; de fysieke boards blijven voorlopig pollen. We voegen in deze ronde geen board-IRQ-drivers toe en blokkeren de kandidaat hier niet op. NIC-buffercorrectness en het bestaande logische wekprotocol blijven wel onderdeel van stappen 2–4. De visie blijft ongewijzigd, met deze expliciete tijdelijke uitzondering.

### L04 — agentlogfix geïntegreerd (planstap 2, afgetekend)

De geteste patch is gepubliceerd als `github.com/xinix00/hop v1.0.1`, commit `b565e2e` (alleen de log-adoptie en de regressietest). `metal/go.mod` en `go.sum` gebruiken deze gepubliceerde versie; downloaden is geslaagd. Geen lokale HOP-replace in de release. [Runner-/agenttests](../reviews/2026-09-06-framework/candidate/evidence/hop-tests.log) slagen; de gerichte adoptietest slaagt ook met `-race`. Het eerdere QEMU-bewijs testte dezelfde patch via een tijdelijke workspace. Stap 3 herhaalt de integratie met de gepubliceerde dependency en de uiteindelijke driverwijzigingen.

### L05 — concrete driverbevindingen in behandeling (planstap 2)

- NIC: RX-lengtes moeten binnen het werkelijke DMA-buffer blijven; bij tg3 werd een buffer al aan hardware teruggegeven vóór de kopie klaar was.
- Opslag: NVMe mag na onzekere completion dezelfde buffer/queue niet voor een nieuwe opdracht gebruiken. Apples RTKit-allocator overlapte het huidige NVMe-DMA-venster.
- Overige drivers: bestaande GIC-routering heeft een verkeerd registeroffset; enkele timeoutpaden geven commandogeheugen te vroeg opnieuw uit. Dit gaat over bestaande drivers, niet over de uitgestelde aansluiting van IRQ op extra boards.

De verantwoordelijke driverreviews leggen de exacte fixes, gerichte regressies en resterende hardwarevragen vast voordat deze punten worden afgetekend.

### L06 — NIC- en opslagcode afgetekend (planstap 2)

[NIC-review](release-nic-review.md): alle zeven Receive-paden zijn binnen hun DMA-buffers begrensd; tg3 kopieert vóór teruggeven, GEM weigert onvolledige frames, virtio valideert de volledige descriptor-ID. Zeven regressiefixtures falen tegen de oorspronkelijke drivercode en slagen met de fixes. MDIO en switchtests slagen. Fysieke IRQ-aansluiting blijft uitgesteld volgens L03.

[Opslagreview](release-storage-review.md): NVMe bewaart onzeker DMA-eigendom na timeout, controleert completion-ID en bounds, reset vóór hergebruik; Apples RTKit-gebied ligt buiten het volledige NVMe-cacheblok. Gerichte NVMe-/RTKit-/allocatorproeven slagen. Warme ANS-firmwareoverdracht en fysieke performance blijven hardwarebewijs.

### L07 — gezamenlijke gates uitgevoerd (planstap 3)

De [framework-hosttests](../reviews/2026-09-06-framework/candidate/evidence/framework-host.log) en [bestaande host-/TamaGo-buildgate](../reviews/2026-09-06-framework/candidate/evidence/host-build-gate.log) slagen met HOP v1.0.1 en de gezamenlijke fixes. De gate omvat kale/GUI-varianten, Apple met VHE/highram en RISC-V met embedvarianten. Een brede standaard-hostaanroep van alle drivers strandt uitsluitend op DVFS, dat TamaGo `runtime/goos` nodig heeft; dit pakket wordt door de targetbuilds getoetst. De toepasselijke hostpackages worden afzonderlijk met de GUI-tag uitgevoerd; de eerste foutlog blijft bewaard. Bundelbouw en kandidaat-QEMU volgen nog.

### L08 — overige drivercode afgetekend (planstap 2)

[Overige-driverreview](release-aux-driver-review.md) bevat de volledige inventaris, inclusief optionele GUI en standaard uitgeschakelde SMC. Bestaande GIC-routering, USB-eigendom na timeouts, HID-rollover, mailbox/PCC-hergebruik, SMC-request-ID en framebuffer-/consolegrenzen zijn hersteld. De [gezamenlijke toepasselijke driverhosttests](../reviews/2026-09-06-framework/candidate/evidence/driver-host-supported.log) slagen met `-tags gui`. Geen extra lock of worker. DVFS houdt zijn beleidsstatus pas bij na bevestigde klokwijziging; dit wordt op target gebouwd.

### L09 — kandidaatcontrole en laatste bronwijziging (planstap 3)

De eerste zeven bundels bouwden alle, zonder bronwijziging tijdens die bouw. Een controle daarna vond één late wijziging: `metal/driver/dvfs/dvfs.go`. Die eerste set is daarom tussentijds bewijs, geen definitieve kandidaat. De builder maakt een nieuwe set na die laatste wijziging. De framework-QEMU en netwerk-/agentacceptatie van dezelfde bron vóór deze uitsluitend Pi-DVFS-wijziging slagen; de nieuwe exacte virt-bundel wordt bovendien rechtstreeks als boot- en flipbeeld in de laatste agenttest gebruikt. Hiermee wordt niet alleen een herbouwde testkopie afgetekend.

### L10 — definitieve kandidaat afgetekend (planstap 3)

[Kandidaatverslag en reproduceerbare scripts](../reviews/2026-09-06-framework/candidate/README.md). Alle zeven bundels zijn opnieuw gebouwd na de laatste wijziging; bronhashes vóór/na en bij de afsluitende controle zijn gelijk. Alleen Pi 4/5-bundels veranderden door DVFS; de virt-bundel bleef gelijk. De lokale artifacts staan in `out-release/2.2.0-candidate`, met NOCFG als controlebuilds; geen fysieke node heeft deze ronde een nieuwe kernel gekregen.

De exacte virt-bundel met SHA-256 `493028fd806154fecef4c4776778518c770380e8221a117bf6451a316b89958d` is als ELF geboot en daarna via de echte agent geflipt. [Bewijs](../reviews/2026-09-06-framework/candidate/evidence/agent-exact/artifact-proof.json): `AGENT_LOG_FLIP_ASSERTIONS_PASSED`, taak `5DTQM9R2B7QV50J0`, bootmarker `30412336993` ongewijzigd, voortgang tot 3670, restart_count 0 en verse logs na de flip. De afzonderlijke netwerkproef hield twee TCP-verbindingen over drie flips; beide 24 rondes / 864 bytes. Framework-lifecycle/SMP/sharing/hergebruik slaagt eveneens.

De review- en bewijsbestanden zijn niet langer gitignored, zodat ze met de code kunnen worden opgenomen. De OS-werkboom is niet als release gepubliceerd; alleen de afgesproken HOP-dependencyfix is gepubliceerd. **De twee stappen vóór hardware zijn hiermee afgerond.** Volgende actie: Derek plaatst 2.2.0 met de juiste boardconfig, daarna H0–H6. Uitgestelde board-IRQ-aansluiting blijft buiten die aftekening; geen nieuwe reviewronde zonder concrete bevinding.

### L11 — volledige docs-overhaul toegevoegd (opdracht Derek, planstap 5)

Derek vraagt een totale overhaul van de verouderde docs nu het contract duidelijk is. Opgenomen als expliciete opdracht én eindvoorwaarde in stap 5: inventariseren, één ingang/leesvolgorde, herschrijven tegen contract/code/hardwarebewijs, dubbelen opruimen, historie apart houden en voorbeelden/links controleren. De twee stappen vóór hardware blijven afgetekend. Deze toevoeging wordt na de hardware-ronde uitgevoerd en is nog niet als afgerond gemarkeerd.

### L12 — hardwaredeelbewijs en open bevindingen (planstap 4)

[Bewaard hardwarebewijs](../reviews/2026-09-06-framework/hardware/) bevat de uitkomsten per node en de gebruikte hostscripts onder `scripts/` als tekst. Deze scripts gebruiken lokale fixturepaden.

- M4 en LicheeRV: twintig start/werk/stopcycli slagen, met terugkeer van gerapporteerde geheugen- en coreclaims naar de baseline (`h1-cycles.json`). De aparte hardwareproef voor schoon geheugen bij een nieuwe eigenaar ontbreekt nog; H1 is niet volledig afgetekend.
- Sharing zonder `core-class`: buur blijft lopen bij stop/vervanging; computechecksums en tien wake-observaties slagen. M4 dedicated SMP voert de tweewerkerproef uit. Niet alle fysieke idle-/wakegevallen zijn hiermee afgetekend.
- **Open plaatsingsfout:** `sharegroup` met `core-class=big` leidt tot onterechte plaatsingsweigering. De agent behandelt kooinummers hier als fysieke coreclaims. Bewijs: `h2-class-combination-failure.json`. Nog geen reparatie afgetekend; de geslaagde sharingproef zonder klasse sluit dit punt niet.
- H5 vóór flips: drie netwerk-contentproeven op beide nodes en drie private-filesysteemproeven op M4 slagen. LicheeRV heeft geen toepasselijk NVMe-volume. Dit is geen herhaling van de genoemde 4000 MB/s-benchmark en geen vergelijking met code vóór de fixes.
- **H4 niet geslaagd:** de hostsuite eindigt met `ConnectionResetError` op de blijvende appverbinding ([foutlog](../reviews/2026-09-06-framework/hardware/h4-failure.log)). De latere inventaris toont op LicheeRV alleen welcome; de eerdere testapps ontbreken. M4 meldt de drie testtaken nog als running. Derek heeft nadien bevestigd dat hij LicheeRV schoon heeft geflasht: alleen welcome is daar de bedoelde uitgangstoestand. De verdwenen testtaken zijn daarom geen foutbewijs. De verbindingsreset blijft een onbesliste meting en wordt opnieuw getoetst. Geen drie-flips-aftekening; herhaalde consolebacklog alleen bewijst geen reboot.

H4-herstel, H5 na flips, ontbrekende H1/H3-deelproeven en H6 blijven open. Fysieke netwerk-IRQ blijft uitgesteld volgens L03.

### L13 — extra hardwareadressen bevestigd (planstap 4)

Derek bevestigt Pi 4 `192.168.1.144`, Pi 5 `192.168.1.193` en Radxa `192.168.1.241`. Alle vijf nodes antwoorden met HTTP 200 op `/health`, `/capacity` en `/tasks`. De drie extra boards melden elk drie appcores en een draaiende welcome-app. Per node is de uitgangstoestand bewaard in `node-inventory-20260906.json`. Deze inventarisatie heeft geen taken gestopt en geen kernel geflipt; compatibiliteit en H1–H6 op de extra boards zijn nog niet afgetekend.

### L14 — schone LicheeRV bevestigd; hardwareproeven hervat

Derek bevestigt de schone flash van LicheeRV en vraagt verder te gaan. We herhalen de flipproef vanuit deze uitgangstoestand. De eerdere reset wordt niet zonder onafhankelijke reproductie aan een OS-fout toegeschreven.

### L15 — extra boards: lifecycle en sharing/SMP uitgevoerd

Na de inventarisatie zijn de welcome-jobs op Pi 4, Pi 5 en Radxa gestopt. Alle drie doorstaan twintig start/compute/stopcycli met nul gerapporteerde geheugen- en coreclaims na iedere stop. Daarna draait op ieder board één tweecore-SMP-app naast twee gedeelde bewoners op de resterende core. De computechecksums kloppen; vervangen van één gedeelde bewoner laat de andere met dezelfde bootmarker doorlopen. Bewijs onder [hardware/resume](../reviews/2026-09-06-framework/hardware/resume/), per board `h1-cycles.json`, `h2-smp-compute.json` en `h2-neighbor-survives.json`. Hardwaregeheugenwissen, de volledige wakeproef, flips en H6 op deze extra boards blijven open.

### L16 — herhaalde flips: onderscheid tussen meetfout en productgedrag

- M4: een verse console toont generatie 1 met alle drie oorspronkelijke bewoners. De oude consoleverbinding bleef hangen en miste die boottekst. De eerste meetmethode kon daardoor een geslaagde adoptie niet bevestigen.
- M4: generaties 2 en 3 slagen inclusief dezelfde TCP-socket, appbootmarkers, taakidentiteit en verse agentlogs (`resume/m4/h4-flips.json`). Bij generatie 4 liep de vernieuwde consolemeting tegen de lezerslimiet; geen volledige H4-aftekening.
- M4: een volgende afzonderlijke poging krijgt wel een echte reset op de app-TCP-verbinding. De nieuwe console toont generatie 5 met alle drie bewoners en herstelde taken (`confirmed/m4/console-after.log`, `after-tasks.json`, `h4-failure.json`). **Apps behouden en verbinding behouden zijn dus afzonderlijke uitkomsten; H4 blijft open.** De code activeert netwerkontvangst vóór opslaginitialisatie, slotpublicatie en NAT-herstel. Dat is een concrete verdachte voor een tijdelijke verkeerde aflevering aan de node-stack; de precieze resetoorzaak is nog niet met een pakkettrace bewezen.
- LicheeRV: de herhaling ná Dereks bevestigde schone flash eindigt met een timeout tijdens de bundeldownload; een latere verse console toont een nieuwe koude boot om 15:16:03 UTC en alleen welcome. Dit is nieuw bewijs uit de herhaling, niet de eerder door Derek verklaarde flash. Geen geslaagde adoptie aangetoond. Bundeldownload/geheugenpiek wordt onderzocht; OOM is nog geen bewezen oorzaak.

### L17 — stille console geeft losgekoppelde lezer weer vrij

Concrete bevinding uit L16: de bestaande leesgoroutine sluit de socket bij EOF, maar de uitvoerlus blijft bij een lege console doorlopen en houdt haar lezersclaim vast. `metal/kern/conport/conport.go` laat de bestaande lus nu ook eindigen op die sluiting, via één atomic-bool. Geen nieuwe mutex, goroutine of pollinglus. De gerichte regressie faalt tegen de oorspronkelijke code en slaagt met de wijziging, ook met de race detector (`resume/conport-before.log`, `resume/conport-after.log`). Deze fix is nog niet op hardware geflipt; productiecode, herbouw en hardwarebewijs blijven daarvan afzonderlijk herkenbaar.

De hardwarebewijsmap is expliciet uitgezonderd van gitignore, zodat deze nieuwe aftekeningen en mislukte proeven daadwerkelijk met de wijzigingen kunnen worden bewaard. H6 en eindaftekening volgen pas na de open correctness-punten; er is nog geen releaseklaarverklaring.

### L18 — console-targetbuilds en M4 I/O na flips

De consolefix bouwt als TamaGo-package voor arm64 en riscv64. Hostregressie met race detector slaagt; geen gewijzigde kernel is hiermee op een board geplaatst.

Op M4 slagen na de flips opnieuw drie 1 MiB-netwerkecho's en drie 16 MiB private-filesysteemproeven: bytes/hash kloppen, bestanden zijn verwijderd en de appbootmarker bleef gelijk. Bewijs: `hardware/m4/h5-*-after-flips.json`. Leestijden blijven circa 123 ms. De eerste schrijfronde na flips duurt circa 210 ms tegenover circa 9–10 ms vóór flips; de twee volgende liggen weer rond 9 ms. Dit verschil is nog niet verklaard en wordt niet als afgetekende performancepariteit gepresenteerd. Netwerkmetingen hebben ruime spreiding. Geen ruwe NVMe-/4000 MB/s-claim.

### L19 — netwerkclaims blijven geldig tijdens adoptie; gerichte hardwareherhaling gestart

De nieuwe kernel liet externe RX toe vóór de apppublicaties/NAT hersteld waren. Verkeer naar een overgedragen apppoort kon daardoor tijdelijk naar de node-stack gaan. De switch houdt nu die bestaande poortclaims vast tijdens adoptie: gepubliceerd appverkeer en antwoorden op overgedragen NAT-poorten worden in dat venster geclaimd en gedropt, zodat TCP kan herhalen in plaats van een node-reset te ontvangen. Na slot- en NAT-herstel wordt de tijdelijke verzameling vrijgegeven. Geen nieuwe mutex, worker of pakketqueue; de bestaande switchlock blijft de eigenaar.

De switch-/console-hosttests slagen. De gerichte nieuwe adoptietest slaagt met de race detector. Een brede race-aanroep liep op een bestaande `checkptr`-beperking van de host-ringfixture; dat is geen geslaagde brede race-aftekening. De wijziging plus consolefix zijn in drie afzonderlijke configvolle Apple-bundels gebouwd (`apple-g11` t/m `g13`); de hardwareherhaling loopt en wordt pas na resultaat afgetekend.

### L20 — M4: drie flips met de reparaties en beheer daarna geslaagd

Met beide reparaties slagen generaties **6, 7 en 8** op de M4. De hostsuite houdt exact dezelfde TCP-socket (`192.168.1.208:58262`) over alle drie sprongen; de bootmarkers, taak-ID's, starttijd en restart_count van de twee gedeelde bewoners en de SMP-app blijven gelijk, voortgang stijgt en verse agentlogs komen terug. Bewijs: [fixed/m4/h4-flips.json](../reviews/2026-09-06-framework/hardware/fixed/m4/h4-flips.json). De derde bundel had eerst een expliciet gemelde dial-timeout vóór downloaden/springen; die poging is herhaald terwijl dezelfde appverbinding open bleef. Een verse consoleverbinding bevestigde generatie 8 voor de actieve hostsuite, omdat de eerdere consoleverbinding opnieuw geen EOF gaf. Geen stil weggelaten downloadfout.

Daarna slagen twaalf opeenvolgende consoleverbindingen zonder de lezerslimiet te vullen. Dedicated SMP stoppen geeft aantoonbaar 192 MiB en twee coreclaims vrij; opnieuw starten levert weer twee werkende workers. Eén gedeelde bewoner vervangen laat de buur met dezelfde bootmarker doorlopen. [Beheerbewijs](../reviews/2026-09-06-framework/hardware/fixed/m4/h4-post-flip-management.json), `console-reader-reuse.json` en `fixed/post-fixed.log`.

De gerichte adoptietest faalt zonder de nieuwe ontvangstclaim en slaagt met de fix, ook met de race detector. De volledige bestaande host-/TamaGo-gate slaagt met de wijzigingen (`resume/updated-host-target-gate.log`). Bundlehashes en overeenkomst tussen gewijzigde productiebron en de gebouwde kopie staan in `fixed/flip-build-manifest.json` en `fixed/changed-source-proof.json`.

Dit sluit de getoetste M4-inbound-appverbinding en het beheerpad na drie flips. Het bewijst niet alle uitgaande NAT-scenario's of onbeperkte uptime. Open blijven onder meer LicheeRV-download/uitval (UART-toegang gevraagd), class-constrained sharing, hardwaregeheugenwissen, volledige wake-/I/O-aftekening, de overige boardflips, H6 en de docs-overhaul. De release is nog niet volledig afgetekend.

### L21 — fysieke netwerk-IRQ na de docs, als volgende puntrelease

Derek: “zet er maar op (IRQ) NA de docs :) het wordt gewoon een nieuwe punt release”. De huidige polling is volgens hem snel genoeg; de vervolgstap richt zich vooral op zuiniger draaien. Het afrondingsplan bevat nu dit expliciete vervolg na de docs-overhaul, inclusief boardtests en het bijwerken van de documentatie bij de IRQ-wijziging. De huidige hardware-/correctness-aftekening blijft op polling; fysieke IRQ is geen nieuwe releaseblokkade. Geen versienummer of reeds gemeten energiewinst toegekend. L03 blijft gelden voor de huidige ronde; L21 legt de vervolgvolgorde vast.

### L22 — mogelijkheden en boardbewijs zichtbaar maken in de docs

Derek vraagt een “HIER ZIJN WIJ”-tabel: laten zien wat HopOS al kan en fysieke IRQ als gepland vervolg benoemen. De [werkstand](release-status.md) bevat nu een mogelijkhedenoverzicht en een hardwarematrix, met concrete M4-flipaftekening. QEMU heeft netwerk-IRQ al; de fysieke boards nog niet. Open verificatie blijft onderscheiden van ontbrekende functionaliteit. De tabel wordt bijgewerkt tijdens punten 1–3 en opgenomen in de docs-overhaul; deze toevoeging vervangt of onderbreekt de lopende hardware-/correctness-opdracht niet.

### L23 — fixes bundelen als 2.2.1; LicheeRV daarna hertesten

Derek kiest een normale release **2.2.1** voor de gemaakte fixes. Daarna testen we de boards die de update niet via de oude downloader konden ophalen; momenteel alleen LicheeRV. De eerder voorbereide losse hardware-reviewimage is daarmee niet de gevraagde vervolgstap. De nieuwe downloadcode is hostgetoetst en als target gebouwd, maar blijft op LicheeRV expliciet **wacht op 2.2.1 + hardwarehertest**. Punten 1–3 worden ondertussen op M4, Pi 4, Pi 5 en Radxa doorgezet. Dit scopebesluit verklaart de resterende RV-hardwareaftekening; het is geen geslaagde flipclaim voor dat board.

### L24 — aanvullende geheugen- en wakeproeven; nieuwe kandidaat nog niet afgetekend

Op M4, Pi 4, Pi 5 en Radxa slagen drie geheugenhergebruikcycli: drie afzonderlijke 1 MiB-vensters binnen de eigen partitie zijn eerst nul, worden met niet-nulpatroon gevuld en zijn bij hergebruik van dezelfde fysieke partitie opnieuw nul. De gedeelde buur houdt zijn bootmarker. Dit bemonstert 3 MiB; het is geen hardwarecontrole van iedere byte. Op dezelfde vier boards slagen tien rondes waarin beide gedeelde testapps zonder eigen periodieke voortgangstaak slapen en weer antwoorden. Bewijs: `hardware/completion/<board>/h1-memory-canary.json` en `h3-quiet-shared-wakes.json`.

De kandidaat met classplaatsing, exact-length download, consolevrijgave en adoptieclaims bouwt voor alle vijf boards; de host-/targetgate inclusief Apple slaagt. Ook uitgaande NAT-allocatie wordt tijdens adoptie tegengehouden onder de bestaande lock, totdat de oude mappings terugstaan. Gerichte regressies slagen. De RISC-V-flipbundel neemt nu tevens de cage-startstub mee, via dezelfde buildhelper als de flashimage.

De eerste kandidaatflip behoudt taken en beide getoetste inbound-sockets op M4 (generatie 9), Pi 5 (1) en Radxa (1). De volgende class-sharing/SMP-proef vindt een integratiefout: de hoofdadapter `envSlots` geeft de optionele fysieke plaatsingsvraag niet door. Daardoor blijven SMP-jobs wachten naast het gedeelde pool. Reparatie en herhaling volgen; classplaatsing is nog niet afgetekend.

**Pi 4: nieuwe open flipbevinding.** Download en SHA-controle slagen; de laatste console meldt een geleende 130 MiB-partitie op 0x21200000. Daarna zijn beheer en netwerkconsole onbereikbaar. Er is geen bevestigde nieuwe kernelgeneratie. Een handmatige herstart is gevraagd; oorzaak wordt onderzocht. Dit staat los van het eerdere RV-downloadprobleem en corrigeert de verwachting in L23 dat alleen RV nog niet kon updaten. Bewijs: `completion/pi4/h4-lift-console.log` en `completion-failure.json`.

M4 eerste write na deze flip: 210,34 ms, daarna circa 9 ms. GC-pauze slechts 54 µs; de oude interne system-TCP-verbinding wordt vervangen. De transportstatistiek telt alleen levende verbindingen en mag daarom niet als monotone tellerdelta worden gelezen. Een afzonderlijke metadata-aanroep vóór de volgende bulk-write moet de hersteltijd lokaliseren. Nog geen ruwe 4000 MB/s-aftekening.

### L25 — adapter gerepareerd; twee meetuitkomsten gescheiden

`envSlots` geeft nu `CanPlaceDedicated` expliciet door. De bestaande adapter staat in een afzonderlijk bestand om de daadwerkelijke methodedoorgifte hostgetoetst te houden; die regressie is toegevoegd aan `tools/test.sh`. Gerichte race-test, volledige host-/targetgate en vijftien nieuwe boardbundels (reviewmarkers 31–33) slagen. De HOP-dependencywijziging wordt nog als normale gepinde dependency geïntegreerd; de tijdelijke build-replace is geen release-oplossing.

M4 adopteert generatie 10 en Radxa generatie 2 met beide class-shared bewoners. Beide apps antwoorden nadien met dezelfde bootmarkers. De hostsuite verwachtte ten onrechte generatie 1 doordat haar beginsnapshot de volle consolebuffer afkapte vóór de generatieregel; deze twee timeouts zijn daarom meetfouten, geen mislukte adoptie. De mislukte suite-uitkomst blijft bewaard onder `completion/adapter-fixed`; de vervolgmethode leest de buffer volledig en gebruikt de bevestigde generatie.

**Pi 5 wordt na zijn tweede flip onbereikbaar.** De eerste flip naar generatie 1 was bevestigd; de tweede overgang is niet bevestigd. De bewaarde console is daar bovendien afgekapt vóór het verzoek en lokaliseert de uitval niet. Een herstart en eventuele bestaande UART-toegang zijn gevraagd voor beide Pis. Geen speculatieve driverwijziging. Het Pi4-geheugenvenster overlapt volgens de inventaris geen bewoners; zonder UART kan de laatste netwerkconsole niet onderscheiden tussen een hang vóór de sprong en een nieuwe kernel zonder netwerk. `completion/pi4/flip-review.json` legt die grens vast.

### L26 — M4 laatste fixes: class/SMP, drie bidirectionele flips en I/O geslaagd

M4 slaagt met de gerepareerde adapter: eerst twee `core-class=big` gedeelde bewoners plaatsen, daarna dedicated SMP; buurvervanging, tien stille gedeelde wake-rondes en computechecksums slagen. Daarna slagen generaties **11, 12 en 13** met dezelfde twee inkomende app-TCP-sockets én twee door apps gestarte uitgaande TCP-verbindingen zonder reconnect. Bootmarkers, taak-ID's, starttijden en restart_count blijven gelijk; verse logs komen terug. Na de flips geeft SMP stoppen opnieuw 192 MiB en twee coreclaims vrij; herstart en buurvervanging slagen. Bewijs: `completion/adapter-fixed/m4/h4-three-final-flips.json`, `h4-final-management.json` en de egress-serverregistratie.

De afzonderlijke metadata-aanroep direct na generatie 12 duurt **200,47 ms**; de daaropvolgende eerste 16 MiB-write duurt **9,19 ms**. Bij de andere twee flips, zonder voorafgaande metadata-aanroep, duurt de eerste write circa 212 ms. Daarmee is de eenmalige hersteltijd gelokaliseerd buiten de bulk-write, bij het heropenen van de interne systemverbinding; content/hash en opruimen blijven correct. Dit bewijst geen ruwe 4000 MB/s en geen performancevergelijking met een andere OS-versie.

HOP-classplaatsing is gepubliceerd als **v1.0.2**, commit `8d60d9b`, na de volledige HOP-tests en runner-racetests; de OS-dependency wordt normaal op die tag gepind. Er is geen OS-release gepubliceerd.

De Radxa-herhaling wees dezelfde bundel correct af als “already running”: alleen een ingebakken configmarker maakt op DTB-configboards de ELF niet anders. De markerbundels 31–33 waren daar bytegelijk. Voor de begrensde herhaalproef komen afzonderlijke testbuilds met uitsluitend een extra boottekst, vastgelegd onder `completion/radxa-markers`. Dit is een fixturecorrectie, geen productfix of mislukte kernelovergang.

### L27 — Pi-relocatiecorrecties voor de hertest

In `board/raspi/cpuinit_body.h` gebruikt cache-invalidatie voortaan de door flip gepatchte `RamStart/RamSize`, in plaats van het vaste koude laadbereik. De twee vroege foutvectortabellen gebruiken het reeds bestaande Apple-patroon met absoluut doeladres; de korte branch kon een verder dan 128 MiB verplaatste kernel niet bereiken. Geen nieuw mechanisme of worker. Pi4/Pi5-targetbuilds en beide FLIPABI2-bundelbuilds met relocatiediff slagen; gegenereerde instructies zijn gecontroleerd (`completion/pi-relocation-fix`).

Dit zijn aantoonbare relocatiecorrecties, **geen bewezen oplossing van de hardware-uitval**. De cachefout kan door de voorafgaande scrub worden gemaskeerd; de foutsprong breekt diagnose zodra een exception optreedt, maar bewijst niet waardoor die exception ontstaat. Beide Pis blijven wachten op herstel/gerichte hardwarehertest.

### L28 — Radxa drie flips en normale releasebuild afgetekend; overdracht 2.2.1

Radxa slaagt voor class-shared plaatsing vóór SMP, buurvervanging, tien stille wake-rondes en drie flips naar generaties **3, 4 en 5**. De twee inkomende én twee uitgaande TCP-verbindingen blijven werken, met gelijke appbootmarkers/taakidentiteit en verse logs. Daarna slagen vrijgeven van 192 MiB/twee SMP-coreclaims, herstart en buurvervanging. Bewijs: `completion/radxa-markers/radxa/h4-three-final-flips.json` en `h4-final-management.json`. Netwerk-contentproeven vóór/na slagen; geen NVMe-volume op dit board.

De uiteindelijke productiebron, inclusief Pi-relocatiecorrecties, bouwt met de normaal gepinde **HOP v1.0.2**, zonder tijdelijke replace of testmarker, voor alle zeven fliptargets. De volledige host-/TamaGo-gate inclusief Apple en de echte adaptertest slaagt. Bronmanifest, bundlehashes en bouwlogs: `completion/release-221-check/`. Dit is buildbewijs; het plaatst de Pi-/RV-fixes niet stilzwijgend op hardware.

**Overdracht voor 2.2.1:** fixes en normale dependency-integratie staan in de OS-werkboom; de OS-release/tag zijn niet gepubliceerd. LicheeRV krijgt volgens Dereks besluit de nieuwe release en daarna de download-/flip-/class-hertest. Pi 4 en Pi 5 wachten op herstart en een gerichte flipronde, liefst met bestaande UART voor de ontbrekende overgangsdiagnose. H6 (dertig minuten gecombineerd gebruik), docs-overhaul en eindaftekening blijven daarna in het draaiboek; fysieke IRQ volgt pas na de docs in een volgende puntrelease. Geen volledige hardware- of releaseklaarverklaring.

### L29 — Derek herstart beide Pis; gerichte hertest hervat

Derek bevestigt de herstart. Beide Pis antwoorden weer met alleen welcome. Per board volgt eerst één flip vanuit de koude 2.2.0-uitgangstoestand met live gedeelde bewoners en SMP naar de kandidaat met de Pi-relocatiecorrecties en gepubliceerde HOP v1.0.2. Alleen bij geslaagde overgang volgen classplaatsing en drie herhaalflips. Bij uitval geen blinde herhaling. De afzonderlijke testbundels hebben uitsluitend een extra boottekst om byte-identieke updates te vermijden; bewijs onder `completion/pi-final/`. LicheeRV blijft volgens L23 wachten op de normale 2.2.1-installatie.

### L30 — Pi-hertest opnieuw mislukt; geen vervolgflips uitgevoerd

Na Dereks herstart zijn op beide Pis de SMP-app en twee gedeelde bewoners gestart. De gerichte overgang naar bundel 51 met Pi-relocatiecorrecties haalt de bundel volledig op en verifieert SHA-256. Daarna loopt de bestaande appverbinding vast; een nieuwe kernelgeneratie is niet bevestigd. De laatste ontvangen console meldt het geleende 130 MiB-venster (Pi4 0x21200000, Pi5 0x25a00000). Beheer en netwerkconsole vallen weg. De vervolgflips 52–54 en class-herhaling zijn **niet uitgevoerd**. Bewijs: `completion/pi-final/<board>/h4-lift-console.log`, `completion-failure.json` en `suite.log`.

De Pi-relocatiecorrecties sluiten deze uitval dus niet. De uitgaande kernel draait tijdens de download/plaatsing nog de geïnstalleerde versie; een nieuwe-kernelcorrectie is op zichzelf geen bewijs over dat uitgaande pad. De netwerkconsole vertelt niet of de sprong al gemaakt was. Volgende gerichte diagnostiek vereist UART-uitvoer tijdens één overgang, of een afzonderlijk afgesproken koude installatie van de kandidaat met diagnose vóór een volgende flip. Geen extra herhaalpogingen of speculatieve driverfixes.

Stand punten 1–3: bronfixes, normale dependency-integratie en alle buildgates gereed; M4/Radxa definitieve sharing/SMP-, bidirectionele drie-flips- en beheerproeven geslaagd; vier ARM-boards hebben het geheugenhergebruik-/stille-wake-deelbewijs. M4 eerste-I/O-vertraging verklaard als eenmalig intern transportherstel; geen 4000 MB/s-hermeting. Pi4/Pi5-flipcorrectness blijft open; RV wacht op Dereks 2.2.1-installatie. Het logboek geeft dus geen volledige aftekening of vrijgaveclaim voor alle boards.

### L31 — watchdogverwachting expliciet getoetst aan het beleid

Derek wijst terecht op het ontbrekende automatische herstel: “maar de watchdog hoort dat te solven”. Beide Pi-consoles bevestigen vóór de flip de BCM-watchdog met 12 seconden timeout én `HOPOS_CANARY_LIVE`; hij stond dus niet simpelweg uit in de uitgangstoestand.

De bron bevat wel een concreet beleidsgat: `armBootGuard` blijft onbeperkt voeden totdat `nodeCanary` overneemt, en die blijft vóór zijn eerste geslaagde probe eveneens onbeperkt voeden. Een opstart die intern nog schedult maar nooit netwerk/agent krijgt, hoeft daardoor niet te resetten. Correctie: één gedeelde termijn van twee minuten voor beide blinde flip-opstartpaden, zonder verlenging bij overdracht; daarna geen voeding meer. Bestaande workers, geen extra lock/worker. Intentional reboot moet bovendien de vroege voedingslus stoppen. Hostregressie en een gecontroleerde hardwareproef op M4 volgen.

Dit verklaart een ontbrekende watchdoggarantie, maar bewijst niet welk pad de Pis bereikten. De uiteindelijke self-dial controleert de interne agentroute en dekt volgens het bestaande contract geen uitsluitend defecte NIC-ontvangst. Die grens blijft zichtbaar; een timerfix wordt niet gepresenteerd als bewezen Pi-herstel.

### L32 — watchdog-herstelproef slaagt; normale proef vindt klokfout in de correctie

De M4 boot met de testkopie die bewust na `armBootGuard` blijft schedulen zonder agentopstart. Hij komt autonoom koud terug met welcome na **165,48 seconden** vanaf het flipverzoek. Dat past bij twee minuten voedingsbudget, de 30-secondenhardwaretimer en bootduur. De blackbox droeg na reset nul bytes over: de specifieke stall-/expiry-markers zijn dus niet teruggelezen. Dit is waargenomen herstel met bekende fault-injection; geen directe timingmeting van iedere watchdogwrite.

De daaropvolgende normale flip vindt een fout in de eerste deadline-implementatie: `time.Now` volgt op TamaGo de boardtijd, en SNTP verandert tijdens boot die offset. Daardoor meldt een gezonde nieuwe kernel onmiddellijk `HOPOS_BOOT_GUARD_EXPIRED`. Deze kandidaat wordt niet als correct afgetekend. De deadline gaat naar de bestaande ruwe hardwareteller (`dev.Counter` met `idle.CounterHz`); geen nieuwe klokarchitectuur. Normale boot en de herstelproef worden daarna herhaald. Bewijs van de fout blijft onder `completion/watchdog/normal-clock-jump-failure.*`.

### L33 — ruwe teller: normale flip met SNTP en alle gates geslaagd

De watchdogdeadline gebruikt nu de bestaande `dev.Counter()` en de frequentie uit `idle.CounterHz()`; `time.Now` speelt bij het budget geen rol meer. Twee voedingspaden blijven één deadline delen. De gerichte hosttests met race detector slagen. Een normale M4-flip naar generatie 1 slaagt inclusief SNTP, geadopteerde welcome-taak en `HOPOS_CANARY_LIVE`, zonder vervroegde expiry. Dezelfde code bouwt voor alle zeven targets en doorstaat de volledige host-/TamaGo-gate (`completion/release-221-counter-check`). De gecontroleerde vastloopproef wordt nogmaals uitgevoerd met deze definitieve tellerimplementatie.

De uitgebreide reviewmap is momenteel weer gitignored; de eerdere uitspraken over automatisch meepubliceren gelden daarom niet voor de huidige werkboom. Een compacte aftekening is apart bewaard in [release-evidence/2026-09-06-hardware.json](release-evidence/2026-09-06-hardware.json), met de definitieve [host-/targetgate](release-evidence/2026-09-06-host-target-gate.log). De volledige lokale scripts, consolelogs en mislukte proeven blijven onder de reviewmap beschikbaar.

### L34 — definitieve watchdog: normale boot én automatisch herstel op M4 geslaagd

Met de ruwe teller slaagt eerst de normale flip inclusief SNTP, residentbehoud en levende canary. Daarna volgt dezelfde gecontroleerde vastloop na `armBootGuard`, met de scheduler nog actief en zonder agentopstart. De M4 komt **autonoom koud terug na 167,13 seconden** vanaf het verzoek, met welcome. Geen handmatige reset tijdens deze proef. Ook deze keer draagt de blackbox nul bytes over; de afzonderlijke stall-/expiry-logregels zijn niet teruggelezen. Het gemeten herstel en de vooraf vastgelegde testinjectie blijven herkenbaar van die ontbrekende interne trace.

Bewijs: `completion/watchdog-counter/hardware-result.json`, de twee bundlehashes, bronmanifest en de normale-flipcontrole. Compact meegenomen in [de hardware-aftekening](release-evidence/2026-09-06-hardware.json). De hardwaregeteste watchdogbron heeft SHA-256 `3c0dcae70241465677a1e5e1ad1f581b13c71d923361379e0064d164f063fac8`, gelijk aan de productieboom. Daarna is de M4 teruggezet op de normale kandidaat: flip/adoptie en canary slagen opnieuw, welcome antwoordt met HTTP 200.

Dit sluit het onbeperkte blinde voeden in het getoetste flip-opstartpad. De Pi-uitval blijft open: geen bewijs dat beide Pis in ditzelfde pad bleven staan, en een puur defect NIC-ontvangstpad valt buiten de interne canary. RV-hertest na installatie van 2.2.1 blijft volgens afspraak uitgesteld. De zeven normale targetbuilds, volledige gate en gerichte racetests zijn geslaagd; geen OS-release/tag gepubliceerd.

### L35 — 2.2.1 geplaatst; RV-hertest en Pi5-UART-diagnose gestart

Derek bevestigt de geflashte LicheeRV en daarna de aangesloten Pi5-UART. De werkboom is inmiddels gecommit als 2.2.1 (HEAD `e76ab82`, frameworkcommit `9bf3c74`). RV antwoordt op 192.168.1.150 met alleen welcome en de nieuwe watchdogbeleidsregels. De hertest gebruikt verse builds van deze bron, gepinde HOP-dependency en de bestaande node-ID/MAC; drie configmarkers onderscheiden de RV-bundels zonder productiecode-instrumentatie.

RV: eerst class-sharing, aanvullende geheugencanary en stille wake; daarna drie flips met beide TCP-richtingen, verse logs en vrijgeven/hergebruik. Geen SMP op de ene apphart en geen NVMe-volume. Bewijs onder `completion/rv-221`. Pi5 wordt afzonderlijk met UART `/dev/cu.usbmodem21302` onderzocht; één gecontroleerde overgang, geen impliciete aftekening voor Pi4 op basis van vermoedelijke gelijkenis.

### L36 — Pi5-reeks geslaagd; Derek geeft Pi4 voorrang op RV

Pi5 doorstaat op 2.2.1 met UART de definitieve generaties **7, 8 en 9** met exact dezelfde twee inkomende app-TCP-sockets over alle drie overgangen. De twee class-shared bewoners op core 1 en de SMP-app op cores 2/3 behouden bootmarker en taakidentiteit; tweewerkerchecksums kloppen na iedere flip. Generatie 9 had eerst een expliciete dial-timeout vóór de sprong; de herhaalde aanvraag slaagt terwijl dezelfde sockets open blijven. Daarna slagen buurvervanging, compute op alle drie bewoners en verse agentlogs. Bewijs onder `completion/pi5-uart-221`. Dit sluit de uitgevoerde Pi5-reeks; de eerdere uitvaloorzaak is niet onomstotelijk vastgesteld.

RV: class-sharing, drie canarycycli en tien stille wake-rondes slagen. De eerste flip valt uit, gevolgd door autonome koude terugkeer. Een aparte diagnose met console vooraf en getelde serverwrites valt eveneens uit: de server kan 131072 bytes naar zijn socket schrijven, daarna BrokenPipe; koude terugkeer wordt na 89,30 seconden waargenomen. Geen volledig opgehaalde bundel of nieuwe generatie bevestigd. Grote allocatie/heapgroei is een hypothese, geen bewezen oorzaak. Bewijs onder `completion/rv-221`; verdere diagnose wacht.

Derek: “Pi 4 eerst, hebben we die reeks gehad”. Pi4 antwoordt op de huidige beheercontrole niet. Herstart en verplaatsing van de UART van Pi5 naar Pi4 gevraagd. De Pi5-UART wordt vrijgegeven. Eerst volgt de Pi4-reeks; de eerdere vraag om RV-UART heeft nu geen prioriteit.

### L37 — Pi4 opnieuw bereikbaar; UART-bedrading eerst bevestigen

Derek geeft de Pi4 vrij voor de reeks. Beheer antwoordt met welcome; daarna zijn één tweecore-SMP-app en twee gedeelde bewoners gestart en de computechecksums gecontroleerd. De koude console toont nog het oude watchdogbeleid. De twee voorbereide 2.2.1-testbundels staan klaar.

De geopende UART heeft vooralsnog slechts een oude/onvolledige regel opgeleverd, niet de verse Pi4-jobuitvoer. Derek noemt de mogelijke omwisseling van RX/TX; omwisselen gevraagd, capture blijft open. **Nog geen flip uitgevoerd in deze reeks:** eerst een bruikbare seriële trace bevestigen, zodat een eventuele uitval deze keer gelokaliseerd kan worden.

### L38 — Pi4: drie flips, residentbehoud en vrijgeven/hergebruik geslaagd

Derek stelt een termijn van vijftien minuten en vraagt de UART-reader opnieuw in te stellen. Reader opnieuw geopend op 115200 8N1; geen leesbare verse uitvoer. Pi4 antwoordt wel met alle drie bewoners. Op uitdrukkelijk verzoek doorgetest met een vooraf geopende netwerkconsole; de ontbrekende seriële trace is geen reden meer om de functionele proef uit te stellen.

Generaties **1, 2 en 3** slagen, ieder op de eerste aanvraag, met exact dezelfde twee inkomende TCP-sockets door de gehele reeks. Taakidentiteiten en bootmarkers van de twee gedeelde apps en de tweecore-SMP-app blijven gelijk; computechecksums kloppen na iedere overgang. Maximale gemeten echo-RTT per overgang: **5,220 / 8,014 / 4,323 seconden**. Dit bewijst verbindingbehoud, geen onderbrekingsvrije latencygarantie. Uitgaande TCP is in deze Pi4-reeks niet meegenomen. De koude uitgangskernel logde nog het oude watchdogbeleid; de aangenomen kandidaten zijn van bronrelease 2.2.1, met alleen een geïsoleerde bootmarker om de bundels te onderscheiden.

Daarna slagen buurvervanging zonder herstart van de buren, SMP-stop met vrijgave van **192 MiB en 2048 CPU-shares**, volledige stop tot nul claims, en opnieuw eerst twee class-shared bewoners en daarna SMP op de overgebleven twee cores. Alle computechecksums en een exacte nieuw aangevraagde stdout-marker slagen. De eerste logassertie in het testscript las alleen de eerste SSE-regel (een normale opstartregel); gecorrigeerd door tot de specifieke verse marker te lezen. Geen productiefix nodig.

Bewijs: `completion/pi4-221/h4-final-*`, `flip-live-console.log`, `postflip-neighbor.json`, `postflip-smp-release.json`, `all-released.json`, `final-class-first-smp.json`; samengevat in de publiceerbare hardware-aftekening. De eerdere uitvaloorzaak blijft onbekend. Deze Pi4-reeks is afgetekend; RV-diagnose loopt parallel binnen de afgesproken termijn. H6, docs-overhaul en fysieke IRQ blijven in hun afgesproken volgorde staan.

### L39 — RV-UART bewijst de download-OOM; watchdog herstelt autonoom

Derek sluit de LicheeRV aan op dezelfde USBmodem. De opnieuw geopende reader op `/dev/cu.usbmodem21302`, 115200 8N1, geeft nu een volledige verse boot en fouttrace. Een gerichte controle geeft een kleine ongeldige bundel met correcte SHA; die wordt schoon afgewezen. Aangekondigde lengtes van 1 en 4 MiB met direct EOF geven eveneens een gewone fout. Bij **6.150.136 aangekondigde bytes en nul verstuurde payloadbytes** valt de node uit. Ook vanuit een verse koude boot, zonder de voorafgaande allocatiereeks, reproduceert dit.

De UART meldt exact: `runtime: out of memory: cannot allocate 8388608-byte block (8028160 in use)`, gevolgd door `fatal error: out of memory`. De stack wijst naar `runtime.makeslice` → `kernflip.readBundle` op `fetch.go:81`. Daarmee is deze reproductie gelokaliseerd op de grote downloadallocatie, vóór bodyread, relocatie en sprong. De eerdere hypothese is nu bevestigd als OOM; dit is geen bewijs van een DWMAC-bulkfout. De hardwarewatchdog brengt de node autonoom koud terug met welcome; ook die boot staat op UART.

De eerste correctierichting (64 MiB) is vóór implementatie verworpen: het vaste RV-plan houdt het koude kernvenster buiten de pool, zodat na één lening van 66 MiB geen tweede dergelijke lening meer past. De kandidaat wordt **48 MiB**, op hetzelfde adres, met **206 MiB app-pool** (126 + 64 + 16 MiB). Twee flipvensters van elk 50 MiB passen in de grote regio; de normale plaatsbaarheidscontrole blijft nodig naast bewoners. Geen nieuw bufferbeheer, geen extra worker of mutex. Een nieuw koud flashimage wordt voorbereid omdat de lopende 32-MiB-kernel de kandidaat al bij downloaden niet kan bereiken. **Nog geen hardware-aftekening voor deze correctie.** Bewijs: `completion/rv-length-control`, `completion/rv-221/uart.log`.

### L40 — correctierichting ingetrokken: download hoort in het nieuwe venster

Derek corrigeert terecht het ontwerp: de download moet, net als app-plaatsing, in het gereserveerde nieuwe slot/poolgeheugen landen; het kernelvenster hoeft daarvoor niet groter. De voorbereide 48-MiB-wijziging en bijbehorende budgettest zijn ingetrokken. Het gebouwde kandidaatimage is niet geflasht en uit de aangeboden artifactmap verwijderd. LicheeRV blijft op 32 MiB. De UART-OOM is bewezen, maar de oplossing wordt nu gericht op directe download naar gereserveerd poolgeheugen, met correcte vrijgave bij iedere fout en behoud van hash-/bundelvalidatie vóór de sprong. Dit vervangt de correctierichting uit L39; nog geen fix-aftekening.

### L41 — v2.2.2: bestaande pool-/plaatsingsfuncties hergebruikt; hardware morgen

De download reserveert nu via `BorrowKernWindow` het normale nieuwe kernvenster, kiest via `StageAddr` de tijdelijke plek bovenin, scrubt via `Scrub` en streamt daarheen met een vaste **32-KiB-buffer**. SHA-256 en de bestaande FNV-inhoudssom worden tijdens de download berekend. De bestaande bundelparser leest via `ReaderAt`; `place.Build` blijft de plaatsingsmeetlat. De bestaande device-reader is uit de slotcode naar `dev.ReaderAt` verplaatst en wordt door beide paden gebruikt. Geen grotere kernel, geen tweede allocator, geen extra worker of mutex.

De tijdelijke bron blijft binnen de eigen lening en onder de handoff-staart. De platte bestemming mag de bron niet overlappen; alle bundel-, ELF-, ABI-, resident- en relocatiecontroles blijven vóór de sprong staan. De bestaande lifecycle-afbakening houdt de lening exclusief; iedere terugkerende fout geeft haar eenmaal terug. Het byte-slice-pad voor ingebakken testfixtures blijft beschikbaar via dezelfde parser en plaatsing. LicheeRV blijft **32 MiB kern / 222 MiB app-pool**.

Validatie: hosttests, gerichte racetests voor kernflip/plaatsing/layout, volledige host-/TamaGo-gate voor alle boards, echte RISC-V-flipbundel met dual-link-relocatiecontrole en koud kaartimage slagen. Nieuwe tests dekken begrensde parserreads, een virtuele 64-MiB-ELF zonder die in te lezen, kapotte/afgebroken metadata, late ongeldige relocaties, hash-/lengtefouten, vaste downloadschrijfgrootte en fouten bij reserveren/schrijven. Een bredere losse slots-racerun strandt in de bestaande ownership-fixture op `checkptr` bij `dev.Clear`; de gewone slots-suite in de gate slaagt. Dat is niet als geslaagde volledige slots-racerun geboekt. Tweede code-review vindt geen blokkerende fout in eigendom, overlap of vrijgave.

Derek maakt **v2.2.2** en vraagt het hierbij te houden; de laatste hardwaretest volgt morgen op die release. Geen verdere wijzigingen of hardwareproeven gestart. De eerder geslaagde ARM-reeksen gelden voor hun geteste kandidaten; de nieuwe downloader heeft nog geen hardware-aftekening. Morgen eerst LicheeRV koud installeren (de oude downloader kan zichzelf niet door zijn OOM heen vervangen), daarna foutpad/vrijgave en de drie-flips-reeks met resident-/TCP-behoud, beheer en hergebruik. Vervolgens de resterende release-hardwarecontrole en H6 volgens het draaiboek. Docs-overhaul blijft gepland; fysieke IRQ blijft uitgesteld tot daarna.

### L42 — docs eerst; LicheeRV en laatste hardwarecontrole morgen erachter

Derek zet de volgorde expliciet om: nu documenteren, LicheeRV achter de documentatieronde voor morgen. Het draaiboek is daarmee bijgewerkt, inclusief de verouderde dependency-, Pi- en update-status. De functionele wijzigingen voor v2.2.2 blijven vaststaan.

De documentatieronde heeft een afgebakend begin en eind: inventariseren wat actueel, dubbel of historisch is; één ingang en leesroute maken; visie/contract, architectuur/lifecycle, sharing/SMP, drivers/boards, bouwen/installeren/flippen en beheer/testbewijs onderling consistent maken; de “Hier staan we”-tabel opnemen; voorbeelden, versies en links controleren. Gereed wanneer een nieuwe lezer het beloofde gedrag, gebruik en bewijs kan vinden zonder tegenstrijdige uitleg. Hardware-aftekeningen blijven tijdens het schrijven open waar zij nog ontbreken.

Daarna morgen: release-identiteit vastleggen, LicheeRV koud op v2.2.2, downloadfouten/vrijgave en opeenvolgende flips met resident-/TCP-behoud controleren, resterende hardware-/duurproef afwerken en resultaten toevoegen. Pas dan de eindaftekening. Fysieke netwerk-IRQ blijft het afzonderlijke vervolg na de docs en deze afronding.

### L43 — English documentation in docs/v2; structure before prose

Derek requests the replacement documentation under `docs/v2/`, preserving the existing version for comparison. He clarifies that the documentation must be English and factual, with usage, architecture, the shared caller/network path, board capabilities, and measurements. Approximately 4000 MB/s M4 writes is retained as Derek's measurement, with unrecorded test conditions left unspecified.

The initial Dutch drafts were withdrawn from the new documentation directory and retained as temporary working material. `docs/v2/STRUCTURE.md` now defines the reading order, page responsibilities, architectural thread, and completion criteria. The new folder is explicitly included by `.gitignore`. No product code or existing documentation pages were replaced. The remaining work is to write and check the English pages against this structure; hardware tests remain scheduled after the docs for 7 September.

### L44 — English v2 documentation and website index complete

The new set is under `docs/v2/`, alongside the unchanged previous documentation. Its 13 public pages cover getting started, apps and placement, operations, architecture, lifecycle, the shared network/system-call path, kernel flip, configuration, boards/drivers, development, measurements and status. `index.md` is the website entry; `menu.md` uses the current website viewer's section/link syntax. README, structure and inventory are repository/editorial material outside that menu.

The actual viewer source in the local `gethop.org` checkout was inspected and used to render the new menu and all 13 pages: default `index`, all index routes found, no missing menu pages or broken local links. Across all 17 Markdown files, local links resolve; two JSON blocks parse and eighteen shell blocks pass syntax checking. The documented Go app example builds for ARM64 and RISC-V with the existing TamaGo compiler against the current metal source. No app was deployed and no hardware test was run. Commands and reference fields were checked against the existing source and pinned dependencies; the new docs identify slot ABI 10 and flip ABI 2 separately from release v2.2.2.

Measurements include Derek's approximately 4000 MB/s M4 write result with its provenance and unrecorded conditions, separate from the review's latency and correctness results. The new status page preserves the open hardware acceptance of the new downloader. The old topics are mapped in `docs/v2/INVENTORY.md`; old pages and historical evidence have not been removed or replaced. No product code changed in this documentation round.

The current website still reads `main/docs/menu.md`; it has not been switched or published by this work. The v2 index/menu are ready for that later selection or promotion. The documentation pass is complete as a parallel version; the remaining acceptance work is the v2.2.2 hardware round on 7 September, followed by final sign-off. Physical IRQ remains the separate later task.

### L45 — documentation promoted; LicheeRV sharing investigation remains open

Derek ends today's source review and schedules debugging and the outstanding flip test for tomorrow. He reports failure when two apps share the LicheeRV app hart. The source review has not established the cause; no product fix or hardware acceptance is claimed. The recent wakeAt arithmetic correction, shared slot/context addressing, and register save/restore did not reveal an explanation. Follow up the ownership of CtxWake during forced stop, the mixed-writer control-page cacheline containing CtrlShared and CtrlRXDoor, and per-connection system-service memory use. These are review leads, not established causes of this failure. No UART or new hardware test was performed after the request to review code first.

At Derek's request, the previous documentation tree (including this logbook, the plan and evidence) is moved to docs/v1/, and the English v2 pages are promoted to docs/. Index and menu now occupy the website's existing main/docs/ entry points. Relative Markdown links are adjusted for the move; the repository README points to current usage pages. Previously ignored local review material remains ignored. Publication still depends on committing and pushing these changes; no deployment was performed.

Tomorrow: identify the running release and the two app builds, reproduce the shared-core failure and capture its actual failure path, apply the smallest supported correction, then perform the v2.2.2 pool-download/flip checks. Sharing acceptance remains open alongside the outstanding flip acceptance. Physical IRQ remains the later task.

### L46 — two website follow-ups for release day

Derek plans publication for 7 September and asks to put the website's LicheeRV status and developer-reported approximately 4000 MB/s M4 result on tomorrow's TODO. Both are added to the release plan and the website TODO: complete LicheeRV sharing/FLIP verification, reproduce and document M4 write throughput, then update the public wording from the results. No new measurement or hardware acceptance is claimed today.

### L47 — LicheeRV UART attached; two welcome residents pass the first reproduction checks

On 7 September, UART at `/dev/cu.usbmodem21302` captures the running LicheeRV at 115200 baud. Initial state: one welcome job, one app hart, 222 MiB pool, 32 MiB kernel window; the agent identifies itself only as `hopos-dev`, so the precise installed kernel artifact is not established. The welcome page identifies its app build as `2026.09.06-1805`, Go 1.26.4, riscv64.

The existing welcome job was changed to sharegroup `rv-release-share`, then a second welcome job was added on port 18080. Each requests 64 MiB and 1024 CPU shares; they occupy slots 2 and 3 on the single app hart. Both report `core: shared` and serve HTTP. Ten quiet-wake rounds, three neighbor stop/replacement cycles and 600 requests across four concurrent HTTP clients pass. The surviving welcome task keeps its identity through replacements; both task identities remain unchanged during the traffic check. No fault or panic appears in the captured UART for these checks.

This does not reproduce Derek's reported failure and is not a fix sign-off. The exact failing app pair and operation have been requested. No product code changed. Local evidence: `../reviews/2026-09-07-licheerv-sharing/` (`uart.log`, original jobs/tasks/capacity, submitted job definitions, `welcome-check.json`, `traffic.json`, boot-console capture). The original welcome definition is retained for restoration. Sharing investigation and FLIP acceptance remain open.

### L48 — exact Stulp sequence reproduces kernel OOM; one-buffer candidate ready

Derek supplies the existing root jobs: cloudflared-lean (32 MiB), Stulp (48 MiB), then stulp-plugins (126 MiB), all in sharegroup `huis`. Welcome and the synthetic neighbor are removed; the three definitions are submitted unchanged with three-second intervals. Secret environment values are read only for submission and are not copied into review evidence. UART records cloudflared-lean and Stulp starting, then kernel OOM during the plugin download: `leantls.readRecord`, with a failed 4-MiB arena allocation and 12,091,392 bytes in use. Both system-service goroutines retain 0x110000-byte input buffers and each connection also owns a maximum read-response buffer. This establishes memory pressure during startup, not a switcher fault.

An attempted candidate installation then exposes the old flip downloader on the initially installed kernel: `kernflip.readBundle` allocates the whole 6-MB bundle and fails on an 8-MiB allocation. No candidate is installed. The watchdog recovers the node. Derek subsequently reports flashing v2.2.2. The same three-job sequence reproduces kernel OOM again, now in `leanelf.File.section`/`symtab` during plugin placement (4-MiB arena allocation, 12,156,928 bytes in use). The exact initial kernel release was not exposed by its `hopos-dev` identity; the second run is identified by Derek's v2.2.2 flash and the changed stack addresses. These are two observed allocation sites under pressure, not proof that TLS or ELF parsing alone caused the total memory demand.

The first candidate merely made two connection buffers lazy; Derek asks to establish why they exist before settling on that fix. That candidate is superseded and was not installed. The current candidate uses one reusable connection work buffer. Requests are decoded before a read response reuses their storage; DecodeReq copies the path and OpRead does not consume request Data. The response is sent before reading the next request. Allocation grows only to the current message/read size. No new lock, worker, ABI change or larger kernel window. Small response encoding and TCP buffers remain; this does not claim zero kernel-side memory.

Validation: full host/TamaGo gate passes for all existing target variants. Targeted race checks pass. Tests cover small log/storage-less calls without maximum buffer allocation, maximum-to-small frame transitions, and real hopfs sparse-file reads reusing request storage across large/small/zero reads while preserving response identity and data. Hardware acceptance of the candidate remains open.

Candidate image: `metal/out/hopos-licheerv-single-buffer.img`, SHA-256 `3b5d74535e6ca29266e9b4104af058465b51b1598de767c6e57121cbbd02510c`. It retains the 32-MiB cold kernel and 222-MiB app pool. The exact three-job set requests 206 MiB; a relocated kernel consumes additional app-pool space, so this workload must first be tested from the cold candidate. No card was written. Local UART, both reproduction records, manifests and build/gate logs are under `../reviews/2026-09-07-licheerv-sharing/`. Next: cold candidate, identical three-job sequence, verify application behavior and sustained progress, then a separate FLIP test with enough free staging space. Public status must reflect those outcomes before sign-off.


### L49 — single-buffer candidate passes the original startup and replacement reproduction

Derek writes the approved candidate to the card and reports it running. From that cold installation, welcome is removed and the three unchanged root job definitions are submitted three seconds apart: cloudflared-lean 32 MiB, Stulp 48 MiB, plugins 126 MiB, sharegroup `huis`. All three start and remain running on the one app hart. The plugins are then stopped and replaced three times; both neighbors retain their task IDs and the agent reports app claims returning from 206 MiB to 80 MiB and back. UART contains no kernel OOM in these runs. This closes the reproduced startup allocation failure on this candidate, without changing idling, sharing, the app ABI tail or the cold kernel window.

Application logs reveal a separate configuration issue: the unchanged cloudflared origin and plugin attach address point to `10.100.0.2`, whereas this placement puts Stulp at `10.100.0.4`. Process survival is therefore not evidence of successful plugin attachment or a working tunnel origin. Those endpoints need the actual service or its published node ports.

### L50 — revised RV downloader and four live flips pass; cold-window limitation measured

At Derek's request, the application trio is removed temporarily and a 48-MiB hardwarecheck resident is installed. The corrected 6.16-MB bundles download into pool staging and produce generations 1–4, preserving the same resident task ID and application boot identity. The exact same host socket carries verified echo payloads across generations 1–2; a new socket carries them across generations 3–4. The first request for generation 3 fails before downloading with a TCP dial timeout to the local artifact server; the resident and management remain available. Retrying succeeds. This is four successful swaps with one separately recorded rejected request, not a clean three-flip single-socket acceptance. Maximum observed echo RTT is approximately 3.45 seconds during the swaps.

A full-bundle wrong-hash request and an identical-bundle request are subsequently rejected. App claims remain unchanged; a fresh app starts, serves status and stops afterward. The next successful destination loan also establishes that the temporary loan was released. No download OOM occurs with the revised path.

The destination windows alternate between `0x8dc00000` and `0x8ba00000` (34 MiB allocator reservations, including the handoff). They do not return to the original `0x84000000` cold window: that remains a board-plan hole. Thus the original 206-MiB workload no longer fits after these flips; the nominal 222-MiB capacity field must not be read as current placeable memory. Reclaiming or reusing the cold window remains a separate memory-plan issue. No allocator change is included here.

A subsequent, explicitly requested cold-restoration image with `hopos.reboot=1` lands as generation 5 but fails to reset: its early reboot call precedes the LicheeRV watchdog probe. UART explicitly reports `HOPOS_REBOOT_UNAVAILABLE`; welcome being seeded does not establish a cold boot. The correction retries the existing reboot function after the existing board probe, before starting the canary. Its hardware result is recorded separately below; the four successful live flips above precede this additional change.

Public compact evidence: [RV startup and FLIP results](release-evidence/2026-09-07-licheerv.json). Local detailed records: `stulp-single-buffer.json`, `single-buffer-replacements.json`, `single-buffer-flips.json`, `flip-rejections.json`, `postflip-reuse.json`, artifact manifests and UART under `../reviews/2026-09-07-licheerv-sharing/`. No secret environment values are published.


### L51 — explicit cold restoration completed; kept separate from FLIP acceptance

The normal `hopos.reboot=1` option already existed. It was used only to restore capacity after the completed live-update tests, not to make those swaps pass. Derek questions this detour; the unresolved issue is reuse of the original cold window, and reboot is not its fix. A minimal retry of `rebootNow()` after `boardWarn()` allows the existing LicheeRV watchdog probe to complete before the request is retried. The full host/target gate passes with this change. The restoration bundle lands as generation 6, arms the watchdog after its probe, and subsequently cold-boots the flashed single-buffer image at `0x84000000..0x86000000`. Observed restoration time is 108.8 seconds including download/placement/reset/boot. This deliberately ends app continuity and is excluded from the four successful live-flip results.

The three original app definitions were restored unchanged, again with three-second submission intervals. All three report running with zero restarts. Their hardcoded service endpoints are left unchanged; the integration limitation in L49 remains. The retry is an additional uncommitted source change in `metal/cmd/hopos/main.go`, not part of the image on the SD card. Local evidence: `cold-restoration-after-probe.json`, `reboot-after-probe-artifact.json`, `gate-after-reboot-order.log`, and `stulp-restored-after-flips.json`. No commit or release was made.


### L52 — remove the explicit reboot path; online updates use FLIP

Derek rejects deliberate reboot as an update or capacity-restoration mechanism. Remove both boot-time `hopos.reboot` checks, `rebootNow`, the optional `wdHardware.Reboot` callback, its Apple wiring/helper, and the test dedicated to this removed feature. Remove the current configuration entry and mark the archived entry as historical. The late-probe retry from L51 is withdrawn with the rest of this path. Automatic watchdog recovery from faults and the bounded FLIP boot guard remain: they are not the explicit reboot command.

The L49–L51 hardware observations remain historical evidence, not the prescribed update procedure. No additional board reset or deployment is performed for this removal. The current cold-window allocation limitation remains open and must be resolved by memory ownership/reuse through FLIP. Validation of this removal is recorded below.

Validation: `sh tools/test.sh` passes the complete host and TamaGo target gate after removal (`../reviews/2026-09-07-licheerv-sharing/gate-remove-reboot.log`). Source search finds no remaining explicit reboot option, callback or helper in `metal/`, `image/` or `tools/`. `git diff --check` passes. Changes remain uncommitted and have not been deployed.


### L53 — LicheeRV releases the cold window; four FLIPs and full workload restoration pass

Derek specifies the ownership rule: a departed kernel relinquishes its window like a departed app, and a second flip may return to the original window to restore the board's memory geometry. The common partition allocator now rebuilds ownership from the reusable board regions and excludes only the active kernel. A board can declare its cold kernel region as reusable after departure; that region receives the same alignment, overlap and persistent-administration checks as pool regions. LicheeRV declares its existing 32-MiB window. Its fixed administration is outside that window. Other boards are not opted in blindly: some retain boot data or vectors inside their cold reservation.

The cold region is available to normal app allocation. Kernel borrowing prefers its original base only when the requested span is entirely free; an app there retains its ownership and forces a different destination. Borrowing, rollback, scrubbing, staging and adoption reuse the existing paths and lock. The LicheeRV reservation includes the existing 256-KiB handoff tail rather than growing the reservation for it. Runtime RAM after the transition is 31.75 MiB inside a 32-MiB reservation. Capacity reporting excludes the current kernel claim. Pool source ordering and boundaries are preserved for existing placement behavior; normal release still coalesces neighboring free claims. No new worker, mutex, reboot path or handoff fields.

Host regression tests verify a resident across allocator reconstruction, return to the cold window, rollback of a failed loan, the 32/48/126-MiB placement order after return, and an app occupying the cold window without being displaced by FLIP. Layout tests reject reusable kernel overlap with pools or persistent scratch and reject misalignment. The full host/TamaGo gate passes; targeted slots race tests pass.

Hardware starts from the already running single-buffer cold image. The three jobs are temporarily removed and a 48-MiB test resident is installed. Four live swaps pass on ONE continuously held host TCP socket, checking payload and application boot identity throughout; the task identity also remains unchanged. Generation 1 inherits the old downloader's 34-MiB loan at `0x8dc00000`. Generation 2 returns to the original `0x84000000` in a 32-MiB reservation. Generation 3 uses `0x86000000`; generation 4 returns to `0x84000000` again. No reboot is used. After stopping the test resident, all three unchanged user jobs start in the requested order, three seconds apart: 206 MiB used out of 222 MiB, including the 126-MiB plugins. This supersedes the cold-window limitation and paired-socket limitation in L50. The supplied application endpoint configuration remains a separate integration issue.

Evidence: [window reuse and four-flip results](release-evidence/2026-09-07-licheerv-window-reuse.json); local `window-reuse-hardware.json`, `window-reuse-artifacts.json`, `gate-kernel-reuse.log` and UART under `../reviews/2026-09-07-licheerv-sharing/`. The public record identifies both tested bundle hashes and source hashes. The running candidate includes reboot-option removal. No commit or published release is made.


L53 follow-up: two further plugin replacement cycles pass on the returned kernel. The third replacement stalls during artifact download at 2,676,544 of 10,642,201 bytes and exceeds the 60-second test wait; it is not recorded as a passed cycle. No UART OOM or node reset occurs. Cancellation removes the task and releases its 126-MiB claim. Submitting the identical definition again streams and places the image in 5.62 seconds; all three apps run again, and both neighbors retain their original IDs. This is a separate unresolved transport-progress observation, not a failure to reclaim the kernel window. Records: `window-reuse-replacements.json` and `reuse-plugin-download-retry.json`. Do not turn the successful FLIP/window result into a blanket transport acceptance.


### L54 — transport isolation and a reproduced false-idle timeout

Derek requests investigation of the isolated download stall. Both neighboring apps remain resident throughout the replacement tests. Ten downloads from the original GitHub RISC-V artifact and ten from a local plain-HTTP mirror complete. The actual RISC-V ELF is 10,642,201 bytes, SHA-256 `d4b37244d2b215f6c7329124801010c3ebaaef4707c067181a7b7c21d8da257a`. An initial size comparison accidentally selected the ARM artifact from the multi-architecture job; it does not establish a source change. The first ten-cycle script still selected the original RISC-V URL, so its evidence is classified as original-source coverage, not local HTTP coverage.

A controlled server sends 3 MiB and then goes silent while retaining its connection. The existing timeout aborts that download after approximately 62.8 seconds including setup, reports the actual 3,145,728-byte count, and permits claim release. Both neighbors remain unchanged. Thus permanent failure of the basic blocked-read close path is not reproduced. TCP capture is restricted to the authorized node; raw captures and job environment remain local, unpublished test evidence.

Source review finds a separate concrete defect in HOP v1.0.2 `internal/runner/hopos_stream.go`: its idle timer is refreshed only through batched UI progress reporting (at least 256 KiB), rather than every positive read. A deterministic virtual-time test fails against the old reader: it stops after 9 KiB even though 1 KiB arrives every second. Hardware confirms the contract violation: after a 3-MiB prefix the server sends 1 KiB each second, yet the old node reports “no data for 1m0s” after 3,206,144 bytes (61.6 seconds total). This establishes a false-idle bug. It does not prove the cause of the earlier isolated 2.7-MB observation, which did not recur in the ordinary download repetitions.

The minimal correction moves only the progress-report batching: every positive read now refreshes the existing idle timer; UI reports retain the same batching threshold. No extra timer, goroutine, mutex, retry policy or buffer is introduced. HOP v1.0.2 was checked out for review at `/Users/derek/Git/hop`, branch `fix/download-idle-progress`; code and regression test are uncommitted. `go test ./...` in HOP and targeted runner/HTTP race tests pass. The HopOS candidate gate also passes using an isolated build-time local HOP replacement; the production `metal/go.mod` still pins published HOP v1.0.2.

The candidate is installed with FLIPs 5 and 6, keeping cloudflared and Stulp task identities and the same open Stulp HTTP connection; generation 6 returns to the original kernel window. The test harness initially compared task lists by order, which is not stable; comparing the recorded identity sets confirms preservation. The hardware slow-stream and silence tests against this candidate are recorded next. Local records use the `transport-*` prefix under `../reviews/2026-09-07-licheerv-sharing/`.


### L55 — per-read idle tracking passes hardware; release dependency still to publish

The identical slow-stream fixture succeeds with the HOP correction: 3 MiB immediately, then 1 KiB every second for 70 seconds, followed by the remaining image. The plugins start after 81.54 seconds, whereas the previous kernel incorrectly aborted the same fixture at 61.62 seconds. A genuinely silent stream still aborts normally after 61.71 seconds including setup; its 126-MiB claim is released and both neighbors remain running. Ten subsequent downloads of the original RISC-V artifact from GitHub pass on the corrected candidate. Cloudflared and Stulp preserve the same IDs across the entire investigation, including both candidate FLIPs, both deliberate failure fixtures and all replacements. The final three jobs use the original definitions again.

This closes the reproduced false-idle defect with before/after evidence. The original isolated 2.7-MB event did not recur in 20 ordinary original-source downloads (10 before, 10 after) or 10 local downloads. No unproven TCP/NIC cause is assigned and no Lean changes were made. This is not an assertion that every transport fault is impossible.

Release handoff: the fix and regression test are in `/Users/derek/Git/hop`, branch `fix/download-idle-progress`, based on published v1.0.2. No commit/tag/push was made. Publish that HOP change, then update the production HopOS requirement from v1.0.2 to the actual published version and verify the release build. The tested kernel candidates used an isolated local replacement; `metal/go.mod` has deliberately not been pointed at a nonexistent tag or a local checkout. The running board has the tested candidate without a reboot.

Public evidence: [transport before/after results](release-evidence/2026-09-07-licheerv-transport.json). Local original-source/local cycles, controlled-server records, candidate manifests, gate output and UART use `transport-*`, `hop-transport-tests.log` and `gate-transport-candidate.log` under `../reviews/2026-09-07-licheerv-sharing/`. The packet capture covers only the authorized node and is not published.
