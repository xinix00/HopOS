#!/usr/bin/env python3
# De initrd van de Radxa Zero 3E: één bestand dat hopos.cfg en het image van
# de bewoner (Hop, of een kale app) samen draagt. De tegenhanger in de kern
# is board/rk3566/src/initrd.rs; de vorm staat daar en hier, byte voor byte,
# en board/rk3566/testdata/mini.ird (door dit script gemaakt) houdt ze gelijk.
#
# Waarom één bestand: U-Boot 2023.10 (de donor, radxa-build/radxa-zero3 b6:
# "U-Boot latest-2023.10-8-eed05a18") laadt per extlinux-label precies één
# initrd (boot/pxe_utils.c: label->initrd gaat als één pad door
# get_relfile_envaddr), en de kern heeft geen SD-driver om zelf te lezen.
#
#   0      8 bytes  magic "HOPOSIRD"
#   8      u32 LE   versie (1)
#   12     u32 LE   lengte van de config (C)
#   16     C bytes  hopos.cfg (tekst)
#          nullen tot een veelvoud van 8
#   P      u64 LE   lengte van het image (E; 0 = geen image)
#   P+8    E bytes  het image (ELF)
#   einde  precies P + 8 + E: meer of minder is een fout
#
#   radxa-initrd.py pack  <uit> <hopos.cfg> [<image.elf>]
#   radxa-initrd.py split <in>  <cfg-uit> <image-uit>
#
# `split` toetst dezelfde regels als de kern en schrijft het image alleen als
# het er is (anders een leeg bestand).
import struct, sys

MAGIC, VERSION = b"HOPOSIRD", 1


def pack(cfg, image):
    out = bytearray(MAGIC + struct.pack("<II", VERSION, len(cfg)) + cfg)
    out += b"\x00" * (-len(out) % 8)
    out += struct.pack("<Q", len(image)) + image
    return bytes(out)


def split(blob):
    if len(blob) < 16 or blob[:8] != MAGIC:
        sys.exit("radxa-initrd: no HOPOSIRD magic")
    version, clen = struct.unpack_from("<II", blob, 8)
    if version != VERSION:
        sys.exit(f"radxa-initrd: version {version}, expected {VERSION}")
    cend = 16 + clen
    p = cend + (-cend % 8)
    if p + 8 > len(blob):
        sys.exit(f"radxa-initrd: config of {clen} bytes runs past the end ({len(blob)})")
    (elen,) = struct.unpack_from("<Q", blob, p)
    if p + 8 + elen != len(blob):
        sys.exit(f"radxa-initrd: image of {elen} bytes at {p + 8} does not end at {len(blob)}")
    return blob[16:cend], blob[p + 8:]


def main(argv):
    if len(argv) in (4, 5) and argv[1] == "pack":
        cfg = open(argv[3], "rb").read()
        image = open(argv[4], "rb").read() if len(argv) == 5 else b""
        open(argv[2], "wb").write(pack(cfg, image))
    elif len(argv) == 5 and argv[1] == "split":
        cfg, image = split(open(argv[2], "rb").read())
        open(argv[3], "wb").write(cfg)
        open(argv[4], "wb").write(image)
    else:
        sys.exit("usage: radxa-initrd.py pack <out> <cfg> [<elf>] | split <in> <cfg-out> <elf-out>")


if __name__ == "__main__":
    main(sys.argv)
