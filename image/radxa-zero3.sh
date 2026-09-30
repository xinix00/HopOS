#!/bin/sh
# Bouw HopOS v3 voor de Radxa Zero 3E (Rockchip RK3566): de kern, het
# arm64-Image, en een compleet, dd-baar kaart-image. De vorm van
# OLD/image/radxa-zero3.sh, zonder Go: de ELF-naar-Image-stap (Go's mkkernel)
# en de kaartbouwer (Go's mkcard) staan hieronder als python3, want dat is
# het enige dat deze machine er al voor heeft.
#
#   image/radxa-zero3.sh                 de kaart met Hop als bewoner
#   APP=appspike image/...               appspike, twee keer door de kern
#                                        geplaatst (de kale app-rol)
#   APP=/pad/naar/elf image/...          een kant-en-klare ELF (rol app)
#   APP= image/...                       zonder image (HOPOS_SLOT_NONE)
#   HOP_DIR=pad image/...                de hop-repo (standaard ../hop/hop)
#   ROLE=hop|app image/...               de rol van het image (hopos.stage)
#   CFG=~/mijn-node.cfg image/...        met je eigen hopos.cfg (sleutels
#                                        horen daar, niet in de repo)
#   NODE=radxa-2 image/...               een andere hopos.node in de APPEND
#   RADXA_DONOR=pad image/...            een eigen donor-boot-blok
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
# officiële image gehaald en gehasht. U-Boot's distro-boot vindt daarna
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
#    hopos.cfg wint van dezelfde sleutel in de APPEND.
if [ -n "$CFG" ]; then
	cp "$CFG" "$OUT/hopos.cfg"
else
	cat > "$OUT/hopos.cfg" <<EOF
# HopOS v3 op de Radxa Zero 3E. Sleutels die het board leest: hopos.node
# (de naam, en daaruit het MAC-adres) en hopos.mac (aa:bb:cc:dd:ee:ff, wint).
hopos.node=${NODE:-radxa-1}
EOF
fi
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
#    release b6, gepind met hash. Dit zijn exact de bytes waarmee alle
#    metingen van 05/06-08 gedaan zijn. Eerst de eigen cache, dan de cache
#    van de Go-boom (dezelfde bytes), dan de download: alleen de eerste
#    16 MiB van de 2,5 GB-stream, xz-uitgepakt in python.
DONOR="${RADXA_DONOR:-$DIR/image/radxa/donor-boot.bin}"
DONOR_URL="https://github.com/radxa-build/radxa-zero3/releases/download/b6/radxa-zero3_debian_bullseye_cli_b6.img.xz"
DONOR_SHA="9a582a0c6fcd8b41d5627284aa9b831a46a08e7606c6fc31901f48725354a7b9"
OLD_DONOR="$DIR/OLD/image/radxa/donor-boot.bin"
if [ ! -f "$DONOR" ] && [ -z "$RADXA_DONOR" ] && [ -f "$OLD_DONOR" ]; then
	mkdir -p "$(dirname "$DONOR")"
	cp "$OLD_DONOR" "$DONOR"
	echo "== donor-boot uit de Go-cache overgenomen: $OLD_DONOR" >&2
fi
if [ ! -f "$DONOR" ]; then
	if [ -n "$RADXA_DONOR" ]; then
		echo "FOUT: RADXA_DONOR=$RADXA_DONOR bestaat niet" >&2
		exit 1
	fi
	echo "== donor-boot ophalen (eenmalig, ~10 MB van de stream): $DONOR_URL" >&2
	mkdir -p "$(dirname "$DONOR")"
	if ! python3 - "$DONOR_URL" "$DONOR.part" <<'PYEOF'
import lzma, sys, urllib.request
WANT, SKIP = 16 << 20, 64 * 512
dec, buf = lzma.LZMADecompressor(), bytearray()
with urllib.request.urlopen(sys.argv[1]) as r:
    while len(buf) < WANT:
        chunk = r.read(1 << 20)
        if not chunk:
            sys.exit(f"stream ended at {len(buf)} bytes, expected {WANT}")
        buf += dec.decompress(chunk)
open(sys.argv[2], "wb").write(bytes(buf[SKIP:WANT]))
PYEOF
	then
		rm -f "$DONOR.part"
		echo "FOUT: de donor ontbreekt en de download faalde." >&2
		echo "  Nodig: $DONOR (16 MiB - 32 KiB: LBA 64..32767 van" >&2
		echo "  radxa-zero3_debian_bullseye_cli_b6.img, sha256 $DONOR_SHA)." >&2
		echo "  Haal $DONOR_URL, pak uit, en: dd if=<img> of=$DONOR bs=512 skip=64 count=32704" >&2
		exit 1
	fi
	mv "$DONOR.part" "$DONOR"
fi
GOT=$(shasum -a 256 "$DONOR" | cut -d' ' -f1)
if [ "$GOT" != "$DONOR_SHA" ]; then
	echo "FOUT: $DONOR heeft hash $GOT, verwacht $DONOR_SHA" >&2
	echo "  (verwijder het bestand voor een verse download, of zet RADXA_DONOR)" >&2
	exit 1
fi

