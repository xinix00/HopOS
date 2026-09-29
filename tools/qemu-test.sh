#!/bin/sh
# De QEMU-poort van HopOS v3 (handboek §9: een emulator-boot met dezelfde
# markers als het ijzer). Bouwt de kern, boot hem op -M virt met dezelfde
# regel als image/qemu-run.sh, vangt de console en slaagt alleen als de
# markers er staan:
#
#   HOPOS_BOOT      de kern haalde kmain, de console en de executor;
#   HOPOS_TICK 3    drie seconden executor, timer en IRQ-deur;
#   HOPOS_NIC_UP    de virtio-net (QEMU heeft er altijd een in deze regel).
#   HOPOS_NET_UP    pomp, switch, poort 0 en een DHCP-lease van user-net;
#   HOPOS_SYSTEM_UP de system-listener op poort 10100;
#   extern          een TCP-verbinding van de host via hostfwd naar
#                   10.0.2.15:10100 die de kern accepteert en weigert (geen
#                   slot achter 10.0.2.2): de host ziet EOF en de kern meldt
#                   HOPOS_SYSTEM_REFUSED. EOF alleen bewijst niets (slirp
#                   sluit ook als de gast niet luistert), de marker wel.
#   appspike        het ABI-bewijs: appspike (door QEMU gestaged, zie
#                   image/qemu-run.sh) draait in slot 1 op een app-core in
#                   zijn stage-2-kooi, al zijn toetsen groen via de servicer
#                   op de console (HOPOS_APPSPIKE_DONE ... fail=0), en de kern
#                   ziet exit 0 (HOPOS_SLOT_DONE).
#
# Faalt er een, dan drukt het script de hele console af en faalt: rood is
# rood. Een HOPOS_PANIC of HOPOS_EXCEPTION is meteen rood.
#
#   tools/qemu-test.sh          TIMEOUT=30 standaard, in seconden
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TIMEOUT="${TIMEOUT:-30}"
SYSPORT="${SYSPORT:-10100}" # de host-kant van de hostfwd naar de system-API
TARGET=aarch64-unknown-none-softfloat
LOG="$(mktemp -t hopos-qemu.XXXXXX)"
trap 'rm -f "$LOG"; [ -n "${QPID:-}" ] && kill "$QPID" 2>/dev/null; true' EXIT INT TERM

cd "$DIR"
echo "== bouwen: hopos (qemuvirt)"
cargo build --quiet --release --target "$TARGET" -p hopos --features board-qemuvirt 2>/dev/null ||
	cargo build --release --target "$TARGET" -p hopos --features board-qemuvirt
KERNEL="$DIR/target/$TARGET/release/hopos"
echo "== bouwen: appspike"
cargo build --quiet --release --target "$TARGET" -p appspike 2>/dev/null ||
	cargo build --release --target "$TARGET" -p appspike
SPIKE="$DIR/target/$TARGET/release/appspike"
SPIKE_SIZE=$(wc -c <"$SPIKE" | tr -d ' ')
# De markers van het ABI-bewijs (grep -E): het aantal toetsen groeit met
# appspike, dus "alles groen" is fail=0.
SLOT_MARKS="HOPOS_SLOT_START slot=1|slot 1: HOPOS_APPSPIKE_NETLOG|HOPOS_APPSPIKE_DONE pass=[0-9]+ fail=0|HOPOS_SLOT_DONE slot=1 exit=0"

echo "== booten op QEMU virt (tot ${TIMEOUT}s)"
qemu-system-aarch64 -M virt,gic-version=3,highmem-ecam=off,virtualization=on \
	-cpu cortex-a53 -smp 4 -m 3G \
	-nographic -monitor none -serial stdio \
	-global virtio-mmio.force-legacy=false \
	-device virtio-net-device,netdev=n0,bus=virtio-mmio-bus.0 \
	-netdev "user,id=n0,hostfwd=tcp:127.0.0.1:${SYSPORT}-:10100" \
	-device "loader,file=$SPIKE,addr=0xb0200000,force-raw=on" \
	-device "loader,addr=0xb0100000,data=$SPIKE_SIZE,data-len=8" \
	-kernel "$KERNEL" </dev/null >"$LOG" 2>&1 &
