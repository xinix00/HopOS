#!/usr/bin/env python3
# load-probe.py — zet een HopOS-image via m1n1's proxy op een Apple-silicon-
# machine en start hem. De "firmware-rol" van dit script: wat U-Boot/booti op
# de Radxa doet (image op zijn adres, x0 = DTB) doet dit script hier, plus het
# param-blok (board/apple/params.go) met wat HopOS uit de ADT nodig heeft.
#
#   M1N1DEVICE=/dev/cu.usbmodem<serienr>1 python3 image/apple/load-probe.py metal/out/probeapple.img
#
# Omgeving: M1N1 = pad naar de m1n1-clone (default ~/Git/m1n1), M1N1DEVICE =
# de proxy-poort. Na de sprong leest het script /dev/cu.debug-console
# (CONSOLE=... om te kiezen, CONSOLE=none om over te slaan) op 1500000 baud
# en echoot wat de probe zegt.
import os, sys, struct, time, subprocess, pathlib

M1N1 = os.path.expanduser(os.environ.get("M1N1", "~/Git/m1n1"))
sys.path.append(os.path.join(M1N1, "proxyclient"))
from m1n1.setup import *  # noqa: E402

# Pariteit met board/apple/apple.go en params.go.
RAM_BASE   = 0x101_0000_0000
PARAM_BASE = RAM_BASE + 0xE100
PARAM_MAGIC = 0x454C505041504F48  # "HOPAPPLE"
PARAM_VERSION = 3
MAX_CPUS = 16
SPIN_TABLE_ENTRY = 64  # sizeof(struct spin_table): mpidr, flag, target, args[4], retval
SPIN_TARGET_OFF = 16

img_path = pathlib.Path(sys.argv[1])
img = img_path.read_bytes()

# PHASE=all (default) doet alles over één lijn. De snelle meetbank splitst:
# PHASE=load over m1n1's USB-gadget (serial-mode, ~9MB/s), dan macvdmtool
# debugusb, dan PHASE=boot over de dockchannel (kis ch-0) die daarna de console
# is. m1n1 blijft tussendoor gewoon draaien, dus wat PHASE=load in RAM legde
# (image + param-blok) staat er nog.
PHASE = os.environ.get("PHASE", "all")

def boot_and_console():
    u.msr(DAIF, 0x3C0)
    print("Booting at %#x ..." % RAM_BASE)
    try:
        p.kboot_boot(RAM_BASE)
    except Exception as e:
        # De proxy sluit zijn USB-gadget vóór de sprong; een afgebroken antwoord
        # hier is normaal.
        print("(proxy closed: %s)" % e)
    console = os.environ.get("CONSOLE", "proxy")
    if console == "none":
        return
    import serial
    limit = float(os.environ.get("CONSOLE_SECONDS", "0")) or None
    if console == "proxy":
        s = iface.dev
        s.timeout = 1
    else:
        s = serial.Serial(console, 1500000, timeout=1)
    print("--- console %s (%s) ---" % (console, "%.0fs" % limit if limit else "Ctrl-C to stop"))
    t0 = time.time()
    try:
        while limit is None or time.time() - t0 < limit:
            d = s.read(4096)
            if d:
                sys.stdout.write(d.decode("utf-8", "replace"))
                sys.stdout.flush()
    except KeyboardInterrupt:
        pass

if PHASE == "boot":
    boot_and_console()
    sys.exit(0)

def adt_reg(path, fallback):
    try:
        addr, size = u.adt[path].get_reg(0)
        return addr
    except Exception as e:
        print("ADT %s: %s → fallback %#x" % (path, e, fallback))
        return fallback

dock  = adt_reg("/arm-io/dockchannel-uart", 0x3_8812_8000)
uart0 = adt_reg("/arm-io/uart0", 0x3_ad20_0000)

adt_phys = (u.ba.devtree - u.ba.virt_base + u.ba.phys_base) & 0xFFFFFFFFFFFFFFFF
adt_size = u.ba.devtree_size
chosen = u.adt["/chosen"]
dram_base, dram_size = chosen.dram_base, chosen.dram_size
fb = u.ba.video

# m1n1's spin-table: het symbool uit de ELF van precies deze m1n1-build,
# gerelokeerd naar waar m1n1 nu draait (u.base). Een verkeerde offset zou een
# core naar een willekeurig woord laten kijken, dus we controleren: de
# flag-woorden van de gestarte cores moeten 1 zijn.
def spin_table_addr():
    # m1n1.bin is de RAW link (m1n1-raw.ld): dezelfde code 0x4000 lager dan in
    # m1n1.elf. Symbolen dus uit m1n1-raw.elf, anders lees je naast de tabel.
    elf = os.path.join(M1N1, "build", "m1n1-raw.elf")
    out = subprocess.check_output(["aarch64-elf-nm", elf]).decode()
    sym = [l for l in out.splitlines() if l.endswith(" spin_table")]
    base = [l for l in out.splitlines() if l.endswith(" _base")]
    return u.base + int(sym[0].split()[0], 16) - int(base[0].split()[0], 16)

