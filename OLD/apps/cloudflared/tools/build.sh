#!/bin/sh
# Host-tests + de twee slot-images. Zelfde recept als de andere HopOS-apps
# (welcome, de satellieten): de logica is host-getest, de main gaat door de
# tamago-gate — applib bestaat alleen onder GOOS=tamago.
#
# Vereist tools/prepare-cloudflared.sh (eenmalig, of na een versiebump).
set -e
cd "$(dirname "$0")/.."

if [ ! -d build/cloudflared-patched ]; then
	echo "!! build/cloudflared-patched ontbreekt — draai eerst tools/prepare-cloudflared.sh" >&2
	exit 1
fi

VERSION="${VERSION:-dev}"
TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"

# De host-tests raken cloudflared niet (internal/cfd bouwt alleen de
# opdrachtregel), dus die lopen zonder tamago.
GOWORK=off go test ./internal/...
GOWORK=off go vet ./internal/... ./cmd/...

if [ ! -x "$TAMAGO" ]; then
	echo "tamago-gate OVERGESLAGEN ($TAMAGO ontbreekt)" >&2
	exit 0
fi

mkdir -p out
# arm64: canoniek linkadres, één artifact draait in elk slot.
printf "  %-30s" "cloudflared-arm64-tamago.elf"
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags linkcpuinit -trimpath \
	-ldflags "-w -T 0x50010000 -R 0x1000 -X main.version=$VERSION" \
	-o out/cloudflared-arm64-tamago.elf ./cmd/cloudflared-hopos
du -h out/cloudflared-arm64-tamago.elf | cut -f1

# riscv64 (LicheeRV): geen tweede translatiefase, dus het linkadres IS de
# partitie en het RAM-plan komt uit de linkramsize-tag. linkcpuinit hoort erbij
# en is niet optioneel: een slot draait daar in S-MODE, en tamago's eigen
# opstart-assembly schrijft mie/mstatus — M-mode-CSR's, dus een illegal
# instruction op instructie twee van het entry (gemeten 31-07: mcause 2, mtval
# 0x30429073 = csrw mie). Met de tag levert HopOS de S-mode-veilige cpuinit.
printf "  %-30s" "cloudflared-riscv64-tamago.elf"
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=riscv64 \
	"$TAMAGO" build -tags "linkramsize linkcpuinit" -trimpath \
	-ldflags "-w -T 0x88010000 -R 0x1000 -X main.version=$VERSION" \
	-o out/cloudflared-riscv64-tamago.elf ./cmd/cloudflared-hopos
du -h out/cloudflared-riscv64-tamago.elf | cut -f1

echo "OK: host-tests groen, out/cloudflared-{arm64,riscv64}-tamago.elf gebouwd" >&2
