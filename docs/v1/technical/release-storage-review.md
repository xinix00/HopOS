# Storagecorrectness — aftekening stap 2

Meetlat: [release-afronding](release-afronding.md), stap 2, en E1/E2/E5/E9 uit het [frameworkcontract](framework-contract.md). Datum: 6 september 2026. Dit verslag tekent codecontrole en gerichte hostproeven af. M4-hardwarebewijs blijft stap 4, H4/H5/H6. Geen nieuwe I/O-laag, lock of achtergrondtaak toegevoegd.

| Onderdeel | Code-oordeel | Bewijs / begrenzing |
| --- | --- | --- |
| `driver/nvme/nvme.go` generiek | Hersteld: reset bevestigen vóór DMA wissen; onzekere completion sluit verdere I/O af met dezelfde fout; CID controleren; transfer- en namespacegrenzen zonder opteloverflow; 4KB-controllerpagina en ondersteunde metadata-vrije namespace afdwingen. | Host PRP-, grens- en echte vijfseconden-timeoutproef slagen. QEMU-regressie op definitieve kandidaat blijft stap 3. Ondersteunt één namespace, 64 queueplaatsen, één lopende opdracht onder bestaande mutex, maximaal 1MiB en maximaal MDTS. |
| `driver/nvme/apple.go` | Hersteld: controller uit vóór queues/TCB wissen of herprogrammeren. Gecachete databuffer na initwissen cleanen; controller-identify na DMA invalideren. | De reset- en cachevolgorde is statisch gecontroleerd; ANS heeft geen QEMU-model. Apple behoudt lineair SQ-slot 0 en invalidatie na completion; geen speculative queue recovery. |
| `board/apple/storage.go` | Hersteld: RTKit-allocator begint na volledige NVMe-cachemapping, op +4MiB. Voorheen +0x120000 terwijl NVMe tot +3MiB wist en gebruikte: RTKit-buffers werden overschreven. Overflow-/nulallocaties worden geweigerd. | Hostfixture met echte allocatorbron en uit `apple.go` overgenomen constanten slaagt. Wederzijdse compiletime-assertie in `board/apple/hop/board.go` koppelt de boardgrens aan `nvme.AppleDMAReserved` zonder verboden base→driver-import. |
| `driver/rtkit` | Hersteld: `Poll` levert applicatie-endpoints aan bestaande callback (SMC-antwoorden vielen voorheen weg). Crashlog wordt uitsluitend binnen de eigen toegewezen buffer gelezen. | Hosttests voor aflevering en ontbrekende/kleine crashlogbuffer slagen. Geen nieuwe poller: caller blijft eigenaar. |
| `driver/pcie` | Akkoord voor huidige beperkte toepassingen: ECAM-enumeratie en firmware-BAR-lezen versus expliciete kale-fabrictoewijzing blijven gescheiden. | Statische controle; pakket compileert op host. `nvme.Probe` zoekt bewust uitsluitend bus 0/functie 0; geen claim voor NVMe achter willekeurige UEFI-bridges. BAR-toewijzing veronderstelt 64-bit memory BAR en passend boardvenster. |
| `driver/brcmpcie` | Akkoord voor huidige vaste Pi 4/5 boardconfiguraties: PERST tijdens setup, vaste inbound/outboundmapping, bounded linkwachten en configpas na link. | Statische controle; pakket compileert op host. Geen algemene allocator/validator voor willekeurige PCIe-windows toegevoegd. Hardwaregedrag Pi-varianten is hiermee niet opnieuw bewezen. |
| `board/apple/pcie.go`, `apcie.go`, `sart.go` | Akkoord binnen M4/t8132-profiel: reeds opgebrachte controller blijft intact, begrensde bringupwaits, bestaande RID/DART-configuratie; SART hergebruikt bestaand identiek venster. | Statisch gelezen. Registerrecepten en firmwaretunablebetekenis zijn niet via hosttests bewezen; andere Apple-generaties krijgen hiermee geen supportclaim. |

## Wat de foutafhandeling nu betekent

Na een NVMe-timeout, verkeerde CID of mislukte NVMMU-invalidatie blijft de vaste DMA-regio van de controller gereserveerd. Volgende reads/writes falen vóór iedere bufferkopie of queuewrite. Er is geen automatische retry/reset in het datapad. Een gewone foutstatus met bevestigde completion consumeert de completion en geeft de fout terug; volgende I/O mag doorgaan. Init mag queues pas opnieuw gebruiken na bevestigde `CSTS.RDY=0`.

Tijdens flip blijven deze board-DMA-regio's buiten app- en kernelallocaties. De nieuwe kernel reset de NVMe-kant vóór hergebruik. Dit is geen toezegging dat een onvoltooide storage-RPC zonder fout over de flip heen loopt of dat het bestaande scratchfilesystem persistent wordt. De RTKit-boot voert zijn bestaande firmware-handshake opnieuw uit; de daadwerkelijke overdracht van firmwarebuffers en herhaald openen op M4 moeten bij H4 aantoonbaar slagen. Geen nieuwe warmboot-slaapvolgorde verzonnen zonder hardwarebewijs.

## Uitgevoerde proeven

`cd metal && go test ./driver/nvme ./driver/rtkit ./driver/pcie ./driver/brcmpcie` slaagde op 6 september 2026. `TestTimeoutRetainsDMA` laat werkelijk vijf seconden zonder completion verstrijken, biedt daarna een late completion aan en bewijst dat de volgende write geen DMA-buffer of queue aanraakt. `TestInvalidTransferBeforeDMA` weigert zowel namespace-einde als `MaxUint64`-LBA en een ongeïnitialiseerde blocksize vóór fysiek geheugen wordt aangeraakt. De bestaande `TestDataPRPs` toetst 1/2/256 pagina's.

`go test ./board/apple` kan niet met standaard Go: het board importeert TamaGo `runtime/goos`. Daarom is de echte `storage.go` samen met `storage_test.go` apart in `/tmp/hopos-storage-review` getest, met uitsluitend `DRAMBase`, `RamBase`, `HopRAMSize`, `StructBase`, `StorageDMAPA` en `StorageDMASize` letterlijk uit `apple.go` als hardwaregrens. Deze fixture slaagde. De volledige Apple-build en compiletime-grensassertie horen bij de kandidaatmatrix van stap 3.

## Overdracht naar het centrale logboek

Code- en hostdeel van deze beperkte storagecontrole gereed. Open bewijs: kandidaatbuild/QEMU (stap 3), M4 ANS-heropenen na drie flips, correct gelezen/geschreven data, dezelfde diskworkload voor/na en gecombineerd gebruik (H4–H6). Geen hardware in deze deelopdracht aangeraakt. Deze regels mogen pas na die afzonderlijke bewijzen als hardware-akkoord worden afgetekend.
