# TODO

## Upstream-PR's netstack: in de gaten houden tot merge

Ingediend 09-08 (details en exit-checklist: `docs/netstack-upstream.md`):

- [ ] [soypat/lneto#178](https://github.com/soypat/lneto/pull/178) — deadline-gedreven waits
- [ ] [soypat/lneto#179](https://github.com/soypat/lneto/pull/179) — window scaling (RFC 7323)
- [ ] [soypat/lneto#180](https://github.com/soypat/lneto/pull/180) — sequentiële efemere poorten
- [ ] [usbarmory/go-net#5](https://github.com/usbarmory/go-net/pull/5) — `nodefaultstack`-tag

Snel checken: `gh pr status --repo soypat/lneto` (en idem voor go-net).
Reviewvragen kunnen komen; de onderbouwing per PR staat in
`~/Git/netstack-prs.md`.

**Nog te PR'en — ronde 2** (klaar om te vuren, afgemaakt 09-08 avond; wacht
op de reacties op ronde 1; concept-teksten in `~/Git/netstack-prs.md`, elk
standalone groen vanaf upstream-main met een rood-bewezen test):

- [ ] lneto `arp-resolve-all-pending` — ARP-reply lost álle wachtende
      cache-entries op
- [ ] lneto `listener-pool-maintenance` — CheckTimeouts aangedreven uit de
      accept-lus (half-open-flood maakte een listener voorgoed doof)
- [ ] lneto `seed-neighbor` — SeedNeighbor: statische buren, nul ARP
- [ ] go-net `listener-pool-size` — pools op MaxListenerConns (16MB/listener-bug)
- [ ] go-net `passive-peers` — PassivePeers aan (listener-replies naar peer-MAC)
- [ ] go-net statische gw-MAC + SeedNeighbor-passthrough — **GEBLOKKEERD**
      tot lneto SeedNeighbor gemerged én getagd heeft (compileert eerder niet;
      code staat klaar als hopos-commit f191782)

**Als alles upstream geland is — de replaces ERUIT, overal:**

- [ ] `metal/go.mod`: de twee `replace`-regels (lneto, go-net) verwijderen en
      de requires op echte upstream-versies pinnen
- [ ] nálopen dat er nergens anders een pad-replace achterblijft (surf en hop
      horen er geen te hebben; chain-beta zet ze alleen tijdelijk en draait ze
      zelf terug — controleer met `grep -rn "=> /Users" */go.mod`)
- [ ] daarna: gate + QEMU-smoke, en de eerstvolgende release is weer een
      STABIELE (reproduceerbaar uit module-versies, geen chain-beta)

Volledige exit-checklist: `docs/netstack-upstream.md`. Tot die tijd: clones op
branch `hopos` laten staan — de replace volgt de werkboom.

## Display-app: read-deadline bijstellen

De switch stuurt bij slot-dood geen TCP-RST meer namens de dode app
(`hopswitch/rst.go` is 26-07 gesloopt — een dode peer merken is app-werk, want
stilte kan altijd van het netwerk komen en HOP heeft al twee lagen die dit
dekken: de health-check op de task en de app-eigen ping).

Gevolg: een verdwenen venster wacht weer op de eigen read-deadline van de
display-app in plaats van meteen te sluiten. Die stond op 30s.

**Te doen** (de display-app zit niet in deze repo, maar op de artifact-server):
zet de read-deadline op een paar keer het ping-interval. Met het huidige tempo
van ~10s pingen dus richting 12-15s; wil je het sneller, dan moet het
ping-interval mee omlaag (bijv. 3s pingen, ~8s deadline). Korter dan het
ping-interval sloopt gezonde verbindingen.

Zie `docs/technical/networking.md` → "Failure handling: noticing a dead peer is
the app's job".

## SURF-apps: `net/http` eruit, `apphttp` erin (~2,9MB per app)

`net/http` linkt onvoorwaardelijk `crypto/tls` mee. Gemeten 26-07 op `app/hello`
(tamago-build, `-w -T 0x50010000`):

| | image |
|---|---|
| `applib` alleen | 1,71 MB |
| + `appnet` (gVisor-netstack) | 4,70 MB |
| + `net/http` | 7,99 MB |
| + `apphttp` i.p.v. `net/http` | **5,06 MB** |

Die laatste sprong van 3,29MB is dus voor ~54% TLS/PKI (`crypto/tls`,
`x/crypto`, `x509`, `math/big`, `asn1`) — méér dan de hele netstack kost. Een app
die geen TLS nodig heeft, betaalt hem nu wel. `metal/app/applib/apphttp` is er
daarom: minimale HTTP/1.1-GET-client, geen TLS, 0 bytes `crypto/tls` in de
binary, en maar 0,36MB boven de netstack-vloer.

**Het repo-deel is KLAAR** — `apphttp` dekt inmiddels beide kanten:
`Serve`/`ListenAndServe` (één handler, Content-Length-antwoorden, keep-alive,
Flush→chunked, Hijack voor de KVM-WebSocket — zie de package-doc van
`serve.go`) en `Do` met methode + body, dus POST (`apphttp.go`, "de tweede
helft").

**Wat rest is per SURF-app** (eigen repo). Hieronder wat de env/poorten in
`docs/config.md` verraden — de apps zelf zitten hier niet, dus per app nog
nalopen wát er precies over HTTP gaat:

| App | Uit de config | Vermoedelijke actie |
|---|---|---|
| display | `ports: surf:7878, http:80` | serveert → wacht op `apphttp.Serve` |
| launcher | `HOP_ADDR` + serveert | POST't naar de agent → wacht op `Serve` + POST |
| taskman | `HOP_ADDR` (agent-API op `10.100.0.1:8080`) | client op plain http → `apphttp` |
| clock, calc | alleen `SURF_ADDR` | eerst checken of ze überhaupt HTTP doen |
| browser | alleen `SURF_ADDR` | **checken:** doet deze https? dan `net/http` houden |

Welke apps écht https doen weet jij; alleen díe houden `net/http` (de winst
vervalt daar, want TLS is dan geen dood gewicht maar functionaliteit).

**Niet migreren:** de `apploader` in deze repo. Die haalt artifacts óók van
https-URL's (GitHub-release-assets — x509-fout gemeten 20-07, `hop apply
https://…/app.elf`) en houdt dus `net/http` plus de x509-rootbundel. `app/hello`
blijft er ook op staan: dat is de documentatie van "gewoon Go, ongewijzigd"
(`docs/app.md`) en het is bovendien een server.

Dit is puur image-omvang, geen functionaliteit — niets hieraan is dringend.
Wat je inlevert t.o.v. `net/http`: geen https (faalt luid met een duidelijke
melding) en geen chunked transfer (`Content-Length` verplicht). Redirects worden
wél gevolgd.

Zie de package-doc van `metal/app/applib/apphttp` voor de volledige afweging.

## USB-HID op ijzer: drie boards wachten op één toetsaanslag

Gebouwd 06-08, gate groen op alle drie de smaken, **nul boards geverifieerd**.
De stack (`metal/gui/driver/usb/{xhci,dwc3,hid}` + `metal/gui/usbin`, alles
achter `-tags gui`) is compleet tot en met de interrupt-endpoint. HOP serveert
de events op `10.100.0.1:7879` en het adres reist mee in de fb-grant
(`INPUT_ADDR`); de display belt en leest — zie `stack/surfserve/hopinput.go` in
hop-os-surf.

Wat de eerste boot per board moet uitwijzen:

**Radxa (rk3566)** — de agent logt per DWC3-core één regel met GSNPSID/GCTL/
GUSB2PHYCFG/GUSB3PIPECTL, en daarna per controller de rauwe PORTSC's. Drie
uitkomsten, drie vervolgen:
- `id=00000000` of `ffffffff` → de core is niet geklokt. Dan is de CRU-gate aan
  de beurt, en die is **niet geverifieerd** — we hebben hem bewust niet gegokt
  (de TSADC van deze week is precies wat dat kost). Referentie eerst:
  `clk-rk3568.c` + `rk3568-cru.h`.
- Cores komen op, maar op geen enkele poort staat CCS → de fysieke connector
  van de Zero 3E hangt niet aan een DWC3 maar aan de losse EHCI/OHCI-companions
  (0xFD800000/0xFD840000, 0xFD880000/0xFD8C0000). Dán pas is een tweede driver
  te verantwoorden.
- CCS staat aan → doorlopen tot de attach-regel.

**Pi 5** — het goedkoopste pad: de PCIe-link naar de RP1 staat al voor de GEM.
Openstaand: of de RP1 zijn USB-blokken zelf uit reset en op klok zet. Zo niet,
dan is dat het RP1-CLOCKS/RESETS-blok en dus echt werk.

**Pi 4** — `driver/brcmpcie` kreeg een BCM2711-variant (RGR1_SW_INIT_1 voor
bridge+PERST, HARD_DEBUG op 0x4204, burst 128B, geen RESCAL/MDIO-PLL/UBUS-remap).
Die is **geschreven tegen de Linux-referentie en nooit uitgevoerd**. Eerste
meting: traint de link en meldt de VL805 zich als `1106:3483`?

Eén ding is bewust niet vooruitgebouwd: het inbound-window van de BCM2711 is
identiek gemapt (PCIe 0 → DRAM 0), dus `BusOff` is nul en de vraag "wat als een
window verschuift" doet zich hier niet voor. Zodra hij dat wél doet is dat een
meting, geen gok.

## De staging-botsing: twee allocators in één raam

**Symptoom** (Pi 5, 06-08, en al eerder op de Radxa): een app van ~5,8 MB in een
partitie van 32 MB weigert te plaatsen met `elf parse: bad magic number
'[0 0 0 0]'`, terwijl dezelfde app in 128 MB probleemloos start. Gemeten
drempel: 5,38 MB ✓, 5,77 MB ✗.

**Wat vaststaat.** `layout.StageAddr` legt de image tegen de BOVENKANT van het
app-RAM, en niemand vertelt de runtime dat die bovenkant bezet is:
`cpu/memlimit` rekent zijn muur op `RamStart + RamSize − RamStackOffset`, en
`ramStackOffset` is `0x100` — letterlijk de bovenkant. Daar bovenop stonden in
de apploader twee rekenfouten die precies het interessante geval oversloegen:

1. de aanscherping gold alleen `if lim < RAMSize/2`. Bij 30 MB app-RAM en een
   image van 5,5 MB was `lim` 22,5 MB en de drempel 15 MB — dus hij werd
   **niet** gezet, in exact het geval waarvoor hij bedoeld was;
2. `lim` rekende vanaf adres 0, terwijl `SetMemoryLimit` runtime-geheugen telt
   vanaf het heapfundament. Dat is de ~9 MB image+bss van de loader te veel.

Wat er dus overbleef was het voorlopige plafond `RAMSize/2` = 15 MB. Met een
heapfundament op ~9,4 MB mag `bloc` daarmee tot ~24,4 MB komen — en de
stagingbodem ligt op `30 − imgSize`. Botsing zodra `imgSize > 5,6 MB`, en de
gemeten drempel ligt tussen 5,38 en 5,77. Dat klopt tot op ~200 KB.

**Gerepareerd 06-08.** `memlimit.ArmBelow(top)` zet hetzelfde plafond met een
EXPLICIETE muur, en `applib.StageImage` roept hem aan met de stagingbodem —
de enige plek die dat adres kent. Te weinig ruimte over faalt daar nu luid
("image van N MB laat de runtime M MB over") in plaats van stil te corrumperen.
De twee foute regels in de apploader zijn weg.

**Wat NIET vaststaat, en nu gemeten wordt.** Dát het de heap is die eroverheen
loopt, is afgeleid en niet waargenomen. `StageImage` kijkt daarom na het
downloaden de ELF-magic terug: staat hij er niet meer, dan is de download níet
de dader (wij schreven hem net) en meldt de loader het mét zijn `MemStats`. Eén
boot zegt dan of `Sys` inderdaad tegen de stagingbodem aan staat.

**De knop voor grotere images in kleine partities** is het voorlopige plafond
`debug.SetMemoryLimit(RAMSize/2)` in `app/apploader/main.go`: wat de
TLS-handshake daar aan arena mag pakken, kan de image niet meer gebruiken.
Verlagen kan, maar niet blind — 8 MB is de ondergrens waar de keten het nog
doet (`minStageHeap`), en dit pad is de enige startroute van élke job.

**Wat twee stages NIET oplost** (Dereks vraag, 06-08): de loader en de image
moeten hoe dan ook tegelijk in de partitie staan, dus het plafond blijft
`partitie ≥ image + loader + werkruimte`. "De app over de loader heen gooien"
gebeurt trouwens al — `placeFromStaging` schrijft de segmenten op hun
linkadressen, dwars over waar de loader stond. Wil je een image van 20 MB in
32 MB, dan moet de loader de partitie uít (bijvoorbeeld een gedeelde
scratch-partitie voor fase 1, waarna HOP de segmenten naar de echte partitie
schrijft) — dat is een andere, grotere verbouwing.
