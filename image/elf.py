"""Het ELF-werk van de image-scripts, één keer: de PT_LOAD-segmenten, de
secties, de symbolen en de toets van een PIE (alleen RELATIVE-relocaties,
alleen in de RW-sectie). Als module voor image/flip-bundle.sh, als commando
voor de shell:

  python3 image/elf.py entry ELF          e_entry, hex
  python3 image/elf.py pe ELF RAW OUT     de PIE-toets van ELF, dan RAW
                                          (objcopy -O binary, met de MZ/PE-kop
                                          van board/uefi) tot op een
                                          paginagrens aangevuld naar OUT: de
                                          BOOTAA64.EFI van image/uefi-run.sh
"""
import struct
import sys

R_AARCH64_RELATIVE = 1027
SHT_SYMTAB, SHT_RELA = 2, 4
SHF_WRITE, SHF_ALLOC = 1, 2


def loads(elf):
    """De ingang en de PT_LOAD-segmenten (paddr, offset, filesz, memsz)."""
    if elf[:4] != b"\x7fELF" or elf[4] != 2 or elf[5] != 1:
        raise ValueError("not a little-endian ELF64")
    entry, phoff = struct.unpack_from("<QQ", elf, 24)
    phentsize, phnum = struct.unpack_from("<HH", elf, 54)
    segs = []
    for i in range(phnum):
        kind, _flags, off, _vaddr, paddr, filesz, memsz = struct.unpack_from("<IIQQQQQ", elf, phoff + i * phentsize)
        if kind == 1:
            segs.append((paddr, off, filesz, memsz))
    return entry, segs


def sections(elf):
    """De sectiekoppen: (name, type, flags, addr, offset, size, link, info)."""
    shoff, = struct.unpack_from("<Q", elf, 0x28)
    shentsize, shnum, _shstrndx = struct.unpack_from("<HHH", elf, 0x3a)
    return [struct.unpack_from("<IIQQQQII", elf, shoff + i * shentsize) for i in range(shnum)]


def section_name(elf, sec):
    secs = sections(elf)
    _, _, shstrndx = struct.unpack_from("<HHH", elf, 0x3a)
    o = secs[shstrndx][4] + sec[0]
    return elf[o:elf.index(b"\0", o)].decode()


def symbols(elf):
    """Naam naar waarde, uit de symbooltabel."""
    secs = sections(elf)
    syms = {}
    for s in secs:
        if s[1] != SHT_SYMTAB:
            continue
        strtab = secs[s[6]]
        for k in range(s[5] // 24):
            name, _info, _other, _shndx, value, _size = struct.unpack_from("<IBBHQQ", elf, s[4] + k * 24)
            o = strtab[4] + name
            syms[elf[o:elf.index(b"\0", o)].decode()] = value
    return syms


def relative_only(elf):
    """De toets van een PIE die zijn eigen relocaties toepast (de stub van
    board/uefi, de flip): elke relocatie RELATIVE en in de RW-sectie. Geeft
    het aantal relocaties, de start van de RW-sectie en de foute als
    (index, sectie, type, offset)."""
    secs = sections(elf)
    data_start = min(s[3] for s in secs if s[2] & SHF_ALLOC and s[2] & SHF_WRITE)
    n, bad = 0, []
    for s in secs:
        if s[1] != SHT_RELA:
            continue
        for k in range(s[5] // 24):
            r_off, r_info, _ = struct.unpack_from("<QQq", elf, s[4] + k * 24)
            n += 1
            if r_info & 0xffffffff != R_AARCH64_RELATIVE or r_off < data_start:
                bad.append((k, s, r_info & 0xffffffff, r_off))
    return n, data_start, bad


def pe(elf_path, raw_path, out_path):
    elf = open(elf_path, "rb").read()
    n, data_start, bad = relative_only(elf)
    for k, s, typ, off in bad[:5]:
        print(f"uefi-run: relocation {k} in {section_name(elf, s)}: type {typ} at {off:#x} (data starts at {data_start:#x})", file=sys.stderr)
    if bad:
        sys.exit(f"uefi-run: {len(bad)} of {n} relocations are not RELATIVE in the RW section, refusing the image")
    img = open(raw_path, "rb").read()
    if img[:2] != b"MZ" or img[0x40:0x44] != b"PE\0\0":
        sys.exit("uefi-run: no MZ/PE header at the start of the image")
    # De PE-header noemt .data met SizeOfRawData tot aan een paginagrens: vul aan.
    img += b"\0" * (-len(img) % 4096)
    open(out_path, "wb").write(img)
    print(f"uefi-run: {out_path} ({len(img)} bytes, {n} relocations, data at {data_start:#x})", file=sys.stderr)


if __name__ == "__main__":
    cmd, args = sys.argv[1], sys.argv[2:]
    if cmd == "entry":
        print(hex(loads(open(args[0], "rb").read())[0]))
    elif cmd == "pe":
        pe(*args)
    else:
        sys.exit(f"elf.py: unknown command {cmd} (entry, pe)")
