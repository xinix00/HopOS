# leannet — eigen netstack in `xinix00/lean`

> **12-08: de stack is gebouwd en lneto is uit hop-os verdwenen.** Het
> ontwerpdocument leeft nu bij de code: `~/Git/lean/leannet/DESIGN.md` (waarom
> zelf bouwen, wat er bewust NIET in zit, en de regel voor toevoegingen), met
> de openstaande punten in `~/Git/lean/TODO.md`. Dít dossier blijft staan als
> het hop-os-verhaal: de naad-inventaris, het budget-vertaaltabel per wereld en
> de flip-volgorde. Bij tegenspraak wint DESIGN.md.

**Besluit 11-08 (Derek):** we stoppen met het repareren van lneto en bouwen de
stack zelf, in lean, met gVisor én lneto als playbook. Dit dossier legt de
ontwerpkeuzes vast; het testplan is de 29-punten-review
(`~/Git/lneto/BEVINDINGEN.md`) — élk punt wordt hier een regressietest vanaf
dag één.

**Waarom (de meting, 11-08):** wij onderhouden ~13k regels lneto+go-net via
twee forks voor een slice waarvan ethernet/arp/ipv4/udp klein zijn en
TCP + x/xnet de bugs dragen (8 van de 14 echte bevindingen in tcp/, de rest in
x/xnet). De FIN-familie (verloren kale FIN wordt nooit hergezonden) is precies
de klasse die ons op ijzer raakte: hangende pool-slots, de 5555-console-dood.
Het geheugenmodel (2MB per listener vooraf + 256KB per dial uit één
`TCPBufferSize`) veroorzaakte de OOM van 11-08 op het 64MB-HOP-raam. Eigen
KISS-stack ≈ 2,5-4k regels; dns/ntp/dhcp/http hebben we al zelf.

## Scope v1

Ethernet-framing, ARP, IPv4 (geen opties, geen fragmentatie — drop + teller),
ICMPv4 echo (in én uit, voor diagnose), UDP, TCP, en een socket-laag die
`net.Conn`/`net.Listener`/`net.PacketConn` levert voor tamago's
`net.SocketFunc`. Stdlib-only (lean-regel 3), nul imports — ook geen
go-net-interface: leannet definieert zijn eigen device-naad, zodat béíde forks
kunnen sterven.

**Bewust niet in v1:** IPv6, SACK, timestamps/PAWS, out-of-order-reassembly
(zie TCP), Nagle (staat uit; embedded wil push), urgent pointer, syn-cookies.

## Het geheugenmodel: één knop

Dereks eis letterlijk: *"hier heb je voor kleine boards 2MB en deel zelf maar
in, en voor een grote server 40MB — genoeg om een shit load te bufferen."*

- `Config.Budget` (bytes) is de enige knop. hopnet leidt hem af uit het
  RAM-raam (memlimit-stijl), niemand denkt er per board over na.
- Alles wat per verbinding schaalt komt uit die ene pot. **Niets wordt vooraf
  geclaimd**: een listener kost ~niets tot er een verbinding is.
