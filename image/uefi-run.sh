#!/bin/sh
# Bouw HopOS als UEFI-app (BOOTAA64.EFI) en boot hem op QEMU -M virt met
# echte EDK2-firmware: de proeftuin voor de Orion O6N en de Ampere Altra
# (zelfde firmware-familie, zelfde weg: FAT-medium, EFI/BOOT/BOOTAA64.EFI,
# PE-stub, ExitBootServices, kmain). Wat hier boot, hoort van een stick te
# booten. De Go-voorganger: OLD/image/uefi-run.sh.
#
#   image/uefi-run.sh                 bouwen en booten op QEMU/EDK2
#   BUILD_ONLY=1 image/uefi-run.sh    alleen de ESP-map bouwen
#   BOARD=o6n|altra image/uefi-run.sh de fysieke borden (feature board-o6n of
#                                     board-altra van hopos): alleen bouwen,
#                                     want QEMU heeft er geen model van
#   APP=appspike image/uefi-run.sh    een app uit deze werkruimte als
#                                     hopos-stage.elf op de ESP, rol app
#   APP=/pad/elf ROLE=app|hop ...     een kant-en-klare ELF
#   CFG=pad image/uefi-run.sh         hopos.cfg (standaard: een minimale)
#   image/uefi-run.sh -s -S           de rest gaat naar QEMU (gdb)
#
# Het resultaat is een ESP-map, target/uefi-esp[-$BOARD]/: EFI/BOOT/
# BOOTAA64.EFI, hopos.cfg en eventueel hopos-stage.elf. Op een stick: een
# FAT32-partitie (GPT of MBR) en die boom erop; Secure Boot uit (een
# ongesigneerde BOOTAA64 weigert DxeImageVerificationLib anders, Go 13-07).
#
# Het image: de kern als PIE op basis 0 (hopos/efi.ld, RUSTFLAGS
# relocation-model=pie), met de PE-header in de eerste pagina
# (board/uefi/src/entry.rs), plat gemaakt met rust-objcopy. Vóór het
# verpakken toetst dit script dat elke relocatie een R_AARCH64_RELATIVE is
# en in de RW-sectie valt: EDK2 mapt de code read-only, en een relocatie
# daar zou de stub bij zijn eerste store laten vallen (Go-les 13-07). Faalt
# die toets, dan is een toolchain-aanname gebroken: luid, nooit een stille
# kapotte stick.
#
# QEMU: pflash 0 = edk2-aarch64-code.fd uit $QEMU_SHARE (readonly), pflash 1
# = een verse varstore van 64 MB nullen (een oude verwijst naar een andere
# topologie en EDK2 valt dan in de Shell, Go 13-07). De ESP hangt als
# usb-storage achter qemu-xhci (de semantiek van de stick; en zo is de
# enige virtio-blk-pci de data-schijf van hopfs), met bootindex=0 zodat de
# BDS niet eerst PXE probeert op de virtio-net.
set -e

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TARGET=aarch64-unknown-none-softfloat
QEMU_SHARE="${QEMU_SHARE:-/opt/homebrew/share/qemu}"
BOARD="${BOARD:-uefi}"
SMP="${SMP:-4}"
MEM="${MEM:-3G}"
CPU="${CPU:-cortex-a57}"
SYSPORT="${SYSPORT:-10100}"
DISK="${DISK:-$DIR/target/hopos-uefi-disk.img}"
DISK_MIB="${DISK_MIB:-64}"

case "$BOARD" in
uefi) FEATURE=board-uefi ;;
o6n) FEATURE=board-o6n BUILD_ONLY=1 ;;
altra) FEATURE=board-altra BUILD_ONLY=1 ;;
*)
	echo "uefi-run: BOARD=$BOARD onbekend (uefi|o6n|altra)" >&2
	exit 64
	;;
esac
if ! grep -q "^$FEATURE *=" "$DIR/hopos/Cargo.toml"; then
	echo "uefi-run: hopos kent de feature $FEATURE nog niet (het board-crate ontbreekt)" >&2
	exit 1
fi

ESP="${ESP:-$DIR/target/uefi-esp$([ "$BOARD" = uefi ] || echo "-$BOARD")}"
TDIR="$DIR/target/uefi"
OBJCOPY="$(ls "$(rustc --print sysroot)"/lib/rustlib/*/bin/rust-objcopy 2>/dev/null | head -1)"
[ -n "$OBJCOPY" ] || {
	echo "uefi-run: rust-objcopy ontbreekt in de toolchain" >&2
	exit 1
}

cd "$DIR"
# Een eigen target-map: andere RUSTFLAGS dan de virt-build, en zo gooien de
# twee elkaars cache niet weg.
RUSTFLAGS="-C relocation-model=pie" cargo build --quiet --release --target "$TARGET" \
	-p hopos --features "$FEATURE" --target-dir "$TDIR"
ELF="$TDIR/$TARGET/release/hopos"

