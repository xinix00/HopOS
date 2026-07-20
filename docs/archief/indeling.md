# metal/ — wat waar hoort

Stand 2026-07-14, bij de hergroepering van 34 platte packages in lagen. Dit
document is de plaatsingsregel: nieuw pakket? Loop de beslislijst onderaan af.
Wijkt de werkelijkheid af van dit document, dan is één van de twee stuk — fix
die dan ook.

```
metal/
├── go.mod, go.sum      # verder staat er NIETS in de module-root
├── dev/       het MMIO-primitief (Read/Write + barrières, host-stubs)
├── abi/       het HOP↔app-contract
├── cpu/       de ARM64/CPU-laag
├── fw/        hardware-discovery (firmware-tabellen)
├── driver/    devices, één package per device (nic/ voor de NICs)
├── net/       het netwerkvlak van HOP
├── kern/      de orchestrator zelf
├── gui/       het display-vlak, opt-in via `-tags gui` (20-07)
├── board/     per-board bedrading
├── app/       de app-kant
├── cmd/       de HOP-kant binaries
└── out/       buildoutput (gitignored)
```

## De categorieën

**`dev/`** — het laagste primitief: MMIO-reads/writes en geheugenbarrières,
met host-stubs zodat logica-packages op de ontwikkelmachine compileren en
testen. Importeert niets. Hoort hier: alleen wat op dit taalniveau zit —
twijfel je, dan hoort het hier niet.

**`abi/`** — alles wat HOP én de apps allebei importeren: `hopabi` (de
control-page/ring-ABI), `layout` (het IPA-contract + per-board PA-plan),
`ring` (de SPSC-ringen), `checksum` (de content-som die beide kanten over
hetzelfde bestand rekenen). Importeert alleen `dev`. **Een wijziging hier is
een ABI-breuk met elk bestaand app-image** — additief uitbreiden of bewust
breken, nooit stilletjes.

**`cpu/`** — de ARM64-laag, geen devices: `el2` (EL2-entry + stage-2-switch),
`psci` (SMCCC-calls + de dunne wrappers Version/On/Off/AffinityInfo), `smp`
(core-bring-up), `idle` (WFE/event-stream), `trng` (RNDR/SMCCC-entropie),
`drbg` (de gedeelde SHA-256-DRBG achter GetRandomData; het board kiest de
bron). Hoort hier: CPU-instructies, exception levels, firmware-interfaces
van de architectuur.

**`fw/`** — parsers van wat de firmware ons vertelt: `fdt` (device tree),
`acpi` (MADT/MCFG/SPCR…). Géén drivers: fw/ leest tabellen en levert
beschrijvingen, het raakt geen device-registers.

**`driver/`** — één package per device, zo dun mogelijk (Linux is de
referentie, niet de maatstaf). Subcategorie pas bij een écht cluster van ≥3
verwante drivers — `nic/` (gem, genet, igb, virtionet + de gedeelde
PHY-laag mdio) is het precedent; `nvme`, `pcie`, `brcmpcie`, `pl011`, `fb`,
`vcmail`, `dvfs` staan plat. Eén nieuwe storage-driver rechtvaardigt dus nog
geen `blk/`; de derde wel.

**`net/`** — het netwerkvlak van HOP zelf: `hopswitch` (L2-switch + NAT),
`dhcp`, `hopnet` (de node-netwerkopstart). De per-app netstack zit hier
níét — die leeft in `app/applib` (gVisor over de abi-ringen).

**`kern/`** — de orchestrator: `slots` (laden/plaatsen/killen), `slotmgr`,
`stage2` (de kooi), `hopfs` (de schijflaag), `apploaderblob` (de universele
loader als go:embed-bytes in de node-binary). Hoort hier: alles wat alleen
core 0 als vertrouwde kern doet.

