# GUI-ontwerp — surfaces, planes, scenes en het cluster op je scherm

> Aanleiding (Derek, 2026-07-19): "ik heb het liefste wel 3D-acceleratie […]
> op een zo makkelijk mogelijke manier (het KISS-principe)", daarna: "het idee
> is om het via de network stack te doen, zo kunnen andere apps ook elders in
> je cluster draaien", "het moet wel een beetje leuke GUI worden", en tot
> slot: "moeten we niet een HTML-achtig taaltje maken, waarbij we secties
> kunnen updaten — maar dan goed? dat scheelt heel veel traffic". Dit dossier
> legt de uitonderhandelde ontwerpbeslissingen vast vóór de bouw.
> Status: **P1 gebouwd** (19-07, in
> [xinix00/hop-os-surf](https://github.com/xinix00/hop-os-surf) — zie §8);
> P2+ ontwerp akkoord, nog niet gebouwd.

## 1. De kernbeslissing: geen GPU-driver, wél de display controller

3D-acceleratie via de GPU (VideoCore VII op de Pi, Immortalis op de O6)
betekent Mesa-klasse werk: GPU-MMU, job-submission en een shadercompiler —
orde 150k regels, geen bare-metal referentie. **Dat doen we niet**, en het
botst met onze eigen principes (beeld = firmware-buffer; lichte drivers).

Wat we wél doen:

- **3D = software-rasterizer in puur Go** op de framebuffer (fauxgl-klasse:
  z-buffer, meshes, shaders als Go-functies, multi-core). Rekensom: 1080p60 ≈
  124 Mpix/s bij overdraw 1; een Go-rasterizer haalt grofweg 30–100 Mpix/s
  per core, ×4 A76-cores → 1080p30–60 voor eenvoudige scènes, 720p comfortabel.
  Meten vóór optimaliseren; pas daarna eigen rasterizer (float32) of NEON.
- **2D-compositie = de display controller**, het gedocumenteerde kleine
  broertje van de GPU: een registerdriver zonder compiler, honderden tot ~2k
  regels per bord, met de Linux-bron als referentie. Elke moderne SoC heeft
  er één (Linux' DRM/KMS-"planes" is er de gestandaardiseerde abstractie van):

  | Bord | Compositor | Referentie |
  |---|---|---|
  | Pi 4/5 | HVS | `vc4_hvs.c` / `vc4_plane.c` (gen 4/5/6C/6D) |
  | Orion O6 (CIX P1) | Linlon-D60 DPU | CIX BSP-kernel (out-of-tree; mainline 6.19 heeft nog geen display) |
  | Rockchip RK3588/356x | VOP2 | mainline |
  | Allwinner | DE2/DE3 | mainline + publieke manuals |
  | UEFI / QEMU / rest | geen | software-fallback (CPU/DMA) |

- **HVS-aanpak: firmware-pipeline niet vervangen.** PixelValve, HDMI en
  modeset blijven firmware. Wij lezen eerst read-only de display-list die de
  firmware in de HVS-SRAM zette (dumptool over UART — meetinstrument bewijzen),
  en muteren daarna incrementeel: één extra plane, dan alpha/schaling.
- **DMA-blitter als bijvangst:** de BCM-DMA (ook DMA4 op Pi 4/5) heeft een
  2D-modus (TDMODE: stride + breedte×hoogte). Rechthoeken vullen/kopiëren op
  geheugenbandbreedte (~2–4 ms per 1080p32-scherm), asynchroon, ketenbaar,
  ~300 regels. Geen alpha — blenden blijft CPU.

## 2. Window = plane = surface-stream

Eén klein compositor-contract (DRM-planes-light):

```go
type Compositor interface {
    Planes() int
    SetPlane(i int, buf PA, src, dst Rect, z int, alpha uint8)
    Commit() // atomisch, op vsync
}
```

Een plane wordt gevuld door een **surface-stream**, en het maakt de
compositor niet uit waar die vandaan komt. Eén protocol, twee transports:

- **lokaal**: shared-memory ring (slots/share-mechanisme) — zero-copy;
- **remote**: dezelfde berichten over het netwerk (apps hebben al een eigen
  netstack). Remote tiles landen direct in de plane-buffer: ook op de
  display-node geen extra kopie.

Daarmee worden GUI-apps gewone cluster-workloads: de display-node is de node
waar toevallig een scherm aan hangt, `surface:` in het app-manifest, HOP
wijst toe. Failover van een app = het window komt vanzelf terug van een
andere node. Netwerk-transparantie zoals X11 het bedoelde, maar buffers in
plaats van drawing-commands (de X11-valkuil: synchrone round-trips; de
RDP/codec-valkuil: VPU-drivers — allebei bewust vermeden, VNC bewees rauwe
pixels + damage).

Bandbreedte, eerlijk: full-screen 1080p32@60 rauw = 4 Gbps — dat niet over
1GbE. Echte GUI-damage (terminal, dashboard) = 1–10 MB/s per window; een
500×300-grafiek op 10 fps = 6 MB/s. Eén GbE draagt een desktop vol windows;
full-screen video vraagt 2.5GbE, lagere fps of latere tile-compressie (LZ4).
**Met scenes (§4) zakt een dashboard-window naar bytes per seconde** — dan
werkt het ook over federation/WAN.

## 3. SURF — Hop Surface Protocol v0 (de transport-laag)

Alle integers little-endian. V0: één TCP-stream (poort **7878**), berichten:

```
header (8 bytes): type u8 | pad u8 | surface u16 | length u32   (length = payload)
```

| Type | Richting | Payload |
|---|---|---|
| `HELLO` (1) | app→disp | version u16, tokenLen u8, appName string, token [32]byte |
| `CREATE` (2) | app→disp | w u16, h u16, format u8 (v0: alleen XRGB8888) |
| `DAMAGE` (3) | app→disp | frame u32, x u16, y u16, w u16, h u16, dan w·h·4 pixelbytes |
| `PRESENT` (4) | app→disp | frame u32 — alle damage van dit frame wordt atomisch zichtbaar |
| `CONFIGURE` (5) | disp→app | w u16, h u16 |
| `INPUT` (6) | disp→app | kind u8, code u32, value s32, x u16, y u16 |
| `CLOSE` (7) | beide | — |
| `SCENE` (8) | app→disp | volledige widget-boom (TLV, zie §4) |
| `PATCH` (9) | app→disp | 1..n × (id u16, prop u8, waarde) |
| `EVENT` (10) | disp→app | id u16, action u8 (clicked, changed, focus) |

Ontwerpregels:

- **Per-surface double buffering**: DAMAGE schrijft de back-buffer, PRESENT
  flipt — nooit tearing; een traag frame mag de compositor overslaan.
- **Framenummers overal**: DAMAGE verhuist later ongewijzigd naar UDP-tiles
  (idempotent; verlies = oude tile blijft staan; periodiek keyframe).
- **Auth vanaf byte één**: token-veld in HELLO; verificatie stub in QEMU,
  later aan de clustersleutels (zelfde vertrouwen als signed boot).
- **De WM beslist de maat** (toegevoegd 19-07, Dereks punt bij de bouw):
  CREATE draagt slechts een maat-hint, CONFIGURE is de wet — de app
  hertekent op wat de tiling-layout hem geeft (de Wayland-les). DAMAGE op
  een verouderde maat wordt stil gedropt; een presenterende app convergeert
  vanzelf, zonder ack-serials.
- **Plane-commit blijft HOP-domein.** De display-list is DMA en de HVS zit
  niet achter een SMMU: apps leveren surface-inhoud, nooit adressen.

## 4. SURF-scenes — de boom, niet de taal

De pixel-stroom van §3 is het fundament, maar voor GUI-werk is hij
verspilling: een dashboard dat één temperatuur ververst stuurt als
damage-rect ~32 KB (200×40×4), als scene-patch **~20 bytes** — factor ~1000.
Daarom de tweede laag: een **retained scene**. De app stuurt één keer een
widget-boom (`SCENE`), daarna alleen gerichte updates (`PATCH`). De
display-app rendert.

Wat er gratis meekomt (en waarom dit meer is dan een traffic-optimalisatie):

1. **Resize wordt display-side**: `CONFIGURE` re-flowt de boom op de
   display-node; de app wordt niet eens gewekt. Perfect voor tiling.
2. **Semantische input**: niet "klik op (412,88)" maar `EVENT #save clicked`.
   Hit-testing, focus en tab-volgorde zitten één keer in de display-app.
3. **Apps krimpen**: font-rasterizer en widget-tekencode alleen in de
   display-app, niet in elke app opnieuw.
4. **Failover wordt bijna gratis**: app herstart elders en herstuurt z'n
   boom van een paar honderd bytes.
5. **Eén look-and-feel**: de instrumentenpaneel-stijl wordt door de widgets
   afgedwongen — apps kúnnen geen afwijkende GUI maken.

### De twee harde regels (de anti-HTML-clausule)

Elk "klein taaltje" in de geschiedenis is via dezelfde route ontploft:
tekst-syntax → parser → expressies → scripting → HTML+JS opnieuw uitgevonden
(Derek: "nu gaan ze in HTML weer zorgen dat je kan scripten terwijl we daar
JS voor hadden"). Daarom:

1. **Het is geen taal, het is een datastructuur.** Geen tekst-markup, geen
   parser: een **binaire widget-boom** (TLV: id u16, widget u8, props) over
   de draad, app-zijde een Go-builder-API. Aan iets zonder syntax valt geen
   scripting toe te voegen. Alle logica blijft in de app; de boom is dode
   data. Of zoals Derek het samenvatte: **scripting hébben we al — dat is de
   Go die erbij zit.** Het stack-plaatje is dus markup + Go, precies HTML+JS,
   maar met de rollen goed verdeeld: op het web draait de scripting ín de
   renderer (waardoor de browser een application-runtime werd en sandboxing
   een naderhand aangebouwd noodverband); bij ons reist er nooit code naar de
   display-node, alleen data — de "scripting" draait app-zijde, in een eigen
   kooi, op eigen cores. Het sandbox-probleem van de browser kan hier niet
   eens ontstaan. Round-trips per interactie zijn op LAN sub-milliseconde — het
   X11-over-WAN-probleem hebben wij niet, en over WAN zijn de berichten toch
   maar bytes.
2. **De canvas-widget is het overdrukventiel.** Elke "kan de layout ook…?"-
   vraag heeft één antwoord: *nee, pak een canvas*. Een canvas is een
   rechthoek met DAMAGE-pixels erin (het §3-pad) — klokwijzers, grafieken,
   3D, terminals. Daardoor hoeft de widget-set nooit te groeien. Een boom
   van één canvas = het pure pixel-model; beide lagen zijn hetzelfde
   protocol.

### Widget-set v1 (compleet — uitbreiden is een ontwerpbeslissing, geen PR)

```
col | row      layout: vast of gewicht, padding — meer niet
label          tekst; stijl-enum: normal/heading/mono
value          het live-cijfer — PATCH-doelwit nummer één
gauge | bar    instrumentenpaneel
button | list
canvas         pixels (DAMAGE/PRESENT), de rest van de wereld
```

Geen CSS, geen absolute positionering: col/row met gewichten ís het
layoutmodel. Tekst-wrap in drie kolommen? Canvas.

### Versioning

Version staat al in HELLO. Een display-node die een widget-type niet kent
rendert een lege rechthoek met de app-naam erin; de app blijft werken. Geen
capability-onderhandeling. KISS.

## 5. De desktop: het cluster zichtbaar in de chrome

- **Een window vertelt waar het draait**: titelbalk `clock @ node-b`; bij
  failover badget het window even en komt terug met de nieuwe node.
- **Instrumentenpaneel, geen bureau-metafoor**: vlak, 1-px randen, harde
  kleuren, geen schaduwen/gradients. Renderbaar in puur Go zonder gêne.
- **Tiling-first, keyboard-first** (grid in v1); zwevende overlap kan later
  gratis — z-order en alpha doet de HVS toch al.
- **Wallpaper = onderste plane; muiscursor = bovenste plane** (zero-latency
  hardware cursor, zoals echte GPU's het doen).
- **Tekst**: `x/image/font/gofont` + de pure-Go opentype-rasterizer werkt
  bare-metal; bij GC-druk glyphs prerenderen naar een atlas. Dankzij scenes
  hoeft dit alleen in de display-app.

## 6. Input-trapje (USB blokkeert de GUI niet)

INPUT is al netwerk; de bron is inwisselbaar:

1. **Browser-KVM (dag één, ~150 regels)**: de display-app serveert naast
   `GET /screen.png` een `/kvm`-pagina — scherm kijken, muis en toetsen als
   INPUT terugposten. Geen install; elke node krijgt ingebouwde web-KVM.
2. **UART-toetsenbord**: seriële console → toetsen naar de focus-surface.
3. **USB-HID als input-app (P6)**: strak gescoped — alleen boot-protocol HID,
   xhci-init + command/event-ring + enumeratie + één interrupt-endpoint,
   polling in v1: schatting 2–4k regels (de C++-referentie Circle is er
   tienduizenden aan volledige stack kwijt; die scope nemen we niet). De
   xhci zit op de Pi 5 in de RP1 en RP1's PCIe rijden we al voor de GEM.

## 7. De driver-driehoek (drivers uit de core, langs de DMA-as)

Vastgelegd als ontwerpprincipe, 2026-07-19. Dit **amendeert** de regel
"zero MMIO, devices programmed only by the node" (technical/isolation.md):
MMIO wordt een expliciete, door HOP verleende grant; DMA-adressen blijven
onverkort node-domein tenzij een SMMU ze omheint.

1. **Kooi-driver** — apparaat zónder DMA (GPIO, I2C, SPI, PWM, UART):
   MMIO-page in de stage-2 van de kooi, driver volledig in de app. Crash =
   app-herstart (liveness-watchdog wordt driver-failover).
2. **Ring-driver** — DMA zónder SMMU (de Pi): gesplitst. HOP bezit
   descriptors/adressen (generalisatie van wat netRingPA nu ad-hoc doet),
   de app bezit protocol/logica — 90% van de regels. De display-app (HVS)
   en de net-rings zijn hier bestaande voorbeelden van.
3. **SMMU-driver** — DMA mét SMMU (Orion O6, SMMUv3): device-streams aan de
   adresruimte van de kooi gebonden; de hele driver mag de app in. Kandidaat
   voor de eerste: RTL8126.

Bijbehorend contract: **DeviceGrant** in het appboard-contract = MMIO-pages +
IRQ-doorbell (HOP vangt de IRQ, doorbell via het bestaande ring-mechanisme;
orde microseconden — prima voor net/GUI/sensoren) + optioneel
DMA-ring-venster in de partitie-staart. Wat kern blijft: geheugen/stage-2,
cores, DMA-adresvalidatie, klokken, power, pinmux, firmware-mailbox
(klokken/pinmux zijn shared fate).

## 8. Fasering

| Fase | Wat | Bewijs |
|---|---|---|
| P1 | `surf`-transport + fallback-compositor + display-app met `/screen.png` en `/kvm` (pixels-only) | 2× QEMU: `clock`-app op node A, window op node B, klikken vanuit de browser; daarna node A killen → window komt terug (`count: -1`) |
| P2 | scene-laag: TLV-boom, widget-set v1, display-side renderer + col/row-layout, applib-builder | dashboard-app als scene; PATCH-verkeer meten (doel: bytes/s waar pixels KB/s waren) |
| P3 | lokale transport over de share/slots-ring (zero-copy, zelfde protocol) | zelfde demo's, 1 node, geen netwerk-hop |
| P4 | HVS: dumptool (read-only, Pi 4 + Pi 5) → plane-editing → surfaces als hardware-planes | echt scherm aan de Pi |
| | **P4 stap 1 gedaan (19-07 avond)**: read-only dumptool gebouwd (metal/gui/hvs + debug-endpoint :9091/hvs op de node (sinds 20-07 het opt-in gui-vlak: alleen gelinkt met -tags gui) — zonder UART bevraagbaar) en de éérste dump gedraaid. VONDST: HVS gen6 leeft (VERSION 0x2453, HVS_EN aan) maar álle drie de kanalen staan uit en de dlist-SRAM is onbeschreven ("HVSR"-vulpatroon) — **de firmware-scanout loopt niet via de ARM-zichtbare HVS-displaylists** (vermoedelijk het moplet-blok @0x7c501000, bcm2712.dtsi). Er is dus geen firmware-lijst om te muteren; plan-bijstelling: mop/moplet dumpen → scanout-pointer vinden (double-buffering bijna gratis) óf zelf de eerste HVS-kanaal-configuratie doen (groter werk). | |
| P5 | DeviceGrant + eerste kooi-driver (GPIO/I2C) | sensor-app tekent meetwaarden in een SURF-scene |
| P6 | USB-HID input-app | toetsenbord/muis aan de display-node zelf |

Plek van de code (besluit Derek 19-07, bij de bouw van P1): de GUI-stack is
een **eigen repo — [xinix00/hop-os-surf](https://github.com/xinix00/hop-os-surf)**.
HopOS zelf draagt géén GUI-code; alles aan de GUI is een gewone app, en in
HopOS landt straks alleen de node-kant (FB-grant, HVS/DPU-plane-commit —
klasse 2 van §7). Dat houdt de scheiding scherp én de TCB-telling eerlijk.
P1 is gebouwd (19-07, ~1,6k regels + tests; display.elf 8,5 MB, clock.elf
5,1 MB):

- `surf/` — transport-protocol, encode/decode, dependency-vrij
- `compositor/` — surfaces (dubbel gebufferd), tiling-grid, titelbalken,
  cursor, 8x8-font (kopie uit driver/fb: app-kant mag driver/ niet importeren)
- `surfserve/` — SURF-sessies + `/screen.png` (PNG-cache per compose-
  generatie) + `/kvm` (browser-KVM) + `/input`
- `window/` — app-kant: `Open`, teken in `image.RGBA`, `Present()`;
  herverbindt zelf met HELLO+CREATE+vol frame (at-most-once per present: een
  app die blijft presenteren heelt zichzelf — de failover-semantiek van §2)
- `face/` + `cmd/clock` — de klok-demo; `cmd/display` — de display-server
- `cmd/display/fbblit.go` — het FB-grant-pad: FB_BASE/WIDTH/HEIGHT/STRIDE in
  de jobspec-env → blit naar het echte scherm. **De node-kant is gebouwd
  (19-07 nacht, HopOS)**: `layout.FbIPA` (vast IPA-venster in GB0 — de
  kooi-IPA-ruimte is 32-bit, een firmware-fb mag fysiek boven de 4GB liggen),
  `stage2.GrantWindow` (eigen L2-slab, Normal-NC, host-getest), en
  `kern/slots/fbgrant.go` (job-env FB=1 → exclusieve claim + FB_*-env; HOP's
  fb-console gaat van het glas en komt terug als het slot vrijkomt). Bewezen
  op QEMU-UEFI met ramfb/GOP: de klok tikt op het glas — de Pi is hetzelfde
  codepad (raspi-Framebuffer via DTB/mailbox) en wacht op een SD-flash.
- **P2 gebouwd (19-07 avond)**: alles in `scene/` (TLV-boom + col/row-layout
  + panel-renderer + hit-test + zelfherstellende client met Go-builders —
  `ui/` was intussen vergeven aan de pixel-hulplaag) en `surfserve/scene.go`
  (display-kant: SCENE/PATCH per surface, CONFIGURE = display-side re-flow,
  input → hover/press/scroll display-side, semantische EVENT's terug).
  Host-getest incl. end-to-end (klik → EVENT → callback; PATCH ≤32 bytes op
  de draad). `cmd/dash` is de bewijs-app: toont zijn eigen draadverkeer.

De end-to-end-keten (window↔TCP↔sessie↔compositor↔PNG↔input↔reconnect) is
host-getest; het screenshot-meetinstrument werkt headless
(`SCREENSHOT_OUT=… go test ./surfserve -run Screenshot`).

## 9. Open punten

- fauxgl is float64 en alloceert per frame — meten op ijzer vóór er iets
  eigens komt.
- Hard plane-limiet per scanline (LBM) verifiëren in de vc4-bron; getal hier
  noteren.
- Display-list-formaten verschillen per generatie (Pi 4 = gen5, Pi 5 = gen6
  **C/D-stepping apart** — zelfde stepping-soap als het C1-erratum).
- BCM2712 PCIe-IOMMU bruikbaar? Zo ja: xhci wordt klasse 3 i.p.v. klasse 2.
- `image/png` + opentype alloceren stevig — bij zichtbare GC in de soak:
  glyph-atlas + PNG-cache.
- Poortkeuze 7878 en de namen SURF/scene zijn voorstellen, geen dogma's.
- **Hairpin-NAT ontbreekt** (hopswitch/nat.go: "pas bij behoefte" — de
  behoefte is er nu): een app kan een gepubliceerde poort niet via het
  node-IP bereiken. De QEMU-demo (19-07) omzeilt het met het interne
  slot-IP in SURF_ADDR (10.100.0.slot+1), maar dat lekt slotplaatsing de
  jobspec in; hairpin of service-discovery ("display" als naam) is netter.
- ~~PATCH-waarde-encoding per prop-type vastpinnen~~ — GEDAAN bij de bouw
  van P2 (19-07): één prop-TLV voor SCENE én PATCH (`key u8 | type u8 |
  data`; types str/i32/u8/strlist), volledig gedocumenteerd in
  hop-os-surf/scene/scene.go. Een waarde-patch is 11-17 draadbytes.
- Scene-canvas (het overdrukventiel) is in P2 nog een placeholder op de
  display: het DAMAGE-doorvoerpad naar het canvas-rect komt zodra de eerste
  app hem nodig heeft.

## 10. Vergezicht-roadmap: het web, maar dan native (P7+, dromen mét datum)

> Derek, 2026-07-19: "wat nou als websites straks .elf-bestanden zijn —
> gelijk interactief op je PC :P" — vastgelegd als richting, niet als
> toezegging. Driekwart van de techniek bestaat al; dit is de volgorde
> waarin de rest zou komen, ná P1–P6.

Het idee: een URL-balk op de desktop die `hop apply https://…/app.elf`
doet — een "website" is een gesigneerde Go-binary die in een kooi draait en
via SURF-scenes een window opent. Dit is poging vijf van een oud idee
(Java-applets, ActiveX, Flash, NaCl — allemaal gestorven aan hun
sóftware-sandbox; WASM is poging vier-en-een-half en bouwt een VM van
miljoenen regels om na te doen wat stage-2 in hardware geeft). Onze kooi
geeft een gast-binary: geen gedeelde kernel (nul syscalls), onbenoembare
buren, hele cores (geen SMT-side-channels), nul MMIO/DMA. Een vreemde
binary draait hier beter geïsoleerd dan vreemd JavaScript in een browser.
De maten kloppen ook: app = 6,1 MB tegen 2–3 MB voor een gemiddelde
webpagina; "page load" over GbE = seconden; *niets is persistent* = elke
reboot de schoonste incognito-modus die er bestaat.

Wat er dan nog moet komen, in volgorde:

1. **Publisher-signing** — het uitgestelde signing-punt uit het PLAN wordt
   het vertrouwensmodel: publishers signen met eigen ed25519-keys, de
   node-eigenaar kiest wiens keys hij vertrouwt (RSS-model, geen centrale
   store). Dit is het echte ontwerpwerk; de techniek is de makkelijke helft.
2. **Egress-beleid op de HOP-switch** — gast-apps mogen alleen terugpraten
   naar hun origin: één filterregel op een switch die al bestaat.
3. **Gast-slots via core-deling** — fase-6 (meerdere kooien op één core via
   HVC-yield) ís het tabblad-model: bezoekers-apps claimen geen hele core.
4. **De URL-balk** — een gewone scene-app die HELLO's doorgeeft aan HOP.

Eerlijke restpunten: gedeelde L2/SLC-caches blijven een theoretisch
side-channel (beter dan ieder alternatief, niet nul), en een kwaadwillende
gast kan z'n eigen core opstoken — per ontwerp APP-fout, begrensd, kill =
revocatie. Structureel voordeel dat blijft: naar de display-node reist
alleen data (scenes), nooit code — een "website" kán de renderer niet
exploiteren.

Werknaam voor browsen: **hoppen**. Dat kon geen toeval zijn.

## 11. Wat er expliciet níét komt

Geen V3D/Mesa-port, geen shadercompiler, geen video-codecs (VPU), geen
drawing-command-protocol, geen zwevende windows in v1, geen compositor die
firmware-modeset vervangt. En in de scene-laag: geen tekst-markup, geen
parser, geen expressies, geen scripting, geen CSS — elke druk in die
richting heeft als antwoord "canvas". Logica hoort in de app; de boom is
data. En we worden niet de nieuwe Windows — Windows verstopt waar dingen
draaien; wij laten het zien.
