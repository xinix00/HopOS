#!/bin/sh
# Bouw HopOS v3 voor de Radxa Zero 3E (Rockchip RK3566): de kern, het
# arm64-Image, en een compleet, dd-baar kaart-image. De vorm van
# image/radxa-zero3.sh op tag v2.2.8 (de Go-generatie), zonder Go: de
# ELF-naar-Image-stap (Go's mkkernel) staat hieronder als python3, de kaart
# bouwt tools/mkcard (de port van Go's mkcard).
#
#   image/radxa-zero3.sh                 de kaart met Hop als bewoner
#   APP=appspike image/...               appspike, twee keer door de kern
#                                        geplaatst (de kale app-rol)
#   APP=/pad/naar/elf image/...          een kant-en-klare ELF (rol app)
#   APP= image/...                       zonder image (HOPOS_SLOT_NONE)
#   HOP_DIR=pad image/...                de hop-repo (standaard ../hop/hop)
#   ROLE=hop|app image/...               de rol van het image (hopos.stage)
#   CFG=pad image/...                    de config van de node (standaard
#                                        image/cfg/hop-config-headless.cfg;
#                                        met GUI=1 hoort
#                                        hop-config-headfull.cfg erbij)
#   NODE=radxa-2 image/...               een andere hopos.node in de APPEND
#                                        (ook de bron van het MAC-adres)
#   RADXA_DONOR=pad image/...            een eigen donor-boot-blok (standaard
#                                        image/firmware/radxa/donor-boot.bin)
#   GUI=1 image/...                      de gui-smaak (`--features gui`,
#                                        docs/gui.md): de console op het glas
#                                        via VOP2 en HDMI, de DWC3's voor de
#                                        invoer, en de framebuffer-grant
#
# Uitvoer in target/radxa-zero3/: hopos.elf, hopos.img (het arm64-Image),
# hop.elf (het image van de bewoner, gestript), hopos.cfg, hopos.ird (de
# initrd: hopos.cfg plus hop.elf), extlinux.conf en hopos-radxa-zero3.img
# (de kaart). Na de bouw leest het script hopos.img, extlinux.conf,
# hopos.cfg en hop.elf terug uit het kaart-image en vergelijkt hun sha256.
#
#   diskutil unmountDisk /dev/diskN
#   sudo dd if=target/radxa-zero3/hopos-radxa-zero3.img of=/dev/rdiskN bs=4m
#
# Boot-route: de BootROM leest de U-Boot-keten raw van vaste LBA's vóór de
# eerste partitie (idbloader met TPL/SPL op LBA 64, u-boot.itb met TF-A en
# U-Boot op LBA 16384). Die bytes zijn van Radxa: de donor, eenmalig uit hun
# officiële image gehaald en gehasht (image/firmware/radxa/LEESMIJ.txt). U-Boot's distro-boot vindt daarna
# /extlinux/extlinux.conf op onze FAT-partitie (type 0x0C: macOS mount hem
# na het flashen, dus een nieuwe kern is een cp en de APPEND-regel is
# bewerkbaar), laadt het Image en hopos.ird (als initrd) en `booti` springt
# op EL2 met x0 = DTB. Seriële console: USB-UART op de 40-pins header (pin
# 8 = TX, 10 = RX, 6 = GND), 1500000 8N1.
#
# Hop als bewoner (30-09): de kern heeft geen SD-driver en U-Boot 2023.10
# laadt per label precies één initrd, dus draagt die ene initrd hopos.cfg
# én hop.elf (de container van image/radxa-initrd.py; de kern splitst hem in
# board/rk3566/src/initrd.rs, waar ook staat waarom niet de komma-lijst of
# een ingebakken Hop). De rol staat als hopos.stage in de APPEND.
set -e
DIR="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$DIR/target/radxa-zero3"
TARGET=aarch64-unknown-none-softfloat
HOP_DIR="${HOP_DIR:-$DIR/../hop/hop}"
# De grootste initrd die de kern naar de heap haalt (board_rk3566::INITRD_MAX).
INITRD_MAX=16777216
mkdir -p "$OUT"

# 1. De kern.
echo "== cargo build (board-rk3566)" >&2
# Een eigen target-map: een build voor een ander board in de gedeelde map
# zou anders het ELF onder deze stap vervangen (dezelfde binary-naam).
FEATURE=board-rk3566
if [ "${GUI:-0}" = 1 ]; then
	FEATURE="$FEATURE,gui"
fi
(cd "$DIR" && cargo build --release --target "$TARGET" \
	--target-dir "$OUT/cargo" -p hopos --features "$FEATURE")
