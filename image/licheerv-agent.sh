#!/bin/sh -e
# Bouw HopOS voor de Sipeed LicheeRV Nano (Sophgo SG2002 / CV181x, XuanTie
# C906) — het eerste RISC-V-board. Naast rpi4-agent.sh / rpi5-agent.sh /
# uefi-run.sh; zelfde vorm, ander silicium.
#
# Boot-route: ons image vervangt OpenSBI in het MONITOR-slot van fip.bin. De
# vendor-FSBL doet clock/DDR-init en springt ons in M-mode binnen — U-Boot en
# Linux komen er niet meer aan te pas.
#
#   image/licheerv-agent.sh                 → out/hopos-licheerv.img (+ fip.bin)
#   image/licheerv-agent.sh /dev/diskN      → idem + fip.bin op een bestaande kaart
#
# Het .img is een compleet SD-kaart-image: MBR + FAT16-bootpartitie met fip.bin.
# Één keer schrijven en booten — geen donor-image van 1,6GB, geen mount-gedoe:
#
#   diskutil unmountDisk /dev/diskN
#   sudo dd if=metal/out/hopos-licheerv.img of=/dev/rdiskN bs=4m
#
# De /dev/diskN-vorm hierboven is de snelle iteratie: die vervangt alleen
# fip.bin op een kaart die al partitie + FAT heeft (dus na één dd, of na het
# Sipeed donor-image).
#
# Dit bouwt de ECHTE agent-main (cmd/hopos, -tags licheerv) — dezelfde binary-
# vorm als op de Pi's en UEFI, en hier ook een volwaardige node: de DWMAC met de
# interne ePHY is in bedrijf (100Mbit, DHCP + NTP), dus de node haalt elk
# image gewoon over het netwerk. Alleen een framebuffer heeft dit board niet, dus
# gui-code zit er nooit in. (De pre-agent slot-demo cmd/hopos-lrv en zijn
# DEMO=1-knop zijn 04-08 gesloopt: de agent legt dezelfde proef af op ijzer.)
#
# Eerste keer een kaart: één dd van het .img hierboven is genoeg (dat is óók het
# release-asset hopos-licheerv-headless.img.gz — mét -headless in de naam, want
# overal elders betekent een naam zónder dat suffix de gui-smaak, en die bestaat
# hier niet: geen framebuffer op dit silicium). Daarna is elke iteratie alleen
# nog fip.bin vervangen. Zie docs/archief/licheerv-bringup.md voor het draaiboek
# en de metingen.
#
# Nodig: de tamago-toolchain (TAMAGO), een riscv64 binutils (riscv64-elf-as/ld/
# objcopy, `brew install riscv-gnu-toolchain` of vergelijkbaar) en de
# donor-fip + fiptool uit de Sipeed release (LICHEERV_DONOR / LICHEERV_FIPTOOL —
# default: image/licheerv/, vendor-bestanden, gitignored — vers uit een Sipeed-release te halen).

TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"
. "$DIR/image/lib.sh"

# De ingebakken blobs zijn build-input, geen bronbestand: na deze build horen ze
# weg te zijn (ook als hij halverwege faalt). Op dít board weegt dat het zwaarst:
# de config die hier belandt kan een échte apikey bevatten, hij is gitignored (dus
# `git status` zwijgt erover) en zonder opruimen bouwt een volgende build hem
# ongemerkt in een ánder image in.
trap clean_embeds EXIT INT TERM

OUT="$DIR/metal/out"
DONOR="${LICHEERV_DONOR:-$DIR/image/licheerv/donor-fip.bin}"
FIPTOOL="${LICHEERV_FIPTOOL:-$DIR/image/licheerv/fiptool.py}"

# LET OP — MONITOR_RUNADDR is NIET DRAM-start (GEMETEN 30-07): de FSBL laadt ná
# ons image ook LOADER_2ND (U-Boot) en DECOMPRIMEERT dat naar 0x80200020
# (~600KB). Een image dat over 0x80200000 heen komt wordt daar stil overschreven
# — de Go-runtime valt dan over een kapotte pointer in .data. LOADER_2ND
# weglaten kan niet (de FSBL panict na zijn retries), dus we leggen ons image
# erbóven. Alles onder RUNADDR is vuil gebied.
# 0x84000000 en niet 0x83000000: HOP heeft 64MB (was 80), en de 16MB die
# vrijkomt hangt ONDER dit adres — daar groeit pool B naar 64MB. SlotBase
# (0x88000000) blijft staan, want dat is het linkadres van elk app-image.
RUNADDR=0x84000000
SLOTBASE=0x88000000	# board/licheerv: SlotBase (de app-partitie)

mkdir -p "$OUT"

[ -x "$TAMAGO" ] || { echo "tamago-go niet gevonden op $TAMAGO (zet TAMAGO)" >&2; exit 1; }
command -v riscv64-elf-as >/dev/null || { echo "riscv64-elf-as ontbreekt (riscv64 binutils)" >&2; exit 1; }
[ -f "$DONOR" ] || { echo "donor-fip ontbreekt: $DONOR (zet LICHEERV_DONOR)" >&2; exit 1; }
[ -f "$FIPTOOL" ] || { echo "fiptool ontbreekt: $FIPTOOL (zet LICHEERV_FIPTOOL)" >&2; exit 1; }