QPID=$!

# De toets van buiten: verbinden via de hostfwd en wachten tot de kern de
# verbinding sluit (EOF). Slaagt alleen als de gast antwoordt.
PROBE=""
probe() {
	python3 - "$SYSPORT" <<'PY'
import socket, sys
s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=5)
s.settimeout(5)
n = 0
while True:
    b = s.recv(4096)
    if not b:
        break
    n += len(b)
print(f"connected to 127.0.0.1:{sys.argv[1]}, closed by the kernel after {n} bytes")
PY
}

# Wachten tot alle markers er zijn, iets roods verschijnt, QEMU stopt, of
# de tijd op is.
need="HOPOS_BOOT|HOPOS_TICK 3|HOPOS_NIC_UP|HOPOS_NET_UP|HOPOS_SYSTEM_UP|HOPOS_SYSTEM_REFUSED"
elapsed=0
while :; do
	ok=1
	for m in "HOPOS_BOOT" "HOPOS_TICK 3" "HOPOS_NIC_UP" "HOPOS_NET_UP" "HOPOS_SYSTEM_UP" "HOPOS_SYSTEM_REFUSED"; do
		grep -q "$m" "$LOG" || ok=0
	done
	(IFS='|' && for m in $SLOT_MARKS; do grep -q -E "$m" "$LOG" || exit 1; done) || ok=0
	# De toets van buiten meteen na HOPOS_SYSTEM_UP. Bekend gebrek (29-09):
	# de kern bedient de system-API met één verbinding tegelijk en blijft
	# lezen op een half-open verbinding van een geparkeerde app, dus ná de
	# slot-keten hangt hij; en vroeg botst hij soms met de NET-toets van
	# slot 1. Wordt hard zodra de listener per verbinding een taak spawnt.
	if [ -z "$PROBE" ] && grep -q "HOPOS_SYSTEM_UP" "$LOG"; then
		PROBE="$(probe 2>&1)" && PROBE="ok  extern: $PROBE" || PROBE="ROOD extern: $(echo "$PROBE" | tail -1)"
	fi
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
for m in "HOPOS_BOOT" "HOPOS_TICK 3" "HOPOS_NIC_UP" "HOPOS_NET_UP" "HOPOS_SYSTEM_UP" "HOPOS_SYSTEM_REFUSED"; do
	if grep -q "$m" "$LOG"; then
		echo "   ok  $m: $(grep -m1 "$m" "$LOG" | tr -d '\r')"
	else
		echo "   ROOD $m ontbreekt"
		fail=1
	fi
done
IFS_WAS="$IFS"
IFS='|'
for m in $SLOT_MARKS; do
	if grep -q -E "$m" "$LOG"; then
		echo "   ok  $m: $(grep -m1 -E "$m" "$LOG" | tr -d '\r')"
	else
		echo "   ROOD $m ontbreekt"
		fail=1
	fi
done
IFS="$IFS_WAS"
case "$PROBE" in
"ok "*) echo "   $PROBE" ;;
"") echo "   ROOD extern: nooit geprobeerd (geen HOPOS_SYSTEM_UP)" && fail=1 ;;
*) echo "   $PROBE" && fail=1 ;;
esac
if grep -q -E "HOPOS_PANIC|HOPOS_EXCEPTION" "$LOG"; then
	echo "   ROOD panic of exception"
	fail=1
fi
if [ "$fail" != 0 ]; then
	KEEP="${LOG}.rood"; cp "$LOG" "$KEEP"; echo "== console bewaard in $KEEP"
	echo "== console (${need} gezocht):"
	tr -d '\r' <"$LOG"
	exit 1
fi
echo "qemu-poort groen"
