#!/bin/sh
# Bouw de SD-kaart van HopOS v3 voor de Raspberry Pi 4 (board/rpi4):
# kernel8.img, config.txt, cmdline.txt en het image van Hop, en als de
# firmware er ligt het complete, dd-bare kaart-image.
#
#   image/rpi4.sh                    Hop als bewoner (standaard)
#   APP=appspike image/rpi4.sh       appspike, twee keer door de kern geplaatst
#   APP=/pad/naar/elf image/rpi4.sh  een kant-en-klare ELF (rol app)
#   APP= image/rpi4.sh               zonder image
#   GUI=1 image/rpi4.sh              de gui-smaak (`--features gui`, docs/gui.md):
#                                    de console op het glas via de VideoCore-
#                                    mailbox, de VL805 over PCIe voor de invoer, en de
#                                    framebuffer-grant aan een display-app
#   FW=pad image/rpi4.sh             waar start4.elf, fixup4.dat,
#                                    bcm2711-rpi-4-b.dtb en bl31.bin liggen
#                                    (standaard image/firmware/rpi4; herkomst
#                                    en het bl31-bouwrecept in de LEESMIJ.txt
#                                    daar)
#   CFG=pad image/rpi4.sh            de config van de node (standaard
#                                    image/cfg/hop-config-headless.cfg; met
#                                    GUI=1 hoort hop-config-headfull.cfg
#                                    erbij): elke regel wordt een token in
#                                    cmdline.txt
#   EXTRA="hopos.node=..." image/... meer tokens, vóór die van CFG (de
#                                    eerste waarde wint)
#
# Het boot-recept is dat van de Go-generatie (image/rpi4-agent.sh op tag
# v2.2.8): de firmware laadt kernel8.img RAUW op 0x80000 (geen arm64-Image-header, het
# bewezen pad), de DTB op 0x0f000000, en TF-A bl31.bin als armstub: de stock
# armstub8 heeft GEEN PSCI, en de eerste CPU_ON zou in een lege EL3-vector
# hangen. Nieuw in v3: het image van Hop gaat als `initramfs` op 0x0f200000
# de kaart op (board/raspi/src/map.rs: het laadvenster), en de rol staat in
# cmdline.txt (`hopos.stage=hop|app`).
#
# Uitvoer: target/sd-rpi4/ (de losse bestanden, voor een bestaande
# FAT-kaart) en target/hopos-rpi4.img (MBR + FAT16, dd-baar).
set -e

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TARGET=aarch64-unknown-none-softfloat
HOP_DIR="${HOP_DIR:-$DIR/../hop/hop}"
FW="${FW:-$DIR/image/firmware/rpi4}"
OUT="$DIR/target/sd-rpi4"
CARD="$DIR/target/hopos-rpi4.img"
STAGE_MAX=14680064 # 0x1000_0000 - 0x0F20_0000 (board_raspi::map::STAGE_MAX)

cd "$DIR"
FEATURE=board-rpi4
if [ "${GUI:-0}" = 1 ]; then
	FEATURE="$FEATURE,gui"
fi
cargo build --quiet --release --target "$TARGET" -p hopos --features "$FEATURE"
OBJCOPY="$(ls "$(rustc --print sysroot)"/lib/rustlib/*/bin/rust-objcopy 2>/dev/null | head -1)"
if [ -z "$OBJCOPY" ]; then
	echo "rpi4: rust-objcopy missing (rustup component add llvm-tools)" >&2
	exit 1
fi
rm -rf "$OUT"
mkdir -p "$OUT"
"$OBJCOPY" -O binary "$DIR/target/$TARGET/release/hopos" "$OUT/kernel8.img"

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
		echo "rpi4: $IMAGE is $SIZE bytes, the loader window holds $STAGE_MAX" >&2
		exit 1
	fi
	INITRAMFS="initramfs hop.elf 0x0f200000"
fi

cat >"$OUT/config.txt" <<EOF
# HopOS v3 - Raspberry Pi 4 (image/rpi4.sh, docs/boards-pi.md)
arm_64bit=1
kernel=kernel8.img
device_tree_address=0x0f000000
# TF-A BL31 als armstub: levert PSCI. De stock armstub8 heeft dat NIET.
armstub=bl31.bin
enable_uart=1
uart_2ndstage=1
# Houd de PL011 bij GPIO14/15 (anders claimt Bluetooth hem).
dtoverlay=disable-bt
# Het image van Hop in het laadvenster (board/raspi/src/map.rs).
$INITRAMFS
EOF
# De config: de Pi leest hem uit /chosen/bootargs (board/raspi/src/cfg.rs),
# dus elke regel van CFG wordt een token in cmdline.txt, na hopos.stage en
# EXTRA (de eerste waarde wint). Een bootarg heeft geen spatie: een waarde
# met een spatie weigeren, anders knipt de Pi hem stil in tweeën.
CFG="${CFG:-$DIR/image/cfg/hop-config-headless.cfg}"
[ -f "$CFG" ] || {
	echo "rpi4: CFG=$CFG does not exist" >&2
	exit 1
}
TOKENS="$(sed -e 's/\r$//' -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' -e '/^#/d' -e '/^$/d' "$CFG")"
if printf '%s\n' "$TOKENS" | grep -q '[[:space:]]'; then
	echo "rpi4: $CFG has a value with a space; on the Pi every line is one cmdline token" >&2
	exit 1
fi
echo "hopos.stage=${ROLE:-hop}${EXTRA:+ $EXTRA} $(printf '%s' "$TOKENS" | tr '\n' ' ')" >"$OUT/cmdline.txt"

echo "rpi4: $OUT/kernel8.img ($(wc -c <"$OUT/kernel8.img" | tr -d ' ') bytes), config.txt, cmdline.txt${IMAGE:+, hop.elf ($SIZE bytes, role $ROLE)}" >&2

# Het kaart-image: de firmware erbij, en een FAT die de Pi-firmware leest.
# tools/mkcard is de port van het bewezen gereedschap van de Go-generatie
# (MBR + FAT16 met LFN, reproduceerbaar, geen mount); ontbreekt de
# firmware, dan LUID overslaan en is target/sd-rpi4/ plus de firmware op een
# bestaande kaart de weg.
rm -f "$CARD"
MISSING=""
for f in start4.elf fixup4.dat bcm2711-rpi-4-b.dtb bl31.bin; do
	if [ -f "$FW/$f" ]; then cp "$FW/$f" "$OUT/"; else MISSING="$MISSING $f"; fi
done
if [ -n "$MISSING" ]; then
	echo "rpi4: NO card image, missing in $FW:$MISSING (see image/firmware/rpi4/LEESMIJ.txt)" >&2
else
	set -- "$OUT/kernel8.img" "$OUT/config.txt" "$OUT/cmdline.txt" \
		"$OUT/start4.elf" "$OUT/fixup4.dat" "$OUT/bcm2711-rpi-4-b.dtb" "$OUT/bl31.bin"
	[ -f "$OUT/hop.elf" ] && set -- "$@" "$OUT/hop.elf"
	cargo run -q -p mkcard -- -o "$CARD" -size 64 -start 8192 -label bootfs -vollabel "$@" >&2
	echo "rpi4: $CARD (dd: diskutil unmountDisk /dev/diskN && sudo dd if=$CARD of=/dev/rdiskN bs=4m)" >&2
fi
