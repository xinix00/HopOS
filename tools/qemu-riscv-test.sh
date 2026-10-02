#!/bin/sh
# De QEMU-poort van HopOS v3 op riscv64 (docs/boards-riscv.md): de kern in
# machine mode op -M virt met -bios none (HopOS is zelf de monitor, zoals
# op de LicheeRV), twee harts, 1 GB, een virtio-net met user-net en een
# verse virtio-blk-schijf. Slaagt alleen als de markers er staan:
#
#   HOPOS_BOOT      de kern haalde kmain, de console en de executor;
#   HOPOS_TICK 3    drie seconden executor, de CLINT-slaap en de IRQ-deur;
#   HOPOS_NIC_UP    de virtio-net over mmio;
#   HOPOS_DISK_UP   de virtio-blk over mmio;
#   HOPOS_FS_UP     hopfs erop, vers;
#   HOPOS_NET_UP    pomp, switch, poort 0 en een DHCP-lease van user-net;
#   HOPOS_RV_CAGE_UP  de zelftest van de kooi op hart 1
#                   (board/qemuvirt-riscv/src/cage.rs): de M-mode-switcher
#                   parkeert op zijn CLINT, de kick (msip) wekt hem, een
#                   bewoner boot koud in S-mode achter PMP (TOR) plus Sv39
#                   (LINK_BASE naar de pool), yieldt, wordt hervat op mepc+4
#                   en exit; een tweede die naar de kern schrijft faalt op de
#                   whitelist (mcause 7); een derde die nooit yieldt, velt de
#                   kill-tick na zijn intrekking; een vierde die slaapt tot
#                   nooit, gaat dood bij de ronde na zijn intrekking;
#   HOPOS_RV_HART_UP  de kooi-lijm (hopos/src/cage_riscv.rs) zette hart 1
#                   klaar: sched-blok, wekker, bel, slaap en kill-tick;
#   HOPOS_SYSTEM_UP de system-listener op poort 10100;
#   extern          een TCP-verbinding van de host via hostfwd naar
#                   10.0.2.15:10100 die de kern accepteert en weigert (geen
#                   slot achter 10.0.2.2): de host ziet EOF en de kern meldt
#                   een NIEUWE regel HOPOS_SYSTEM_REFUSED. Drie keer, op de
#                   momenten van tools/qemu-test.sh: meteen na
#                   HOPOS_SYSTEM_UP, zodra de app van slot 1 zijn netwerk
#                   heeft, en na de stop van slot 2;
#   appspike        het ABI-bewijs op riscv64 (standaard aan; APP= slaat het
#                   over): appspike, door QEMU gestaged, in slot 1 op hart 1
#                   door de lifecycle van de kern (PMP plus Sv39, de
#                   M-mode-switcher), al zijn toetsen groen via de servicer op
#                   de console (HOPOS_APPSPIKE_DONE pass=9 fail=0: control-page,
#                   env, log, frame, klok, heartbeat, net met de system-API,
#                   hopfs en heap), exit 0 (HOPOS_SLOT_DONE) en de bevestigde
#                   stop (HOPOS_SLOT_STOPPED); daarna hetzelfde in slot 2 op
#                   hetzelfde hart, nu warm (de switcher draait al).
#
# Rood is rood: een ontbrekende marker, een HOPOS_PANIC of een
# HOPOS_EXCEPTION drukt de hele console af en faalt.
#
#   tools/qemu-riscv-test.sh            TIMEOUT=40 standaard, in seconden
#   APP= tools/qemu-riscv-test.sh       alleen de boot-poort (geen slots)
#   KEEP_LOG=pad tools/qemu-riscv-test.sh   bewaart ook een groene console
#   SYSPORT=poort                       de host-kant van de hostfwd; bezet =
#                                       een vrije poort van het OS
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TIMEOUT="${TIMEOUT:-40}"
TARGET=riscv64gc-unknown-none-elf
APP="${APP-appspike}"
LOG="$(mktemp -t hopos-rv.XXXXXX)"
DISK="$(mktemp -t hopos-rvdisk.XXXXXX)"
STAGE="$(mktemp -t hopos-rvstage.XXXXXX)"
trap 'rm -f "$LOG" "$DISK" "$STAGE"; [ -n "${QPID:-}" ] && kill "$QPID" 2>/dev/null; true' EXIT INT TERM

