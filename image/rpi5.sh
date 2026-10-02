#!/bin/sh
# Bouw de SD-kaart van HopOS v3 voor de Raspberry Pi 5 (board/rpi5):
# hop-agent5.img, config.txt, cmdline.txt en het image van Hop, en als de
# DTB er ligt het complete, dd-bare kaart-image.
#
#   image/rpi5.sh                    Hop als bewoner (standaard)
#   APP=appspike image/rpi5.sh       appspike, twee keer door de kern geplaatst
#   APP=/pad/naar/elf image/rpi5.sh  een kant-en-klare ELF (rol app)
#   APP= image/rpi5.sh               zonder image
#   GUI=1 image/rpi5.sh              de gui-smaak (`--features gui`, docs/gui.md):
#                                    de console op het glas via de VideoCore-
#                                    mailbox, de RP1-xHCI's voor de invoer, en de
#                                    framebuffer-grant aan een display-app
#   FW=pad image/rpi5.sh             waar bcm2712-rpi-5-b.dtb en
#                                    overlays/bcm2712d0.dtbo liggen (standaard
#                                    image/firmware/rpi5; herkomst in de
#                                    LEESMIJ.txt daar)
#   CFG=pad image/rpi5.sh            de config van de node (standaard
#                                    image/cfg/hop-config-headless.cfg; met
#                                    GUI=1 hoort hop-config-headfull.cfg
#                                    erbij): elke regel wordt een token in
#                                    cmdline.txt
#   EXTRA="hopos.node=..." image/... meer tokens, vóór die van CFG (de
#                                    eerste waarde wint)
#
# Het boot-recept is dat van de Go-generatie (image/rpi5-agent.sh op tag
# v2.2.8): de Pi 5 heeft geen start*.elf (de firmware zit in de EEPROM), laadt het image
# RAUW op 0x80000 (hij negeert kernel_address, gemeten 09-07) en eist
# `os_check=0`; zonder passende DTB weigert hij te booten. De armstub van de
# EEPROM (TF-A BL31) levert PSCI. Nieuw in v3: het image van Hop gaat als
# `initramfs` op 0x0f200000 de kaart op (board/raspi/src/map.rs: het
# laadvenster), en de rol staat in cmdline.txt (`hopos.stage=hop|app`).
#
# Uitvoer: target/sd-rpi5/ (de losse bestanden, voor een bestaande
# FAT-kaart) en target/hopos-rpi5.img (MBR + FAT16, dd-baar).
set -e

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TARGET=aarch64-unknown-none-softfloat
HOP_DIR="${HOP_DIR:-$DIR/../hop/hop}"
FW="${FW:-$DIR/image/firmware/rpi5}"
OUT="$DIR/target/sd-rpi5"
CARD="$DIR/target/hopos-rpi5.img"
STAGE_MAX=14680064 # 0x1000_0000 - 0x0F20_0000 (board_raspi::map::STAGE_MAX)

cd "$DIR"
FEATURE=board-rpi5
if [ "${GUI:-0}" = 1 ]; then
	FEATURE="$FEATURE,gui"
fi
cargo build --quiet --release --target "$TARGET" -p hopos --features "$FEATURE"
OBJCOPY="$(ls "$(rustc --print sysroot)"/lib/rustlib/*/bin/rust-objcopy 2>/dev/null | head -1)"
if [ -z "$OBJCOPY" ]; then
	echo "rpi5: rust-objcopy missing (rustup component add llvm-tools)" >&2
	exit 1
fi
rm -rf "$OUT"
mkdir -p "$OUT"
"$OBJCOPY" -O binary "$DIR/target/$TARGET/release/hopos" "$OUT/hop-agent5.img"

# Het image voor de staging: zelfde keuzes als image/qemu-run.sh.
APP="${APP-hop}"
IMAGE=""
case "$APP" in
"") ;;
hop)
	# Tegen de applib van deze werkboom (tools/hop-build.sh, HOP_PATCH=0
	# voor de tag van de hop-repo): de stage-1 en de vectortabel van
	# applib (30-09, de eerste Pi 5-boot) moeten in Hop zitten.
	IMAGE="$(HOP_DIR="$HOP_DIR" sh "$DIR/tools/hop-build.sh" "$TARGET")"
	ROLE="${ROLE:-hop}"
	;;
