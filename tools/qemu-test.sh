#!/bin/sh
# De QEMU-poort van HopOS v3 (handboek §9: een emulator-boot met dezelfde
# markers als het ijzer). Bouwt de kern, boot hem op -M virt met dezelfde
# regel als image/qemu-run.sh, vangt de console en slaagt alleen als de
# markers er staan:
#
#   HOPOS_BOOT      de kern haalde kmain, de console en de executor;
#   HOPOS_TICK 3    drie seconden executor, timer en IRQ-deur;
#   HOPOS_NIC_UP    de virtio-net (QEMU heeft er altijd een in deze regel).
#   HOPOS_DISK_UP   de virtio-blk op een verse schijf van 64 MiB (per run
#                   een eigen tijdelijk bestand, dus altijd leeg);
#   HOPOS_FS_UP     hopfs erop gemount, vers (fresh=1);
#   HOPOS_NET_UP    pomp, switch, poort 0 en een DHCP-lease van user-net;
#   HOPOS_SYSTEM_UP de system-listener op poort 10100;
#   extern          een TCP-verbinding van de host via hostfwd naar
#                   10.0.2.15:10100 die de kern accepteert en weigert (geen
#                   slot achter 10.0.2.2): de host ziet EOF en de kern meldt
#                   een NIEUWE regel HOPOS_SYSTEM_REFUSED. EOF alleen bewijst
#                   niets (slirp sluit ook als de gast niet luistert), de
#                   marker wel. Drie keer, op willekeurige momenten van de
#                   keten: meteen na HOPOS_SYSTEM_UP, zodra de app van slot 1
#                   zijn netwerk heeft (vlak voor zijn NET-toets, die dan met
#                   de toets van buiten samenvalt), en na de stop van slot 2
#                   (bewijst dat de listener na de keten niet hangt).
#   appspike        het ABI-bewijs: appspike (door QEMU gestaged, zie
#                   image/qemu-run.sh; zonder rolwoord op STAGE_ROLE_PA is
#                   het een gewone app, dus plaatst de kern hem zelf; de
#                   kring met Hop staat in tools/qemu-test-hop.sh) draait in slot 1 op een app-core in
#                   zijn stage-2-kooi, al zijn toetsen groen via de servicer
#                   op de console (HOPOS_APPSPIKE_DONE pass=9 fail=0, met
#                   HOPOS_APPSPIKE_FS: schrijven, stat, lezen, lijst en weg in
#                   de eigen root via de system-API naar hopfs), en de kern
#                   ziet exit 0 (HOPOS_SLOT_DONE); daarna hetzelfde in slot 2
#                   op de warm geparkeerde core, met zijn logregel over de
#                   system-API (die bleef vóór 29-09 hangen achter de
#                   half-open verbinding van slot 1).
#
# Faalt er een, dan drukt het script de hele console af en faalt: rood is
# rood. Een HOPOS_PANIC of HOPOS_EXCEPTION is meteen rood.
#
#   tools/qemu-test.sh          TIMEOUT=30 standaard, in seconden
#   KEEP_LOG=pad tools/qemu-test.sh   bewaart ook een groene console
#   SYSPORT=poort               de host-kant van de hostfwd; bezet = een vrije
#                               poort van het OS, luid gemeld
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TIMEOUT="${TIMEOUT:-30}"
TARGET=aarch64-unknown-none-softfloat
LOG="$(mktemp -t hopos-qemu.XXXXXX)"
DISK="$(mktemp -t hopos-disk.XXXXXX)"
trap 'rm -f "$LOG" "$DISK"; [ -n "${QPID:-}" ] && kill "$QPID" 2>/dev/null; true' EXIT INT TERM

# Een host-poort: de gevraagde als hij vrij is, anders een vrije van het
# OS. Zo draait de toets naast een andere QEMU.
port() {
	python3 - "$1" "$2" <<'PY'
import socket, sys
want, name = int(sys.argv[1]), sys.argv[2]
s = socket.socket()
try:
    s.bind(("127.0.0.1", want))
    print(want)
except OSError:
    s.close()
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    got = s.getsockname()[1]
    print(f"   {name} {want} is taken, using {got}", file=sys.stderr)
    print(got)
s.close()
PY
}
SYSPORT="$(port "${SYSPORT:-10100}" SYSPORT)" # de host-kant van de hostfwd naar de system-API
# Een verse, ijle schijf van 64 MiB: hopfs begint leeg.
dd if=/dev/zero of="$DISK" bs=1048576 count=0 seek=64 2>/dev/null

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
SLOT_MARKS="HOPOS_DISK_UP model=virtio-blk blocks=131072|HOPOS_FS_UP fresh=1"
SLOT_MARKS="$SLOT_MARKS|HOPOS_SLOT_START slot=1|slot 1: HOPOS_APPSPIKE_NETLOG|slot 1: HOPOS_APPSPIKE_FS ok|slot 1: HOPOS_APPSPIKE_DONE pass=9 fail=0|HOPOS_SLOT_DONE slot=1 exit=0|slot 1: stopped.*HOPOS_SLOT_STOPPED"
SLOT_MARKS="$SLOT_MARKS|HOPOS_SLOT_START slot=2|slot 2: HOPOS_APPSPIKE_NETLOG|slot 2: HOPOS_APPSPIKE_FS ok|slot 2: HOPOS_APPSPIKE_DONE pass=9 fail=0|HOPOS_SLOT_DONE slot=2 exit=0|slot 2: stopped.*HOPOS_SLOT_STOPPED"
# De momenten van de toets van buiten (grep -E), in volgorde.
PROBE_AT="HOPOS_SYSTEM_UP|slot 1: .*HOPOS_APPNET_UP|slot 2: stopped.*HOPOS_SLOT_STOPPED"

