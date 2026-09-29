# De kern-flip

Een draaiende node vervangt zijn kern zonder herstart. De apps, Hop, hun
verbindingen door de switch, de NAT-flows en de volumes blijven; alleen de
kern wisselt. Dit is de procedure per board, de markers die erbij horen, de
faalmodi en wat de boot-guard doet. De code: `hopos/src/flip.rs` (beleid),
`kern/src/kernflip.rs` (bundel, blob, recorder), `cpu/src/el2/chain.rs` (de
sprong), `image/flip-bundle.sh` (de bundel), de `FLIP_*`-blokken in
`board/*/src/slots.rs` (de adressen) en `board/uefi/src/flip.rs` (de
feitenpagina van UEFI).

## Wat er overgaat, en wat niet

| Wat | Hoe |
| --- | --- |
| De bewoners (Hop en elke app) | Hun kooien, partities en cores blijven staan; het handoff-blob draagt de boekhouding en de nieuwe kern adopteert ze (`HOPOS_FLIP_ADOPT`). Hop wordt niet herstart. |
| De gepubliceerde poorten | De nieuwe kern publiceert ze opnieuw (`HOPOS_HOP_PUBLISH`, `republish`); een TCP-verbinding naar een gepubliceerde poort overleeft de sprong (DNAT is stateloos). |
| De NAT-flows (conntrack) | De switch-actor geeft een snapshot als waarde (`Command::SnapshotNat`) en zet daarmee de masquerade dicht; na de landing houdt de nieuwe switch de node-poorten vast vóór zijn eerste ronde, en na de adoptie komen de flows terug (`RestoreNat`). |
| De volumes (hopfs) | Vóór de sprong legt de actor de boom vast en neemt hij niets meer aan (`HOPOS_FS_FROZEN generation=N`); de nieuwe kern mount precies die generatie (`HOPOS_FS_UP fresh=0 generation=N`). |
| De switch-code van de app-cores | Blijft staan; de nieuwe kern adopteert haar alleen bij een gelijke som, en die som toetst de oude kern al vóór de sprong. |
| Niet: de system-API-verbindingen | Die zijn van de kern; Hop's system-client bouwt ze opnieuw op. |
| Niet: file calls tussen bevriezing en sprong | Die krijgen `Busy` (`HOPOS_FS_FROZEN_CALL`); de aanroeper doet ze opnieuw op de nieuwe kern. |
| Niet: een nieuwe uitgaande verbinding tussen snapshot en sprong | Die krijgt geen flow; zijn SYN-retransmit krijgt er op de nieuwe kern een. |
| Niet: de neighbor-cache | Leert passief terug binnen één ARP-ronde (een verkeerd overgenomen next-hop is erger). |

## De procedure, voor elk board

De flip heeft één trigger: `POST /flip` op de agent-API van Hop, achter
dezelfde HMAC als een jobspec (met `hopos.insecure=1` zonder). Er is geen
console-commando en geen config-sleutel (Derek, 01-09: één weg, geen
opties).

1. Bouw de bundel van dezelfde bron als de draaiende kern, met een eigen
   stempel zodat je de twee op de console uit elkaar houdt:

   ```sh
   HOPOS_STAMP=B sh image/flip-bundle.sh <board>
   ```

   Board: `virt`, `uefi`, `o6n`, `altra`, `rpi4`, `rpi5`, `radxa`. Uitvoer:
   `target/hopos-<board>.flip`, `.sha256` (het vertrouwensanker) en
   `.switch` (de som van de switch-code, ter controle).

2. Zet hem op een webserver op de laptop:

   ```sh
   cd target && python3 -m http.server 8000
   ```

3. Vraag de flip aan:

   ```sh
   curl -X POST -H 'Content-Type: application/json' \
     -d '{"url":"http://LAPTOP:8000/hopos-<board>.flip","sha256":"<de som uit .sha256>"}' \
     http://NODE:8080/flip
   ```

   `202` betekent: de kern nam de bundel aan en springt over een halve
   seconde. `502` met een reden: de kern weigerde vóór de sprong en draait
   gewoon door.

