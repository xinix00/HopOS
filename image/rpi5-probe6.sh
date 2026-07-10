#!/bin/sh
# Bouw probe6 (PCIe-RC-bring-up, fase P2) voor de Raspberry Pi 5 en zet
# alles voor de SD-kaart klaar in sd-rpi5/. Zelfde recept als rpi5-probe.sh
# (raw image op 0x80000, os_check=0) — zie dat script voor de meetlog
# achter elke keuze.

set -e

TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"

cd "$DIR/metal"
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags linkcpuinit -trimpath \
	-ldflags "-s -w -T 0x90000 -R 0x1000" -o probe6.elf ./cmd/probe6

cd "$DIR"
mkdir -p sd-rpi5
go run "$DIR/image/mkkernel/main.go" -elf metal/probe6.elf -o sd-rpi5/kernel_2712.img -load 0x80000 -raw

cat > sd-rpi5/config.txt <<'EOF'
# HopOS probe6 — PCIe-bring-up op de Raspberry Pi 5 (zie docs/rpi5.md)
arm_64bit=1
kernel=kernel_2712.img
os_check=0
device_tree_address=0x0f000000
uart_2ndstage=1
EOF

echo "sd-rpi5/ klaar: probe6 als kernel_2712.img (+ config.txt)" >&2
echo "meting: UART 115200 op de debug-connector; kabel in de ethernet-poort!" >&2