mkdir -p "$ESP/EFI/BOOT"
EFI="$ESP/EFI/BOOT/BOOTAA64.EFI"
"$OBJCOPY" -O binary "$ELF" "$EFI.raw"
python3 - "$ELF" "$EFI.raw" "$EFI" <<'PY'
import struct, sys
elf, raw, out = sys.argv[1], sys.argv[2], sys.argv[3]
b = open(elf, "rb").read()
shoff, = struct.unpack_from("<Q", b, 0x28)
shentsize, shnum, shstrndx = struct.unpack_from("<HHH", b, 0x3a)
secs = []
for i in range(shnum):
    name, typ, flags, addr, off, size = struct.unpack_from("<IIQQQQ", b, shoff + i * shentsize)
    secs.append((name, typ, flags, addr, off, size))
strtab = secs[shstrndx]
def nm(s):
    o = strtab[4] + s[0]
    return b[o:b.index(b"\0", o)].decode()
ALLOC, WRITE = 2, 1
data_start = min(s[3] for s in secs if s[2] & ALLOC and s[2] & WRITE)
bad = 0
n = 0
for s in secs:
    if s[1] != 4:  # SHT_RELA
        continue
    for k in range(s[5] // 24):
        r_off, r_info, _ = struct.unpack_from("<QQq", b, s[4] + k * 24)
        n += 1
        if r_info & 0xffffffff != 1027 or r_off < data_start:
            bad += 1
            if bad <= 5:
                print(f"uefi-run: relocation {k} in {nm(s)}: type {r_info & 0xffffffff} at {r_off:#x} (data starts at {data_start:#x})", file=sys.stderr)
if bad:
    print(f"uefi-run: {bad} of {n} relocations are not RELATIVE in the RW section, refusing the image", file=sys.stderr)
    sys.exit(1)
img = open(raw, "rb").read()
if img[:2] != b"MZ" or img[0x40:0x44] != b"PE\0\0":
    print("uefi-run: no MZ/PE header at the start of the image", file=sys.stderr)
    sys.exit(1)
# De PE-header noemt .data met SizeOfRawData tot aan een paginagrens: vul aan.
img += b"\0" * (-len(img) % 4096)
open(out, "wb").write(img)
print(f"uefi-run: {out} ({len(img)} bytes, {n} relocations, data at {data_start:#x})", file=sys.stderr)
PY
rm -f "$EFI.raw"

# De config: standaard een minimale (geen sleutels), of CFG.
if [ -n "${CFG:-}" ]; then
	cp "$CFG" "$ESP/hopos.cfg"
elif [ ! -e "$ESP/hopos.cfg" ]; then
	printf '# HopOS node config (hopos.cfg op de ESP-root)\n' >"$ESP/hopos.cfg"
fi

# Het gestagede image (de UEFI-tegenhanger van QEMU's -device loader).
APP="${APP-}"
rm -f "$ESP/hopos-stage.elf"
if [ -n "$APP" ]; then
	case "$APP" in
	*/*) IMAGE="$APP" ;;
	*)
		cargo build --quiet --release --target "$TARGET" -p "$APP"
		IMAGE="$DIR/target/$TARGET/release/$APP"
		;;
	esac
	"$OBJCOPY" --strip-debug "$IMAGE" "$ESP/hopos-stage.elf"
	ROLE="${ROLE:-app}"
	grep -v '^hopos.stage=' "$ESP/hopos.cfg" >"$ESP/hopos.cfg.new" || true
	echo "hopos.stage=$ROLE" >>"$ESP/hopos.cfg.new"
	mv "$ESP/hopos.cfg.new" "$ESP/hopos.cfg"
fi
echo "uefi-run: ESP in $ESP" >&2
[ -n "${BUILD_ONLY:-}" ] && exit 0

for f in "$QEMU_SHARE/edk2-aarch64-code.fd"; do
	[ -e "$f" ] || {
		echo "uefi-run: $f ontbreekt (QEMU_SHARE)" >&2
		exit 1
	}
done
VARS="${VARS:-$DIR/target/uefi-vars.fd}"
dd if=/dev/zero of="$VARS" bs=1048576 count=64 2>/dev/null
if [ ! -e "$DISK" ]; then
	dd if=/dev/zero of="$DISK" bs=1048576 count=0 seek="$DISK_MIB" 2>/dev/null
fi

exec qemu-system-aarch64 -M virt,gic-version=3,virtualization=on \
	-cpu "$CPU" -smp "$SMP" -m "$MEM" \
	-nographic -monitor none -serial stdio \
	-drive "if=pflash,format=raw,readonly=on,file=$QEMU_SHARE/edk2-aarch64-code.fd" \
	-drive "if=pflash,format=raw,file=$VARS" \
	-device qemu-xhci \
	-drive "file=fat:$ESP,format=raw,if=none,id=esp,readonly=on" \
	-device usb-storage,drive=esp,bootindex=0 \
	-device virtio-net-pci,netdev=n0,romfile= \
	-netdev "user,id=n0,hostfwd=tcp:127.0.0.1:${SYSPORT}-:10100" \
	-drive "if=none,format=raw,file=$DISK,id=disk0" \
	-device virtio-blk-pci,drive=disk0 \
	"$@"
