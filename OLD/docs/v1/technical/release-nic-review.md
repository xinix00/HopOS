# NIC-correctness — aftekening voor de releasekandidaat

Onderdeel van **stap 2** van het [afrondingsplan](release-afronding.md), getoetst aan [E1–E9](framework-contract.md). Datum: 6 september 2026. Deze afgebakende doorloop sluit de hieronder gevonden codefouten. De buildmatrix en integratietests op de uiteindelijke kandidaat horen bij stap 3; de hardware-aftekening volgt in H0–H6. Er zijn tijdens deze doorloop geen hardwarecommando's uitgevoerd.

**Uitkomst:** de zeven bestaande NIC-drivers zijn doorgelopen op initialisatie, DMA-bereik, eigendom, publicatie, completion, foutpaden en hun boardkoppeling. Kleine reparaties, geen nieuwe lock, goroutine, driverinterface of interruptdriver. MDIO en de netwerknaad zijn meegenomen. Dit is geen certificering van alle chips die dezelfde drivernaam dragen.

Derek heeft het aanbrengen/toetsen van de netwerk-IRQ op alle boards expliciet naar later verschoven. De huidige polling blijft staan en is voor deze ronde geen releaseblokkade. De visie blijft één netwerk-IRQ en één logische deurbel; deze aftekening claimt niet dat de fysieke IRQ-route al op ieder board bestaat.

## Aangetoonde fouten en reparaties

| ID / meetlat | Voorheen | Reparatie en bewijs |
| --- | --- | --- |
| N1 — E2, E5, E6 | `tg3.Receive` gaf de RX-buffer aan DMA terug vóór `CopyOut`. Een volgende ontvangst kon de nog gelezen inhoud overschrijven. De producer-check had bovendien geen expliciete barrier vóór het descriptorlezen. | Eerst completion waarnemen, descriptor/buffer lezen en kopiëren, dan barrier en beide indexen publiceren. Hosttests controleren payload en recycle; de echte DMA-interleaving/cacheordening moet H5/H6 op M4 bevestigen. |
| N2 — E1, E4 | Alle zeven RX-paden vertrouwden de ontvangen lengte verder dan de eigen buffer; meestal begrensden ze alleen op de buffer van de aanroeper. `hopnet` gebruikt werkelijk een grote buffer: `layout.NetMTU` is 65535. Een foutieve completion kon dus bytes uit een naastgelegen DMA-buffer afleveren. | Per driver grenzen aan de werkelijk aangeboden buffer, inclusief virtio-header, GENET-padding en FCS waar relevant. Ongeldige lengtes leveren geen frame op; de descriptor komt terug in de ring. `receive_bounds_test.go` in ieder driverpakket test geldige lengte, uiterste geldige lengte en overschrijding met behoud van de rest van de bestemming. |
| N3 — E4, E5 | Virtio castte de 32-bit completion-ID eerst naar 16-bit: `0x10000` werd een geldige index 0. TG3 gebruikte de onderste opaque-index zonder bereikcontrole. | Volledige virtio-ID valideren vóór de cast; TG3-index begrenzen tot zijn standaard RX-ring. Tests bewijzen afwijzing en voortgang van de completion-index. |
| N4 — E9 | Virtio zond van een te groot TX-frame stilzwijgend alleen het begin en retourneerde succes. | Ongeldige lengte geeft een fout vóór DMA-publicatie. De test controleert zowel foutretour als ongewijzigde producer. De tijdelijke `frame`-slice en truncatietak zijn vervallen. |
| N5 — E5, E9 | GEM controleerde alleen de lengte, niet of de descriptor een compleet frame droeg. GENET publiceerde RX-consumption zonder expliciete barrier na de kopie. | GEM vereist SOF én EOF en geeft partiële frames terug zonder aflevering. GENET rondt de kopie met `dev.MB()` af vóór zijn consumer-write. GEM krijgt een test voor alle drie onvolledige flagcombinaties. |

