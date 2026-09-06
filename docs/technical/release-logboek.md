# Release-logboek

Leidend document: [afrondingsplan](release-afronding.md). Elke wijziging hieronder hoort bij een stap daarvan. **Afgetekend** betekent dat het genoemde bewijs bestaat; het zegt niets over nog open hardwareproeven. Oude resultaten blijven historisch bewijs wanneer de kandidaat verandert.

## Opdracht en huidige stand

Derek heeft op 6 september de twee stappen vóór hardware opgedragen: resterende codepunten sluiten (planstap 2) en één testbare kandidaat maken (planstap 3). Daarna plaatst Derek **2.2.0** voor de laatste hardware-ronde. Eerdere interpretatie dat beide nodes al de uiteindelijke 2.2 draaien is hiermee vervallen. In deze fase geen hardware-deploy of flash.

| Planstap | Status | Bewijs / restant |
| --- | --- | --- |
| 1. Framework tegen visie | Afgetekend voor de uitgevoerde codepaden en lokale regressies | [Contract](framework-contract.md), [reparaties](../reviews/2026-09-06-framework/fixes/README.md). Board-/drivergrenzen gaan naar stap 2; hardwarebewijs blijft apart. |
| 2. Codepunten sluiten | Afgetekend voor code/host | HOP v1.0.1 geïntegreerd; NIC-, opslag- en overige driverreviews uitgevoerd, concrete fixes getest. L04–L08. |
| 3. Kandidaat bouwen | Afgetekend | Definitieve bron-/bundlehashes, zeven builds, hostgates en relevante QEMU-proeven slagen. L09–L10. |
| 4. Hardware | Wacht op Derek / 2.2.0 | H0–H6 uit het plan; eerdere uitlezing van nodes is geen kandidaat-aftekening. |
| 5–6. Eindaftekening | Open | Volgt na hardware en eventuele gerichte reparaties. |

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