# Een host-poort: de gevraagde als hij vrij is, anders een vrije van het
# OS (zoals tools/qemu-test.sh), zodat de toets naast een andere QEMU draait.
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
SYSPORT="$(port "${SYSPORT:-10100}" SYSPORT)"

cd "$DIR"
cargo build --quiet --release --target "$TARGET" -p hopos --features board-qemuvirt-riscv
KERNEL="$DIR/target/$TARGET/release/hopos"
truncate -s 64m "$DISK"

# De kern gebruikt geen f-registers: daarop rust de overgang naar een
# bewoner op de OS-core (cpu/src/riscv/oscore.rs), die de f-registers van de
# kern niet bewaart. De uitzonderingen zijn de switcher van de app-harts
# (__hopos_parkenter tot __hopos_mmode_end, cpu/src/riscv/switch.rs) en die
# overgang zelf (__hopos_os_enter tot __hopos_os_end): die bewaren juist de
# f-registers van hun bewoners, met de hand geschreven, en tellen hier niet
# mee. Met een riscv-objdump in het PATH wordt dat hier getoetst.
if command -v riscv64-elf-objdump >/dev/null 2>&1; then
	SW_LO=$(riscv64-elf-nm "$KERNEL" | awk '$3 == "__hopos_parkenter" { print $1 }')
	SW_HI=$(riscv64-elf-nm "$KERNEL" | awk '$3 == "__hopos_mmode_end" { print $1 }')
	OS_LO=$(riscv64-elf-nm "$KERNEL" | awk '$3 == "__hopos_os_enter" { print $1 }')
	OS_HI=$(riscv64-elf-nm "$KERNEL" | awk '$3 == "__hopos_os_end" { print $1 }')
	# Veld 3 van objdump is het mnemonic (adres, hex, mnemonic, operanden):
	# zo telt een hexwoord als `fadde7e3` niet mee. Alles min de switcher en
	# de overgang.
	fp_count() {
		riscv64-elf-objdump -d "$@" "$KERNEL" | awk -F'\t' '$3 ~ /^(fl[dw]|fs[dw]|fmv\.|fcvt|fadd|fsub|fmul|fdiv|fsqrt|fsgnj|fmin|fmax|fmadd|fmsub|fnm|feq|flt|fle|fclass|fr?csr|fs?csr|frflags|fsflags)/ { n++ } END { print n + 0 }'
	}
	FP_ALL=$(fp_count)
	FP_SW=$(fp_count --start-address="0x${SW_LO:-0}" --stop-address="0x${SW_HI:-0}")
	FP_OS=$(fp_count --start-address="0x${OS_LO:-0}" --stop-address="0x${OS_HI:-0}")
	FP=$((FP_ALL - FP_SW - FP_OS))
	if [ "$FP" != 0 ]; then
		echo "FAIL: the kern image carries $FP FP instructions outside the switcher and the OS-core switch, which do not save the kern's own f0..f31 (cpu/src/riscv/oscore.rs)"
		exit 1
	fi
fi

