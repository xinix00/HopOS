# Hier staan we

Werkstand van de release-afronding, 6 september 2026. Deze tabel wordt tijdens de hardware-ronde bijgewerkt en krijgt bij de docs-overhaul een plek in de publieke documentatie. Het [contract](framework-contract.md) beschrijft de visie; het [logboek](release-logboek.md) verantwoordt het bewijs. Een open test betekent niet dat een functie ontbreekt.

## Wat HopOS kan

| Mogelijkheid | In het OS | Stand van het bewijs |
| --- | --- | --- |
| Compute op ARM64 en RISC-V via hetzelfde framework | Ja | Host-/targetgates, QEMU en fysieke lifecycleproeven. |
| Eigen geheugenpartitie per app, vrijgeven na bevestigde stop | Ja | Twintig start/werk/stopcycli op alle vijf testboards; gerapporteerde claims keren terug. Op vier ARM-boards zijn daarnaast drie 1 MiB-vensters bij hergebruik schoon teruggevonden. |
| Dedicated cores en vertrouwde core sharing | Ja | Gedeelde buur blijft lopen bij stop/vervanging op alle vijf boards. Combinatie sharing + core-class en daaropvolgend SMP slagen op M4 en Radxa na de plaatsings- en adapterfix. |
| SMP binnen één app | Ja | Tweewerker-compute naast gedeelde bewoners op M4, Pi 4, Pi 5 en Radxa. LicheeRV heeft één appcore; SMP daar niet toepasbaar. |
| Slapen en wekken via één logische deurbel per bewoner | Ja | Voortgang en wake-observaties uitgevoerd; volledige fysieke idle-/wake-aftekening volgt nog. |
| Kernel vervangen zonder apps te herstarten — FLIP | Ja | M4: drie opeenvolgende flips met dezelfde app-TCP-socket, gelijke bootmarkers, oplopende voortgang en herstelde agentlogs. Daarna stop/vervang/hergebruik geslaagd. QEMU-flipbewijs bestaat afzonderlijk. |
| Watchdogherstel bij mislukte flip-opstart | Ja; blinde voeding nu begrensd op de ruwe hardwareteller | M4: normale boot met SNTP slaagt; gecontroleerd vastgelopen opstart herstelt zelfstandig na circa 167 s. Pi-uitval nog niet verklaard. |
| Netwerk en agentbeheer | Ja | Lifecycle, compute via HTTP en contentcontrole uitgevoerd. Netwerkpoorten blijven tijdens flip-adoptie geclaimd. |
| Opslag via de app-API | Ja, afhankelijk van board/opslag | M4: bytes/hash en opruimen vóór en na flips gecontroleerd. De genoemde circa 4000 MB/s is nog niet opnieuw gemeten; de eenmalige circa 200 ms bij de eerste aanroep na flip is gelokaliseerd bij intern verbindingsherstel. |
| Fysieke netwerk-IRQ als vervanging van polling | Gepland voor volgende puntrelease, na de docs | QEMU heeft netwerk-IRQ al. De fysieke boards gebruiken nu polling, dat voor de huidige release snel genoeg is; lager energieverbruik is het doel van het vervolg. |

## Waar het op hardware is aangetoond

| Board | Architectuur | Appcores | 20 lifecyclecycli | Sharing / buur behouden | SMP-compute | Drie flips + blijvende app-TCP + beheer daarna | Fysieke netwerk-IRQ |
| --- | --- | ---: | --- | --- | --- | --- | --- |
| Mac mini M4 | ARM64 | 9 | Geslaagd | Geslaagd | Geslaagd | Geslaagd met laatste fixes, generaties 11–13; inkomende én uitgaande TCP | Gepland |
| LicheeRV | RISC-V | 1 | Geslaagd | Geslaagd | Niet toepasbaar | Wacht op 2.2.1 en hertest; oude downloader valt uit | Gepland |
| Raspberry Pi 4 | ARM64 | 3 | Geslaagd | Geslaagd | Geslaagd | Herhaling na reset valt opnieuw uit na download; UART-diagnose open | Gepland |
| Raspberry Pi 5 | ARM64 | 3 | Geslaagd | Geslaagd | Geslaagd | Eerste flip geslaagd; herhaling na reset valt opnieuw uit; UART-diagnose open | Gepland |
| Radxa | ARM64 | 3 | Geslaagd | Geslaagd | Geslaagd | Geslaagd, generaties 3–5; inkomende én uitgaande TCP | Gepland |

Bewijs: [hardwaremap](../reviews/2026-09-06-framework/hardware/), met name logboek L15, L26 en L28. De kolom lifecycle tekent de uitgevoerde cycli af, niet automatisch de aanvullende proef op schoon geheugen. De drie-flips-proef is een begrensde acceptatietest, geen claim van onbeperkte foutloosheid. Uitgaande NAT-verbindingen zijn op M4 en Radxa in dezelfde drie-flips-proef getoetst.

De resterende afwerkpunten staan in het [draaiboek](release-afronding.md): bekende fouten sluiten, ontbrekende hardware-/I/O-proeven, dertig minuten gecombineerd gebruik, docs-overhaul en eindaftekening. Fysieke netwerk-IRQ is het expliciet geplande functionele vervolg; open aftekeningen worden niet als ontbrekende productfuncties gepresenteerd.