*/*)
	IMAGE="$APP"
	ROLE="${ROLE:-app}"
	;;
*)
	cargo build --quiet --release --target "$TARGET" -p "$APP"
	IMAGE="$DIR/target/$TARGET/release/$APP"
	ROLE="${ROLE:-app}"
	;;
esac
INITRAMFS=""
if [ -n "$IMAGE" ]; then
	"$OBJCOPY" --strip-debug "$IMAGE" "$OUT/hop.elf"
	SIZE=$(wc -c <"$OUT/hop.elf" | tr -d ' ')
	if [ "$SIZE" -gt "$STAGE_MAX" ]; then
		echo "rpi5: $IMAGE is $SIZE bytes, the loader window holds $STAGE_MAX" >&2
		exit 1
	fi
	INITRAMFS="initramfs hop.elf 0x0f200000"
fi

cat >"$OUT/config.txt" <<EOF
# HopOS v3 - Raspberry Pi 5 (image/rpi5.sh, docs/boards-pi.md)
arm_64bit=1
kernel=hop-agent5.img
os_check=0
device_tree_address=0x0f000000
uart_2ndstage=1
# De stil-vloer van het klokbeleid (board/raspi/src/clock.rs vraagt het
# minimum op en volgt): zonder deze regel klemt de Pi 5-firmware de vloer op
# 1500 MHz (Go, gemeten 11-07), gelijk aan arm_freq hieronder, en dan is er
# niets te draaien (HOPOS_CLOCK_NONE, "one ARM clock").
arm_freq_min=800
# Thermische cap voor fanloos bedrijf (arm_freq is het max; arm_freq_max
# bestaat niet, gemeten 11-07). 2400 MHz liep zonder fan binnen minuten naar
# 84 C; het klokbeleid volgt dit maximum vanzelf.
arm_freq=1500
# Het image van Hop in het laadvenster (board/raspi/src/map.rs).
$INITRAMFS
EOF
# De config: de Pi leest hem uit /chosen/bootargs (board/raspi/src/cfg.rs),
# dus elke regel van CFG wordt een token in cmdline.txt, na hopos.stage en
# EXTRA (de eerste waarde wint). Een bootarg heeft geen spatie: een waarde
# met een spatie weigeren, anders knipt de Pi hem stil in tweeën.
CFG="${CFG:-$DIR/image/cfg/hop-config-headless.cfg}"
[ -f "$CFG" ] || {
	echo "rpi5: CFG=$CFG does not exist" >&2
	exit 1
}
TOKENS="$(sed -e 's/\r$//' -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' -e '/^#/d' -e '/^$/d' "$CFG")"
if printf '%s\n' "$TOKENS" | grep -q '[[:space:]]'; then
	echo "rpi5: $CFG has a value with a space; on the Pi every line is one cmdline token" >&2
	exit 1
fi
echo "hopos.stage=${ROLE:-hop}${EXTRA:+ $EXTRA} $(printf '%s' "$TOKENS" | tr '\n' ' ')" >"$OUT/cmdline.txt"

echo "rpi5: $OUT/hop-agent5.img ($(wc -c <"$OUT/hop-agent5.img" | tr -d ' ') bytes), config.txt, cmdline.txt${IMAGE:+, hop.elf ($SIZE bytes, role $ROLE)}" >&2

# Het kaart-image: de firmware erbij, en een FAT die de Pi-firmware leest.
# tools/mkcard is de port van het bewezen gereedschap van de Go-generatie
# (MBR + FAT16 met LFN, reproduceerbaar, geen mount); ontbreekt de
# firmware, dan LUID overslaan en is target/sd-rpi5/ plus de firmware op een
# bestaande kaart de weg.
rm -f "$CARD"
MISSING=""
mkdir -p "$OUT/overlays"
for f in bcm2712-rpi-5-b.dtb overlays/bcm2712d0.dtbo; do
	if [ -f "$FW/$f" ]; then cp "$FW/$f" "$OUT/$f"; else MISSING="$MISSING $f"; fi
done
if [ -n "$MISSING" ]; then
	echo "rpi5: NO card image, missing in $FW:$MISSING (see image/firmware/rpi5/LEESMIJ.txt)" >&2
else
	set -- "$OUT/hop-agent5.img" "$OUT/config.txt" "$OUT/cmdline.txt" \
		"$OUT/bcm2712-rpi-5-b.dtb" "$OUT/overlays/bcm2712d0.dtbo=overlays/bcm2712d0.dtbo"
	[ -f "$OUT/hop.elf" ] && set -- "$@" "$OUT/hop.elf"
	cargo run -q -p mkcard -- -o "$CARD" -size 64 -start 8192 -label bootfs -vollabel "$@" >&2
	echo "rpi5: $CARD (dd: diskutil unmountDisk /dev/diskN && sudo dd if=$CARD of=/dev/rdiskN bs=4m)" >&2
fi