4. Kijk naar de console, in deze volgorde:

   | Kern | Marker | Betekenis |
   | --- | --- | --- |
   | A | `HOP_FLIP` (slot 1) | Hop haalt de bundel |
   | A | `HOPOS_FLIP_SWITCHCODE_OK` | de switch-code van de bundel is die van de bewoners |
   | A | `HOPOS_FLIP_STAGED` | som getoetst, beeld plat en gerelokeerd naar het koude adres |
   | A | `HOP_FLIP_ACCEPTED` (slot 1) | Hop kreeg het ja |
   | A | `HOPOS_FS_FROZEN generation=N` | hopfs vastgelegd en dicht (alleen met een schijf) |
   | A | `HOPOS_FLIP_NAT_CAPTURED flows=K` | de conntrack gevangen, de masquerade dicht |
   | A | `HOPOS_FLIP_JUMP gen=G` | de laatste regel van kern A |
   | B | de bunny, `HOPOS_FLIP_BOOT gen=G` | het blob is gelezen |
   | B | `HOPOS_BOOT gen=G stamp=B` | de nieuwe kern met het nieuwe stempel |
   | B | `HOPOS_FS_UP fresh=0 generation=N` | dezelfde N als bij `HOPOS_FS_FROZEN` |
   | B | `HOPOS_FLIP_ADOPT n of n resident(s)` | alle bewoners terug |
   | B | `HOPOS_HOP_RESUMED` | Hop draait door, niet herstart |
   | B | `HOPOS_FLIP_NAT restored=K of=K` | elke flow terug |
   | B | `HOPOS_FLIP_SETTLED` | de guard is tevreden: adoptie en net binnen de gratie |

   Daarna: `GET /tasks` op de agent-poort antwoordt, en een jobspec naar de
   leader wordt door kern B geplaatst.

### Per board

| Board | Bundel | Koud adres | Staging, blob, recorder, trampoline | Bewezen |
| --- | --- | --- | --- | --- |
| QEMU virt | `flip-bundle.sh virt` | `0x4020_0000` (link.ld) | de staging van QEMU op `0xB020_0000`, de boot-scratch-pagina's op `0xB000_0000` | `sh tools/qemu-test-flip.sh`, ook `MISMATCH=1` |
| UEFI (EDK2) | `flip-bundle.sh uefi` | de basis die de firmware koos (`__efi_head`), binnen `SizeOfImage` | de loader-regio van het kernvenster (`0x5000_0000` + 256 MB) | `sh tools/qemu-uefi-flip-test.sh` |
| Orion O6N | `flip-bundle.sh o6n` | idem | idem, venster op `0x8800_0000` | nog niet op ijzer |
| Ampere Altra | `flip-bundle.sh altra` | idem | idem, venster op `0x8800_0000` | nog niet op ijzer |
| Pi 4, Pi 5 | `flip-bundle.sh rpi4` / `rpi5` | `0x80000` (link-raspi.ld) | de initramfs-plek van Hop op `0x0F20_0000`, de boot-scratch op `0x0F10_0000` | nog niet op ijzer |
| Radxa Zero 3E | `flip-bundle.sh radxa` | `0x0221_0000` (link-rk3566.ld) | het staging-venster op `0x0780_0000`, de recorder op `FLIP_SCRATCH_PA`, de trampoline op de bovenste pagina van de kern-RAM | nog niet op ijzer |

Wat per board eerst te kijken is, morgen op het ijzer:

