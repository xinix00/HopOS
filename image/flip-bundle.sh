#!/bin/sh
# Bouwt de KERN-FLIP-BUNDEL van één board: het artifact waarmee een
# draaiende HopOS zijn kern vervangt zonder herstart, terwijl de apps (en
# Hop) doordraaien (OLD/image/flip-bundle.sh, OLD/docs/kernel-flip.md).
#
#   image/flip-bundle.sh virt       -> target/hopos-virt.flip (+ .sha256)
#   HOPOS_STAMP=B image/flip-bundle.sh virt   een ander versie-stempel op
#                                   de boot-regel (tools/qemu-test-flip.sh)
#
# Een bundel is de kern-ELF (zonder debug-info, mét symbolen) plus een
# HOPRELO1-staart met de relocatietabel (kern::kernflip::Bundle). Die tabel
# komt uit een DIFF: dezelfde build op twee linkadressen, waarbij elk
# verschillend 8-byte-woord exact de basis-delta moet dragen; elk ander
# verschil laat de bouw hard falen. Daarom linkt dit script de kern twee
# keer (HOPOS_LINK_BASE, hopos/build.rs).
#
# De ELF in de bundel is die op het SCHADUWadres: de draaiende kern legt hem
# plat neer en relokeert hem naar zijn eigen koude adres. Dat maakt de
# relocatie op elke flip echt werk (geen delta nul die een kapotte tabel
# verbergt), en dit script bewijst vooraf dat het resultaat byte voor byte
# de koude build is.
#
# De node krijgt hem op aanvraag, via de agent-API van Hop, achter dezelfde
# HMAC als een jobspec: POST /flip {"url","sha256"} met de som die dit
# script print. Die som is het vertrouwensanker.
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)"
BOARD="${1:-}"
TARGET=aarch64-unknown-none-softfloat

# Per board: feature, het koude linkadres (link.ld) en het schaduwadres.
# Het schaduwadres doet niets in het eindproduct, het is diff-bewijs, maar
# moet dezelfde uitlijning hebben (2 MB: de ADRP-paginaoffsets blijven
# gelijk) en ver genoeg weg liggen voor een echte delta.
case "$BOARD" in
virt) FEATURE=board-qemuvirt; COLD=0x40200000; SHADOW=0x60200000 ;;
*)
	echo "gebruik: $0 virt" >&2
	exit 64
	;;
esac

OUT="$DIR/target/hopos-$BOARD.flip"
# Een eigen target-map: de gewone build (image/qemu-run.sh) blijft staan.
TD="$DIR/target/flip-$BOARD"
STAMP="${HOPOS_STAMP:-dev}"
build() {
	(cd "$DIR" && CARGO_TARGET_DIR="$TD" HOPOS_LINK_BASE="$1" HOPOS_STAMP="$STAMP" \
		cargo build --quiet --release --target "$TARGET" -p hopos --features "$FEATURE")
	cp "$TD/$TARGET/release/hopos" "$2"
}
build "$SHADOW" "$TD/flip-shadow.elf"
build "$COLD" "$TD/flip-cold.elf"

OBJCOPY="$(ls "$(rustc --print sysroot)"/lib/rustlib/*/bin/rust-objcopy 2>/dev/null | head -1)"
if [ -n "$OBJCOPY" ]; then
	"$OBJCOPY" --strip-debug "$TD/flip-shadow.elf" "$TD/flip-shadow.stripped"
else
	cp "$TD/flip-shadow.elf" "$TD/flip-shadow.stripped"
fi

python3 - "$TD/flip-shadow.elf" "$TD/flip-cold.elf" "$TD/flip-shadow.stripped" "$OUT" "$SHADOW" "$COLD" <<'PY'
import hashlib, struct, sys

shadow_path, cold_path, stripped_path, out_path = sys.argv[1:5]
shadow_base, cold_base = int(sys.argv[5], 16), int(sys.argv[6], 16)
MAGIC = 0x314F4C4552504F48  # "HOPRELO1"
FLIP_ABI = 3                # kern::kernflip::FLIP_ABI
M64 = (1 << 64) - 1