- Elke verbinding start op een floor (~4KiB: 2 rx + 2 tx) en groeit op
  gemeten druk — het lean-mechanisme (besluit Derek 11-08: als simpel "gewoon
  goed" is, is dat gewoon perfect; gVisor is 300× de omvang):
  - rx verdubbelt wanneer de peer het venster tot de rand vult,
  - tx verdubbelt wanneer de write de ring vult én de peer méér venster biedt
    dan de ring groot is — vraag bewijst, configuratie niet,
  - grow-only, geklemd op per-conn-cap = `Budget/4` én op wat de pot vrij
    heeft.
  Zo blijft een health-check op zijn floor en klimt een download naar de cap:
  2MB-board → 512KiB-venster, 40MB-server → 10MB-venster. Opwarmkosten:
  verdubbelen-per-vol ≈ log2(cap/floor) RTT's (~300ms op een 25ms-pad) — op
  LAN onmeetbaar. gVisors RTT-schatter (rx-doel = wat de app per RTT uitleest)
  blijft de gedocumenteerde verfijning, maar komt er alléén als de meetbank
  een echt gat laat zien; de naad is er (beide groeibeslissingen lopen door
  één growRing).
- Geadverteerd receive-venster = alleen wat al gealloceerd is (nooit
  reneg-en). Groei = nieuwe ring + kopie (op het groeimoment is er weinig
  gebufferd). Close geeft alles terug aan de pot.
- Pot leeg: nieuwe verbindingen krijgen de floor; past zelfs de floor niet,
  dan luid weigeren (RST + teller + logregel). Lean-regel 2: falen doet het
  hardop.

## TCP — de lessen ingebakken

De nummers verwijzen naar BEVINDINGEN.md; elk is daar een bewezen faalscenario
en wordt hier een test vóór de feature af is.

- **Retransmissie werkt op sequence-ruimte, niet op "data"**: SYN en FIN
  tellen mee in de boekhouding en worden dus hergezonden (#1, #2). Geen apart
  requeue-mechanisme per vlag.
- RTO per RFC 6298, go-back-N, exponentiële backoff; **klok is monotoon in
  het contract** (default `time.Since(start)`, nooit `UnixNano`) (#9, #10).
- Fast retransmit op 3 dup-ACKs in élke staat met onbevestigde data, ook de
  sluitstaten (#15).
- LAST-ACK sluit alleen op `ACK == snd.NXT` (#3); FIN-WAIT-1→2 idem (#2).
- Data boven de eigen FIN kan niet bestaan: `Close()` klinkt de
  sequence-grens vast, `Write` erna is `ErrClosed` (#13).
- **In-order only receive (v1)**: out-of-order segment → drop + teller + wél
  meteen een dup-ACK terug, zodat de peer fast-retransmit kan doen. Reassembly
  is een latere laag achter dezelfde ring-API. RFC-legaal, scheelt de helft
  van de complexiteit waar lneto's bugs zaten.
- **Window scaling vanaf dag één** — nodig voor vensters > 64KiB (de
  40MB-server); wij schreven lneto's WS-PR zelf. WS-optie en schaalstaat leven
  en sterven samen met de verbinding (#18, #19, #28).
- Ephemerale poorten: sequentieel, levende poorten overslaan, re-pick bij
  botsing (#14). Half-open verbindingen: alleen floor-geheugen + korte
  timeout, opruiming stack-gedreven, niet accept-gedreven (#6, #17).
- TIME-WAIT kort en configureerbaar (default in de orde van seconden, geen
  2MSL van 4 minuten op een embedded node).
- Challenge-ACK rate-limited; RST valideert exact SEQ.

## ARP

Dedup bij insert (#12), replies alleen verwerken als ze aan ons gericht zijn;
gratuitous alleen als refresh van een bestáánde entry, met logregel bij
MAC-wissel (#8). Callback-vlaggen hebben een levenscyclus (#20). Statische
seeds respecteren de subnet-regel expliciet of weigeren luid (#21). Eén
mutex-regime: cache-mutaties altijd onder hetzelfde slot als ingress (#11).

## Concurrency

Per verbinding een eigen lock; demux-tabellen read-mostly (RWMutex). Ingress
is één goroutine per NIC (zoals nu), het zware werk (copy, checksum) gebeurt
onder de conn-lock en schaalt dus met cores. Multi-queue RX voor de Altra is
een latere stap; de demux is er niet op voor-ontworpen maar staat het niet in
de weg.

## Naden

- **Device-naad**: leannet definieert een minimaal interface (Transmit/
  Receive/MTU/MAC — zelfde vorm als go-net's `NetworkDevice`), zodat dwmac,
  locnet en hopswitch ongewijzigd aanhaken; hopnet adapteert.
- **Socket-naad**: het `SocketFunc`-contract. Fouten zijn fouten — nooit een
  foutobject als verbinding retourneren (#4); intern getypeerde returns, `any`
  pas op de rand. Echte deadlines op álle blokkerende paden (deadline-gedreven,
  nooit iteration-capped — onze eigen lneto-PR #178).
- **Flip-naad**: hopnet krijgt een A/B-keuze leannet ↔ go-net/lneto, zoals bij
  gVisor→lneto. Pas flippen als QEMU-gate + meetbank + OOM-reproductie
  (ramSize=64MB + SSE-last) het bewijzen; daarna op ijzer (LicheeRV, Radxa);
  dán pas de forks eruit.

## Testplan

- Alle 29 bevindingen uit BEVINDINGEN.md als regressietest, geschreven vóór
  of tegelijk met de feature die ze raken.
- Eén loss-injectie-steiger (twee stacks + pomp met `mangle func([]byte) bool`)
  in plaats van vier kopieën (#24); stream-integriteit onder verlies met vaste
  seed; dial-churn; half-open-flood.
- Budget-tests: idle listener ≤ floor; N dials kosten N×gebruik, niet
  N×config; pot-leeg weigert luid.
- Geen wandklok en geen sleep in tests: geïnjecteerde monotone klok overal
  (#5, #22 — en soypat's eigen testregel).
- Doorvoer-referenties: LicheeRV RX ≥ 8,84 MB/s zonder drops (huidige
  lneto-stand); Altra-doel: per-conn-venster tot de cap, meetbank beslist.

## Eerlijke beperking v1: geen congestion control

v1 heeft flow control (venster van de peer) maar geen cwnd/slow-start. Voor de
praktijk: downloads (de bulk) worden door de cc van de zender gestuurd, dus
GitHub/Bunny remmen zichzelf; onze eigen uploads zijn klein (heartbeats, API).
Het risico zit bij grote node→internet-uploads over een congested pad — die
bestaan vandaag niet. Zodra ze bestaan: slow-start + cwnd erin (de plek is er:
de zendlus klemt nu alleen op sndWnd; cwnd wordt een tweede klem). Tot die
tijd is dit een gedocumenteerde keuze, geen vergeten hoekje.

## Naad-inventaris (11-08, uit de hop-os-boom gemeten)

Wat leannet exact moet aanbieden om drop-in te zijn:

- **Device-contract**: precies twee methodes, `Receive(buf []byte) (int,
  error)` (0,nil = niets, poll-model) en `Transmit(buf []byte) error`
  (thread-safe aan onze kant vereist, buffer herbruikbaar na terugkeer).
  Zelfde vorm als leandhcp.NIC. Frames < 14 bytes zelf droppen. Constanten
  MTU=1500 / EthernetMaximumSize=18 horen erbij (3 bufferallocaties in
  hop-os). Een alias-pakketje `metal/net/netdev` maakt board/hopswitch/
  drivers voorgoed stack-onafhankelijk.
- **TX-pomp is stack-plicht**: hop-os heeft geen eigen TX-lus; go-net's
  lifetimeGoroutine duwde egress uit tijdens blokkerende calls. leannet moet
  dat zelf regelen (notify of eigen pomp).
- **Socket**: toewijsbaar aan tamago's `net.SocketFunc`; Conn/Listener/
  PacketConn met échte deadlines, ReadFrom levert `*net.UDPAddr`. Efemere
  poorten **≥ 49152** — hopswitch.MasqEnd = 49152 is daar per constructie
  disjunct van.
- **PassivePeers-equivalent**: zonder passief buur-leren komen same-subnet
  listener-antwoorden alleen via een hairpinnende router aan. Regressietest.
- **SeedNeighbor + statische gateway-MAC**: appnet plant de gateway
  deterministisch (`layout.SlotMAC(0)`, nul ARP) — die twee ingangen moeten
  bestaan.
- **DHCP blijft buiten de stack** (leandhcp op het rauwe device); leannet mag
  het device niet exclusief claimen.
- **A/B-schakelaar**: het bewezen recept van de gVisor→lneto-flip — build-tag
  in appnet (twee up_*.go, één Up()), in hopnet alleen het stack-stuk van
  Up() splitsen (de ordening ProbeNIC→Net en SocketFunc→KeepAlive is duur
  bevochten en blijft gedeeld), en netmeter's newStack achter dezelfde tag:
  dáár wordt de flip verdiend.
- **Module-mechaniek**: lean is al een directe dep van metal — leannet reist
  mee zonder refork-dans; wel een lean-tag-bump in metal/go.mod vóór
  apps-release (GOWORK=off).

## Volgorde

1. Frames (ethernet/arp/ipv4/udp/tcp-header, in-place, nul allocaties)
2. Budget-pot + groeiende rings
3. TCP-kern (state machine + RTO + close-familie mét de tests)
4. UDP, ICMP, ARP
5. Socket-laag + deadlines
6. hopnet-naad + A/B op de bank
