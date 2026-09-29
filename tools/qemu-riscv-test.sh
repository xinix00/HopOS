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
#   HOPOS_RV_CAGE_UP  de kooi op hart 1 (board/qemuvirt-riscv/src/cage.rs):
#                   de M-mode-switcher parkeert op zijn CLINT, de kick
#                   (msip) wekt hem, een bewoner boot koud in S-mode achter
#                   PMP (TOR) plus Sv39 (LINK_BASE naar de pool), yieldt, wordt
#                   hervat op mepc+4 en exit; een tweede bewoner die naar de
#                   kern schrijft, faalt op de whitelist (mcause 7) en wordt
#                   dood gemeld op zijn control-page.
#
# Met APP=appspike legt QEMU ook appspike neer op de staging
# (board/qemuvirt-riscv/src/slots.rs), en eist de poort daarbovenop
# HOPOS_APPSPIKE_DONE en HOPOS_SLOT_DONE: de keten door de lifecycle van de
# kern. Die is nog rood (docs/boards-riscv.md, "Niet gedaan").
#
# Rood is rood: een ontbrekende marker, een HOPOS_PANIC of een
# HOPOS_EXCEPTION drukt de hele console af en faalt.
#
#   tools/qemu-riscv-test.sh            TIMEOUT=40 standaard, in seconden
#   APP=appspike tools/qemu-riscv-test.sh   plus de keten door de lifecycle
#   KEEP_LOG=pad tools/qemu-riscv-test.sh   bewaart ook een groene console
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TIMEOUT="${TIMEOUT:-40}"
TARGET=riscv64gc-unknown-none-elf
APP="${APP-}"
LOG="$(mktemp -t hopos-rv.XXXXXX)"
DISK="$(mktemp -t hopos-rvdisk.XXXXXX)"
STAGE="$(mktemp -t hopos-rvstage.XXXXXX)"
trap 'rm -f "$LOG" "$DISK" "$STAGE"; [ -n "${QPID:-}" ] && kill "$QPID" 2>/dev/null; true' EXIT INT TERM

cd "$DIR"
cargo build --quiet --release --target "$TARGET" -p hopos --features board-qemuvirt-riscv
KERNEL="$DIR/target/$TARGET/release/hopos"
truncate -s 64m "$DISK"

# De staging: het image rauw op STAGE_PA, zijn maat op STAGE_HDR_PA en zijn
# rol (0 = app) op STAGE_ROLE_PA (board/qemuvirt-riscv/src/slots.rs).
STAGE_HDR=0xA8100000
STAGE_PA=0xA8200000
STAGE_MAX=14680064
LOADERS=""
WANT_CAGE=0
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
	WANT_CAGE=1
fi

# shellcheck disable=SC2086
qemu-system-riscv64 -M virt -m 1G -smp 2 -bios none -nographic \
	-kernel "$KERNEL" \
	-global virtio-mmio.force-legacy=false \
	-netdev user,id=n0 -device virtio-net-device,netdev=n0 \
	-drive file="$DISK",if=none,format=raw,id=d0 -device virtio-blk-device,drive=d0 \
	$LOADERS >"$LOG" 2>&1 &
QPID=$!

need="HOPOS_BOOT HOPOS_TICK_3 HOPOS_NIC_UP HOPOS_DISK_UP HOPOS_FS_UP HOPOS_NET_UP HOPOS_RV_CAGE_UP"
if [ "$WANT_CAGE" = 1 ]; then
	need="$need HOPOS_APPSPIKE_DONE HOPOS_SLOT_DONE"
fi

has() {
	case "$1" in
	HOPOS_TICK_3) grep -q "HOPOS_TICK 3 " "$LOG" ;;
	*) grep -q "$1" "$LOG" ;;
	esac
}

t=0
while [ "$t" -lt "$TIMEOUT" ]; do
	sleep 1
	t=$((t + 1))
	if grep -q "HOPOS_PANIC\|HOPOS_EXCEPTION" "$LOG"; then
		break
	fi
	missing=""
	for m in $need; do
		has "$m" || missing="$missing $m"
	done
	[ -z "$missing" ] && break
done
kill "$QPID" 2>/dev/null || true
wait "$QPID" 2>/dev/null || true
QPID=""

missing=""
for m in $need; do
	has "$m" || missing="$missing $m"
done
if grep -q "HOPOS_PANIC\|HOPOS_EXCEPTION" "$LOG" || [ -n "$missing" ]; then
	cat "$LOG"
	echo
	echo "FAIL: qemu riscv64: missing:${missing:- none}$(grep -q 'HOPOS_PANIC\|HOPOS_EXCEPTION' "$LOG" && echo ', panic or exception')"
	exit 1
fi
[ -n "${KEEP_LOG:-}" ] && cp "$LOG" "$KEEP_LOG"
grep -E "HOPOS_(BOOT|TICK 3 |NIC_UP|DISK_UP|FS_UP|NET_UP|RV_CAGE_UP|APPSPIKE_DONE|SLOT_DONE)" "$LOG" | tr -d '\r'
echo "qemu riscv64 groen in ${t} s"
