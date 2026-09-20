# Afrondingsplan voor de eerste publieke release

Voortgang, aftekeningen en besluiten staan in het [release-logboek](release-logboek.md). Dat logboek wordt bij iedere afgeronde wijziging of nieuwe bevinding bijgewerkt; aftekenen gebeurt uitsluitend met genoemd bewijs.

## Open acceptance items — 9 September 2026 (L69)

This is the current remaining-work list. Earlier dated checklists below remain history; completed LicheeRV sharing/FLIP work and published HOP dependencies must not be reopened merely because an old checkbox is still empty. Derek confirms on 17 September that O6N boots and is fully operational (L74); bring-up is complete. The remaining O6N entries concern documented release tests and measurements.

- [x] **TCP between nodes (L68–L70):** reproduced and corrected. Six 64 MiB transfers pass on Pi4/Pi5; four 128 MiB transfers, including simultaneous bidirectional traffic, pass full SHA-256 verification. Idle wake activity settles, no restarts occur, and test claims are released. See [L70 evidence](release-evidence/2026-09-09-pi-tcp.json). Acceptance applies to the recorded review builds; release packaging is the next item.
- [ ] **O6N display stability:** after the accepted two-FLIP regression, the display process exited with code 2 and restarted once at 09:35:41 UTC on 18 September. The retained stack lacks the panic header and faulting frame; cause remains unknown. Capture a complete failure and resolve it before GUI stability signoff. See L77 in the [logbook](release-logboek.md).
- [ ] **O6N physical input confirmation:** L77 follow-up starts all ten native xHCI controllers, detects Logitech keyboard/mouse receiver (046d:c548), connects the display input stream and automatically restores it across two live FLIPs with all five residents retained. Grant adoption prevents the console from drawing over SURF; non-HID enumeration churn is fixed. Host/build gates and final NVMe integrity pass. Confirm physical cursor/click/keyboard and replug behavior with the operator; enumeration alone does not close these checks. See [USB evidence](release-evidence/2026-09-18-o6n-usb.json).