**`gui/`** — het display-vlak (docs/archief/gui-ontwerp.md), sinds 20-07 een eigen
categorie omdat GUI los van de rest moet kunnen: `hvs` (de read-only
BCM2712-HVS-dumptool), `debug` (het :9091-endpoint erover), `fbgrant` (het
fb-weggeef-beleid: claim, FB_*-env, console van/terug op het glas). Alleen
cmd/-binaries linken dit, en uitsluitend achter `-tags gui` — elke board-tag
heeft zo twee smaken: kaal (headless) en gui — elk imagescript heeft de
knop (default gui, `GUI=0` = kaal); beide smaken blijven groen via de
tamago-gate. De grens: de firmware-framebuffer (`driver/fb`, de log-console)
is géén gui — dat is de universele console; alles voorbij dat ene
firmware-beeld (het glas weggeven, HVS, planes, straks de driver-driehoek)
is wél gui. De mechaniek eronder (stage2.GrantWindow, layout.FbIPA en het
registratiepunt kern/slots/grants.go) blijft basis: het generieke
DeviceGrant-primitief — gui/fbgrant is de eerste provider die zich daar
(via cmd's gui-smaak) op registreert.

**`board/`** — de hardware-integrator, per board in TWEE helften:

- **de basis** (`board/<x>`): wat élk image — ook een app — nodig heeft om op
  te draaien: runtime-hooks (hwinit/printk/rng/timers), cpuinit-asm, het
  PA-plan en de registratie van het app-contract (`board/appboard`: CoreID +
  SetTimerOffset — het enige dat een app van zijn board ziet).
- **de HOP-bedrading** (`board/<x>/hop`): de volledige `board.Board`-
  implementatie mét drivers (ProbeNIC, PCIe, framebuffer-discovery, DHCP).
  Alleen cmd/-binaries importeren deze helft.

Zo linkt een app-image nooit de driverstack van zijn board mee (gemeten: ~2,5k
regels gem/brcmpcie/dhcp/vcmail per Pi-5-app-image, door de linker niet te
elimineren omdat het interface-methods waren). `raspi` is de SoC-gedeelde laag
onder rpi4/rpi5, mét een eigen gedeelde hop-helft: `raspi/hop` draagt `Base`
(alles wat rpi4/hop en rpi5/hop identiek deden — PSCI/timers/lease/fb-
discovery — plus StartDVFS) en `raspi/vcfb` de VideoCore-framebuffer-
discovery. Dat is het precedent voor toekomstige SoC-packages (O6N/cixp1:
onder board/, naast de boards die hem gebruiken).

Sinds 15-07 linkt een app-image zelfs geen boárd meer: **`hopslot`** is het
generieke app-board — onder stage-2 raakt een app geen MMIO of firmware-tabel,
dus alles wat hij nodig heeft (arch-timer, stille printk, MMIO-vrije RNG, de
kale EL1-cpuinit, slot via de door HOP gepatchte slotHint) is
board-onafhankelijk. De kooi ís het board: één app-binary draait ongewijzigd
op QEMU, de Pi's en de Altra. `applib` importeert hopslot (applib/board.go);
board-tags doen voor app-images niets meer en bestaan alleen nog voor
HOP-binaries. De per-board basis-helften blijven voor de HOP-kern zelf (die
boot via firmware en wél UART/DTB/ACPI raakt).

**`app/`** — alles wat ín een slot draait: `applib` (runtime; kiest ook het
board — altijd `board/hopslot` — zodat een app-dir alleen main.go bevat),
`applib/appnet` (de per-slot netstack: default gVisor, `-tags lnetonet` de
lichte lneto-backend), `appspike` (de referentie-app), `apploader` (downloadt
het echte app-image in het slot). Binaries van de app-kant horen hier, niet
in `cmd/`.

**`cmd/`** — de HOP-kant binaries: `hopos` (de agent), `hopos-embed` (de
fase-P1-kern met ingebakken app-image), `probeuefi` (UEFI/ACPI-discovery —
het meetinstrument voor de O6N-bring-up; probe4/5/6 zijn gesloopt 18-07,
functie geproductiseerd, terughalen kan uit git history).

## De importrichting

Pijlen wijzen alleen omlaag; concreet, en AFGEDWONGEN door
`tools/importcheck.go` (draait in tools/test.sh, leest ook code achter
build-tags — een verkeerde import is een buildfout, geen reviewtaak; de
regel-tabel dáár en dit hoofdstuk horen samen te wijzigen):

1. `dev` importeert niets; `abi` alleen `dev`; `fw` alleen `dev`.
2. **`app/` importeert nooit `kern/`, `net/` of `driver/`.** De app-kant
   kent HOP uitsluitend via `abi/` (+ `dev`/`cpu`/`board/appboard`/de
   board-basis om op te draaien). Dit is de isolatie op source-niveau: een
   app kán niet tegen HOP-internals linken — ook niet transitief, want de
   board-basis mag uit `driver/` uitsluitend de console-uitzondering
   (`pl011`/`fb`; printk is een runtime-hook en kan niet init-geïnjecteerd
   worden zonder vroege bootdiagnose te verliezen) en nooit `net/` of het
   board-contract.
3. **Niets importeert `app/`** (behalve app/ zelf en de app-binaries). De
   loader komt de node-binary in als bytes (`kern/apploaderblob`), niet als
   import.
4. `board/<x>/hop` integreert de hardware-kant (cpu, fw, driver, net/dhcp);
   `kern/` integreert de OS-kant (abi, net, driver/nvme via hopfs);
   `cmd/` knoopt board-hop + kern aan elkaar. Andersom nooit.
   `gui/` zit als opt-in vlak bóven kern: het mag driver, het
   board-contract én kern lezen (fbgrant: stage2.GrantWindow + de
   slots-hooktypen), en alleen `cmd/` (achter `-tags gui`) importeert het
   terug — kern/net/board raken gui nooit; kern kent gui alleen als
   niet-geregistreerde grant-haak (kern/slots/grants.go). Het
   Display-contract van gui/debug implementeert een board structureel
   (gui-getagde methoden, board/rpi5/hop/gui.go), zonder import.
5. `board/appboard` (het app-contract) importeert niets; het contract
   `board` alleen appboard + de typen die het draagt (driver/fb,
   driver/pcie, net/dhcp). `driver/` importeert board dus nóóit — types die
   drivers aannemen (pcie.Window, fb.Desc) wonen bij de driver zelf.

## Buildoutput

Alle artifacts van `image/*.sh` landen in `metal/out/` (gitignored als één
dir). De enige uitzondering is go:embed — dat kan niet buiten de eigen
package-dir reiken — dus deze twee plekken bevatten gebouwde elfs náást de
code (gitignored via `*.elf`/`*.elf.gz`):

- `cmd/hopos-embed/app*.elf` — de ingebakken app-images van de P1-variant;
- `kern/apploaderblob/apploader.elf.gz` — de universele loader in de node (gzip; lazy uitgepakt bij de eerste jobstart).

## Nieuw pakket? Beslislijst

1. Importeren app én HOP het? → `abi/` (en besef: dit ís het contract).
2. Praat het met device-registers? → `driver/` (NIC? → `driver/nic/`).
3. Parseert het firmware-tabellen zonder registers te raken? → `fw/`.
4. CPU-instructie, EL-niveau of architectuur-firmware? → `cpu/`.
5. Alleen voor core 0 als vertrouwde kern? → `kern/`.
6. HOP's netwerkvlak? → `net/`. Draait het in een slot? → `app/`.
   Display-werk voorbij de firmware-framebuffer? → `gui/` (opt-in vlak,
   alleen gelinkt met `-tags gui`).
7. Bedrading van één board of SoC? → `board/<naam>`: runtime-hooks/boot in
   de basis, alles met drivers in `board/<naam>/hop`.
8. Is het een binary? → HOP-kant `cmd/`, app-kant `app/`.
9. Past het nergens? Eerst overleggen, niet een elfde categorie beginnen.

Host-testbare logica? Voeg het pakket toe aan de `go test`-lijst in
`tools/test.sh` — packages zonder tests draaien daar mee als compile-check.