# De staging: het image rauw op STAGE_PA, zijn maat op STAGE_HDR_PA en zijn
# rol (0 = app) op STAGE_ROLE_PA (board/qemuvirt-riscv/src/slots.rs).
STAGE_HDR=0xA8100000
STAGE_PA=0xA8200000
STAGE_MAX=14680064
LOADERS=""
if [ -n "$APP" ]; then
	cargo build --quiet --release --target "$TARGET" -p "$APP"
	ELF="$DIR/target/$TARGET/release/$APP"
	OBJCOPY="$(ls "$(rustc --print sysroot)"/lib/rustlib/*/bin/rust-objcopy 2>/dev/null | head -1)"
	if [ -n "$OBJCOPY" ]; then
		"$OBJCOPY" --strip-debug "$ELF" "$STAGE"
	else
		cp "$ELF" "$STAGE"
	fi
	SIZE=$(wc -c <"$STAGE" | tr -d ' ')
	if [ "$SIZE" -gt "$STAGE_MAX" ]; then
		echo "FAIL: $APP is $SIZE bytes, the staging holds $STAGE_MAX"
		exit 1
	fi
	LOADERS="-device loader,file=$STAGE,addr=$STAGE_PA,force-raw=on -device loader,addr=$STAGE_HDR,data=$SIZE,data-len=8 -device loader,addr=$((STAGE_HDR + 8)),data=0,data-len=8"
fi

# shellcheck disable=SC2086
qemu-system-riscv64 -M virt -m 1G -smp 2 -bios none -nographic \
	-kernel "$KERNEL" \
	-global virtio-mmio.force-legacy=false \
	-netdev "user,id=n0,hostfwd=tcp:127.0.0.1:${SYSPORT}-:10100" \
	-device virtio-net-device,netdev=n0 \
	-drive file="$DISK",if=none,format=raw,id=d0 -device virtio-blk-device,drive=d0 \
	$LOADERS </dev/null >"$LOG" 2>&1 &
QPID=$!

# De vaste markers, en die van de appspike-keten (grep -E, per slot).
NEED="HOPOS_BOOT|HOPOS_TICK 3 |HOPOS_NIC_UP|HOPOS_DISK_UP|HOPOS_FS_UP fresh=1|HOPOS_NET_UP|HOPOS_RV_CAGE_UP|HOPOS_SYSTEM_UP"
PROBE_AT=""
if [ -n "$APP" ]; then
	NEED="$NEED|HOPOS_RV_HART_UP|HOPOS_SYSTEM_REFUSED"
	for i in 1 2; do
		NEED="$NEED|HOPOS_SLOT_START slot=$i|slot $i: HOPOS_APPSPIKE_NETLOG|slot $i: HOPOS_APPSPIKE_FS ok|slot $i: HOPOS_APPSPIKE_DONE pass=9 fail=0|HOPOS_SLOT_DONE slot=$i exit=0|slot $i: stopped.*HOPOS_SLOT_STOPPED"
	done
	PROBE_AT="HOPOS_SYSTEM_UP|slot 1: .*HOPOS_APPNET_UP|slot 2: stopped.*HOPOS_SLOT_STOPPED"
fi

# De toets van buiten: verbinden via de hostfwd en wachten tot de kern de
# verbinding sluit (EOF), en dan een nieuwe weigeringsregel van de kern.
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

all_there() {
	(IFS='|' && for m in $NEED; do grep -q -E "$m" "$LOG" || exit 1; done)
}

PROBES=""
next_probe=1
nprobes=0
[ -n "$PROBE_AT" ] && nprobes=$(echo "$PROBE_AT" | awk -F'|' '{print NF}')
elapsed=0
while :; do
	if [ "$next_probe" -le "$nprobes" ]; then
		at=$(echo "$PROBE_AT" | cut -d'|' -f"$next_probe")
		if grep -q -E "$at" "$LOG"; then
			PROBES="$PROBES
   $(probe_at "$at")"
			next_probe=$((next_probe + 1))
			continue
		fi
	fi
	[ "$next_probe" -gt "$nprobes" ] && all_there && break
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
IFS_WAS="$IFS"
IFS='|'
for m in $NEED; do
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
	KEEP="${LOG}.rood"
	cp "$LOG" "$KEEP"
	echo "== console bewaard in $KEEP"
	tr -d '\r' <"$LOG"
	echo
	echo "FAIL: qemu riscv64"
	exit 1
fi
[ -n "${KEEP_LOG:-}" ] && tr -d '\r' <"$LOG" >"$KEEP_LOG"
echo "qemu riscv64 groen in $((elapsed / 10)) s"
