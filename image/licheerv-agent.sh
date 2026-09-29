#!/bin/sh
# Bouw HopOS v3 voor de Sipeed LicheeRV Nano (Sophgo SG2002 / CV181x,
# XuanTie C906): de kern-binary als MONITOR-slot van fip.bin, zoals de
# Go-generatie (OLD/image/licheerv-agent.sh). De vendor-FSBL doet klok- en
# DDR-init en springt ons in MACHINE MODE binnen op RUNADDR; OpenSBI, U-Boot
# en Linux komen er niet meer aan te pas. HopOS is zelf de monitor: de kooi is
# PMP plus Sv39, en PMP kan alleen machine mode (docs/boards-riscv.md).
#
#   image/licheerv-agent.sh                → target/licheerv/fip-licheerv.bin
#   image/licheerv-agent.sh /dev/diskN     → idem, plus fip.bin op de
#                                            FAT-bootpartitie van een kaart
#                                            die er al een heeft (de snelle
#                                            iteratie; de eerste kaart komt
#                                            van het Sipeed donor-image)
#
# Nodig: de donor-FIP en fiptool.py uit een Sipeed-release
# (LICHEERV_DONOR, LICHEERV_FIPTOOL; default image/licheerv/, vendor-
# bestanden, gitignored) en rust-objcopy (rustup component add llvm-tools).
#
# GEHASHT: de donor is vendor-code die vóór ons draait (FSBL, DDR-training).
# Het script drukt de SHA-256 van de donor en van de nieuwe FIP af; met
# LICHEERV_DONOR_SHA256 gezet weigert het elke andere donor. Zo is "welke
# FSBL draaide er onder deze boot" een getal in het logboek, geen gok.
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TARGET=riscv64gc-unknown-none-elf
OUT="$DIR/target/licheerv"
DONOR="${LICHEERV_DONOR:-$DIR/image/licheerv/donor-fip.bin}"
FIPTOOL="${LICHEERV_FIPTOOL:-$DIR/image/licheerv/fiptool.py}"

# RUNADDR is NIET DRAM-start (GEMETEN 30-07): de FSBL laadt na ons image ook
# LOADER_2ND (U-Boot) en decomprimeert dat naar 0x8020_0020 (~600 KB), en hij
# woont zelf in het lage DRAM terwijl hij dat doet. 0x8040_0000 is geprobeerd
# (19-08) en het board BOOT DAAR NIET. Dit getal staat ook in hopos/build.rs
# (KERN_BASE van board-licheerv) en board/licheerv/src/lib.rs (KERN_RAM).
RUNADDR=0x84000000

sha() { shasum -a 256 "$1" | cut -d' ' -f1; }

[ -f "$DONOR" ] || { echo "donor-fip ontbreekt: $DONOR (zet LICHEERV_DONOR)" >&2; exit 1; }
[ -f "$FIPTOOL" ] || { echo "fiptool ontbreekt: $FIPTOOL (zet LICHEERV_FIPTOOL)" >&2; exit 1; }
OBJCOPY="$(ls "$(rustc --print sysroot)"/lib/rustlib/*/bin/rust-objcopy 2>/dev/null | head -1)"
[ -n "$OBJCOPY" ] || { echo "rust-objcopy ontbreekt (rustup component add llvm-tools)" >&2; exit 1; }

DONOR_SHA="$(sha "$DONOR")"
if [ -n "${LICHEERV_DONOR_SHA256:-}" ] && [ "$DONOR_SHA" != "$LICHEERV_DONOR_SHA256" ]; then
	echo "WEIGER: donor $DONOR heeft sha256 $DONOR_SHA, verwacht $LICHEERV_DONOR_SHA256" >&2
	exit 1
fi
echo "donor: $DONOR sha256=$DONOR_SHA" >&2

mkdir -p "$OUT"
cd "$DIR"
echo "== kern bouwen (hopos --features board-licheerv, $TARGET) ==" >&2
cargo build --quiet --release --target "$TARGET" -p hopos --features board-licheerv
ELF="$DIR/target/$TARGET/release/hopos"

# Het ELF begint op RUNADDR met _start (link-riscv.ld, KERN_BASE uit
# build.rs); de monitor-blob is het platte beeld vanaf daar. Geen trapstub
# ervoor zoals in Go: `_start` zet zelf mtvec op de trap van de kern.
# De ELF-entry (e_entry, offset 24, little-endian) moet RUNADDR zijn: een
# ander linkscript is een image dat de FSBL midden in iets laat springen.
ENTRY="$(python3 -c 'import struct,sys; print(hex(struct.unpack_from("<Q", open(sys.argv[1],"rb").read(32), 24)[0]))' "$ELF")"
[ "$ENTRY" = "$RUNADDR" ] || { echo "WEIGER: entry $ENTRY is niet RUNADDR $RUNADDR (linkscript?)" >&2; exit 1; }
"$OBJCOPY" -O binary "$ELF" "$OUT/monitor.bin"
echo "monitor: $(wc -c <"$OUT/monitor.bin" | tr -d ' ') bytes sha256=$(sha "$OUT/monitor.bin")" >&2

echo "== fip-licheerv.bin ==" >&2
# De donor draagt een param2-blok (CVLD02) met een runaddr voor LOADER_2ND;
# die op nul zetten, zoals de Go-build deed, anders springt de FSBL na ons
# alsnog U-Boot in.
python3 - "$DONOR" "$OUT/donor-runaddr0.bin" <<'PYEOF'
import struct, sys
d = bytearray(open(sys.argv[1], "rb").read())
hits, i = [], 0
while True:
    i = d.find(b"CVLD02\n\x00", i)
    if i < 0:
        break
    ra = struct.unpack_from("<I", d, i + 60)[0]
    if 0x80000000 <= ra < 0x90000000:
        hits.append(i)
    i += 1
if len(hits) != 1:
    sys.exit(f"fip: {len(hits)} param2 blocks found, expected 1")
struct.pack_into("<I", d, hits[0] + 60, 0)
open(sys.argv[2], "wb").write(bytes(d))
PYEOF
python3 "$FIPTOOL" genfip "$OUT/fip-licheerv.bin" \
	--OLD_FIP "$OUT/donor-runaddr0.bin" \
	--MONITOR "$OUT/monitor.bin" \
	--MONITOR_RUNADDR "$RUNADDR" 2>/dev/null
echo "fip: $OUT/fip-licheerv.bin sha256=$(sha "$OUT/fip-licheerv.bin")" >&2

DISK="${1:-}"
[ -n "$DISK" ] || { echo "flash: image/licheerv-agent.sh /dev/diskN" >&2; exit 0; }
diskutil mount "${DISK}s1" >/dev/null
MNT=$(diskutil info "${DISK}s1" | sed -n 's/.*Mount Point: *//p')
[ -n "$MNT" ] && mount | grep -q " on $MNT " || {
	echo "WEIGER: ${DISK}s1 is niet echt gemount (mountpoint: '$MNT')" >&2
	exit 1
}
cp "$OUT/fip-licheerv.bin" "$MNT/fip.bin"
sync
[ "$(sha "$MNT/fip.bin")" = "$(sha "$OUT/fip-licheerv.bin")" ] || {
	echo "FOUT: teruglezen van fip.bin klopt niet" >&2
	exit 1
}
diskutil unmount "${DISK}s1" >/dev/null
echo "fip.bin op ${DISK}s1 (sha256 geverifieerd): de kaart kan in de LicheeRV." >&2
