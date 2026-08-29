#!/bin/sh -e
# Bouw de HopOS-bring-up-probe voor Apple silicon (Mac mini M4, t8132) —
# naast radxa-zero3.sh; zelfde vorm, ander silicium en een andere loader.
#
#   image/apple-m4.sh                → cmd/probeapple → metal/out/probeapple.img
#   AGENT=1 image/apple-m4.sh        → de ECHTE agent (cmd/hopos, -tags apple)
#                                      → metal/out/hopos-apple.img
#   EMBED=1 image/apple-m4.sh        → het acceptatiedraaiboek met de app INGEBAKKEN
#                                      (cmd/hopos-embed): bewijst de kooi zonder
#                                      netwerk → metal/out/hopos-apple-embed.img
#   image/apple/boot-cycle.sh <img>  → via m1n1's proxy op de mini laden en starten
#
# Boot-route: iBoot → m1n1 (als custom kernel op Permissive Security) → de
# proxy over USB → load-probe.py schrijft het image op zijn linkadres en
# springt (kboot_boot). Geen kaart, geen U-Boot: het "image" reist over de
# kabel. De vaste-adres-conventie is dezelfde als op elk ander board: het
# venster begint op apple.RamBase (1TiB + 4GB), tekst op +0x10000.
#
# De tamago-fork: RAM op 1TiB ligt buiten tamago's vlakke 39-bit-map, dus
# deze build gebruikt image/apple/go.work (replace naar ../tamago, branch
# hopos-highram). Alle andere scripts blijven GOWORK=off.

TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"
. "$DIR/image/lib.sh"

cd "$DIR/metal"
mkdir -p out

# highram: de tamago-fork geeft de Go-heap 42-bit-adressen (goos/mem_highram.go)
# — met de default 40 bits weigert de runtime élke reservering boven 1TiB
# ("base outside usable address space", gemeten 28-08, de eerste boot).
# VHE (asmflags): Apple's EL2 heeft E2H vast op 1; cpu/el2/sysreg.h kiest dan
# de _EL12-encoderingen voor de EL1-registers van een app.
if [ "${AGENT:-0}" = 1 ]; then
	TARGET=./cmd/hopos; NAME=hopos-apple; TAGS="apple linkcpuinit highram"
elif [ "${EMBED:-0}" = 1 ]; then
	TARGET=./cmd/hopos-embed; NAME=hopos-apple-embed; TAGS="apple linkcpuinit highram"
	# De app-image die de main via go:embed meedraagt: board-onafhankelijk (de
	# stage-2 ís de relocatie), op de slot-1-IPA gelinkt en tegen de gewone
	# upstream-tamago gebouwd (GOWORK=off: het app-RAM ligt op IPA 0x50000000,
	# ver onder 512GB — highram en VHE zijn HOP-zaken). Gitignored build-output.
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
		"$TAMAGO" build -tags "linkcpuinit" -trimpath \
		-ldflags "-w -T 0x50010000 -R 0x1000" -o cmd/hopos-embed/app.elf ./app/appspike
else
	TARGET=./cmd/probeapple; NAME=probeapple; TAGS="linkcpuinit highram"
fi
ASMFLAGS="all=-D=VHE"

# Linkadres 0x101_0001_0000 in het venster vanaf 0x101_0000_0000
# (apple.RamBase): de +0x10000-vorm van elk board.
GOWORK="$DIR/image/apple/go.work" GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags "$TAGS" -trimpath -asmflags "$ASMFLAGS" \
	-ldflags "-T 0x10100010000 -R 0x1000" -o "out/$NAME.elf" "$TARGET"

# ELF → platte image mét arm64-Image-header (m1n1's payload-pad herkent die
# straks aan het ARM\x64-magic); text_offset = 4GB t.o.v. de DRAM-basis.
cd "$DIR"
go run "$DIR/image/mkkernel/main.go" "$DIR/image/mkkernel/pe.go" \
	-elf "metal/out/$NAME.elf" -o "metal/out/$NAME.img" -load 0x10100000000 -dram 0x10000000000
