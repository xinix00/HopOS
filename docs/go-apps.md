# Go-apps op HopOS v3

Een app-image dat de slot-ABI spreekt draait, in welke taal het ook gebouwd
is. De Go-generatie (v2.x, tamago) is zo'n klant: slot-ABI 10 van
`OLD/metal/abi/layout` is byte voor byte ABI 1 van `abi/` (de staart van 2 MB,
de control-page-offsets, de ringgeometrie, de HVC-nummers 0, 1, 4, 5 en 6,
het systemapi-frame en de hopabi-ops). De kern laat de Go-stempel daarom toe
(`abi::place::GO_ABI_VERSION`) en vertaalt de sectietabel van de Go-linker,
die in het eerste PT_LOAD staat, terug naar de partitie zoals Go's
`stream.go` dat deed.

Bewezen 02-10-2026 op QEMU virt: `OLD/apps/welcome`, gebouwd met tamago,
geplaatst door Hop, net op met de RX-doorbell als interrupt, de pagina met de
bunny via de uplink-DNAT, en de stop trekt de poort in.

## Wat je nodig hebt

| Wat | Waar | Waarom |
| --- | --- | --- |
| de tamago-toolchain | `~/tamago-go/bin/go` (Go 1.26.4, `GOOS=tamago`) | de compiler en de runtime |
| de drie runtime-patches | `OLD/tools/tamago-go/*.patch`, `apply.sh` | de idle-hook, de bound-addresses in `net`, de `findTimer`-grens; zonder de eerste parkeert een app nooit en zonder de derde faultt de doorbell op de M4 |
| de Go-SDK | `OLD/metal` (module `github.com/xinix00/HopOS/metal/v2`, tag `metal/v2.2.8`) | `app/applib`, `abi`, `cpu/idle`, `board/hopslot`, `dev`: de app-kant van de ABI |
| de app-modules | `OLD/apps/<naam>` met een `replace` naar `../../metal` | welcome, vitals, cloudflared, cloudflared-lean |

De toolchain op deze machine draagt de patches al (`git -C ~/tamago-go
status` toont de vijf gewijzigde runtime-bestanden). Op een verse machine:

```sh
git clone https://github.com/usbarmory/tamago-go ~/tamago-go
git -C ~/tamago-go checkout --detach "$(cat OLD/tools/tamago-go/BASE_REVISION)"
(cd ~/tamago-go/src && ./make.bash)
TAMAGO_SRC=~/tamago-go sh OLD/tools/tamago-go/apply.sh
```

De compiler hoeft na de patches niet opnieuw gebouwd: hij compileert de
runtime uit zijn eigen bronboom mee bij elke build.

## Bouwen

Eén artifact draait in elk slot: arm64 linkt op het canonieke adres
`SlotBase(1) + 0x10000`, net als een Rust-app (`applib/link.ld`). De
`-R 0x1000` houdt de segmenten op pagina's, `-w` laat DWARF weg en houdt de
symbooltabel (de kern leest er `RamStart`, `RamSize`, de slot-hint en de
stempel uit; nooit `-s`). De tag `linkcpuinit` kiest de kale EL1-tak van
`board/hopslot`.

```sh
cd OLD/apps/welcome
GOWORK=off GOTOOLCHAIN=local GOFLAGS=-mod=mod \
GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
~/tamago-go/bin/go build -trimpath -tags linkcpuinit \
  -ldflags "-w -T 0x50010000 -R 0x1000" \
  -o /tmp/welcome-arm64-tamago.elf ./cmd/welcome
git checkout -- go.mod go.sum   # -mod=mod herschrijft ze
```

Dit is het recept van `OLD/tools/apps-release.sh` (stap 2), met de lijst van
apps en hun `cmd`-submap in `OLD/tools/hopos-apps.list`. Dat script bouwt ook
alle apps in één keer en toetst dat er geen gvisor-symbool in zit:

```sh
PUBLISH=0 sh OLD/tools/apps-release.sh
```

