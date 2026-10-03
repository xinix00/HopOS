# Go-apps op HopOS

Een app-image dat de slot-ABI spreekt draait, in welke taal het ook gebouwd
is. Een app waarvan de bron Go is (cloudflared) bouwt met tamago tegen de
Go-SDK, de module `github.com/xinix00/HopOS/metal/v2`. Slot-ABI 10 van die
SDK (`abi/layout`) is byte voor byte ABI 1 van `abi/` (de staart van 2 MB,
de control-page-offsets, de ringgeometrie, de HVC-nummers 0, 1, 4, 5 en 6,
het systemapi-frame en de hopabi-ops). De kern laat de Go-stempel daarom toe
(`abi::place::GO_ABI_VERSION`) en vertaalt de sectietabel van de Go-linker,
die in het eerste PT_LOAD staat, terug naar de partitie
(`kern::system`).

Bewezen 02-10-2026 op QEMU virt: een Go-welcome, gebouwd met tamago,
geplaatst door Hop, net op met de RX-doorbell als interrupt, de pagina met de
bunny via de uplink-DNAT, en de stop trekt de poort in.

## Wat je nodig hebt

| Wat | Waar | Waarom |
| --- | --- | --- |
| de tamago-toolchain | `~/tamago-go/bin/go` (Go 1.26.4, `GOOS=tamago`) | de compiler en de runtime |
| de drie runtime-patches | `go/tamago-go/*.patch`, `apply.sh` | de idle-hook, de bound-addresses in `net`, de `findTimer`-grens; zonder de eerste parkeert een app nooit en zonder de derde faultt de doorbell op de M4 |
| de Go-SDK | module `github.com/xinix00/HopOS/metal/v2` v2.2.8, van GitHub | `app/applib`, `abi`, `cpu/idle`, `board/hopslot`, `dev`: de app-kant van de ABI |
| de app-modules | `go/<naam>`, de SDK gepind in hun `go.mod` | cloudflared |

De toolchain op deze machine draagt de patches al (`git -C ~/tamago-go
status` toont de vijf gewijzigde runtime-bestanden). Op een verse machine:

```sh
git clone https://github.com/usbarmory/tamago-go ~/tamago-go
git -C ~/tamago-go checkout --detach "$(cat go/tamago-go/BASE_REVISION)"
(cd ~/tamago-go/src && ./make.bash)
TAMAGO_SRC=~/tamago-go sh go/tamago-go/apply.sh
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
cd go/cloudflared
sh tools/prepare-cloudflared.sh
GOWORK=off GOTOOLCHAIN=local \
GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
~/tamago-go/bin/go build -trimpath -tags linkcpuinit \
  -ldflags "-w -T 0x50010000 -R 0x1000" \
  -o /tmp/cloudflared-arm64-tamago.elf ./cmd/cloudflared-hopos
```

`prepare-cloudflared.sh` zet de gepinde cloudflared met twee
platform-fallbacks in `build/cloudflared-patched`; tot dan faalt elk
go-commando in die module (zie `go/cloudflared/README.md`). Het image is
28 MB; de kern streamt het rechtstreeks de partitie in en zoekt zijn
symbolen daar in brokken op, niet op de heap.

`go/apps-release.sh` doet hetzelfde voor alle Go-apps in één keer en toetst
dat er geen gvisor-symbool in zit; `METAL=<tag>` bouwt ze tegen een andere
tag van de SDK:

```sh
PUBLISH=0 sh go/apps-release.sh
```

## Draaien

Een jobspec is dezelfde als voor een Rust-app; Hop ziet geen verschil
(`jobs/job-cloudflared.json`):

```json
{"name":"cloudflared","driver":"hop",
 "artifacts":[{"url":"https://.../cloudflared-arm64-tamago.elf","match":{"node.arch":"arm64"}}],
 "memory_limit":268435456,"env":{"TUNNEL_TOKEN":"..."}}
```

Op QEMU neemt de welcome-test een Go-welcome als artifact:

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
`metal/v2` v2.2.8. De Spin-releases (`jobs/job-spin.json`) moeten daarom
nagekeken worden tegen welke metal ze gebouwd zijn.

## Wat de Go-app anders doet dan een Rust-app

- Hij mapt zijn hele staart Normal write-back, ook de control-page
  (`memattr.NormalWB`); een Rust-app zet de control-page op Normal-NC
  (`applib/src/mmu.rs`). Op QEMU maakt dat niets uit; op ijzer is dit de
  eerste plek om te kijken als een Go-app wel boot en niet logt.
- Hij schrijft het write-back-woord in de ringkop niet (`abi::ring::WB_WORD`);
  de kern belooft dan niets en doet push/pull per record.
- Zijn env leest hij tot `CTRL_ENV_LEGACY_MAX`; de kern schrijft nooit meer
  dan `CTRL_ENV_MAX`, dus de bovenste woorden (RNG-zaad, timebase,
  app-fault-rapport) raakt hij niet.
- Hij heeft zijn eigen RNG (RNDR of jitter-DRBG) en zijn eigen klok; het zaad
  van de kern op de control-page leest hij niet.
- SMP: de `CTRL_SMP_*`-woorden zijn de M/g0-overdracht van de Go-runtime en
  staan nog in ABI 1; een Go-app met `cores: 2` is nog niet gedraaid.

## Nog te bewijzen

- Op ijzer: de staart op Normal-WB op de M4, de Radxa en de Pi's; de
  doorbell als vFIQ op de M4 (HVC 5).
- riscv64: een Go-app linkt daar op de fysieke partitie (`-T 0x88010000`,
  tag `linkramsize`); de kooi plaatst apps in het canonieke venster.
  `go/apps-release.sh` bouwt daarom alleen arm64.
- cloudflared zelf (28 MB) en Spin.

## De prijs

De kern kost dit twee kleine stukken code. De prijs zit erbuiten: ABI 1 blijft
gelijk aan Go-ABI 10. Verschuift er iets in de staart, dan gaat
`GO_ABI_VERSION` weg of krijgt de Go-SDK een nieuwe tag met de nieuwe
indeling.
