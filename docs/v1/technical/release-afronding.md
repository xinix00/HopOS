# Afrondingsplan voor de eerste publieke release

Voortgang, aftekeningen en besluiten staan in het [release-logboek](release-logboek.md). Dat logboek wordt bij iedere afgeronde wijziging of nieuwe bevinding bijgewerkt; aftekenen gebeurt uitsluitend met genoemd bewijs.

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

## Vervolg na de docs: fysieke netwerk-IRQ in een volgende puntrelease

Besluit Derek, 6 september: eerst de huidige correctness-ronde en docs afronden. Daarna de fysieke netwerk-IRQ per board aansluiten en toetsen, vanuit dezelfde visie: één netwerk-IRQ en één logische deurbel per bewoner. Dit is een afzonderlijk vervolg op de zes release-afrondingsstappen, geen extra eindvoorwaarde voor de huidige release.

De huidige polling is volgens Derek snel genoeg. Het doel van dit vervolg is vooral minder CPU-werk bij stilte en lager energieverbruik; een snelheidswinst is geen voorwaarde of vooraf bewezen claim. Controleer per board ontvangst, slapen/wekken en flip-overdracht, en vergelijk idle-/energiegedrag en doorvoer met polling. Werk daarna de boarddocumentatie en het testbewijs bij en lever het als nieuwe puntrelease. Het versienummer staat nog niet vast.

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

Finish by checking that the website, documentation and release evidence describe the same candidate and results. Physical network IRQ remains the separate later task.


**7 September hardware update (L49–L50):** original RV startup OOM reproduction and three neighbor-preserving replacements pass on the single-buffer candidate. Revised downloader and four resident-preserving swaps pass; TCP checked in two pairs, with one pre-download dial timeout between them. Hash/identical rejection and subsequent app reuse pass. Still open: one uninterrupted three-flip TCP sequence; original cold-window reuse/capacity limitation; application origin/attach configuration; Cold restoration using the late-probe reboot retry now passes (L51); this does not close cold-window reuse. The earlier blanket “sharing failure” item is superseded by these specific findings. H6 and M4 throughput remain open.


**Scope correction, 7 September (L52):** remove the explicit reboot option, its board callback and the late-probe retry. Keep automatic watchdog fault recovery. Complete cold-window reuse through FLIP ownership/allocation; a reboot is not an acceptable substitute for an online update.


**L53 supersedes the RV kernel-window and TCP continuity blockers:** four consecutive live swaps preserve one resident and one continuously held TCP socket. Swaps 2 and 4 return to the original window; the full 206-MiB user workload starts afterward without reboot. Common allocator implementation and LicheeRV board declaration tested. Remaining overall release work includes application endpoint integration, the separately listed H6/I/O work and M4 throughput measurement; cold-window declarations on other boards require their fixed boot structures to be accounted for.

- [ ] L53 follow-up: identify the isolated plugin download stall (2.7/10.6 MB). Cancellation/release and retry passed; neighbor IDs remained stable. Keep separate from the successful kernel-window acceptance.

**L54 transport investigation:** ten original-source and ten local HTTP downloads pass; the basic 60-second silent-stream abort works. A separate false-idle timeout is reproduced with continuous slow traffic and corrected in the uncommitted HOP checkout. Await the candidate slow/silent hardware results and a published HOP version before changing the production dependency pin. The original isolated stall is not assigned an unproven cause.


**L55 transport result:** false-idle timeout reproduced and corrected; slow and silent hardware fixtures behave correctly, and 30 ordinary downloads pass across before/local/after runs. Original isolated stall not reproduced or assigned an unproven cause. Remaining release action: publish the uncommitted HOP change from `fix/download-idle-progress`, update the HopOS pin to that real tag, and verify the published-dependency build. Other explicitly listed application integration and broader I/O tasks retain their own scope.
