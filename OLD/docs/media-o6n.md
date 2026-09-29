# Lumen op de Orion O6N

Deze lokale configuratie hoort bij Dereks o6n op `192.168.1.205`, met
HopOS **v2.2.8 media** met de lokale VPU-herstelwijziging en
`hopos.storage=stateful`. De draaiende herstelbuild is hieronder vastgelegd.

## Vastgelegde bestanden

- [`job-lumen.json`](../job-lumen.json): alleen Lumen, 11 cores, 24 GiB RAM,
  portal op 8098 en WebDAV op 8099. Behoudt de bestaande volume-namen
  `/medialib`, `/mediakeys` en `/devices`. De private mount
  `/firmware` → `/codec-firmware` bewaart decoderfirmware buiten WebDAV.
- [`image/hopos-media-o6n.cfg`](../image/hopos-media-o6n.cfg): bootconfig met
  dezelfde job als enige `hopos.init[]`, DVFS, 768 MiB codec-arena en stateful
  opslag. Geen desktop-initjobs of applicatiecatalogus.
- [`tools/media-o6n.py`](../tools/media-o6n.py): verwijdert andere gewenste jobs
  en start deze Lumen-config. Weigert een cluster met meerdere nodes; een
  job verwijderen via de leader-API zou anders ook andere nodes beïnvloeden.
  Een identieke, al draaiende Lumen-job wordt niet opnieuw gestart. Een gewijzigde
  job wordt niet toegepast zolang een back-up actief is of de toestand onbekend is.

De app blijft vanwege zijn eigen licenties in `../hop-app-lumen`.
Het vaste artifact is `lumen-20260928-1.elf`, SHA-256:

```text
5d9ba368b21e851ab9ea271e28bdab37c9c626b21ca33eae391206a19907f856
```

Het wordt vanaf de Mac op `192.168.1.208:8002` geladen. Deze server moet ook
bij een koude start bereikbaar zijn. Als hij nog niet draait:

```sh
python3 -m http.server 8002 --bind 0.0.0.0 --directory ../hop-app-lumen/out
```

De ELF staat in `../hop-app-lumen/out/lumen-20260928-1.elf`. Opnieuw bouwen
kan vanuit die repository met `BUILD_ID=lumen-20260928-1 EMBED_SDF=1
./tools/build.sh lumen`, gevolgd door kopiëren van `out/lumen.elf` naar die
versienaam. Archiveer het bestaande artifact; gewijzigde broncode of SDK vraagt
een nieuwe buildnaam en hash. Pas de artifact-URL in job én bootconfig samen aan.

## Op de draaiende o6n toepassen

Vanuit de hoofdmap van deze repository:

```sh
python3 tools/media-o6n.py
```

Dit stopt andere apps en verwijdert hun gewenste jobs, zodat de scheduler ze
niet terugplaatst. Het verwijdert geen volumes of bestanden. Het script
controleert het artifact voordat het apps stopt en controleert daarna de
daadwerkelijke Lumen-portal. Het gebruikt de bestaande open LAN-API.

Portal: <http://192.168.1.205:8098/>. WebDAV: <http://192.168.1.205:8099/>.

## Ook bij een koude start alleen Lumen

Een API-deployment herschrijft de bootstick niet. Zet
`image/hopos-media-o6n.cfg` als **`hopos.cfg` in de root van de UEFI-stick**.
Gebruik daarbij de media-release, met `BOOTAA64-o6n-media.EFI` als
`EFI/BOOT/BOOTAA64.EFI`.

Op 27 september is ook `out/media-o6n/` klaargezet met:

- `hopos.cfg` en `EFI/BOOT/BOOTAA64.EFI` om naar de bestaande stick te kopiëren;
- `hopos-o6n-media-stateful-v2.2.8.img`: de officiële media-image, met alleen
  het configuratievenster vervangen via de bestaande `image/hopcfg`-tool.

Het release-image is tegen de lokale releasechecksums gecontroleerd; de
teruggelezen configuratie uit de aangepaste image is bytegelijk aan de opgeslagen
config. De fysieke bootstick is niet aangepast: hij was niet op de Mac gemount.
Met zijn huidige GUI-initconfig komen display en launcher na een reboot terug;
voer dan het script opnieuw uit, of installeer vooraf de bovenstaande bootconfig.

