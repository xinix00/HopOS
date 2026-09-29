#!/bin/sh
# Host-tests + de twee slot-images. Zelfde recept als de andere HopOS-apps.
#
# Anders dan apps/cloudflared is hier NIETS voor te bereiden: geen gepinde
# vreemde module, geen patch-map, geen prepare-script. Dat is het hele punt.
set -e
cd "$(dirname "$0")/.."

VERSION="${VERSION:-dev}"
TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
[ -x "$TAMAGO" ] || { echo "build: $TAMAGO ontbreekt — zet TAMAGO=/pad/naar/go" >&2; exit 1; }

# De protocol-code is host-getest: HPACK tegen de RFC-voorbeelden, capnp op de
# rondgang, de ingress-tabel op Cloudflare's eigen matching-regels.
GOWORK=off go test ./internal/...

# Het host-binary is geen bijproduct: hiermee wordt de tunnel tegen de ECHTE
# edge geprobeerd vóór er een boot-cyclus aan een node opgaat.
GOWORK=off go build -ldflags "-s -w -X main.version=$VERSION" -o out/cloudflared-lean-host ./cmd/cloudflared-lean

mkdir -p out
for arch in arm64 riscv64; do
	case "$arch" in
	# GEEN -s: HOP's plaatser leest de symboltabel van het image (de v0.4.0-les
	# uit stulp). -w mag wel — dat is alleen DWARF.
	riscv64) ld="-w -T 0x88010000 -R 0x1000" ;;
	arm64)   ld="-w -T 0x50010000 -R 0x1000" ;;
	esac
	elf="out/cloudflared-lean-$arch-tamago.elf"
	GOWORK=off GOTOOLCHAIN=local GOFLAGS=-mod=mod \
		GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH="$arch" \
		"$TAMAGO" build -tags "linkcpuinit" -trimpath \
		-ldflags "$ld -X main.version=$VERSION" -o "$elf" ./cmd/cloudflared-lean
	echo "$elf ($(( $(wc -c < "$elf") / 1024 )) kB)"
done
