#!/bin/sh
# De UEFI-poort van HopOS v3: dezelfde toets als tools/qemu-test.sh, maar
# over de weg van de O6N en de Altra. De kern gaat als BOOTAA64.EFI op een
# ESP (image/uefi-run.sh), QEMU -M virt boot hem met echte EDK2-firmware
# uit $QEMU_SHARE, en alles wat de kern vindt, vindt hij zoals op ijzer:
# de cores, de GIC en de console uit ACPI (MADT, SPCR), de PCIe uit de
# MCFG, en de NIC en de schijf als virtio-net-pci en virtio-blk-pci. Het
# app-image (appspike) staat als hopos-stage.elf op de ESP, met
# `hopos.stage=app` in hopos.cfg.
#
# De markers zijn die van tools/qemu-test.sh (HOPOS_BOOT, HOPOS_TICK 3,
# HOPOS_NIC_UP, HOPOS_NET_UP, HOPOS_SYSTEM_UP, HOPOS_SYSTEM_REFUSED en de
# hele appspike-keten in slot 1 en 2), plus de UEFI-eigen:
#
#   HOPOS_UEFI_STUB  de PE-stub draait onder de firmware (ConOut);
#   HOPOS_CFG        hopos.cfg kwam van de ESP.
#
#   tools/qemu-uefi-test.sh          TIMEOUT=60 standaard, in seconden
#   KEEP_LOG=pad tools/qemu-uefi-test.sh   bewaart ook een groene console
#   SYSPORT=poort                    de host-kant van de hostfwd
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TIMEOUT="${TIMEOUT:-60}"
TARGET=aarch64-unknown-none-softfloat
LOG="$(mktemp -t hopos-uefi.XXXXXX)"
DISK="$(mktemp -t hopos-disk.XXXXXX)"
VARS="$(mktemp -t hopos-vars.XXXXXX)"
ESP="$(mktemp -d -t hopos-esp.XXXXXX)"
trap 'rm -rf "$LOG" "$DISK" "$VARS" "$ESP"; [ -n "${QPID:-}" ] && kill "$QPID" 2>/dev/null; true' EXIT INT TERM

. "$(dirname "$0")/lib.sh"
SYSPORT="$(port "${SYSPORT:-10100}" SYSPORT)" # de host-kant van de hostfwd naar de system-API
# Een verse, ijle schijf van 64 MiB: hopfs begint leeg.
dd if=/dev/zero of="$DISK" bs=1048576 count=0 seek=64 2>/dev/null

cd "$DIR"
echo "== bouwen: BOOTAA64.EFI (board-uefi) en appspike op de ESP"
BUILD_ONLY=1 APP=appspike ESP="$ESP" sh image/uefi-run.sh 2>&1 | sed 's/^/   /'
[ -e "$ESP/EFI/BOOT/BOOTAA64.EFI" ] || {
	echo "ROOD: geen BOOTAA64.EFI"
	exit 1
}
# De markers van het ABI-bewijs (grep -E): het aantal toetsen groeit met
# appspike, dus "alles groen" is fail=0.
SLOT_MARKS="HOPOS_DISK_UP model=virtio-blk blocks=131072|HOPOS_FS_UP fresh=1"
SLOT_MARKS="$SLOT_MARKS|HOPOS_SLOT_START slot=1|slot 1: HOPOS_APPSPIKE_NETLOG|slot 1: HOPOS_APPSPIKE_FS ok|slot 1: HOPOS_APPSPIKE_DONE pass=9 fail=0|HOPOS_SLOT_DONE slot=1 exit=0|slot 1: stopped.*HOPOS_SLOT_STOPPED"
SLOT_MARKS="$SLOT_MARKS|HOPOS_SLOT_START slot=2|slot 2: HOPOS_APPSPIKE_NETLOG|slot 2: HOPOS_APPSPIKE_FS ok|slot 2: HOPOS_APPSPIKE_DONE pass=9 fail=0|HOPOS_SLOT_DONE slot=2 exit=0|slot 2: stopped.*HOPOS_SLOT_STOPPED"
# De momenten van de toets van buiten (grep -E), in volgorde.
PROBE_AT="HOPOS_SYSTEM_UP|slot 1: .*HOPOS_APPNET_UP|slot 2: stopped.*HOPOS_SLOT_STOPPED"

echo "== booten op QEMU virt met EDK2 (tot ${TIMEOUT}s)"
APP=appspike ESP="$ESP" DISK="$DISK" VARS="$VARS" SYSPORT="$SYSPORT" \
	sh image/uefi-run.sh </dev/null >"$LOG" 2>&1 &
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
	for m in "HOPOS_UEFI_STUB" "HOPOS_CFG" "HOPOS_BOOT" "HOPOS_TICK 3" "HOPOS_NIC_UP" "HOPOS_NET_UP" "HOPOS_SYSTEM_UP" "HOPOS_SYSTEM_REFUSED"; do
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
for m in "HOPOS_UEFI_STUB" "HOPOS_CFG" "HOPOS_BOOT" "HOPOS_TICK 3" "HOPOS_NIC_UP" "HOPOS_NET_UP" "HOPOS_SYSTEM_UP" "HOPOS_SYSTEM_REFUSED"; do
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
echo "uefi-poort groen"