p.smp_start_secondaries()
ncpus = len(list(u.adt["/cpus"]))
my_mpidr = u.mrs(MPIDR_EL1) & 0xFFFFFF
release = [0] * MAX_CPUS
mpidr = [0] * MAX_CPUS
boot_cpu = -1
# De spin-table is optioneel: lukt de lookup niet, dan boot de probe zonder
# core-test (release-adressen 0) — eerste licht gaat vóór de cores.
try:
    spin = spin_table_addr()
    print("u.base %#x, spin_table %#x, my mpidr %#x" % (u.base, spin, my_mpidr))
    for cpu in range(min(ncpus, MAX_CPUS)):
        ent = spin + cpu * SPIN_TABLE_ENTRY
        m, flag = p.read64(ent), p.read64(ent + 8)
        mpidr[cpu] = m
        print("  cpu%d: mpidr %#x flag %d" % (cpu, m, flag))
        if m == my_mpidr:
            boot_cpu = cpu
        elif flag:
            release[cpu] = ent + SPIN_TARGET_OFF
    if boot_cpu < 0:
        raise RuntimeError("boot cpu not in spin table (symbol offset wrong?)")
    print("spin table: boot cpu %d, alive %r" % (boot_cpu, [c for c in range(ncpus) if release[c]]))
except Exception as e:
    print("spin table unavailable (%s) — probe will skip the core test" % e)
    release = [0] * MAX_CPUS
    mpidr = [0] * MAX_CPUS
    boot_cpu = 0xFFFF

# Bruikbaar RAM-einde: wat m1n1 in de FDT als memory-node zou zetten
# (phys_base + mem_size); daarboven wonen iBoot's carveouts en de framebuffer.
usable_end = u.ba.phys_base + u.ba.mem_size

# Het MAC-adres van de ingebouwde NIC staat in de ADT (local-mac-address) —
# dezelfde bron die m1n1 voor Linux in de device tree patcht. Na een PERST staat
# in MAC_ADDR_0 alleen nog Broadcom's default (00:10:18:00:00:00), dus dit is de
# enige plek waar het échte adres vandaan komt.
mac = 0
try:
    b = bytes(u.adt["/arm-io/apcie/pci-bridge2/lan-1gb"].local_mac_address)[:6]
    mac = int.from_bytes(b, "big")
    print("nic mac: %s" % ":".join("%02x" % c for c in b))
except Exception as e:
    print("ADT local-mac-address: %s" % e)
# De opslag: ANS (Apple NVMe Storage) is geen PCIe-NVMe maar een co-processor
# achter een RTKit-mailbox, met een SART als adresfilter in plaats van een DART.
# Op M4 (nvme-secure-bar aanwezig) zitten de NVMMU-registers in reg[3] en de
# NVMe-registers in reg[9]; op M1-M3 is dat allebei reg[3]. m1n1 src/nvme.c.
def adt_reg_n(path, n):
    try:
        addr, _ = u.adt[path].get_reg(n)
        return addr
    except Exception as e:
        print("ADT %s reg[%d]: %s" % (path, n, e))
        return 0

ans_base = adt_reg_n("/arm-io/ans", 0)
nvmmu_base = adt_reg_n("/arm-io/ans", 3)
nvme_base = adt_reg_n("/arm-io/ans", 9)
sart_base = adt_reg_n("/arm-io/sart-ans", 0)
try:
    sart_version = int(u.adt["/arm-io/sart-ans"].sart_version)
except Exception as e:
    print("ADT sart-version: %s" % e)
    sart_version = 0
print("ans %#x nvmmu %#x nvme %#x sart %#x (v%d)" % (
    ans_base, nvmmu_base, nvme_base, sart_base, sart_version))

params = struct.pack("<16Q",
    PARAM_MAGIC, PARAM_VERSION, dock, uart0, adt_phys, adt_size,
    dram_base, dram_size, fb.base, fb.stride, fb.width, fb.height,
    ncpus, boot_cpu, usable_end, mac)
params = params.ljust(0x80, b"\0") + struct.pack("<16Q", *release) + struct.pack("<16Q", *mpidr)
assert len(params) == 0x180
# Het RAM-contract van iBoot, zoals een Linux-kernel het ook krijgt:
# [phys_base, phys_base+mem_size) is van ons, en alles wat de firmware er zelf
# al in legde (kernel-image, ADT, trust cache) eindigt op top_of_kernel_data.
# HopOS snijdt zijn partitie-pool daarop; zonder deze drie getallen valt het
# terug op een veilige-maar-royale marge van 4GB onderin.
fw_base = u.ba.phys_base
fw_size = u.ba.mem_size
fw_placed = u.ba.top_of_kernel_data
print("firmware ram: %#x+%dMB, firmware zelf tot %#x (+%.0f MB)" % (
    fw_base, fw_size >> 20, fw_placed, (fw_placed - dram_base) / 2**20))