echo "== booten op QEMU virt (tot ${TIMEOUT}s)"
qemu-system-aarch64 -M virt,gic-version=3,highmem-ecam=off,virtualization=on \
	-cpu cortex-a53 -smp 4 -m 3G \
	-nographic -monitor none -serial stdio \
	-global virtio-mmio.force-legacy=false \
	-device virtio-net-device,netdev=n0,bus=virtio-mmio-bus.0 \
	-netdev "user,id=n0,hostfwd=tcp:127.0.0.1:${SYSPORT}-:10100" \
	-drive "if=none,format=raw,file=$DISK,id=disk0" \
	-device virtio-blk-device,drive=disk0,bus=virtio-mmio-bus.1 \
	-device "loader,file=$SPIKE,addr=0xb0200000,force-raw=on" \
	-device "loader,addr=0xb0100000,data=$SPIKE_SIZE,data-len=8" \
	-kernel "$KERNEL" </dev/null >"$LOG" 2>&1 &
QPID=$!

# De toets van buiten: verbinden via de hostfwd en wachten tot de kern de
# verbinding sluit (EOF). Slaagt alleen als de gast antwoordt.
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

refusals() { grep -c "HOPOS_SYSTEM_REFUSED" "$LOG" || true; }

# Eén toets van buiten op moment $1: EOF van de host én een nieuwe
# weigeringsregel van de kern (tot 3 s na de EOF, want de console loopt via
# QEMU's stdio iets achter).
probe_at() {
	before=$(refusals)
	if out="$(probe 2>&1)"; then
		i=0
		while [ "$(refusals)" -le "$before" ] && [ "$i" -lt 30 ]; do
			sleep 0.1
			i=$((i + 1))
		done
		if [ "$(refusals)" -gt "$before" ]; then
			echo "ok  extern na '$1': $out"
		else
			echo "ROOD extern na '$1': EOF maar geen nieuwe HOPOS_SYSTEM_REFUSED"
		fi
	else
		echo "ROOD extern na '$1': $(echo "$out" | tail -1)"
	fi
}

# Wachten tot alle markers er zijn en alle toetsen van buiten gedaan,
# iets roods verschijnt, QEMU stopt, of de tijd op is.
need="HOPOS_BOOT|HOPOS_TICK 3|HOPOS_NIC_UP|HOPOS_NET_UP|HOPOS_SYSTEM_UP|HOPOS_SYSTEM_REFUSED"
PROBES=""
next_probe=1
nprobes=$(echo "$PROBE_AT" | awk -F'|' '{print NF}')
elapsed=0
while :; do
	ok=1
	for m in "HOPOS_BOOT" "HOPOS_TICK 3" "HOPOS_NIC_UP" "HOPOS_NET_UP" "HOPOS_SYSTEM_UP" "HOPOS_SYSTEM_REFUSED"; do
		grep -q "$m" "$LOG" || ok=0
	done
	(IFS='|' && for m in $SLOT_MARKS; do grep -q -E "$m" "$LOG" || exit 1; done) || ok=0
	# De toets van buiten, op zijn moment; de keten loopt intussen door.
	if [ "$next_probe" -le "$nprobes" ]; then
		at=$(echo "$PROBE_AT" | cut -d'|' -f"$next_probe")
		if grep -q -E "$at" "$LOG"; then
			PROBES="$PROBES
   $(probe_at "$at")"
			next_probe=$((next_probe + 1))
			continue
		fi
		ok=0
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
[ -n "$PROBES" ] && echo "${PROBES#?}"
case "$PROBES" in
*ROOD*) fail=1 ;;
esac
if [ "$next_probe" -le "$nprobes" ]; then
	echo "   ROOD extern: $((nprobes - next_probe + 1)) van de $nprobes toetsen nooit geprobeerd (moment niet gezien)"
	fail=1
fi
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
[ -n "${KEEP_LOG:-}" ] && tr -d '\r' <"$LOG" >"$KEEP_LOG"
echo "qemu-poort groen"
