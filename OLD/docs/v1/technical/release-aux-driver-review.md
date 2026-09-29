# Afgebakende driverdoorloop: interruptadapter, beheer en optionele invoer/beeld

6 september 2026. Dit is stap 2 van [het afrondingsplan](release-afronding.md), tegen E1–E9 uit [het frameworkcontract](framework-contract.md). Geen nieuwe drivers, IRQ-bedrading of hardwareproeven toegevoegd. NIC, NVMe, PCIe en RTKit hebben een afzonderlijke doorloop. Hieronder betekent **bron + host** een afgesloten broncontrole met gerichte hosttests; het betekent geen hardware-aftekening.

## Gevonden en hersteld

- **GICv3: juiste SPI, daarna enable.** `0x6100` is de locatie van IROUTER voor INTID 32, niet de basis voor indexeren met de volledige INTID. De oude adapter schreef voor bijvoorbeeld INTID 48 het register van INTID 80. Dezelfde fout zat in de gebruikte TamaGo-enablefunctie; alleen de laatste write aanpassen zou dus nog steeds een andere route veranderen. De kleine lokale enablefunctie schrijft nu `0x6000 + 8*INTID`, maskeert MPIDR tot affinity, publiceert route/groep vóór enable en weigert bijzondere/negatieve IDs. Disable schrijft alleen zijn eigen W1C-bit. De bestaande initialisatie en claim/EOI blijven bij TamaGo. Dit is correctie van het reeds gebruikte IRQ-pad, geen extra IRQ. Registerbasis gecontroleerd tegen [de primaire Linux GICv3-registerdefinities](https://github.com/torvalds/linux/blob/master/include/linux/irqchip/arm-gic-v3.h).
- **USB: timeout geeft DMA-eigendom niet terug.** Een command/control-timeout zet nu de bestaande controller-quarantaine. Volgende opdrachten gebruiken geen command-ring/controlbuffer totdat de bestaande recovery een controllerreset heeft bevestigd. Start weigert een draaiende controller en een overlopen CPU-/busvenster; onbekende RUN-uitkomst blijft in quarantaine. Stop vergeet RUN niet bij onbevestigde halt; Reset wacht ook op HCHalted wanneer RUN al laag staat. Geen nieuwe recoverylus of lock.
- **USB: lengtes vóór gebruik.** Arena-uitlijning en optelling kunnen niet omklappen. Control-data blijft binnen zijn 1024-byte-venster; een onmogelijke completion-restlengte wordt afgewezen. Vaste descriptor-koppen moeten volledig ontvangen zijn; EP0/lege interrupt-endpoints en niet-default alternate settings worden niet als HID-inputendpoint geconfigureerd. SuperSpeed EP0 decodeert exponent 9 naar 512 bytes. De bestaande twee HID-rollen blijven de grens.
- **HID: rollover is geen loslaten.** Een fout-/rolloverrapport behoudt de vorige gewone toetsen. Het eerstvolgende geldige rapport bepaalt welke werkelijk losgelaten zijn. Hierdoor ontstaan geen onbedoelde release/press-paren.
- **VideoCore/SMpro/SMC: eerst de vorige opdracht afronden.** De bestaande VideoCore-mutex bewaart het exacte uitstaande bufferadres; een timeout laat die claim staan. Een volgende call wacht eerst op dat antwoord, en kijkt naar het hele adres plus kanaal. Verzoeken passen in het vaste 4-KiB-boardvenster; inboxdrain is begrensd. SMpro laat een nog niet voltooide PCC-buffer staan en kan na completion opnieuw aanvragen. SMC weigert na een onbevestigde opdracht verdere opdrachten: de 4-bit request-ID en gedeelde buffer mogen niet met een late completion worden verward. De laatste is bewust geen automatische coprocessor-reset.
- **DVFS: fout is geen geslaagde klokovergang.** Het beleidsbit verandert alleen na bevestigde firmware-call; een mislukte boot-/busy-overgang blijft daardoor opnieuw te proberen.
- **Framebuffer/scanout: geometrie begrenst writes.** De console weigert te kleine stride en een overlopen bereik, ontkoppelt bij ongeldige herinitialisatie en weigert negatieve headerregels. RK3566-scanout accepteert uitsluitend zijn bestaande 1080p-geometrie met uitgelijnde stride en een bereik dat volledig in het 32-bit DMA-adres past, vóór registerwrites.
- **Console: diagnostiek blijft begrensd.** `Since` gebruikt één teller-momentopname en klemt een toekomstige cursor; geen unsigned-underflow naar een enorme allocatie. Blackbox klemt de 64-bit bewaarde teller vóór conversie naar `int` en laat bij uitschakelen geen oude buffer actief.

## Inventaris en grens van het bewijs

| Onderdeel | Gelezen paden / eigendom | Oordeel binnen deze ronde |
| --- | --- | --- |
| `driver/gicv3` | Init, route/groep/enable, disable, claim/EOI; de bestaande netwerk-IRQ | Bron + host voor registerselectie, affinity en ID-grenzen; echte IRQ-bezorging blijft kandidaat-/hardwaretest. Geen nieuwe board-bedrading. |
| `gui/driver/usb/xhci` | Probe → halt/reset → arena/ringen/contexten → RUN; één eventconsumer; attach/control/interrupt-IN; confirmed Disable Slot; bestaande controllerrecovery | Bron + bestaande ownership/recoverytests en nieuwe bounds/timeouttests. DMA-regio blijft board-owned; geen app-geheugen wordt door deze USB-driver uitgeleend. USB-herplug/reset op ijzer nog niet afgetekend. |
| `gui/driver/usb/dwc3` | Identificatie, core/PHY-reset, hostmodus, vaste begrensde vertragingen; geen bufferallocator | Broncontrole; geen nieuwe defectclaim. Werkelijke PHY-/modeovergang vereist Radxa-hardware. |
| `gui/driver/usb/hid` | Begrensde bootrapporten, toets-/muisdeltas, reset bij detach | Hosttests inclusief rolloverbehoud geslaagd. Alleen boot-keyboard en boot-mouse, geen algemene HID-descriptorinterpreter. |
| `gui/driver/rkscan` | Power/clock, vaste VOP2-modus, framebufferadres/stride, HDMI/PHY-ack en timeouts | Bron + pre-MMIO-bounds-test. Geen DMA-allocatie: board houdt het framebufferbereik. HDMI-fout wordt expliciet gemeld; netwerkbeeld kan blijven werken. Fysieke scanout nog geen hardware-aftekening. |
| `driver/fb` | Firmwarebuffer, init/clear, tekst/header/scroll, disable | Bron + geometrie-/herinitialisatietests. Geen eigendomsoverdracht of hardware-stopfunctie; framebufferdescriptor en reservering komen van het board. |
| `driver/vcmail` | Eén vaste firmwarebuffer, tags, publicatie, exact antwoord, timeout, klok/temperatuur/fb | Bron + bounds-/pendingtest. Zelfde-kernel bufferhergebruik na timeout gerepareerd; Pi-firmware en flipgrens afzonderlijk op ijzer controleren. |
| `driver/smpro` | Eén PCC-kanaal/buffer, doorbell, completion, temperatuur | Bron + pendingtest. Geen onbegrensde softwarepoll; hardwarelatentie bepaalt bestaand budget. ACPI-bereik en werkelijke completion op Altra blijven hardwarebewijs. |
| `driver/smc` | RTKit-boot/endpoint, initialisatieantwoord, request-ID, bounded read, sensorenumeratie | Bron + weigering bij onbekende completion. **Opt-in, standaard uit** op Apple (`hopos.smc=1`): native SMC-bring-up is al volgens boardcode niet bevestigd. Deze review promoveert dat niet tot ondersteunde temperatuurfunctie. RTKit/DMA-allocator valt onder aparte review. |
| `driver/dvfs` | Eén bestaande wachter, mailboxcalls en succesafhankelijke hoog/laag-overgang | Broncontrole; geen nieuwe worker/lock. Telemetrie uit app-controlpage is beleidsinformatie, geen geheugen/corebevoegdheid. Werkelijke frequenties en performance op Pi nog te meten. |
| `driver/conlog` | Best-effort byte-ring, begrensde netwerkread, gereserveerde blackbox over boots | Bestaande hosttests + toekomstige cursor. Gelijktijdige printk mag volgens bestaand contract bytes verliezen/vervlechten; geen betrouwbare auditlogclaim en geen nieuwe printk-lock. |
| `driver/pl011` | Vaste registerwrite, begrensd wachten op TX FIFO | Broncontrole; geen DMA/buffertransfer. Bij niet-antwoordende UART worden bytes opgegeven, zodat diagnostiek de node niet stilzet. UART-uitvoer op hardware niet opnieuw gemeten. |

## Nog expliciet bij kandidaat/hardware te toetsen

Een nieuwe kernel verliest Go-objecten, ook de pending-administratie van firmwaremailboxen. De vaste VideoCore-/PCC-buffer blijft fysiek bestaan. Deze ronde bewijst de timeoutdiscipline **binnen één kernel**, niet dat een Pi-/Altra-flip tijdens een firmwareopdracht veilig opnieuw kan initialiseren. Voor die boards hoort een aantoonbaar afgeronde firmwareopdracht vóór bufferhergebruik bij de hardware-aftekening. Geen algemene quiesce-machine toegevoegd op basis van een onbewezen firmwareaanname.

USB-invoer en native RK3566-beeld zijn optionele GUI-functionaliteit. Ze zijn afzonderlijk meegenomen, niet stil weggefilterd om headless tests groen te krijgen. Een geslaagde headless M4-/RISC-V-kandidaat verklaart deze andere hardwarepaden niet bewezen. De eerder afgesproken kandidaatmatrix controleert hun build; aansluiten, herplug, controllerreset en de feitelijke framebuffer blijven hardwarewerk.

## Uitgevoerde gerichte controle

```sh
GOCACHE=/tmp/hopos-fix-gocache GOWORK=off go -C metal test -tags gui \
  ./driver/gicv3 ./driver/fb ./driver/conlog ./driver/smc ./driver/smpro \
  ./driver/vcmail ./gui/driver/... ./gui/usbin
```

Geslaagd. `dwc3` heeft geen hosttest; de overige hierboven genoemde gerichte tests draaien op hostgeheugen/callbacks, niet op fysieke controllers. De uiteindelijke kandidaat-buildmatrix en hardwarelijst staan in het afrondingsplan.
