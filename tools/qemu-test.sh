#!/bin/sh
# De QEMU-poort van HopOS v3 (handboek §9: een emulator-boot met dezelfde
# markers als het ijzer). Bouwt de kern, boot hem op -M virt met dezelfde
# regel als image/qemu-run.sh, vangt de console en slaagt alleen als de
# markers er staan:
#
#   HOPOS_BOOT      de kern haalde kmain, de console en de executor;
#   HOPOS_TICK 3    drie seconden executor, timer en IRQ-deur;
#   HOPOS_NIC_UP    de virtio-net (QEMU heeft er altijd een in deze regel).
#
# Faalt er een, dan drukt het script de hele console af en faalt: rood is
# rood. Een HOPOS_PANIC of HOPOS_EXCEPTION is meteen rood.
#
#   tools/qemu-test.sh          TIMEOUT=20 standaard, in seconden
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TIMEOUT="${TIMEOUT:-20}"
TARGET=aarch64-unknown-none-softfloat
LOG="$(mktemp -t hopos-qemu.XXXXXX)"
trap 'rm -f "$LOG"; [ -n "${QPID:-}" ] && kill "$QPID" 2>/dev/null; true' EXIT INT TERM

cd "$DIR"
echo "== bouwen: hopos (qemuvirt)"
cargo build --quiet --release --target "$TARGET" -p hopos --features board-qemuvirt 2>/dev/null ||
	cargo build --release --target "$TARGET" -p hopos --features board-qemuvirt
KERNEL="$DIR/target/$TARGET/release/hopos"

echo "== booten op QEMU virt (tot ${TIMEOUT}s)"
qemu-system-aarch64 -M virt,gic-version=3,highmem-ecam=off,virtualization=on \
	-cpu cortex-a53 -smp 4 -m 3G \
	-nographic -monitor none -serial stdio \
	-global virtio-mmio.force-legacy=false \
	-device virtio-net-device,netdev=n0,bus=virtio-mmio-bus.0 \
	-netdev user,id=n0 \
	-kernel "$KERNEL" </dev/null >"$LOG" 2>&1 &
QPID=$!

# Wachten tot alle markers er zijn, iets roods verschijnt, QEMU stopt, of
# de tijd op is.
need="HOPOS_BOOT|HOPOS_TICK 3|HOPOS_NIC_UP"
elapsed=0
while :; do
	ok=1
	for m in "HOPOS_BOOT" "HOPOS_TICK 3" "HOPOS_NIC_UP"; do
		grep -q "$m" "$LOG" || ok=0
	done
	[ "$ok" = 1 ] && break
	grep -q -E "HOPOS_PANIC|HOPOS_EXCEPTION" "$LOG" && break
	kill -0 "$QPID" 2>/dev/null || break
	[ "$elapsed" -ge "$((TIMEOUT * 10))" ] && break
	sleep 0.1
	elapsed=$((elapsed + 1))
done
kill "$QPID" 2>/dev/null || true
wait "$QPID" 2>/dev/null || true
QPID=""

fail=0
for m in "HOPOS_BOOT" "HOPOS_TICK 3" "HOPOS_NIC_UP"; do
	if grep -q "$m" "$LOG"; then
		echo "   ok  $m: $(grep -m1 "$m" "$LOG" | tr -d '\r')"
	else
		echo "   ROOD $m ontbreekt"
		fail=1
	fi
done
if grep -q -E "HOPOS_PANIC|HOPOS_EXCEPTION" "$LOG"; then
	echo "   ROOD panic of exception"
	fail=1
fi
if [ "$fail" != 0 ]; then
	echo "== console (${need} gezocht):"
	tr -d '\r' <"$LOG"
	exit 1
fi
echo "qemu-poort groen"
