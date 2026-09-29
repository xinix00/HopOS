#!/bin/sh
# Bouw HopOS v3 voor de Mac mini M4 (Apple t8132, board/apple): één raw
# bestand dat tegelijk de payload voor m1n1's proxy is én het bootobject voor
# `kmutil configure-boot --raw --entry-point 2048`.
#
#   image/apple-m4.sh                       → target/apple-m4/hopos-apple.img
#   APP=appspike image/apple-m4.sh          + een app-ELF voor de staging
#   APP=hop image/apple-m4.sh               + Hop (../hop/hop, rol hop)
#   image/apple/boot-cycle.sh target/apple-m4/hopos-apple.img [s]
#                                           → via m1n1's proxy laden en starten
#
# Het image is gelinkt op RAM_BASE = DRAM + 4 GB = 0x101_0000_0000
# (hopos/link-apple.ld) en begint met de bootstub (board/apple/src/head.rs):
# offset 0 waar een core uit reset landt (RVBAR), 0x800 waar de firmware de
# boot-core aflevert. Anders dan in Go (mkkernel -apple) vult de linker het
# parameterblok; dit script doet alleen objcopy en de toets die op een stick
# stuk kan gaan zonder dat cargo het ziet: staan de stub, de parameters en
# de kern waar de firmware ze zoekt?
#
# De loader (image/apple/load.py) komt van de Go-meetbank en praat m1n1's
# proxy: M1N1 (de m1n1-clone, standaard ~/Git/m1n1) en een venv met
# pyserial en construct (PYTHON=~/Git/m1n1/venv/bin/python3). Het draaiboek
# en de checklist: docs/boards-apple.md.
set -e

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TARGET=aarch64-unknown-none-softfloat
OUT="$DIR/target/apple-m4"
HOP_DIR="${HOP_DIR:-$DIR/../hop/hop}"
RAM_BASE=0x10100000000

cd "$DIR"
mkdir -p "$OUT"
cargo build --quiet --release --target "$TARGET" -p hopos --features board-apple
OBJCOPY="$(ls "$(rustc --print sysroot)"/lib/rustlib/*/bin/rust-objcopy 2>/dev/null | head -1)"
if [ -z "$OBJCOPY" ]; then
	echo "apple-m4: rust-objcopy missing (rustup component add llvm-tools)" >&2
	exit 1
fi
ELF="$DIR/target/$TARGET/release/hopos"
IMG="$OUT/hopos-apple.img"
"$OBJCOPY" -O binary "$ELF" "$IMG"

# De toets: het parameterblok op 0x100 ("HOPASTUB", doel, grootte, entry)
# moet kloppen met het ELF, en de stub-ingang op 0x800 moet code zijn.
python3 - "$IMG" "$RAM_BASE" <<'PY'
import struct, sys
img = open(sys.argv[1], "rb").read()
base = int(sys.argv[2], 0)
magic, dst, size, entry = struct.unpack_from("<4Q", img, 0x100)
ok = True
def need(cond, what):
    global ok
    if not cond:
        print("apple-m4: " + what, file=sys.stderr)
        ok = False
need(magic == 0x4255545341504f48, "no HOPASTUB magic at 0x100")
need(dst == base, "stub target %#x is not RAM_BASE %#x" % (dst, base))
need(size == len(img), "stub size %#x is not the file size %#x" % (size, len(img)))
need(size % 64 == 0, "stub size %#x is not a multiple of 64" % size)
need(entry == base + 0x10000, "entry %#x is not RAM_BASE + 0x10000" % entry)
need(img[0x800:0x804] != b"\0\0\0\0", "no code at the stub entry 0x800")
need(img[0:4] != b"\0\0\0\0", "no code at the reset stub 0x0")
if not ok:
    sys.exit(1)
print("apple-m4: stub ok: %d bytes, target %#x, entry %#x" % (size, dst, entry), file=sys.stderr)
PY

# Het image voor de staging (optioneel): de loader legt het in de
# loader-regio (board_apple::slots::STAGE_PA) met maat, rol en magic.
APP="${APP-}"
case "$APP" in
"") rm -f "$OUT/stage.elf" "$OUT/stage.role" ;;
hop)
	(cd "$HOP_DIR" && cargo build --quiet --release --target "$TARGET" -p agentd-hopos)
	"$OBJCOPY" --strip-debug "$HOP_DIR/target/$TARGET/release/agentd-hopos" "$OUT/stage.elf"
	echo hop >"$OUT/stage.role"
	;;
*/*)
	"$OBJCOPY" --strip-debug "$APP" "$OUT/stage.elf"
	echo app >"$OUT/stage.role"
	;;
*)
	cargo build --quiet --release --target "$TARGET" -p "$APP"
	"$OBJCOPY" --strip-debug "$DIR/target/$TARGET/release/$APP" "$OUT/stage.elf"
	echo app >"$OUT/stage.role"
	;;
esac

echo "apple-m4: $IMG ($(wc -c <"$IMG" | tr -d ' ') bytes)${APP:+, stage.elf ($(cat "$OUT/stage.role"))}" >&2
echo "apple-m4: load with  STAGE=\${STAGE:-} image/apple/boot-cycle.sh $IMG 90" >&2