De SOF/EOF-bits (14/15) en het vereiste complete-framegedrag zijn gecontroleerd in de primaire [Cadence-registerdefinities](https://github.com/torvalds/linux/blob/master/drivers/net/ethernet/cadence/macb.h) en [GEM-ontvangstimplementatie](https://github.com/torvalds/linux/blob/master/drivers/net/ethernet/cadence/macb_main.c). Er is geen fragmentassembler toegevoegd.

## Aftekening per gebruikte driver

`Code akkoord` betekent: deze begrensde doorloop en de toepasselijke hosttests zijn afgerond. Het bewijst geen werking van MMIO, cacheonderhoud of interrupts op een niet-getest board.

| Driver / board | Gecontroleerde uitvoering en grens | Code / resterend bewijs |
| --- | --- | --- |
| [virtionet](../../../metal/driver/nic/virtionet/virtionet.go), QEMU virt | Modern MMIO-transport; VERSION_1-handshake; vaste QEMU-queues; eigen descriptor/avail/used/bufferregio; publicatie vóór notify; begrensde TX-ringwacht. N2–N4 gerepareerd. Scope blijft de bestaande QEMU-configuratie: geen algemene claim over willekeurige virtio-transports, ongelijke queuecapaciteiten of out-of-order completions. | **Code akkoord.** Nieuwe hosttests groen. Bestaande QEMU-TCP/flipbewijs is van vóór deze laatste NIC-fixes; kandidaat opnieuw integreren in stap 3. |
| [tg3](../../../metal/driver/nic/tg3/tg3.go), M4 | Board enumereert PCIe en reset vóór ringinitialisatie; vaste 8 MiB node-DMA-regio; status/ringen ongecached, pakketbuffers eventueel WB met Pull/Push. TX kopieert naar eigen buffer, bewaakt ringruimte en publiceert descriptor vóór producer. N1–N3 gerepareerd. | **Code akkoord.** H4–H6 op M4: drie flips, inhoudcontrole, ringwrap onder belasting en cache-/DMA-ordening. Geen verklaring voor andere Broadcom-varianten. |
| [dwmac](../../../metal/driver/nic/dwmac/dwmac.go), LicheeRV | Board reserveert 448 KiB en controleert `NeedBytes`; reset wordt afgewacht vóór `initRings`; descriptor op cachelijn-stride; CleanInv/MB rond ownership. Complete/error-status en FCS behandeld; TX-grootte en wachten begrensd. N2 gerepareerd. | **Code akkoord.** Bestaande descriptor-tests plus nieuwe Receive-test groen. H4–H6 op RISC-V blijven nodig; de huidige fliproute bestaat in `kernflip/arch_riscv64.go`. |
| [dwmac4](../../../metal/driver/nic/dwmac4/dwmac4.go), RK3566 | Eigen 8 MiB-planregio; reset vóór ringopbouw; FIFO/descriptor/buffergrootten gecontroleerd; read/write-descriptorvorm per recycle hersteld; OWN na adres/publicatie; TX-timeout. N2 gerepareerd. | **Code akkoord.** Bestaande veld-/layouttests en nieuwe Receive-test groen. Deze ronde geen RK3566-hardware-aftekening; kandidaatbuild apart. |
| [genet](../../../metal/driver/nic/genet/genet.go), Pi 4 | Boardreset en DMA uit/flush vóór ringopbouw; producer/consumer op bekende nulstand; registerdescriptors wijzen naar vaste buffers; complete/error-bits en 2-byte-padding; TX-ringruimte en timeout. N2/N5 gerepareerd. | **Code akkoord.** Nieuwe Receive-test groen. Warm/netboot-reset van niet-nul indices staat al als hardwarevoorwaarde in de bron; deze ronde geen Pi 4-hardware-aftekening. |
| [gem](../../../metal/driver/nic/gem/gem.go), Pi 5/RP1 | RX/TX uit vóór ringopbouw; eigen descriptor/bufferregio en board-BusOff; OWN/USED bepaalt eigendom; descriptor vóór TSTART; begrensde TX-wacht en bestaande RP1-TSTART-herhaling. N2/N5 gerepareerd. | **Code akkoord.** Receive-grenzen en partiële frames getest. RP1-AXI, autonegotiatie, reset tijdens lopende DMA en sustained traffic blijven Pi 5-hardwarebewijs; deze ronde geen hardware-aftekening. |
| [igb](../../../metal/driver/nic/igb/igb.go), UEFI | Board reset en link vóór init; 2 MiB-uitlijning en capaciteit gecontroleerd; advanced RX-descriptor opnieuw gewapend; TX houdt één ringentry vrij en wacht op DD vóór hergebruik; buffer CleanInv vóór DMA/publicatie. N2 gerepareerd. Queue-enable wordt kort gepolld; boardreadiness wordt pas na DHCP gemeld. | **Code akkoord binnen bestaand boardpad.** Nieuwe Receive-test groen. PCIe/queue-enable/NVM/cachegedrag van de concrete adapter vereist ijzer; deze ronde geen UEFI-hardware-aftekening. |
| [mdio](../../../metal/driver/nic/mdio), PHY-glue | Scan en autonegotiatie begrensd; aparte fast-Ethernetroute zonder gigabitregisters; RTL8211F-delaywijzigingen keren terug naar registerpagina nul. | **Code akkoord, ongewijzigd.** Bestaande hosttests groen. PHY/clock/link blijven boardbewijs. |

## Netwerknaad, stop en flip

- [hopnet.Up/rxLoop](../../../metal/net/hopnet/hopnet.go) heeft één fysieke RX-lezer; [hopswitch.Uplink](../../../metal/net/hopswitch/nat.go) serialiseert bestaande TX-aanroepers met zijn reeds aanwezige `uplinkTxMu`. De kale hardwaredrivers vereisen deze aanroepvolgorde. DHCP-bring-up is vóór de stack actief wordt.
- De netwerk-DMA-regio is van de node, niet van een app. App-stop geeft die regio niet vrij. Drivers kopiëren uitgaande frames naar eigen buffers vóór terugkeer, zodat latere DMA geen pointer naar een vrijgegeven app-partitie nodig heeft.
- [locdev](../../../metal/net/hopnet/locnet.go) begrenst zijn bestaande self-dialwachtrij en kopieert bij enqueue. Interne switchframes gaan via de bestaande hostbel; fysieke RX gebruikt het boardpad. Geen nieuwe achtergrondtaak toegevoegd.
- Een kernel-flip draagt app-ringen en NAT-eigendom over; hij neemt geen Go-driverobject of NIC-ringindex over. De nieuwe kernel brengt de NIC via `ProbeNIC` opnieuw op in zijn vaste node-DMA-regio. In-flight Ethernetframes mogen daarbij verloren gaan; TCP-voortgang is een afzonderlijke acceptatie-eis. Hiervoor blijven H4–H6 nodig.
- Alleen [qemuvirt](../../../metal/board/qemuvirt/hop/net.go) levert `WaitNIC`/GICv3; de andere boards gebruiken de bestaande 300 µs-pollroute. Ook het IRQ-pad heeft een 10 ms-vangrail. [support.md](../support.md) beschrijft dit NIC-onderscheid correct. Deze uitgestelde IRQ-uitbreiding mag niet worden afgevinkt als reeds gerealiseerd.

## Bewijs en reproduceerbaarheid

Host: `go version go1.26.4 darwin/arm64`. Referentie vóór de NIC-fixes: commit `c65aa04dc2cbc67218f6cdbc60ae97206650817c`.

- [Vóór reparatie](release-evidence/nic-before.log): de nieuwe tests tegen de zeven oorspronkelijke driverbestanden falen op de beschreven grenzen/flags/TX-publicatie. Dit draait met gecontroleerd heapgeheugen, geen fysiek device of actieve uitbraaktest.
- [Na reparatie](release-evidence/nic-after.log): vanuit `metal`, `go test -count=1 ./driver/nic/...` slaagt voor alle acht pakketten.
- [Netwerkregressie](release-evidence/nic-switch.log): vanuit `metal`, `go test ./net/hopswitch ./net/nodemac` slaagt.
- De nieuwe fixtures staan naast iedere driver als `receive_bounds_test.go`. Hostbarriers zijn no-ops: hun plaatsing is statisch beoordeeld; echte ordening moet op de kandidaat/hardware blijken.

**Terug naar het plan:** stap 2 heeft hiermee een concrete NIC-aftekening en reparatiebewijs. Stap 3 moet precies deze bron meenemen in de kandidaat. M4/RISC-V worden pas na de afgesproken H0–H6 afgetekend; de overige boards krijgen in deze ronde hoogstens het daadwerkelijk uitgevoerde buildbewijs.