## Persistentie controleren

De node meldde bij zijn huidige boot expliciet `storage: stateful` en een lege
nieuwe bestandstabel. Het aangebrachte controlebestand staat op de gedeelde
NVMe-volume, via WebDAV op `/persistentie-test.txt`. De originele inhoud en
SHA-256 staan in `out/media-o6n/persistentie-test.txt` en `persistentie-test.json`.
Deze bestanden zijn een lokale meetbasis; schrijf het controlebestand na een
reboot niet opnieuw voordat je het vergelijkt.

HopOS schrijft gewijzigde bestandmetadata elke 10 seconden weg. Wacht na een
nieuwe upload minstens één interval voordat je een koude herstart test. Een
app-herstart of live kernel-flip bewijst geen persistentie na een koude start.
Na jouw herstart, zodra Lumen weer draait:

```sh
python3 - <<'PY'
import hashlib, json, pathlib, urllib.request
reference = json.loads(pathlib.Path('out/media-o6n/persistentie-test.json').read_text())
data = urllib.request.urlopen(reference['url'], timeout=15).read()
assert len(data) == reference['bytes']
assert hashlib.sha256(data).hexdigest() == reference['sha256'], 'Inhoud verschilt'
print('Controlebestand is ongewijzigd teruggevonden.')
PY
```

`WEBDAV_STORAGE=stateful` in de job verwijdert alleen Lumens oude melding over
vluchtige opslag. **De node-instelling `hopos.storage=stateful` regelt de echte
opslag.** Die is in de huidige bootlog gecontroleerd. Er is geen reboot uitgevoerd
als onderdeel van het klaarmaken; de koude-starttest staat nog open.

## Controle op 27 september 2026

Alleen Lumen draait. De ontbrekende firmware is automatisch opgehaald; een
volgende poging gebruikte alle 16 lokale kopieën. Daarna bleek de VPU vast te
zitten: eerst 45 seconden zonder events, daarna `session slot 0 will not
terminate`. Alleen de app of de kernel vervangen loste dat niet op.

Herstel is op het echte apparaat gelukt via SCMI: alleen VPU-domeinen 15..11
uit, daarna 11..15 aan. De gedeelde MM-hub blijft aan. Daarna was TERMINATE
weer nul en verwerkte dezelfde H.264-film met dezelfde instellingen weer beeld.
De waargenomen PGCTRL ging daarbij van `0x07cef000` naar `0x07cefffc`.
SCMI's logische ON-status alleen is dus onvoldoende als initialisatiebewijs.
De definitieve bron controleert bij het starten ook deze ACPI-powerbits en
alle vier de sessies, en herstelt de VPU vóór de codec-engine wordt geregistreerd.
Het koude-startpad met deze extra detectie is nog niet op hardware getest.

De actieve kernel is `v2.2.8-vpu-recovery-2`, met herstel van het vastgelopen
sessieslot en tijdelijk enkele diagnostiekregels. De opgeschoonde build
`v2.2.8-vpu-recovery-3` staat klaar; **niet live toepassen tijdens een back-up**:
codec-sessies worden niet door een kernel-flip voortgezet. De lopende batch is
niet onderbroken voor die cosmetische opschoning/extra opstartdetectie.

Lumen `lumen-20260927-3`, taak `V2F8APTFV56A4K9V`, verwerkt beide afleveringen
van `GAMEOFTHRONES_S1_D1`, Engels TrueHD PID 4353, map `Game Of Thrones`,
profiel `faster-b2`, 11 cores en 24 GiB. Na 225 seconden: 6.024 frames duurzaam
opgeslagen, 564.541.628 bytes uitvoer, 27,25 fps en 6,79% van aflevering 1/2.
De tijdelijke fysieke naam is `Game Of Thrones 4.ts` door eerdere mislukte
reserveringen; de bestaande publicatielogica geeft de voltooide aflevering het
eerste vrije zichtbare nummer.

