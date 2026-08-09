#!/bin/sh
# Bouw de ECHTE HOP-agent voor de Raspberry Pi 4 (metal/cmd/hopos + rpi4-
# board): dezelfde hop/pkg/agentboot-bytes als op de Pi 5/QEMU/Linux, met de
# GENET-NIC (metal/driver/nic/genet). Boot-recept identiek aan rpi4-hopos.sh (raw
# kernel8.img op 0x80000, TF-A bl31.bin als armstub).
#
# Na de boot (UART meldt het IP):
#   agent:  curl http://<ip>:8080/health
#   leader: curl http://<ip>:9080/health

set -e

TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"
. "$DIR/image/lib.sh"

# De ingebakken blobs zijn build-input, geen bronbestand: na deze build horen ze
# weg te zijn (ook als hij halverwege faalt). Anders bouwt een volgende build
# ongemerkt met de resten van deze mee.
trap clean_embeds EXIT INT TERM

cd "$DIR/metal"
mkdir -p out

# 1. De app-image voor jobs: canoniek gelinkt (slot-1-IPA), hopslot-hooks
#    (board-onafhankelijk — zelfde binary als op de Pi 5/QEMU/Altra).
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags "linkcpuinit nodefaultstack" -trimpath \
	-ldflags "-w -T 0x50010000 -R 0x1000" -o out/app4.elf ./app/appspike

# 2. De agent-kern: cmd/hopos met het rpi4-board (build-tag kiest board_rpi4.go)
#    Default gui; GUI=0 bouwt de
#    kale (headless) smaak. (Zelfde knop in alle imagescripts.)
GUITAG=""
[ "${GUI:-1}" = 1 ] && GUITAG=" gui"
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags "rpi4 linkcpuinit$GUITAG nodefaultstack" -trimpath \
	-ldflags "-s -w -T 0x90000 -R 0x1000" -o out/agent4.elf ./cmd/hopos

# 3. ELF → raw kernel8.img.
cd "$DIR"
mkdir -p sd-rpi4
go run "$DIR/image/mkkernel/main.go" "$DIR/image/mkkernel/pe.go" -elf metal/out/agent4.elf -o sd-rpi4/kernel8.img -load 0x80000 -raw

# 4. config.txt — zelfde poortwachters als rpi4-hopos.sh.
cat > sd-rpi4/config.txt <<'EOF'
# HopOS: de echte HOP-agent (fase P2) — Raspberry Pi 4 (zie docs/archief/rpi4.md)
arm_64bit=1
kernel=kernel8.img
device_tree_address=0x0f000000
# TF-A BL31 als armstub: levert PSCI. De stock armstub8 heeft dat NIET.
armstub=bl31.bin
uart_2ndstage=1
# Houd de PL011 bij GPIO14/15 (anders claimt Bluetooth hem).
dtoverlay=disable-bt
# HOP-config als bestand (zelfde recept als de Pi 5): de firmware laadt
# hopos.cfg integraal in RAM ("initramfs", adres boven de DTB op 0x0f000000)
# en HOP leest hem via /chosen/linux,initrd-*. Volle JSON-jobspecs per regel.
initramfs hopos.cfg 0x0f200000
EOF

# config.txt laadt hopos.cfg verplicht en dát bestand staat in .gitignore
# (het bevat de API-key en de S3-geheimen). Luid falen mét het recept,
# zelfde poortwachter als rpi5-agent.sh.
if [ ! -f "$DIR/sd-rpi4/hopos.cfg" ]; then
	echo "" >&2
	echo "FOUT: sd-rpi4/hopos.cfg ontbreekt — config.txt laadt hem verplicht." >&2
	echo "  cp image/hopos-gui.cfg sd-rpi4/hopos.cfg" >&2
	echo "  \$EDITOR sd-rpi4/hopos.cfg   # minimaal hopos.apikey zetten" >&2
	echo "" >&2
	exit 1
fi

echo "sd-rpi4/kernel8.img (HOP-agent, $(du -h sd-rpi4/kernel8.img | cut -f1)) + config.txt + hopos.cfg klaar." >&2
echo "flash: cp sd-rpi4/kernel8.img sd-rpi4/config.txt sd-rpi4/hopos.cfg '/Volumes/NO NAME/'" >&2

# 5. Het complete, dd-bare kaart-image (image/mkcard — zelfde vorm als de
#    LicheeRV en de Radxa): MBR + FAT16 met de firmware er al op, dus gunzip|dd
#    en de kaart boot — geen bestaande bootfs meer nodig. De firmware komt uit
#    sd-rpi4/ (gitignored; herkomst + bl31-bouwrecept in sd-rpi4/LEESMIJ.txt);
#    ontbreekt er iets, dan slaan we dit LUID over en is de cp-flow hierboven
#    gewoon compleet. De config in het image is ALTIJD een template (of
#    CFG=...): nooit sd-rpi4/hopos.cfg, daar wonen de echte sleutels.
rm -f metal/out/hopos-rpi4.img
FW_MISSING=""
for f in start4.elf fixup4.dat bcm2711-rpi-4-b.dtb bl31.bin; do
	[ -f "sd-rpi4/$f" ] || FW_MISSING="$FW_MISSING $f"
done
if [ -z "$FW_MISSING" ]; then
	DEFCFG="$DIR/image/hopos-headless.cfg"
	[ "${GUI:-1}" = 1 ] && DEFCFG="$DIR/image/hopos-gui.cfg"
	go run "$DIR/image/mkcard/main.go" -o metal/out/hopos-rpi4.img -size 64 \
		-start 8192 -label bootfs -vollabel -cfgwindow 1048576 \
		sd-rpi4/kernel8.img sd-rpi4/config.txt "${CFG:-$DEFCFG}=hopos.cfg" \
		sd-rpi4/start4.elf sd-rpi4/fixup4.dat sd-rpi4/bcm2711-rpi-4-b.dtb \
		sd-rpi4/bl31.bin >&2
	echo "metal/out/hopos-rpi4.img (dd-baar, config = $(basename "${CFG:-$DEFCFG}"))" >&2
	echo "flash: diskutil unmountDisk /dev/diskN && sudo dd if=metal/out/hopos-rpi4.img of=/dev/rdiskN bs=4m" >&2
else
	echo "GEEN dd-baar image gebouwd — mist in sd-rpi4/:$FW_MISSING (zie sd-rpi4/LEESMIJ.txt)" >&2
fi
