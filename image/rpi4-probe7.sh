#!/bin/sh
# Bouw probe7 (GENET-netprobe, fase P2) voor de Raspberry Pi 4 → sd-rpi4/.
# Zelfde boot-recept als rpi4-hopos.sh (raw kernel8.img op 0x80000, TF-A
# bl31.bin als armstub — zie dat script voor het dossier).

set -e

TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"

cd "$DIR/metal"
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags "rpi4 linkcpuinit" -trimpath \
	-ldflags "-s -w -T 0x90000 -R 0x1000" -o probe7.elf ./cmd/probe7

cd "$DIR"
mkdir -p sd-rpi4
go run "$DIR/image/mkkernel/main.go" -elf metal/probe7.elf -o sd-rpi4/kernel8.img -load 0x80000 -raw

cat > sd-rpi4/config.txt <<'EOF'
# HopOS probe7 — GENET-netprobe op de Raspberry Pi 4 (zie docs/rpi4.md)
arm_64bit=1
kernel=kernel8.img
device_tree_address=0x0f000000
# TF-A BL31 als armstub: levert PSCI. De stock armstub8 heeft dat NIET.
armstub=bl31.bin
uart_2ndstage=1
# Houd de PL011 bij GPIO14/15 (anders claimt Bluetooth hem).
dtoverlay=disable-bt
EOF

echo "sd-rpi4/kernel8.img (probe7) + config.txt klaar." >&2
echo "flash: cp sd-rpi4/kernel8.img sd-rpi4/config.txt sd-rpi4/bl31.bin '/Volumes/NO NAME/'" >&2
echo "meting: kabel in de ethernet-poort!" >&2