rv() { # rv <tags> <ldflags> <out> <pkg>
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=riscv64 \
		"$TAMAGO" build -tags "$1 nodefaultstack" -trimpath -ldflags "$2" -o "$3" "$4"
}

cd "$DIR/metal"

# 1. De kooi-stub van het slot: HOP zet hem vóór élke app op de partitie
#    (kern/cagestub); de kooi-waarden zet HOP er runtime in (kern/cage rekent
#    ze uit).
echo "== kooi-stub ==" >&2
riscv64-elf-as -march=rv64imac_zicsr -o "$OUT/stub-slot.o" "$DIR/image/licheerv/stub-slot/stub-slot.S"
riscv64-elf-ld -Ttext=$SLOTBASE -o "$OUT/stub-slot.elf" "$OUT/stub-slot.o"
riscv64-elf-objcopy -O binary "$OUT/stub-slot.elf" "$OUT/stub-slot.bin"

# 2. De HOP-kern zelf.
# De platform-config gaat mee ín het image: dit board kan zijn eigen
# FAT-bootpartitie niet lezen (geen SDHCI/FAT-driver — de FSBL las de kaart,
# wij niet), dus is er geen hopos.cfg om naast fip.bin te zetten. De default
# is DEZELFDE headless-template als elk ander board — er zijn precies twee
# default-configs in deze repo (gui en headless), en dit board is headless.
# Zonder hopos.node valt de MAC-afleiding terug op het ingebouwde adres en
# zegt de node dat LUID (HOPOS_MAC_FIXED) — eerlijker dan een ingebakken
# naam die elk bordje stil hetzelfde adres geeft. Eigen config (node-naam,
# sleutels)? CFG=~/mijn-node.cfg — die blijft buiten de repo.
CFG="${CFG:-$DIR/image/hopos-headless.cfg}"
[ -f "$CFG" ] || { echo "config ontbreekt: $CFG" >&2; exit 1; }
cp "$CFG" "$DIR/metal/cmd/hopos/cfgblob/hopos.cfg"
# De config wordt een raw-patchbaar venster (image/hopcfg), net als op de
# FAT-boards — maar hier zit hij ín de kernel (go:embed, ongecomprimeerd) en
# dus in de monitor-payload van de FIP; hopcfg werkt daar de FIP-checksums bij.
# 64KiB en niet de 1MiB van de kaarten: dit venster woont permanent in het
# krappe RAM van dit board, en 64KiB is al ~10x de config.
go run "$DIR/image/hopcfg/main.go" pad -window 65536 "$DIR/metal/cmd/hopos/cfgblob/hopos.cfg" >&2
# De kooi-stub gaat mee ín de kern: HOP zet hem vóór élke app op de
# partitie (kern/cagestub). Op ARM is dat de EL2-trampoline in HOP's eigen
# image; hier draait het op het app-hart, dus moet het meeliften.
cp "$OUT/stub-slot.bin" "$DIR/metal/kern/cagestub/stub-slot.bin"
echo "== HOP-kern bouwen (agent: cmd/hopos -tags licheerv,embedcfg,embedcagestub; config $(basename "$CFG")) ==" >&2
rv "licheerv embedcfg embedcagestub" "-s -w -T $((RUNADDR + 0x10000)) -R 0x1000" "$OUT/hopos-lrv.elf" ./cmd/hopos

# 3. De trapstub voor HOP's eigen hart: T-Head-CPU-init (die erven we van de
#    vendor-OpenSBI die we vervangen), BSS nullen, en een vroege trap-vector die
#    mcause/mepc/mtval op de UART dumpt — het meetinstrument van de bring-up.
echo "== trapstub + monitor-blob ==" >&2
riscv64-elf-as -march=rv64imac_zicsr -o "$OUT/trapstub.o" "$DIR/image/licheerv/trapstub/trapstub.S"
riscv64-elf-ld -Ttext=$RUNADDR -o "$OUT/trapstub.elf" "$OUT/trapstub.o"
riscv64-elf-objcopy -O binary "$OUT/trapstub.elf" "$OUT/trapstub.bin"
go run "$DIR/image/licheerv/mkmonitor/main.go" -base $RUNADDR \
	"$OUT/hopos-lrv.elf" "$OUT/monitor.bin" "$OUT/trapstub.bin"

# 4. fip.bin: donor (FSBL + DDR-params) met ons monitor-slot.
#    fiptool neemt MONITOR_RUNADDR uit de OLD_FIP en valt alleen terug op de
#    CLI-vlag als dat veld NUL is (fiptool.py: "if not runaddr"). De donor zegt
#    DRAM-start, dus nullen we het in een kopie — daarna pakt hij $RUNADDR.
#    param2-indeling staat vast: magic "CVLD02\n\0", MONITOR_RUNADDR op +60.
echo "== fip-licheerv.bin ==" >&2
python3 - "$DONOR" "$OUT/donor-runaddr0.bin" <<'PYEOF'
import struct, sys
d = bytearray(open(sys.argv[1], "rb").read())
hits, i = [], 0
while True:
    i = d.find(b"CVLD02\n\x00", i)
    if i < 0:
        break
    ra = struct.unpack_from("<I", d, i + 60)[0]
    if 0x80000000 <= ra < 0x90000000:   # plausibele DRAM-runaddr = het echte blok
        hits.append(i)
    i += 1