- **UEFI, O6N, Altra.** Een geflipte kern komt binnen zonder firmware:
  `_start_efi` ziet x1 = 0 en leest de feiten van de koude stub uit de
  feitenpagina (`FLIP_FACTS_PA`, `board/uefi/src/flip.rs`). Die pagina
  schrijft alleen een stub van déze versie. Boot de node dus eerst koud van
  een stick met deze code; een kern van vóór de flip-ingang (tag v3.0.0-alpha.7 en ouder) heeft geen feitenpagina,
  en een flip vanaf zo'n kern parkeert de nieuwe kern zonder één regel
  (zie de faalmodi). Na de flip staat er geen `HOPOS_UEFI_STUB` op de
  console: er is geen ConOut meer, de eerste regel is de bunny. De nieuwe
  kern moet in de maat van de oude passen (`image too large` anders).
- **Pi 4, Pi 5.** De nieuwe kern komt binnen op `_pi_start` met x0 = de
  DTB-pointer die de firmware de eerste kern gaf, op de OS-core (niet per
  se core 0). Eerst bewijzen: de Pi-ingang aanvaardt een andere core dan
  core 0 en een tweede keer dezelfde DTB. De staging is de plek van het
  initramfs van Hop: na zijn plaatsing dood geheugen.
- **Radxa.** De trampoline staat in de kern-RAM (de bovenste heap-pagina):
  het enige uitvoerbare RAM buiten de pool. `cpu::el2::chain` staat dat toe
  zolang het beeld hem niet raakt. Eerst bewijzen dat de heap die pagina op
  het moment van de flip niet gebruikt (de heap is een bump-allocator; een
  node die zijn heap vol heeft, weigert de flip niet maar overschrijft dan
  zijn eigen laatste pagina vlak voor de sprong, wat niemand meer leest).

## De faalmodi

Vóór de sprong is elke fout een weigering, en draait kern A door. Wat de
kern weigert terwijl Hop nog wacht op zijn FLIP-antwoord, krijgt Hop terug
(`HOP_FLIP_FAIL`, `502`); wat daarna misgaat, staat alleen op de console.

| Marker | Waar | Wat er gebeurt |
| --- | --- | --- |
| `HOPOS_FLIP_REFUSED sha256 mismatch` | haak | de bytes zijn niet wat de som zegt |
| `HOPOS_FLIP_REFUSED bundle invalid` | haak | geen HOPRELO1-staart, een afgekapte stroom, een beeld buiten de grenzen, een relocatie die niet klopt |
| `HOPOS_FLIP_REFUSED bundle carries no switch code sum` | haak | een bundel van versie 1 (alpha.7 en ouder): die kan de switch-code niet laten toetsen |
| `HOPOS_FLIP_REFUSED flip ABI mismatch` | haak | een bundel van een andere generatie (Go was ABI 2) |
| `HOPOS_FLIP_REFUSED switch code mismatch` | haak | de EL2-switch-code van de nieuwe kern is een andere dan die waarin de bewoners draaien; zie "De koude weg" |
| `HOPOS_FLIP_REFUSED image too large` | haak | het beeld past niet in de staging of niet op het koude adres (UEFI: niet in het oude image) |
| `HOPOS_FLIP_REFUSED same bundle` | haak | deze kern kwam al uit die bundel; een flip naar zichzelf wordt geen lus |
| `HOPOS_FS_FREEZE_FAIL` en `HOPOS_FLIP_FAIL` | flip-taak | de commit faalde of de actor antwoordde niet binnen 2 s; er is niets bevroren |
| `HOPOS_FLIP_FAIL` na `HOPOS_FS_FROZEN` | flip-taak | de conntrack, de bewoners, het blob of de indeling van de sprong faalde; hopfs ontdooit (`HOPOS_FS_THAWED`) en de masquerade gaat weer open |

Na de sprong bestaat kern A niet meer, en is een koude herstart de enige
weg terug. Dat is geen geslaagde flip: de apps beginnen dan opnieuw (hopfs
houdt hun volumes).

