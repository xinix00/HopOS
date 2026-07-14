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
`psci` (SMCCC-calls), `smp` (core-bring-up), `idle` (WFE/event-stream),
`trng` (RNDR/SMCCC-entropie). Hoort hier: CPU-instructies, exception levels,
firmware-interfaces van de architectuur.

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

**`board/`** — de hardware-integrator: per board de bedrading van cpu + fw +
driver + net/dhcp tot een werkend platform (`qemuvirt`, `rpi4`, `rpi5`,
`uefi`), met `raspi` als SoC-gedeelde laag onder rpi4/rpi5 — dat is het
precedent voor toekomstige SoC-packages (O6N/cixp1: onder board/, naast de
boards die hem gebruiken).

**`app/`** — alles wat ín een slot draait: `applib` (runtime + appnet),
`appspike` (de referentie-app), `apploader` (downloadt het echte app-image
in het slot). Binaries van de app-kant horen hier, niet in `cmd/`.

**`cmd/`** — de HOP-kant binaries: `hopos` (de agent), `hopos-embed` (de
fase-P1-kern met ingebakken app-image), `probe4/5/6`, `probeuefi`.

## De importrichting

Pijlen wijzen alleen omlaag; concreet, afdwingbaar bij review:

1. `dev` importeert niets; `abi` alleen `dev`; `fw` alleen `dev`.
2. **`app/` importeert nooit `kern/`, `net/` of `driver/`.** De app-kant
   kent HOP uitsluitend via `abi/` (+ `dev`/`cpu`/`board` om op te draaien).
   Dit is de isolatie op source-niveau: een app kán niet tegen HOP-internals
   linken.
3. **Niets importeert `app/`** (behalve app/ zelf en de app-binaries). De
   loader komt de node-binary in als bytes (`kern/apploaderblob`), niet als
   import.
4. `board/` integreert de hardware-kant (cpu, fw, driver, net/dhcp);
   `kern/` integreert de OS-kant (abi, net, driver/nvme via hopfs);
   `cmd/` knoopt board + kern aan elkaar. Andersom nooit.

## Buildoutput

Alle artifacts van `image/*.sh` landen in `metal/out/` (gitignored als één
dir). De enige uitzondering is go:embed — dat kan niet buiten de eigen
package-dir reiken — dus deze twee plekken bevatten gebouwde elfs náást de
code (gitignored via `*.elf`):

- `cmd/hopos-embed/app*.elf` — de ingebakken app-images van de P1-variant;
- `kern/apploaderblob/apploader.elf` — de universele loader in de node.

## Nieuw pakket? Beslislijst

1. Importeren app én HOP het? → `abi/` (en besef: dit ís het contract).
2. Praat het met device-registers? → `driver/` (NIC? → `driver/nic/`).
3. Parseert het firmware-tabellen zonder registers te raken? → `fw/`.
4. CPU-instructie, EL-niveau of architectuur-firmware? → `cpu/`.
5. Alleen voor core 0 als vertrouwde kern? → `kern/`.
6. HOP's netwerkvlak? → `net/`. Draait het in een slot? → `app/`.
7. Bedrading van één board of SoC? → `board/<naam>`.
8. Is het een binary? → HOP-kant `cmd/`, app-kant `app/`.
9. Past het nergens? Eerst overleggen, niet een elfde categorie beginnen.

Host-testbare logica? Voeg het pakket toe aan de `go test`-lijst in
`tools/test.sh` — packages zonder tests draaien daar mee als compile-check.