if len(hits) != 1:
    sys.exit(f"fip: {len(hits)} param2-blokken gevonden, verwacht 1")
struct.pack_into("<I", d, hits[0] + 60, 0)
open(sys.argv[2], "wb").write(bytes(d))
PYEOF
python3 "$FIPTOOL" genfip "$OUT/fip-licheerv.bin" \
	--OLD_FIP "$OUT/donor-runaddr0.bin" \
	--MONITOR "$OUT/monitor.bin" \
	--MONITOR_RUNADDR "$RUNADDR" 2>/dev/null

echo "metal/out/fip-licheerv.bin ($(du -h "$OUT/fip-licheerv.bin" | cut -f1)) klaar." >&2

# 5. Het complete kaart-image: MBR + FAT16 + fip.bin, dd-baar. Zelf gebouwd
#    (image/mkcard, gedeeld met de Radxa en de Pi's) zodat het reproduceerbaar
#    is en geen root of loop-device vraagt — de geometrie is die van het
#    donor-image, want dat is wat de BROM van dit silicium aantoonbaar leest.
#    GEEN -vollabel hier: de BROM-parser is niet van ons, en dit image is
#    bewezen zoals het is.
go run "$DIR/image/mkcard/main.go" -o "$OUT/hopos-licheerv.img" \
	-size 64 "$OUT/fip-licheerv.bin=fip.bin"
echo "flash: diskutil unmountDisk /dev/diskN && sudo dd if=$OUT/hopos-licheerv.img of=/dev/rdiskN bs=4m" >&2

# 6. Optioneel: op de kaart zetten. Standaard mounten en het echte mountpoint
#    bij diskutil opvragen — een eigen mountpoint meegeven rapporteert op macOS
#    "mounted" terwijl er niets gekoppeld is, en dan landt de cp stil op de Mac
#    (gemeten 30-07). Vandaar ook het teruglees-bewijs.
DISK="$1"
[ -n "$DISK" ] || { echo "flash: image/licheerv-agent.sh /dev/diskN" >&2; exit 0; }
diskutil mount "${DISK}s1" >/dev/null
MNT=$(diskutil info "${DISK}s1" | sed -n 's/.*Mount Point: *//p')
[ -n "$MNT" ] && mount | grep -q " on $MNT " || {
	echo "WEIGER: ${DISK}s1 is niet echt gemount (mountpoint: '$MNT')" >&2; exit 1; }
# Ruimte: de FAT-bootpartitie is ~16MB en het Sipeed donor-image zet daar de
# vendor-Linux-kernel (boot.sd, ~12MB) neer. Die booten we nooit — ons image
# vervangt de héle keten — maar hij vreet wel de ruimte die de agent nodig heeft.
# Zonder deze check kapt cp het bestand halverwege af en boot het bordje niet
# meer (gemeten 30-07). Dus: eerst opruimen, luid en met naam.
# De oude fip eerst weg: hij bezet de ruimte die de nieuwe nodig heeft (FAT
# alloceert vóór het vrijgeven, dus overschrijven-in-plaats werkt niet als het
# bestand groter is dan de helft van de vrije ruimte).
rm -f "$MNT/fip.bin"
sync
NEED=$(stat -f %z "$OUT/fip-licheerv.bin")
FREE=$(df -k "$MNT" | awk 'NR==2 {print $4 * 1024}')
if [ "$NEED" -gt "$FREE" ] && [ -f "$MNT/boot.sd" ]; then
	echo "boot.sd ($(du -h "$MNT/boot.sd" | cut -f1), vendor-Linux-kernel) weghalen — HopOS boot hem niet" >&2
	rm -f "$MNT/boot.sd"
	sync
	FREE=$(df -k "$MNT" | awk 'NR==2 {print $4 * 1024}')
fi
[ "$NEED" -le "$FREE" ] || {
	echo "WEIGER: fip is $NEED bytes, maar er is $FREE vrij op ${DISK}s1" >&2
	diskutil unmount "${DISK}s1" >/dev/null; exit 1; }
cp "$OUT/fip-licheerv.bin" "$MNT/fip.bin"
sync
WANT=$(stat -f %z "$OUT/fip-licheerv.bin"); GOT=$(stat -f %z "$MNT/fip.bin")
[ "$WANT" = "$GOT" ] || { echo "FOUT: teruglezen faalt ($GOT != $WANT bytes)" >&2; exit 1; }
diskutil unmount "${DISK}s1" >/dev/null
echo "fip.bin op ${DISK}s1 ($WANT bytes, geverifieerd) — kaart kan in de LicheeRV." >&2