# 6. De kaart: onze MBR (één FAT16-partitie, type 0x0C, actief, vanaf LBA
#    32768 = 16 MiB), de donor raw op LBA 64, en de drie bestanden in de FAT.
#    Deterministisch: vaste tijdstempels, vaste volume-id, geen mount.
#    Geometrie van Go's mkcard: 512-byte sectoren, 2 KB-clusters, 4
#    gereserveerde sectoren, 2 FAT's, 512 root-entries. extlinux.conf past
#    niet in 8.3 en krijgt een VFAT-LFN (U-Boot leest die).
CARD="$OUT/hopos-radxa-zero3.img"
python3 - "$CARD" "$DONOR" "$OUT/hopos.img" "$OUT/hopos.ird" "$OUT/extlinux.conf" <<'PYEOF'
import struct, sys
card, donor, kernel, ird, extl = sys.argv[1:6]
SEC, SPC, RES, NFAT, ROOTENT = 512, 4, 4, 2, 512
CARD_MB, START = 64, 32768
CLUS = SEC * SPC
total = CARD_MB * 2048 - START
root_secs = ROOTENT * 32 // SEC
spf = 1
while True:
    data = total - RES - NFAT * spf - root_secs
    clusters = data // SPC
    need = ((clusters + 2) * 2 + SEC - 1) // SEC
    if need <= spf:
        break
    spf = need
assert 4085 <= clusters < 65525, clusters
fat_off = RES * SEC
root_off = fat_off + NFAT * spf * SEC
data_off = root_off + root_secs * SEC
part = bytearray(total * SEC)
fat = [0xFFF8, 0xFFFF] + [0] * clusters
nxt = [2]
DATE, TIME = ((2026 - 1980) << 9) | (9 << 5) | 29, 12 << 11

def alloc(content):
    n = max(1, (len(content) + CLUS - 1) // CLUS)
    first = nxt[0]
    if first + n > clusters + 2:
        sys.exit("de kaartpartitie is te klein")
    for c in range(first, first + n):
        fat[c] = c + 1 if c < first + n - 1 else 0xFFFF
    off = data_off + (first - 2) * CLUS
    part[off:off + len(content)] = content
    nxt[0] = first + n
    return first

def short(name, isdir=False):
    base, _, ext = name.upper().partition(".")
    if len(base) <= 8 and len(ext) <= 3 and name == name.lower() and all(ch.isalnum() or ch in "-_" for ch in base + ext):
        return (base.ljust(8) + ext.ljust(3)).encode(), False
    b = "".join(ch for ch in base if ch.isalnum())[:6] + "~1"
    return (b.ljust(8) + ext[:3].ljust(3)).encode(), True

def entry(sname, attr, clus, size, lower=0):
    return sname + struct.pack("<BBBHHHHHHHI", attr, lower, 0, TIME, DATE, DATE, 0, TIME, DATE, clus, size)

def lfn(name, sname):
    ck = 0
    for b in sname:
        ck = (((ck & 1) << 7) + (ck >> 1) + b) & 0xFF
    u = name.encode("utf-16-le") + b"\x00\x00"
    u += b"\xff" * ((-len(u)) % 26)
    parts = [u[i:i + 26] for i in range(0, len(u), 26)]
    out = b""
    for i in range(len(parts), 0, -1):
        p = parts[i - 1]
        seq = i | (0x40 if i == len(parts) else 0)
        out += bytes([seq]) + p[0:10] + bytes([0x0F, 0, ck]) + p[10:22] + b"\x00\x00" + p[22:26]
    return out

def dirent(name, attr, clus, size):
    s, needs = short(name)
    lower = 0x18 if not needs and name != name.upper() else 0
    return (lfn(name, s) if needs else b"") + entry(s, attr, clus, size, lower)

files = {n: open(p, "rb").read() for n, p in (("hopos.img", kernel), ("hopos.ird", ird))}
ext_data = open(extl, "rb").read()
ext_dir_clus = alloc(b"\x00" * CLUS)
ext_file_clus = alloc(ext_data)
sub = entry(b".          ", 0x10, ext_dir_clus, 0) + entry(b"..         ", 0x10, 0, 0)
sub += dirent("extlinux.conf", 0x20, ext_file_clus, len(ext_data))
off = data_off + (ext_dir_clus - 2) * CLUS
part[off:off + len(sub)] = sub
root = entry(b"HOPOS      ", 0x08, 0, 0)
for n, content in files.items():
    root += dirent(n, 0x20, alloc(content), len(content))
root += dirent("extlinux", 0x10, ext_dir_clus, 0)
part[root_off:root_off + len(root)] = root
fatb = struct.pack(f"<{len(fat)}H", *fat)
for i in range(NFAT):
    o = fat_off + i * spf * SEC
    part[o:o + len(fatb)] = fatb
bs = bytearray(SEC)
bs[0:3] = b"\xEB\x3C\x90"
bs[3:11] = b"HOPOS   "
struct.pack_into("<HBHBHHBHHHII", bs, 11, SEC, SPC, RES, NFAT, ROOTENT,
                 total if total < 65536 else 0, 0xF8, spf, 63, 255, START, total if total >= 65536 else 0)
struct.pack_into("<BBBI", bs, 36, 0x80, 0, 0x29, 0x484F5033)
bs[43:54] = b"HOPOS      "
bs[54:62] = b"FAT16   "
bs[510:512] = b"\x55\xAA"
part[0:SEC] = bs
img = bytearray(START * SEC)
d = open(donor, "rb").read()
if len(d) != START * SEC - 64 * SEC:
    sys.exit(f"donor is {len(d)} bytes, verwacht {START * SEC - 64 * SEC}")
img[64 * SEC:START * SEC] = d
mbr = bytearray(16)
mbr[0], mbr[4] = 0x80, 0x0C
mbr[1:4] = mbr[5:8] = b"\xFE\xFF\xFF"
struct.pack_into("<II", mbr, 8, START, total)
img[446:462] = mbr
struct.pack_into("<I", img, 440, 0x484F5033)
img[510:512] = b"\x55\xAA"
open(card, "wb").write(bytes(img) + bytes(part))
print(f"{card}: {CARD_MB} MiB, FAT16 {clusters} clusters, kernel {len(files['hopos.img'])} bytes", file=sys.stderr)
PYEOF

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