| Marker | Wat er gebeurt |
| --- | --- |
| `HOPOS_FLIP_BLOB_BAD` | het paar was geldig maar het blob niet: er leven misschien bewoners, dus geen koude boot eroverheen maar een PSCI-reset (op ijzer: de watchdog) |
| `HOPOS_FLIP_ADOPT_FAIL` | de nieuwe kern weigert de adoptie (switch-code, partities, cores); de guard reset na de gratie |
| `HOPOS_FLIP_NAT_FAIL` | de conntrack kwam niet terug; de bewoners draaien, verbindingen door de masquerade breken |
| `HOPOS_FLIP_GUARD` | geen adoptie of geen net binnen 120 s: PSCI-reset |
| niets na `HOPOS_FLIP_JUMP` | de nieuwe kern hangt vóór zijn console (op UEFI ook: geen feitenpagina, de core parkeert). Alleen een hardware-watchdog helpt; QEMU heeft er geen. De volgende koude boot zegt waar het stopte |
| `HOPOS_FLIP_LAST`, `HOPOS_FLIP_ARCHIVED` | op de volgende koude boot: de stap waar de laatste flip stopte, uit de vluchtrecorder (`FLIP_RECORDER_PA`), en de poging daarvoor (06-09: zonder archief schreef de flip die een gevallen node optilde zijn eigen stappen over het spoor) |

## De boot-guard

Een geflipte kern krijgt twee minuten gratie (`GRACE`, de les van de
Go-kern: langer blind petten verbergt een kern die hangt, korter verliest
een trage DHCP). Binnen die tijd moeten de bewoners geadopteerd zijn én
moet de uplink een adres hebben. Dan `HOPOS_FLIP_SETTLED`, en gaat de
vluchtrecorder leeg: een latere koude boot is geen mislukte flip. Zo niet,
dan `HOPOS_FLIP_GUARD` en een PSCI SYSTEM_RESET: een halve kern met
levende bewoners is erger dan een koude boot. Alleen "de executor draait"
is geen voorwaarde (06-09 op de M4: een guard die dat toetste, liet een
kern zonder net twee minuten petten en dan toch vallen).

Wat de guard niet dekt: een kern die hangt vóór zijn executor draait. Dat
is de hardware-watchdog van het board.

## De koude weg

Past de switch-code niet (een wijziging in `cpu/src/el2/switch.rs` of de
blobs), dan weigert de flip vóór de sprong. De update gaat dan koud: het
nieuwe image op het bootmedium (`image/*.sh`, `image/uefi-run.sh`) en een
herstart. Hop leest zijn staat terug van hopfs (stateful), de apps worden
opnieuw geplaatst.

Een "koude flip" (`cold` als vlag op dezelfde ene trigger, `POST /flip`:
de bewoners netjes stoppen, hopfs vastleggen, springen met een leeg blob,
en de nieuwe kern plaatst Hop koud) is NIET gebouwd. Wat ervoor nodig is:
een image van Hop dat de sprong overleeft. De staging waar de kern Hop koud
uit plaatst, is precies waar de bundel ligt; Hop moet dus eerst naar een
eigen plek in het kernvenster (of Hop staat op hopfs en de nieuwe kern
laadt hem daarvandaan). Tot dan is koud installeren de weg.

## Toetsen

- `sh tools/qemu-test-flip.sh`: virt, alles hierboven, plus een
  TCP-verbinding van de host naar Hop die op kern A opengaat en door kern B
  beantwoord wordt, en hopfs- en conntrack-getallen die aan beide kanten
  gelijk zijn.
- `MISMATCH=1 sh tools/qemu-test-flip.sh`: dezelfde bundel met een andere
  switch-code-som; groen alleen bij een weigering vóór de sprong.
- `sh tools/qemu-uefi-flip-test.sh`: hetzelfde onder EDK2, met de PIE-basis
  en de feitenpagina.
- Host: `net` (`de_conntrack_overleeft_de_flip_via_de_actor`), `kern`
  (`a_version_2_bundle_carries_its_switch_code_sum`,
  `freeze_commits_first_and_names_the_generation`), `cpu` (`chain`).