Tijdens diagnose veroorzaakte een directe hardware-resetproef een watchdog-
herstart; die proefcode is verwijderd. Het oorspronkelijke persistentietest-
bestand kwam bytegelijk terug en de firmware bleef behouden. De oude bootstick
seedde display/launcher opnieuw; het script heeft die weer verwijderd en alleen
Lumen gestart. De fysieke bootstick is nog niet vervangen.

Bewijs: `out/media-o6n/vpu-recovery-console.log`, `vpu-recovery-state.json` en
`vpu-recovery-agents.json`. Downloadbewijs: `firmware-install.log`.

## Definitieve herstelbuild voor een volgende start

Klaargezet in `out/media-o6n/`:

- `EFI/BOOT/BOOTAA64.EFI`: media-kernel `v2.2.8-vpu-recovery-3`;
- `hopos-o6n-media-stateful-v2.2.8-vpu-recovery-3.img`: volledige bootimage met
  `image/hopos-media-o6n.cfg`; de teruggelezen config is bytegelijk;
- `hopos-o6n-v2.2.8-vpu-recovery-3.flip`: vervanging zonder koude reboot,
  uitsluitend wanneer geen codec-sessie actief is;
- `vpu-recovery-flip.json`: URL en SHA-256 voor die flip;
- `vpu-recovery-artifacts.json`: checksums van flip, bootimage en EFI.

Bouwen vanuit de repo:

```sh
MEDIA=1 CFG=image/hopos-media-o6n.cfg HOPOS_VERSION=v2.2.8-vpu-recovery-3 ./image/flip-bundle.sh o6n
BOARD=o6n MEDIA=1 CFG="$PWD/image/hopos-media-o6n.cfg" HOPOS_VERSION=v2.2.8-vpu-recovery-3 ./image/uefi-run.sh agent
```

De firmware blijft een aparte download op het stateful volume. De oorspronkelijke
release-image is behouden naast de herstelimage. Voor een volgende koude start
is de herstelimage de bedoelde versie, plus de meegeleverde media-config.

Validatie: MVE-tests en de codec-slot-ABI-tests slagen. De zuivere hersteltests
lopen apart van het TamaGo-boardpakket (dat pakket kan niet met host-Go worden
gebouwd):

```sh
cd metal
go test -race board/o6n/hop/vpu_recovery.go board/o6n/hop/vpu_recovery_test.go
go test ./media/driver/vpu/mve ./driver/scmi
go test -tags media ./kern/slots
```

De hersteltests dekken de gemeten verkeerde/gezonde opstarttoestand, een vast
slot op elk van de vier posities, de stroomvolgorde en direct stoppen bij elke
mogelijke SCMI-fout. De complete kernel bouwt met de TamaGo-toolchain.

## Automatische codecfirmware

Lumen haalt de volledige set van 16 MVE-firmwares (11 decoders en 5 encoders) automatisch
op vóór de encoder begint, schrijft deze in `/firmware` en gaat met dezelfde
back-up verder. De bron is Sky1-Linux/sky1-firmware, vastgezet op commit
`dd81690747ddb092bcc2a221daa0f8f679ee4bc4`; grootte en SHA-256 worden
gecontroleerd vóór gebruik. De firmware wordt niet in de app gebundeld.
De eerste download vereist HTTPS-toegang tot raw.githubusercontent.com.
Een geldige lokale kopie werkt daarna zonder internet en blijft bij stateful
opslag behouden. Bij een netwerkfout zijn er maximaal drie downloadpogingen
per bestand van elk 35 seconden, met twee minuten als totale downloadlimiet; daarna toont het portaal de fout.

## PWA-push, build lumen-20260927-4

De opgeslagen job en bootconfig bevatten nu ook `/lumen-notifications` →
`/notifications`. Deze private stateful volume bewaart VAPID-sleutels,
abonnementen en wachtende Web Push-events buiten WebDAV. Geen account of
externe meldingenapp nodig; de PWA gebruikt de pushdienst van de browser.
De standaard HTTPS-contact-URL in VAPID is de Lumen-project-URL, instelbaar met
`LUMEN_PUSH_SUBJECT`. Het concrete HTTPS-adres van Dereks PWA is nog niet opgegeven.