def loads(elf):
    if elf[:4] != b"\x7fELF" or elf[4] != 2 or elf[5] != 1:
        sys.exit("flip-bundle: not a little-endian ELF64")
    entry, phoff = struct.unpack_from("<QQ", elf, 24)
    phentsize, phnum = struct.unpack_from("<HH", elf, 54)
    segs = []
    for i in range(phnum):
        p = phoff + i * phentsize
        kind, _flags, off, _vaddr, paddr, filesz, memsz = struct.unpack_from("<IIQQQQQ", elf, p)
        if kind == 1:
            segs.append((paddr, off, filesz, memsz))
    return entry, segs

def flat(elf):
    entry, segs = loads(elf)
    base = min(s[0] for s in segs)
    end = max(s[0] + s[3] for s in segs)
    size = (end - base + 7) & ~7
    img = bytearray(size)
    for paddr, off, filesz, _memsz in segs:
        img[paddr - base:paddr - base + filesz] = elf[off:off + filesz]
    return base, entry, img

s_elf = open(shadow_path, "rb").read()
c_elf = open(cold_path, "rb").read()
s_base, s_entry, s_img = flat(s_elf)
c_base, c_entry, c_img = flat(c_elf)
if (s_base, c_base) != (shadow_base, cold_base):
    sys.exit(f"flip-bundle: link bases {s_base:#x}/{c_base:#x}, expected {shadow_base:#x}/{cold_base:#x}")
if len(s_img) != len(c_img):
    sys.exit(f"flip-bundle: the two links differ in size ({len(s_img)} vs {len(c_img)})")
delta = (c_base - s_base) & M64
if (c_entry - s_entry) & M64 != delta:
    sys.exit("flip-bundle: the entries do not differ by the base delta")
relocs = []
for off in range(0, len(s_img), 8):
    a = struct.unpack_from("<Q", s_img, off)[0]
    b = struct.unpack_from("<Q", c_img, off)[0]
    if a == b:
        continue
    if (b - a) & M64 != delta:
        sys.exit(f"flip-bundle: word at +{off:#x} differs by {(b - a) & M64:#x}, not by the base delta "
                 f"{delta:#x}: an absolute address that is no 8-byte word (ADRP to a fixed symbol?)")
    relocs.append(off)
# Het bewijs: het gerelokeerde schaduwbeeld IS de koude build.
check = bytearray(s_img)
for off in relocs:
    v = struct.unpack_from("<Q", check, off)[0]
    struct.pack_into("<Q", check, off, (v + delta) & M64)
if check != c_img:
    sys.exit("flip-bundle: relocated image differs from the cold build")

elf = open(stripped_path, "rb").read()
b = bytearray(elf)
while len(b) % 8:
    b.append(0)
head = len(b)
b += struct.pack("<QIIQQQQQ", MAGIC, 1, FLIP_ABI, len(elf), s_base, len(s_img), s_entry, len(relocs))
for off in relocs:
    b += struct.pack("<I", off)
while len(b) % 8:
    b.append(0)
b += struct.pack("<QQ", head, MAGIC)
open(out_path, "wb").write(b)
sha = hashlib.sha256(b).hexdigest()
open(out_path + ".sha256", "w").write(sha + "\n")
print(f"flip-bundle: {len(s_img) // 1024} KiB image linked at {s_base:#x}, {len(relocs)} relocations "
      f"to {c_base:#x} (proven equal to the cold build), bundle {len(b)} bytes", file=sys.stderr)
PY

SHA="$(cat "$OUT.sha256")"
echo "" >&2
echo "$OUT gebouwd (stempel $STAMP), sha256 $SHA" >&2
echo "Zet hem op een webserver en vraag de flip aan op de agent-API van Hop:" >&2
echo "  curl -X POST http://<node>:8080/flip -d '{\"url\":\"http://<ip>:<poort>/$(basename "$OUT")\",\"sha256\":\"$SHA\"}'" >&2