params += struct.pack("<8Q", ans_base, nvmmu_base, nvme_base, sart_base, sart_version,
    fw_base, fw_size, fw_placed)
assert len(params) == 0x1C0

# De dockchannel (debugusb-mode) is een WOORD-kanaal: een write waarvan de
# lengte geen veelvoud van 8 is, verliest zijn staart tot er meer bytes komen
# (GEMETEN 28-08: 3038 bytes faalt altijd, 3040 werkt; m1n1's writemem stuurt
# alles in één keer en wacht dan op één ack die nooit komt). Daarom: elke
# write op 8 bytes gepad, in chunks met eigen ack+CRC; sporadisch verlies
# (~1 op 130KB gezien) wordt hersteld door de proxy eerst "vol te maken" met
# nullen tot hij antwoordt (unwedge), te hersyncen met een nop, en de chunk
# opnieuw te sturen. Over de USB-gadget (usbmodem) is dit overbodig maar
# onschadelijk.
import gzip
def pad8(b):
    return b + b"\0" * (-len(b) % 8)

def unwedge():
    dev = iface.dev
    old = dev.timeout
    dev.timeout = 0.05
    for _ in range(20000):
        dev.write(b"\0" * 64)
        if dev.read(64):
            break
    time.sleep(0.4)
    dev.reset_input_buffer()
    dev.timeout = old
    for _ in range(5):
        try:
            iface.nop()
            return
        except Exception:
            time.sleep(0.2)
            dev.reset_input_buffer()

def robust_writemem(addr, data, chunk=int(os.environ.get("CHUNK", "8192"))):
    data = pad8(data)
    assert chunk % 8 == 0
    off, retries, t0 = 0, 0, time.time()
    while off < len(data):
        piece = data[off:off + chunk]
        try:
            iface.writemem(addr + off, piece)
            off += len(piece)
            sys.stdout.write("."); sys.stdout.flush()
        except Exception as e:
            retries += 1
            sys.stdout.write("!(%s)" % type(e).__name__); sys.stdout.flush()
            if retries > 40:
                raise
            unwedge()
    dt = time.time() - t0
    print("\n%d bytes in %.1fs (%.0f KB/s), %d retries" % (len(data), dt, len(data) / dt / 1024, retries))

payload = gzip.compress(img, compresslevel=6)
print("Loading %d bytes (gz %d) to %#x..%#x" % (len(img), len(payload), RAM_BASE, RAM_BASE + len(img)))
gz_addr = u.malloc(len(payload) + 8)
robust_writemem(gz_addr, payload)
tmo = iface.dev.timeout
iface.dev.timeout = None
n = p.gzdec(gz_addr, len(payload), RAM_BASE, len(img))
iface.dev.timeout = tmo
if n != len(img):
    raise SystemExit("gzdec: %d != %d — image corrupt on target" % (n, len(img)))
print("gzdec OK: %d bytes at %#x" % (n, RAM_BASE))
robust_writemem(PARAM_BASE, params)

# Platform-config als tekst op CFG_BASE (apple.CfgBase): het hopos.cfg-bestand
# uit CFG=pad, plus het serial uit de ADT als hopos.serial (node-identiteit).
CFG_BASE, CFG_SIZE = RAM_BASE + 0xF000, 0x1000
cfg = ""
if os.environ.get("CFG"):
    cfg = pathlib.Path(os.environ["CFG"]).read_text()
try:
    sn = u.adt.serial_number  # de wortel is u.adt zelf; u.adt["/"] geeft "Child node '' not found"
    serial_no = sn if isinstance(sn, str) else bytes(sn).split(b"\0")[0].decode()
    cfg += "\nhopos.serial=%s\n" % serial_no
except Exception as e:
    print("ADT serial-number: %s" % e)
cfgb = cfg.encode()
if len(cfgb) >= CFG_SIZE:
    raise SystemExit("config too large: %d >= %d" % (len(cfgb), CFG_SIZE))
robust_writemem(CFG_BASE, cfgb.ljust(CFG_SIZE, b"\0"))
print("config: %d bytes%s" % (len(cfgb), " from " + os.environ["CFG"] if os.environ.get("CFG") else ""))
p.dc_cvau(RAM_BASE, len(img) + 0x10000)
p.ic_ivau(RAM_BASE, len(img) + 0x10000)

print("dockchannel %#x uart0 %#x ADT %#x+%#x DRAM %#x+%dMB fb %#x %dx%d" % (
    dock, uart0, adt_phys, adt_size, dram_base, dram_size >> 20, fb.base, fb.width, fb.height))

if PHASE == "load":
    print("Loaded; not booting (PHASE=load). Switch the port and run PHASE=boot.")
    sys.exit(0)
boot_and_console()
