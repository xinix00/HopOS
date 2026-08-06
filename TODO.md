# TODO

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