- [ ] **Final candidate:** collect the intended fixes and published dependencies, run the applicable build/regression gates and record source and artifact hashes. Include the L59 HOP cage/core separation (still local: v1.0.6 does not contain it), the L70 HopOS ring/Lean transport changes, the TamaGo runtime guard, the L79 TamaGo `findTimer` bound (patch 0003, rebuild all apps), the L79 kernflip scratch fix and the L72 empty-FLIP parked-core and HVC instruction-cache corrections plus seeded Lean ephemeral allocation, GENET warm-ring initialization and bounded early Pi UART output; rebuild affected apps as well as final kernel candidates. Existing app binaries retain their linked runtime/netstack. Rebuild after any acceptance fix; no new broad framework review.
- [x] **No network is not a dead node (L83, 19 September).** Boot retries the network bring-up until it succeeds instead of parking headless; a running node keeps rebinding an expired lease and resets itself through the watchdog only when the server refuses or moves the address. M4-tested with forced probe failures. Publish lean with `leandhcp.ErrRefused`.
- [x] **Ampere network interrupt — out of scope (Derek, 19 September).** HopOS targets nodes of up to about 20 cores; the Altra runs the UEFI profile polled. INTx there is fatal at SoC level (L83, UART evidence) and the ITS/MSI-X route is not planned. `DefaultNICIRQ=known` keeps the O6N wired and unknown UEFI boards polled.
- [ ] **O6N re-check with the interrupt door (L83 point 12) and the current tree.** The UEFI board now serves its NIC line through `idle.ServeIRQFlag` (flag-only vector, governor wake) instead of tamago's `ServiceInterrupts`; proven to claim on the Altra, not yet re-run on the O6N (off today). One flip of the current tree with the L80 A/B before release.
- [x] **Ampere USB input — dropped (20 September).** The opt-in PCIe xHCI path (`hopos.usbpci`) never worked (BAR above 512 GB faulted) and the Altra is a reference bench, not a target; the code is out of the tree. The O6N keeps its native hosts.
- [x] **100 MB/s over the wire (L83 points 26–29, 20 September).** M4 ↔ O6N, both NIC interrupts on, one- and two-core apps: 109–118 MB/s each way, first byte 1.0–1.5 ms, connection cycle 1.3–2.0 ms; in-node 765 MB/s. The 45 MB/s ceiling was the tg3 rearm on the M4 (a lone frame waited for the 10 ms failsafe); fixed the Linux way (`HOSTCC_MODE_NOW` after reopening, rearm at the pump's sleep point). Wi-Fi measurements from the Mac are void (L83 point 20). Board: [boxes-to-tick](boxes-to-tick.md).
- [ ] **Stick and disk kernels are stale.** The O6N stick boots the 18 September kernel (no NIC interrupt: 45 MB/s until flipped), the M4 disk kernel refuses a 2 GB app (768 MB slot cap), the Ampere stick is 18 September. Refresh all three with the release candidate so a cold boot is the release.
- [x] **`GIC_IPI` switcher on real GICv3 boards (L83 points 31–32).** Under it a resident never yields on the O6N (bundle 46) nor on the Altra (92/94); QEMU accepts it; the M4 runs its Apple variant fine. Bundle 47 (same tree without it) runs the desktop on the O6N, so `flip-bundle.sh` builds `uefi` and `o6n` without `-D=GIC_IPI`; the switcher stays in the tree for QEMU until it is understood on hardware (UART on the Ampere).
- [x] **M4 NIC interrupt died after sustained pulls (L83 point 33).** The stray-line guard disabled a served line after 256 claims in one pass; it now ends the pass and keeps the line. Bundle 39 on the M4.
- [ ] **O6N at the wire limit after a power cycle (L83 point 33).** Bundle 45 measured 118 MB/s at 12:12 and 21 ms cycles after the afternoon power cycle; the Mac split puts the failsafe on the O6N side. Controlled repeat: power cycle, flip 45 over the live residents, measure, then the current tree (bundle 52).
- [ ] **O6N stick config wipes the NVMe on every cold boot.** `hopos.nvmewipe=1` is still in the stick's `hopos.cfg` (and in bundle 45's embedded config); remove it on the stick before anything is stored there.
- [x] **Ampere after a power cycle: apps died under bundles 92/94.** Explained by the point above (the `GIC_IPI` switcher), not by parked cores (the M4 proved those survive a switch-code change).
- [x] **M4 network interrupt (L81–L82, 18/19 September) — on by default.** AIC3 driver, I-only service loop, tg3 INTx through the Apple root port (AIC 1253 = port line + 4), ack = interrupt mailbox mask, rearm after the pump. A/B against polling on the same morning: rtt and connection rate equal, inbound throughput about 15 % higher, no 300 µs polling. Bundle 26 runs on the M4 with cloudflared and Spin. Open but not blocking: idle wake-rate measurement, one unattributed internet download EOF, watchdog not firing after a HOP panic (L82 evidence).
- [x] **O6N network interrupt (L80, 18 September) — on by default.** The L76 freeze was a level line left asserted after the ack and a dispatcher that kept claiming it; fixed in `cpu/irq` (unknown or stuck lines are disabled, one console line) and in the RTL8126 driver (mask before ack, write-one-to-clear of all status bits as r8169, rearm after the pump). Verified on the O6N through fifteen live FLIPs with the display and launcher residents retained: rtt p50 156–201 µs against 160–168 µs polled, no interrupt storm. Not yet wired on the generic UEFI board (Ampere): it needs the NIC's line from the DSDT `_PRT` or MSI; polling remains its fallback.
- [x] **M4 round (L78–L79, 18 September) — passed.** FLIP departure bug on fixed-scratch boards found and fixed (`kernflip.BoardScratchInWindow`; QEMU regression green). On the corrected candidate (bundles 11–14, local hop and lean): H1, H2, H3 pass; live FLIPs 11→12→13→14 adopt 3/3 residents with held TCP; three boot benchmark runs (raw 1 MiB 5389–5756 / 1824–1971 MB/s); 9 GiB source-plus-backup on HopFS passes twice; all fourteen Vitals tests pass on three big cores with disk and network repetitions; the thirty-minute soak passes (1775 rounds, 29 replacements) after the TamaGo `findTimer` walk was bounded (toolchain patch 0003), which also explains the cloudflared crash of 6 September. **Release consequence: rebuild every app with the patched toolchain.** Still unexplained: a nine-core Vitals request dispatched only two secondary cores (measurement in progress). cloudflared and Spin restored on generation 4. See L79 in the [logbook](release-logboek.md) and [evidence](release-evidence/2026-09-18-m4-round.json).
- [ ] **Remaining physical lifecycle acceptance (H0–H4):** **Pi4/Pi5/Radxa passed their requested portion in L72**, including sharing/SMP, quiet wake, stop/reuse and three live FLIPs with retained residents/TCP. **O6N passes the recorded L77 lifecycle/share/SMP/wake, three live FLIPs and controlled watchdog recovery.** Complete M4 in its planned hardware round; recheck applicable changes when packaging the final release artifacts. On LicheeRV, retain completed evidence and recheck the changed memory path and applicable final-candidate regressions. O6N bring-up is complete; record its final-candidate test sequences without reopening board bring-up.
- [ ] **Storage and performance (H5):** **O6N two9GiB source-plus-backup integrity passes are complete on the recorded RXACK candidate (L77); current ordering-candidate disk/network measurements have three repetitions.** The later matched DMA-mapping comparison also passes three app repetitions and separate mixed-size, overlapping-write content verification; see [NVMe evidence](release-evidence/2026-09-18-o6n-nvme.json). Verify remaining large database-plus-backup cases on actual HopFS storage, including readback/integrity and release/reuse. Repeat the M4 approximately 4000 MB/s workload three times with explicit conditions; finish the same-workload disk/network comparisons.
- [ ] **Sustained operation and Vitals (H6):** **Pi4/Pi5/Radxa passed full thirty-minute compute/network runs and final Vitals in L72**, including sleep/wake, replacement, content checks, zero unexpected restarts and released claims. **O6N also passes full1800.86s combined compute/network/NVMe with28 replacements, then all14selected Vitals tests on11cores (L77).** M4 sustained operation/Vitals remain for its planned NVMe round.
- [ ] **Documentation, website and final signoff:** remove superseded temporary placeholders, synchronize capabilities and measured results, record remaining board limitations, and identify the exact accepted release artifacts. Release readiness requires closure of known correctness failures within the promised support.