ELF="$OUT/cargo/$TARGET/release/hopos"
cp "$ELF" "$OUT/hopos.elf"

# 2. ELF naar arm64-Image (Linux Documentation/arch/arm64/booting.rst): de
#    PT_LOAD-segmenten plat vanaf IMAGE_BASE, met de 64-byte header vooraan.
#    text_offset = IMAGE_BASE - DRAM_BASE: U-Boot VERPLAATST een Image met
#    relocatable = 0 naar bi_dram[0].start + text_offset, ongeacht waar het
#    bestand geladen werd. GEMETEN 05-08: DRAM-start is 0x20_0000; met
#    text_offset = load landde de kern 2 MB te hoog en zweeg hij na
#    "Starting kernel". Moet gelijk zijn aan hopos/link-rk3566.ld.
python3 - "$OUT/hopos.elf" "$OUT/hopos.img" <<'PYEOF'
import struct, sys
IMAGE_BASE, DRAM_BASE = 0x02200000, 0x00200000
elf = open(sys.argv[1], "rb").read()
if elf[:4] != b"\x7fELF" or elf[4] != 2 or struct.unpack_from("<H", elf, 18)[0] != 183:
    sys.exit("hopos.elf is geen aarch64-ELF64")
entry, phoff = struct.unpack_from("<QQ", elf, 24)
phentsize, phnum = struct.unpack_from("<HH", elf, 54)
file_end, mem_end, segs = IMAGE_BASE, IMAGE_BASE, []
for i in range(phnum):
    p_type, _, off, vaddr, paddr, filesz, memsz, _ = struct.unpack_from("<IIQQQQQQ", elf, phoff + i * phentsize)
    if p_type != 1 or memsz == 0:
        continue
    if paddr < IMAGE_BASE + 64:
        sys.exit(f"segment op {paddr:#x} ligt onder IMAGE_BASE+64: geen plaats voor de header")
    segs.append((paddr, elf[off:off + filesz]))
    file_end = max(file_end, paddr + filesz)
    mem_end = max(mem_end, paddr + memsz)
img = bytearray(file_end - IMAGE_BASE)
for paddr, data in segs:
    img[paddr - IMAGE_BASE:paddr - IMAGE_BASE + len(data)] = data
rel = entry - IMAGE_BASE
if rel <= 0 or rel % 4 or rel >= 1 << 27:
    sys.exit(f"entry {entry:#x} is niet te bereiken met een branch vanaf {IMAGE_BASE:#x}")
struct.pack_into("<I", img, 0, 0x14000000 | (rel // 4))  # code0: b _start
struct.pack_into("<I", img, 4, 0)                          # code1
struct.pack_into("<Q", img, 8, IMAGE_BASE - DRAM_BASE)     # text_offset
struct.pack_into("<Q", img, 16, mem_end - IMAGE_BASE)      # image_size (incl. BSS en stack)
struct.pack_into("<Q", img, 24, 0b010)                     # flags: LE, 4K, relocatable = 0
struct.pack_into("<I", img, 56, 0x644D5241)                # magic "ARM\x64"
open(sys.argv[2], "wb").write(img)
print(f"hopos.img: {len(img)} bytes, image_size {mem_end - IMAGE_BASE:#x}, entry {entry:#x} at {IMAGE_BASE:#x}", file=sys.stderr)
PYEOF

# 3. Het image van de bewoner: dezelfde keuzes als image/rpi4.sh. Gestript
#    (Hop 19 MB naar 1,5 MB, 30-09): de kern haalt de hele initrd naar zijn
#    heap, en de symbolen staan in de build van de hop-repo.
OBJCOPY="$(ls "$(rustc --print sysroot)"/lib/rustlib/*/bin/rust-objcopy 2>/dev/null | head -1)"
if [ -z "$OBJCOPY" ]; then
	echo "FOUT: rust-objcopy ontbreekt (rustup component add llvm-tools)" >&2
	exit 1
fi
APP="${APP-hop}"
IMAGE=""
case "$APP" in
"") ;;
hop)
	echo "== cargo build (agentd-hopos in $HOP_DIR)" >&2
	(cd "$HOP_DIR" && cargo build --quiet --release --target "$TARGET" -p agentd-hopos)
	IMAGE="$HOP_DIR/target/$TARGET/release/agentd-hopos"
	ROLE="${ROLE:-hop}"
	;;
