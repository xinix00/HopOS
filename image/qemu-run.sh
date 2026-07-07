#!/bin/sh
# Boot HopOS in QEMU — PLAN.md fase 1.
#
# Bouwt metal/ met de tamago-go toolchain en start QEMU met het
# i.MX8MP-EVK machinemodel (TamaGo's arm64-referentiebord).
#
#   TAMAGO=~/tamago-go/bin/go image/qemu-run.sh
#
# Console = UART1 op stdio. Stoppen: Ctrl-A X werkt hier niet (-monitor none);
# gebruik Ctrl-C (QEMU draait in de voorgrond).

set -e

TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"

if [ ! -x "$TAMAGO" ]; then
	echo "tamago-go toolchain niet gevonden op $TAMAGO (zet TAMAGO=...)" >&2
	exit 1
fi

cd "$DIR/metal"

GOWORK=off GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags imx8mpevk,linkramsize -trimpath \
	-ldflags "-s -w -T 0x40010000 -R 0x1000" -o hopos.elf .

echo "hopos.elf gebouwd ($(du -h hopos.elf | cut -f1)) — QEMU start..." >&2

exec qemu-system-aarch64 -machine imx8mp-evk -m 512M -smp 1 \
	-nographic -monitor none -semihosting \
	-serial stdio -serial null -net none \
	-kernel hopos.elf "$@"