De portaal- en voortgangspagina bevatten **Meldingen aanzetten/uitzetten**.
De eerste melding volgt na een bruikbare snelheidsmeting (minimaal 2 minuten en
1.000 opgeslagen beelden): **Back-up gestart — Verwachte eindtijd: 13:32 (raming)**.
De raming omvat alle gekozen titels. Na de laatste opgeslagen titel volgt
**Back-up klaar**. Korte ketentests melden geen voltooide film.
De tijd wordt op het ontvangende apparaat in de eigen tijdzone geformatteerd.

Hosttests met de race detector controleren duurzame abonnementen, herstel na een
halve schrijfactie, deduplicatie, uitgesteld verzenden, HTTP 410, versleutelde en
ondertekende HTTP-push, endpoint- en origincontroles, PWA-assets, de browserknop
bij mislukte opslag en service-worker-notificaties. De volledige TamaGo-app
bouwt. Een echte aflevering via Apple's pushdienst op de iPhone vereist nog
het HTTPS-adres en een door Derek toegestaan browserabonnement.

De eerdere `vpu-recovery-3.flip` bevat nog de Lumen-3-initjob. Gebruik voor de
nieuwe app de huidige job/config; die flip is uitsluitend het eerder bewaarde
VPU-herstelartifact. `out/media-o6n/lumen-push-artifacts.json` registreert de
nieuwe app en de bijgewerkte koude-bootimage met dezelfde VPU-kernel.

De update is op 27 september toegepast **nadat** de actieve batch `done`,
2/2 titels en twee zichtbare video's meldde. Lumen `lumen-20260927-4` draait nu
op de o6n; geen kernel-flip uitgevoerd. `/api/push/key` levert een geldige publieke
P-256-sleutel, de beide pagina's bevatten de meldingenknop, manifest/iconen en
`/sw.js` worden correct aangeboden. De bibliotheek bevat nog steeds beide video's.
Bewijs: `out/media-o6n/completed-before-push-update.json` en
`out/media-o6n/push-update-state.json`. Er is geen pushabonnement namens Derek
gecreëerd; toestemming voor meldingen gebeurt via de knop op zijn apparaat.

## Discwissel en automatisch laden, 28 september

`lumen-20260928-1` is live gezet terwijl de vorige job op `error` stond;
er liep geen back-up. D4 had een oude selectie-fout en acht bewaarde video's.
Elke discinvoer krijgt nu een revisie; vervanging wist servercatalogus en oude
voortgang. Een expliciete uitleesactie detecteert ook naam-/capaciteitsverschillen
zonder eject-event. De status-API pollt eerst de disc en leest daarna de catalogus.
Een disc-ID-mismatch maakt de selectie vrij in plaats van een blijvende fout.

De pagina haalt titels en audio automatisch op bij een nieuwe disc, selecteert
Engels en sluit eventueel nog openstaande keuzes van de vorige disc. Laat
terugkerende responses worden genegeerd. **Opnieuw laden** blijft een herstelactie;
stabiele pagina-polls lezen niet steeds opnieuw metadata of schijfsectoren.
De eerdere selectie-fout van D4 wordt bij deze upgrade opgeruimd.

Regressietests (race detector) controleren vervanging zonder lege lade,
dezelfde naam/capaciteit met nieuw event, een late oude audioresponse, automatisch
laden, handmatig opnieuw proberen en de volgorde van de status-snapshot.
Een mobiele Chromium-weergave van de echte node (via een lokale HTTP-relay voor
Chromes LAN-toegang) laadde D4 zelfstandig, deed precies één audioaanvraag, zette
Engels aan en opende de map-/titelkeuze zonder browserfouten. De test bevestigde
de start niet. Acht video's en de bestaande publieke VAPID-identiteit bleven gelijk.
Bewijs: `out/media-o6n/disc-change-before.json`, `disc-change-after.json` en
`disc-change-artifacts.json`. De bijgewerkte koude-bootimage en `hopos.cfg` staan
lokaal klaar; de fysieke stick is niet aangepast.