*/*)
	IMAGE="$APP"
	ROLE="${ROLE:-app}"
	;;
*)
	echo "== cargo build ($APP)" >&2
	(cd "$DIR" && cargo build --quiet --release --target "$TARGET" -p "$APP")
	IMAGE="$DIR/target/$TARGET/release/$APP"
	ROLE="${ROLE:-app}"
	;;
esac
rm -f "$OUT/hop.elf"
if [ -n "$IMAGE" ]; then
	"$OBJCOPY" --strip-debug "$IMAGE" "$OUT/hop.elf"
else
	# Zonder image de rol app: de kern zoekt dan het image, vindt het niet
	# en zegt HOPOS_SLOT_NONE, zonder token voor een Hop die er niet is.
	ROLE=app
fi

# 4. De config, de initrd en de extlinux-regel. De APPEND-regel is het
#    bootargs-kanaal, INITRD het bestand-kanaal; beide GEMETEN werkend op
#    05-08 (de kern leest ze in board_rk3566::boot_param). Een sleutel in
#    hopos.cfg wint van dezelfde sleutel in de APPEND; de gedeelde config
#    zet geen hopos.node, dus het MAC-adres komt uit die van de APPEND.
CFG="${CFG:-$DIR/image/cfg/hop-config-headless.cfg}"
cp "$CFG" "$OUT/hopos.cfg"
python3 "$DIR/image/radxa-initrd.py" pack "$OUT/hopos.ird" "$OUT/hopos.cfg" \
	${IMAGE:+"$OUT/hop.elf"}
IRD_SIZE=$(wc -c <"$OUT/hopos.ird" | tr -d ' ')
if [ "$IRD_SIZE" -gt "$INITRD_MAX" ]; then
	echo "FOUT: hopos.ird is $IRD_SIZE bytes, de kern haalt hoogstens $INITRD_MAX naar zijn heap" >&2
	exit 1
fi
cat > "$OUT/extlinux.conf" <<EOF
# HopOS v3, Radxa Zero 3E. U-Boot's distro-boot pakt de eerste entry.
# hopos.ird is hopos.cfg plus het image van de bewoner (image/radxa-initrd.py).
timeout 1
default hopos

label hopos
    kernel /hopos.img
    initrd /hopos.ird
    append hopos.node=${NODE:-radxa-1} hopos.stage=$ROLE
EOF

# 5. De donor: de bytes LBA 64..32767 van het officiële Radxa Zero 3-image,
#    release b6, gepind met hash (image/firmware/radxa/LEESMIJ.txt: de
#    herkomst en hoe hij opnieuw te maken is). Dit zijn exact de bytes
#    waarmee alle metingen sinds 05-08 gedaan zijn.
DONOR="${RADXA_DONOR:-$DIR/image/firmware/radxa/donor-boot.bin}"
DONOR_SHA="9a582a0c6fcd8b41d5627284aa9b831a46a08e7606c6fc31901f48725354a7b9"
if [ ! -f "$DONOR" ]; then
	echo "FOUT: de donor ontbreekt: $DONOR (zie image/firmware/radxa/LEESMIJ.txt)" >&2
	exit 1
fi
GOT=$(shasum -a 256 "$DONOR" | cut -d' ' -f1)
if [ "$GOT" != "$DONOR_SHA" ]; then
	echo "FOUT: $DONOR heeft hash $GOT, verwacht $DONOR_SHA" >&2
	echo "  (zie image/firmware/radxa/LEESMIJ.txt, of zet RADXA_DONOR)" >&2
	exit 1
fi

# 6. De kaart (tools/mkcard): onze MBR (één FAT16-partitie, type 0x0C,
#    actief, vanaf LBA 32768 = 16 MiB), de donor raw op LBA 64 (byte 32768),
#    en de drie bestanden in de FAT. De vorm van de Go-kaart (tag v2.2.8),
#    op -cfgwindow na: de config zit hier in hopos.ird.
CARD="$OUT/hopos-radxa-zero3.img"
(cd "$DIR" && cargo run -q -p mkcard -- -o "$CARD" -size 64 -start 32768 \
	-label hopos -vollabel -raw "$DONOR@32768" \
	"$OUT/hopos.img" "$OUT/hopos.ird" "$OUT/extlinux.conf=extlinux/extlinux.conf") >&2

# 7. De proef op de kaart: lees de FAT van het image terug zoals U-Boot hem
#    leest (MBR, BPB, root, de cluster-keten, LFN), splits hopos.ird zoals
#    de kern hem splitst, en vergelijk elk bestand met wat erin ging. Een
#    fout in de kaartbouwer of de container is hier rood, niet pas op de
#    seriële console.
RB="$OUT/readback"
rm -rf "$RB"
mkdir -p "$RB"
python3 - "$CARD" "$RB" <<'PYEOF'
import struct, sys
card, out = sys.argv[1:3]
img = open(card, "rb").read()
if img[510:512] != b"\x55\xAA" or img[446 + 4] != 0x0C:
    sys.exit("readback: no MBR with a FAT partition (type 0x0C)")
start = struct.unpack_from("<I", img, 446 + 8)[0] * 512
bs = img[start:start + 512]
sec, spc, res, nfat, rootent = struct.unpack_from("<HBHBH", bs, 11)
spf = struct.unpack_from("<H", bs, 22)[0]
fat_off = start + res * sec
root_off = fat_off + nfat * spf * sec
data_off = root_off + rootent * 32
clus = sec * spc

def chain(first, size):
    data, c, seen = b"", first, set()
    while 2 <= c < 0xFFF8:
        if c in seen:
            sys.exit(f"readback: cluster loop at {c}")
        seen.add(c)
        o = data_off + (c - 2) * clus
        data += img[o:o + clus]
        c = struct.unpack_from("<H", img, fat_off + 2 * c)[0]
    if size is not None and len(data) < size:
        sys.exit(f"readback: chain from {first} holds {len(data)} bytes, the entry says {size}")
    return data[:size] if size is not None else data

def entries(raw):
    lfn = {}
    for i in range(0, len(raw), 32):
        e = raw[i:i + 32]
        if e[0] == 0:
            break
        if e[0] == 0xE5:
            continue
        if e[11] == 0x0F:
            lfn[e[0] & 0x1F] = e[1:11] + e[14:26] + e[28:32]
            continue
        if lfn:
            name = b"".join(lfn[k] for k in sorted(lfn)).decode("utf-16-le").split("\x00")[0]
        else:
            base, ext = e[0:8].decode().strip(), e[8:11].decode().strip()
            name = (base + ("." + ext if ext else "")).lower()
        lfn = {}
        yield name, e[11], struct.unpack_from("<H", e, 26)[0], struct.unpack_from("<I", e, 28)[0]

root = {n: (a, c, s) for n, a, c, s in entries(img[root_off:data_off])}
got = {}
for n in ("hopos.img", "hopos.ird"):
    if n not in root:
        sys.exit(f"readback: {n} missing from the FAT root ({sorted(root)})")
    got[n] = chain(root[n][1], root[n][2])
if "extlinux" not in root or not root["extlinux"][0] & 0x10:
    sys.exit("readback: no extlinux directory")
sub = {n: (a, c, s) for n, a, c, s in entries(chain(root["extlinux"][1], None))}
if "extlinux.conf" not in sub:
    sys.exit(f"readback: extlinux/extlinux.conf missing ({sorted(sub)})")
got["extlinux.conf"] = chain(sub["extlinux.conf"][1], sub["extlinux.conf"][2])
for n, d in got.items():
    open(f"{out}/{n}", "wb").write(d)
PYEOF
python3 "$DIR/image/radxa-initrd.py" split "$RB/hopos.ird" "$RB/hopos.cfg" "$RB/hop.elf"
same() {
	a=$(shasum -a 256 "$1" | cut -d' ' -f1)
	b=$(shasum -a 256 "$2" | cut -d' ' -f1)
	if [ "$a" != "$b" ]; then
		echo "FOUT: $(basename "$2") uit de kaart heeft sha256 $b, erin ging $a" >&2
		exit 1
	fi
	echo "   ok  $(basename "$2") terug uit de kaart, sha256 $a" >&2
}
echo "== proef: de kaart teruggelezen" >&2
same "$OUT/hopos.img" "$RB/hopos.img"
same "$OUT/extlinux.conf" "$RB/extlinux.conf"
same "$OUT/hopos.cfg" "$RB/hopos.cfg"
if [ -n "$IMAGE" ]; then
	same "$OUT/hop.elf" "$RB/hop.elf"
elif [ -s "$RB/hop.elf" ]; then
	echo "FOUT: er zit een image in hopos.ird terwijl APP leeg was" >&2
	exit 1
fi
if ! grep -q "hopos.stage=$ROLE" "$RB/extlinux.conf"; then
	echo "FOUT: hopos.stage=$ROLE staat niet in de APPEND van de kaart" >&2
	exit 1
fi

echo "" >&2
echo "$CARD klaar (dd-baar): hopos.img + hopos.ird (hopos.cfg${IMAGE:+ + hop.elf, rol $ROLE}) + extlinux/extlinux.conf" >&2
echo "flash:   diskutil unmountDisk /dev/diskN && sudo dd if=$CARD of=/dev/rdiskN bs=4m" >&2
echo "console: 1500000 8N1 op de 40-pins header (pin 8 TX, 10 RX, 6 GND)" >&2