Voor cloudflared eerst `OLD/apps/cloudflared/tools/prepare-cloudflared.sh`
(zet de gepinde cloudflared met twee platform-fallbacks in
`build/cloudflared-patched`; tot dan faalt elk go-commando in die module), zie
`OLD/apps/cloudflared/README.md`. Het image is 30 MB; de kern streamt het
rechtstreeks de partitie in en leest alleen de symbooltabel op de heap
(`kern::system::MAX_SYMBOLS`, 16 MB).

## Draaien

Een jobspec is dezelfde als voor een Rust-app; Hop ziet geen verschil:

```json
{"name":"welcome","driver":"hop",
 "artifacts":[{"url":"https://.../welcome-arm64-tamago.elf","match":{"node.arch":"arm64"}}],
 "memory_limit":67108864,"ports":{"http":80}}
```

Op QEMU, met de kring van de welcome-test:

```sh
GO_ELF=/tmp/welcome-arm64-tamago.elf sh tools/qemu-test-welcome.sh
```

De console toont de Go-app via zijn outbox, zoals elke app:

```
slot 2: mem: Go memory limit 23MB, GOGC 25 (window 30MB, image+bss 2MB)
slot 2: appnet: RX doorbell served as interrupt
slot 2: welcome dev: slot 2, 1 core, 30.0 MB RAM, serving http://10.100.0.3
```

Een Go-image met een oudere stempel dan 10 weigert de kern met `image speaks
slot ABI <n>, this HopOS speaks 1`; bouw het dan opnieuw tegen
`metal/v2.2.8`. De Spin-releases (`OLD/job-spin.json`) moeten daarom
nagekeken worden tegen welke metal ze gebouwd zijn.

## Wat de Go-app anders doet dan een Rust-app

- Hij mapt zijn hele staart Normal write-back, ook de control-page
  (`memattr.NormalWB`); een Rust-app zet de control-page op Normal-NC
  (`applib/src/mmu.rs`). Op QEMU maakt dat niets uit; op ijzer is dit de
  eerste plek om te kijken als een Go-app wel boot en niet logt.
- Hij schrijft het write-back-woord in de ringkop niet (`abi::ring::WB_WORD`
  is van 01-10); de kern belooft dan niets en doet push/pull per record. Dat
  is de Go-snelheid van vóór die datum, niet trager.
- Zijn env leest hij tot `CTRL_ENV_LEGACY_MAX`; de kern schrijft nooit meer
  dan `CTRL_ENV_MAX`, dus de bovenste woorden (RNG-zaad, timebase,
  app-fault-rapport) raakt hij niet.
- Hij heeft zijn eigen RNG (RNDR of jitter-DRBG) en zijn eigen klok; het zaad
  van de kern op de control-page leest hij niet.
- SMP: de `CTRL_SMP_*`-woorden zijn de M/g0-overdracht van de Go-runtime en
  staan nog in ABI 1; een Go-app met `cores: 2` is op v3 nog niet gedraaid.

## Nog te bewijzen

- Op ijzer: de staart op Normal-WB op de M4, de Radxa en de Pi's; de
  doorbell als vFIQ op de M4 (HVC 5).
- riscv64: de Go-apps linkten daar op de fysieke partitie (`-T 0x88010000`,
  tag `linkramsize`); de v3-kooi verplaatst naar het canonieke venster.
- cloudflared zelf (30 MB) en Spin.

## De prijs

De kern kost dit twee kleine stukken code. De prijs zit erbuiten: `OLD/metal`
blijft de Go-SDK en mag niet per package verdwijnen zodra de Rust-crate de
poort haalt (de regel van `rustdoc/PORT.md` §6.5 geldt niet voor
`app/applib`, `abi`, `cpu/idle`, `board/hopslot`, `board/appboard`, `dev` en
`cpu/memattr`), en ABI 1 blijft gelijk aan Go-ABI 10. Verschuift er iets in
de staart, dan gaat `GO_ABI_VERSION` weg of wordt de Go-applib hertaald en
opnieuw getagd.