**L72 requested items 2 and 4: passed for Pi4, Pi5 and Radxa.** Each board completed lifecycle/reuse, shared and SMP compute, quiet wake, three actual live FLIPs retaining the same residents/TCP sockets, a full thirty-minute combined run and final Vitals. All three are restored to Welcome only; temporary test services are removed. Pi4's warm GENET descriptor/counter mismatch is fixed and verified on ordinary production64–67. Failed client-path attempts and Pi5's staged artifact scope remain explicit in the [L72 sign-off table](release-logboek.md#l72--pi4-pi5-and-radxa-final-lifecycle-and-sustained-use-round-passed) and [artifact evidence](release-evidence/2026-09-09-board-acceptance.json). The open checkboxes above now concern the remaining machines and final release packaging, not unfinished tests on these three boards.

**Later point releases:** work through the complete [board checklist B01–B18](../../support.md), including physical network IRQ, idle/power/DVFS, storage, network, display and other board facilities. Accurate SMP idle accounting remains follow-up work; Vitals currently explains why that percentage is unavailable. These optional improvements do not absorb the TCP correctness item above.

**Actuele volgorde (besluit Derek, 6 september, L42):** de volledige docs-overhaul is afgerond als parallelle Engelse set in `docs/v2/` (stap 5, L44); daarna morgen de laatste hardwarecontrole op v2.2.2, met eerst LicheeRV koud installeren en de flipreeks toetsen (stap 4, inclusief resterend H6); daarna eindaftekening (stap 6). De functionele wijzigingen voor v2.2.2 blijven hierbij. Fysieke netwerk-IRQ volgt afzonderlijk na deze afronding. De stapnummers hieronder blijven vaste verwijzingen; hun numerieke volgorde is voor deze laatste ronde dus aangepast.

Dit plan komt uit de frameworkreview van 6 september 2026. De [meetlat](framework-contract.md) staat vast; we beginnen de review niet opnieuw. Het [reparatieverslag](../reviews/2026-09-06-framework/fixes/README.md) en het [aanvullende testbewijs](../reviews/2026-09-06-framework/acceptance/README.md) leggen vast wat al gedaan is.

**Begin:** de visie is beschreven, het framework is eraan getoetst, de gevonden frameworkfouten zijn gerepareerd en de lokale regressies zijn uitgevoerd. De HOP-patches zijn geïntegreerd via v1.0.2; documentatie en de laatste hardware-aftekening staan nog open.

**Eind:** de releasecode volgt onze visie, alle bekende blokkerende fouten binnen de beloofde functies zijn opgelost en de uiteindelijke kandidaat heeft de toepasselijke tests doorstaan. Naar ons beste vermogen werkt dit goed. Ongeteste boards en beperkingen staan eerlijk bij de release; die worden niet als bewezen ondersteuning gepresenteerd.

## De visie blijft de meetlat

- Compute met weinig draaiende delen: het gedeelde framework regelt eigendom, uitvoering, wachten, wekken en vrijgeven.
- Eén netwerk-IRQ en één logische deurbel per bewoner; architecturen verzorgen de fysieke uitvoering daarvan.
- Elke app heeft een eigen geheugenstuk. Na bevestigde beëindiging komt dat vrij, samen met haar coreclaim. Een core met een andere bewoner of groepsreservering blijft bezet.
- Dedicated waar isolatie nodig is; expliciet vertrouwde apps mogen cores delen. Sharing introduceert geen algemene preëmptieve scheduler.
- Eerst publiceren, dan bellen. Eerst een conflicterende actie afronden, dan de volgende. Bestaande volgorde vóór extra locks, administratie of achtergrondwerk.
- Een kernel-flip behoudt apps, hun geheugen en eigendom. Beheerverbindingen mogen opnieuw aansluiten; doorlopende appverbindingen en agentherstel worden afzonderlijk getoetst.
- Alleen benodigde code. Een fix mag vereenvoudigen; nieuwe functies en algemene herontwerpen horen niet in deze afronding.

**Afbakening van Derek, 6 september:** de fysieke netwerk-IRQ-aansluiting op alle boards en de bijbehorende hardwaretest krijgen een eigen vervolg na de docs-overhaul, voor een volgende puntrelease. De huidige pollingpaden blijven in deze kandidaat bestaan. Dit is een expliciet uitgesteld onderdeel van de visie, geen blokkade voor deze correctness-ronde en geen claim dat alle boards al interruptgedreven werken. Het bestaande logische deurbel-/wekprotocol blijft wel binnen deze ronde.

## De zes stappen

| Stap | Werk en concrete uitkomst | Huidige status |
| --- | --- | --- |
| 1. Meetlat en framework afronden | Bestaande bevindingen koppelen aan reparatie en bewijs. De frameworkdoorloop niet opnieuw doen. Overgebleven board-/drivergrenzen meenemen in stap 2. | Frameworkreparaties en gerichte host-/QEMU-regressies uitgevoerd. |
| 2. Resterende codepunten sluiten | De voorbereide log-adoptiepatch in de HOP-dependency opnemen via een gepubliceerde versie. Eén afgebakende correctness-doorloop van de gebruikte drivers: initialisatie, buffer-/DMA-grenzen, eigendom, publicatie, completion, foutafhandeling en overdracht bij stop/flip. Per driver: akkoord, concrete fix of expliciet buiten releasesupport. | Afgetekend voor code/host: HOP v1.0.2 geïntegreerd; drie driverreviews en hun fixes getest. Hardwaregrenzen en uitgestelde IRQ expliciet beschreven. Zie logboek L04–L08. |
| 3. Eén testbare kandidaat maken | Na de resterende fixes bronversie, dependencies, buildinstellingen en image-hashes vastleggen. Gerichte regressies van die fixes en de bestaande build-/integratiematrix op deze kandidaat uitvoeren. | Afgetekend: host-/targetgates, zeven definitieve bundels, lifecycle/netwerk-QEMU en echte agent op de exacte virt-bundel slagen. Bron-/artifact-hashes vastgelegd; logboek L09–L10. |
| 4. Laatste hardware-ronde | Onderstaande vaste lijst op de M4 en RISC-V uitvoeren. Begin met versie-/compatibiliteitscontrole, daarna flip naar de kandidaat en testen. Resultaten en eventuele afwijkingen bewaren. | ARM-deelproeven en flipreeksen geslaagd op de vastgelegde kandidaten. Nieuwe downloader voor 2.2.2 nog op hardware toetsen; morgen na de docs, met eerst LicheeRV. Zie L36–L42. |
| 5. Bevindingen, docs-overhaul en release vastleggen | Alleen aangetoonde fouten uit de ronde herstellen en gericht hertesten. Herstructureer en actualiseer de volledige documentatie vanuit het contract, zoals hieronder beschreven. Leg support, beperkingen, performance en bewijs vast. | Afgerond als parallelle Engelse set in docs/v2, met website-index/menu, gecontroleerde links/voorbeelden en inventaris. Oude docs behouden; website nog niet omgeschakeld. Zie L44. |
| 6. Aftekenen | Controleer onderstaande eindvoorwaarden; wijs exact de geteste kandidaat aan als releaseklaar. Publicatie is daarna de uitvoerhandeling, geen nieuwe reviewronde. | Open. |

Stap 2 is een controle op het bestaande aanbod, geen opdracht tot meer drivers of functies. Voor de release beloofde drivers moeten slagen; niet-geteste varianten krijgen expliciet geen hardware-aftekening. De claims voor andere boards blijven beperkt tot het werkelijk uitgevoerde bewijs, bijvoorbeeld alleen een geslaagde build.

## Stap 5: volledige docs-overhaul

Opdracht van Derek, 6 september: de docs zijn verouderd; nu het contract helder is, structureren we ze opnieuw als één samenhangend geheel. Dit is een eigen afwerkpunt, op Dereks verzoek nu vóór de laatste hardwarehertest. Het is geen extra productfunctie en geen reden om de afgesloten codecontrole opnieuw te beginnen.

1. Inventariseer alle publieke en technische docs. Geef iedere pagina één bestemming: actueel houden/herschrijven, samenvoegen, of als historisch onderzoek archiveren. Behoud het testbewijs.
2. Maak één duidelijke ingang en leesvolgorde: **visie en contract → architectuur en lifecycle → apps en core sharing → drivers en boards → bouwen/installeren/flippen → beheer, testen en releasebewijs**. Houd eenvoudige gebruiksinstructies leesbaar zonder interne onderzoeksverslagen nodig te hebben.
3. Herschrijf claims tegen de uiteindelijke code en metingen: IRQ/deurbel, geheugen/core-eigendom, stopbevestiging, sharing/SMP en zero-reboot flip. Benoem de uitgestelde board-IRQ-aansluiting, best-effort gedrag en werkelijk geteste hardware expliciet.
4. Geef elk onderwerp één actuele uitleg; verwijder dubbele of strijdige beschrijvingen. Markeer historische plannen/reviews als historisch en verwijs naar hun opvolger. Werk README, docs-index/menu, supportmatrix en onderlinge links mee bij.
5. Controleer voorbeelden, commando's, versie-/ABI-nummers en links. Teken de overhaul af wanneer een nieuwe lezer kan vinden wat het OS belooft, hoe het werkt, hoe hij het gebruikt en welk bewijs daarbij hoort, zonder tegenstrijdige routes.

Neem de actuele [Hier staan we-tabel](release-status.md) mee: mogelijkheden, boardbewijs en gepland werk blijven afzonderlijk herkenbaar.

Uitkomst: een kleine, geordende en actuele documentatieset die dezelfde eenvoud als het OS uitstraalt. Geen nieuwe wiki-infrastructuur of documentatieframework nodig.

## Follow-up after documentation: the complete board checklist

Decision Derek, 9 September: use the complete [board support checklist](../../support.md), including the new O6N. Review the whole small contract and the selected board facilities: ownership/start/stop, app and node idle, wake/doorbells, DVFS/clocks, temperature, Ethernet and network IRQ, PCIe/NVMe, framebuffer/input, entropy, watchdog and FLIP/build coverage. IRQ is one item in this list, not the exclusive next task. Close each gap with evidence, a supported fallback or an explicit scope decision; optional peripherals do not become additional acceptance conditions for the current compute release.

The current polling is fast enough for Derek's use. Any selected IRQ work should compare receive correctness, idle/power behavior, throughput and FLIP against that baseline; no throughput gain is assumed. Reuse the existing framework and update the matrix after acceptance.

**Updatepad, laatste besluit Derek:** de codewijzigingen worden v2.2.2. De LicheeRV-download-OOM is in de bron gecorrigeerd door de bundel rechtstreeks in het geleende venster te downloaden; de kern blijft 32 MiB. Eerst docs, daarna morgen koud installeren op LicheeRV en de nieuwe downloader op hardware toetsen. Pi 4 en Pi 5 hebben inmiddels hun drie-flips-reeksen doorstaan; die bewijzen hun geteste kandidaten, niet automatisch de nieuwe downloader. Zie L38–L42.

## De vaste hardwarelijst

Testnodes: **192.168.1.122 — M4 mini**, **192.168.1.150 — LicheeRV**, **192.168.1.144 — Pi 4**, **192.168.1.193 — Pi 5** en **192.168.1.241 — Radxa**. Derek heeft deze nodes voor de hardware-ronde beschikbaar gesteld. De beheer-API meldt negen appcores op M4, één op LicheeRV en drie op elk van de overige boards. Bereikbaarheid is gecontroleerd; dit is geen hardware-aftekening.

Release **2.2** is een versienaam; flip-ABI **2** is een afzonderlijk compatibiliteitsnummer. Voor de eerste sprong controleren we de concrete images en het overdrachtscontract, inclusief eventuele switchcode-eisen. Een passend ABI-nummer alleen bewijst niet dat twee willekeurige builds uitwisselbaar zijn. Een extra flash is niet de standaardstap: met een compatibele draaiende 2.2-kernel gaan we verder via flip.

| Volgorde | Proef | Geslaagd wanneer |
| --- | --- | --- |
| H0. Uitgangspunt | Noteer draaiende versie, kandidaat-hash, bootconfig, topologie, actieve testapps en vrije claims. Controleer flipcompatibiliteit. | We weten welke code en instellingen we testen en vergelijken. |
| H1. Start, stop, hergebruik | Twintig cycli van starten, werk uitvoeren, stoppen en opnieuw plaatsen; controleer geheugen en coreclaims. Controleer schoon geheugen bij een nieuwe eigenaar met een daarvoor gemaakte vertrouwde fixture. | Werk klopt; na bevestigde stop zijn de verwachte claims vrij; geen oude inhoud bij de volgende eigenaar en geen oplopend resourceverlies. |
| H2. Sharing en SMP | Twee vertrouwde bewoners delen een core; stop/vervang één en controleer de ander. Test op de M4 ook dedicated SMP en het vrijgeven van alle bijbehorende cores. | De buur loopt door; geen verkeerde core wordt gestopt; nieuwe plaatsing gebruikt alleen vrijgegeven capaciteit. SMP op de ééncore-RISC-V is niet van toepassing. |
| H3. Slapen en deurbel | Laat bewoners slapen en wek ze met toegestaan netwerk-/runtimewerk; herhaal ook met alle gedeelde bewoners slapend. Toets fysieke wake/idle op beide architecturen. | Geen verloren werk of blijvend slapende bewoner; geen extra pollinglus nodig om de test te laten slagen. |
| H4. Drie opeenvolgende flips | Houd appvoortgang en een app-TCP-verbinding bij; neem gedeelde bewoners en op de M4 SMP mee. Controleer taken, verse logs en daarna stop/vervang/hergebruik. | Geen appherstart; bootmarker gelijk en voortgang stijgt; dezelfde appverbinding blijft werken; agent neemt eigendom over en kan apps weer beheren. Logging herstelt volgens het bestaande best-effortcontract. |
| H5. I/O en performance | Draai dezelfde disk-/netwerkworkload vóór en na de kandidaat, op dezelfde node en met dezelfde instellingen, driemaal per versie. Meet start/stop/geheugenwissen apart. | Data-inhoud klopt; geen onverklaarde terugval buiten de gemeten spreiding. De genoemde circa 4000 MB/s telt alleen mee als die workload op het betreffende apparaat opnieuw is gemeten. |
| H6. Gecombineerd gebruik | Dertig minuten compute en I/O met normale slaap-/wekovergangen en enkele start-/stopacties. | Geen vastloper, onverwachte herstart, datacorruptie of oplopend verlies van geheugen-/coreclaims. |

Bij een gecontroleerd mislukte flip hoort ook automatisch watchdogherstel bij H4. Het blinde opstartbudget is begrensd; noteer de reset en terugkeer afzonderlijk van een geslaagde zero-reboot-flip. De interne agentprobe bewijst geen externe NIC-ontvangst.

Dit zijn begrensde acceptatieproeven, geen bewijs van onbeperkte uptime. Niet ieder geval is op ieder board toepasbaar; vermeld bij een overgeslagen proef de concrete reden. Actieve uitbraaktests blijven buiten deze afgesproken ronde. Een mislukte proef levert één bevinding met reproduceerbaar begin, verwacht gedrag en werkelijk resultaat op.

## Wanneer stoppen we met reviewen?

- [ ] De uiteindelijke code voldoet aan de visie en de regels E1–E9; bekende afwijkingen zijn opgelost of de betreffende functie behoort expliciet niet tot de releasebelofte.
- [x] De HOP-logpatch zit in de werkelijk gebouwde, gepubliceerde dependency; geen tijdelijke lokale vervanging in de release.
- [x] De voor de release gebruikte drivers zijn afgetekend tegen stap 2 (code-/hostscope; fysieke werking volgt in stap 4).
- [x] Buildmatrix en relevante hosttests slagen met de laatste bronwijzigingen (L41); eerder QEMU-integratiebewijs blijft aan de daar vastgelegde kandidaat gekoppeld.
- [ ] De toepasselijke hardwareproeven H0–H6 slagen op M4 en RISC-V, met bewaard bewijs.
- [ ] Er zijn geen open fouten binnen het beloofde aanbod die tot verkeerd eigendom, verloren wekwerk, vastlopen, onbedoelde herstart of datacorruptie leiden; gemeten performance heeft geen onverklaarde regressie.
- [ ] Releaseversie, bron-/image-hashes, supportmatrix, beperkingen en testresultaten zijn vastgelegd en beschikbaar in versiebeheer.
- [x] De volledige docs-overhaul staat als parallelle Engelse set in docs/v2: index/menu, actuele uitleg, gecontroleerde voorbeelden/links en inventaris; oude docs behouden op Dereks verzoek (L44).

**Als deze lijst is afgetekend, is deze ronde klaar.** We voegen dan geen nieuwe architectuurlagen, testsuites of reviewrondes toe zonder een concrete nieuwe bevinding. We hebben een kleine OS-kern die aantoonbaar doet wat we beloven, met duidelijke grenzen aan wat we hebben getest.

## Actuele aanvulling — L45

De Engelse documentatie staat nu rechtstreeks in `docs/`; de vorige set, dit draaiboek en het logboek staan in `docs/v1/`. Morgen eerst de gemelde LicheeRV-uitval met twee gedeelde apps reproduceren en debuggen, daarna de nieuwe flipdownload testen. Sharing is hiervoor opnieuw open; de bronreview heeft nog geen oorzaak bewezen.

## Release-day TODO — 7 September 2026

- [ ] **Website release cleanup (Derek, L56):** review all public pages for temporary investigation notes, placeholders and superseded failure notices. Replace them with the current, verified release capabilities and concrete remaining limitations. In particular, remove outdated suggestions that LicheeRV sharing or FLIP is broken; L53–L55 record the completed framework investigation. Keep debugging history in the logbook, and keep the separate application integration check explicit in the acceptance list. Check the final website against the documentation and release evidence before publication; no temporary status wording may remain merely because it was accurate during development.

Derek plans to publish tomorrow. Complete these two website follow-ups alongside the existing acceptance work:

- [ ] **LicheeRV:** reproduce and resolve the two-app shared-core failure, verify neighbor survival and stop/reuse, then test the revised download and consecutive FLIPs with resident/TCP preservation. Record the tested build and results; update the website and documentation status from that evidence.
- [ ] **M4 write throughput:** repeat the approximately 4000 MB/s workload three times on the release candidate. Record the command, storage device, data size, cache/flush policy, timing boundary and results. Update the website and measurements page with the reproducible result and conditions, replacing the current developer-reported attribution when supported.

Finish by checking that the website, documentation and release evidence describe the same candidate and results. The complete [board checklist](../../support.md) defines the separate follow-up; physical network IRQ is one of its items.


**7 September hardware update (L49–L50):** original RV startup OOM reproduction and three neighbor-preserving replacements pass on the single-buffer candidate. Revised downloader and four resident-preserving swaps pass; TCP checked in two pairs, with one pre-download dial timeout between them. Hash/identical rejection and subsequent app reuse pass. Still open: one uninterrupted three-flip TCP sequence; original cold-window reuse/capacity limitation; application origin/attach configuration; Cold restoration using the late-probe reboot retry now passes (L51); this does not close cold-window reuse. The earlier blanket “sharing failure” item is superseded by these specific findings. H6 and M4 throughput remain open.


**Scope correction, 7 September (L52):** remove the explicit reboot option, its board callback and the late-probe retry. Keep automatic watchdog fault recovery. Complete cold-window reuse through FLIP ownership/allocation; a reboot is not an acceptable substitute for an online update.


**L53 supersedes the RV kernel-window and TCP continuity blockers:** four consecutive live swaps preserve one resident and one continuously held TCP socket. Swaps 2 and 4 return to the original window; the full 206-MiB user workload starts afterward without reboot. Common allocator implementation and LicheeRV board declaration tested. Remaining overall release work includes application endpoint integration, the separately listed H6/I/O work and M4 throughput measurement; cold-window declarations on other boards require their fixed boot structures to be accounted for.

- [ ] L53 follow-up: identify the isolated plugin download stall (2.7/10.6 MB). Cancellation/release and retry passed; neighbor IDs remained stable. Keep separate from the successful kernel-window acceptance.

**L54 transport investigation:** ten original-source and ten local HTTP downloads pass; the basic 60-second silent-stream abort works. A separate false-idle timeout is reproduced with continuous slow traffic and corrected in the uncommitted HOP checkout. Await the candidate slow/silent hardware results and a published HOP version before changing the production dependency pin. The original isolated stall is not assigned an unproven cause.


**L55 transport result:** false-idle timeout reproduced and corrected; slow and silent hardware fixtures behave correctly, and 30 ordinary downloads pass across before/local/after runs. Original isolated stall not reproduced or assigned an unproven cause. Remaining release action: publish the uncommitted HOP change from `fix/download-idle-progress`, update the HopOS pin to that real tag, and verify the published-dependency build. Other explicitly listed application integration and broader I/O tasks retain their own scope.


**L59 cage/core separation:** remove the shared-app cage offset and the SMP requirement that cage identity equal its primary core. First-free cage allocation, independent physical placement and existing-app request translation now pass host/target tests and mixed sharing/SMP QEMU acceptance, including live FLIP and cleanup. Two requested LicheeRV FLIPs pass; the second returns to the original kernel window. Stulp, plugins and cloudflared are running from unchanged definitions as cages 1–3, with three seconds between submissions. Derek checks Stulp behavior. Publish the HOP allocation change together with the pending idle fix, pin that published dependency, and retain final artifact acceptance. ARM physical acceptance of the changed SMP switch remains distinct from QEMU evidence. The requested simplification review removed obsolete placement hints/adapters and multi-cage task reservations; no new locks, worker loops or administration allocation were added.


**L60 operator acceptance:** Derek confirms Stulp works correctly after L59, with no observed regression. Close the LicheeRV application integration item for this candidate. Remaining order: publish/pin HOP and build the release candidate; test M4, Pi 4, Pi 5 and Radxa physical sharing/SMP, stop/reuse and compatible consecutive FLIPs; finish H5 I/O comparisons (including the M4 write measurement) and H6 thirty-minute combined compute/I/O; remove temporary website wording and synchronize docs; sign off exact release artifacts. The changed ARM switch requires stopping incompatible old residents for the initial transition; subsequent same-switch FLIPs test resident/TCP continuity. No additional broad review or IRQ work is added.


**L61 storage capacity correction:** compact extent indexing removes the old aggregate16GiB logical-span limit. Host capacity/error/reuse tests and the target gate pass; the M4 candidate and9GiB source-plus-backup verification fixture are built. Physical deployment/testing remains open pending target and temporary-data confirmation. Existing HopFS metadata is not transferred across FLIP. Close this concrete regression with hardware evidence before final release; it belongs to the existing storage acceptance, not a new architecture expansion.


**L62 M4 update:** with Derek's completed backup and explicit one-time authorization, the extent candidate is installed by FLIP. Spin and cloudflared are restored and running. The automated large-copy fixture faulted before progress; its cause remains unestablished and physical HopFS capacity acceptance is still open for Derek's database/backup check. Do not add metadata migration: Derek explicitly excludes that work for this transition.


**L63 supersedes L62 startup signoff:** the first restored apps faulted shortly after the running snapshot. A diagnostic FLIP restores observed local HTTP operation, but the ARM transition cause remains open. Cloudflared also requires the original deployment order because its configured origin is Spin at 10.100.0.3. Verify sustained local and tunnel operation before claiming services restored; physical large-copy acceptance remains open.


**L64 service restoration:** Derek restores cloudflared first, Spin second, preserving the tunnel origin at 10.100.0.3. Both tasks report zero restarts and local Spin HTTP returns 200; Derek confirms operation. Immediate restoration is complete. The original ARM transition fault investigation and physical large-copy acceptance remain separate open items. Preserve this deployment order on future updates.


**L65 board support inventory:** the current English matrix is [docs/support.md](../../support.md). It replaces the archived support snapshot, includes O6N and separates wired paths, fallbacks, missing facilities and hardware acceptance. The final follow-up now reviews this entire checklist rather than only network IRQ. Documentation inventory only: no board, image or driver changes.


**L67 large-memory correction:** shared allocation/release/adoption now accounts for architecture translation storage in one claim. M4 5 GiB and 18 GiB app mapping tests pass; the 18 GiB resident and both user services survive a real generation-5 FLIP with unchanged IDs and patterns, then release/reuse succeeds. Spin is restored at 2 GiB with cloudflared first. See [evidence](release-evidence/2026-09-09-large-partitions.json). The ARM code-publication gap is corrected; the historical fault's precise cause remains unproven. HopFS disk-copy acceptance, other-board final artifact checks and the complete board follow-up list remain separate.


**L68 measurement and transport follow-up:** Vitals capability/reporting corrections are separate from hardware acceptance. Repeat the reported Pi cross-node transfer beyond 1 MiB with known artifacts; the saved trace shows severe throughput collapse and excessive RX wake activity. Host paired-stack transfers pass, but the hardware cause remains unproven. Close this concrete transport failure before final signoff; do not defer it with the optional physical network-IRQ work. Pi timer quantization itself matches the existing event-stream setting.


**L70 supersedes the open L68 TCP item:** Pi4/Pi5 physical transfers, larger full-content checks and simultaneous bidirectional checks pass on the recorded review builds. Both nodes are restored to Welcome only. No kernel FLIP was needed. The remaining candidate-build step must carry the changes across HopOS, Lean and the TamaGo runtime and rebuild affected apps; this hardware result does not automatically update already published binaries.
