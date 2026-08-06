#!/bin/sh
# Bouw de ECHTE HOP-agent voor de Raspberry Pi 5 (metal/cmd/hopos + rpi5-
# board): dezelfde hop/pkg/agentboot-bytes als op Linux/macOS/QEMU, bare-metal
# op het board. Netwerk = HOP's eigen keten (PCIe-RC → RP1 → GEM → DHCP, P2).
# Boot-recept identiek aan rpi5-hopos.sh (raw op 0x80000, os_check=0).
#
# Na de boot (UART meldt het IP):
#   agent:  curl http://<ip>:8080/health
#   leader: curl http://<ip>:9080/health
# Job submitten vanaf de Mac (metal/out/app5.elf serveren met python3 -m http.server):
#   curl -X POST http://<ip>:9080/v1/jobs -d '{"name":"werkje","driver":"hop",
#     "artifacts":[{"url":"http://<mac-ip>:8000/app5.elf"}],
#     "memory_limit":100663296}'

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
#    (board-onafhankelijk — zelfde binary als op de Pi 4/QEMU/Altra).
#    Zonder -s: slots patcht RamStart/RamSize via de symboltabel.
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags linkcpuinit -trimpath \
	-ldflags "-w -T 0x50010000 -R 0x1000" -o out/app5.elf ./app/appspike

# 1b. De universele apploader op zijn go:embed-plek (recept in image/lib.sh).
bake_apploader arm64 linkcpuinit 0x50010000

# 2. De agent-kern: cmd/hopos met het rpi5-board (build-tag kiest board_rpi5.go)
#    + de ingebakken apploader (embedloader). Twee smaken per board: kaal
#    (headless) en gui (metal/gui: HVS-dumptool + :9091-debug + fb-grant).
#    Default gui; GUI=0 bouwt de kale smaak. (Zelfde knop in alle imagescripts.)
GUITAG=""
[ "${GUI:-1}" = 1 ] && GUITAG=" gui"
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags "rpi5 linkcpuinit embedloader$GUITAG" -trimpath \
	-ldflags "-s -w -T 0x90000 -R 0x1000" -o out/agent5.elf ./cmd/hopos

# 3. ELF → raw image (Circle-recept, mkkernel).
cd "$DIR"
mkdir -p sd-rpi5
go run "$DIR/image/mkkernel/main.go" "$DIR/image/mkkernel/pe.go" -elf metal/out/agent5.elf -o sd-rpi5/hop-agent5.img -load 0x80000 -raw

# 4. config.txt — zelfde poortwachters, kernel wijst naar de agent.
cat > sd-rpi5/config.txt <<'EOF'
# HopOS: de echte HOP-agent (fase P2) — Raspberry Pi 5 (zie docs/archief/rpi5.md)
arm_64bit=1
kernel=hop-agent5.img
os_check=0
device_tree_address=0x0f000000
uart_2ndstage=1
# Lagere idle-vloer voor het dvfs-klokbeleid (metal/driver/dvfs vraagt de min op
# en volgt): zonder deze regel klemt de Pi 5-firmware op 1500MHz (gemeten
# 2026-07-11). Accepteert de firmware 800 niet, dan meldt de dvfs-regel dat.
arm_freq_min=800
# Thermische cap voor fanloos bedrijf (turbo-ceiling; arm_freq_max bestaat
# NIET, arm_freq ís het max — gemeten 2026-07-11). dvfs volgt dit firmware-
# max vanzelf via de mailbox: 2400MHz liep zonder fan binnen minuten naar 84°C.
arm_freq=1500
# 32-bpp framebuffer: terug sinds 04-08 — de 19-07-freezes die 16-bpp
# motiveerden bleken bij de OOM-ontknoping software (A/B-storm: 0/100 zonder
# één mitigatie, mét memlimit); de meting die dit mocht terugzetten is gedaan.
framebuffer_depth=32
framebuffer_ignore_alpha=1
# HOP-config als bestand (19-07): de firmware laadt hopos.cfg integraal in
# RAM ("initramfs"-mechanisme, adres boven de DTB op 0x0f000000) en HOP leest
# hem via /chosen/linux,initrd-*. Volle JSON-jobspecs per regel — het
# 1024-byte-bootargs-plafond van cmdline.txt geldt hier niet.
initramfs hopos.cfg 0x0f200000
EOF

# config.txt laadt hopos.cfg verplicht (`initramfs hopos.cfg`) en dát bestand
# staat in .gitignore (het bevat de API-key en de S3-geheimen). Een verse clone
# heeft hem dus niet — en zonder config boot de node zonder API-auth-sleutel en
# zonder init-jobs. Luid falen mét het recept, i.p.v. een stick afleveren die
# stil half werkt.
if [ ! -f "$DIR/sd-rpi5/hopos.cfg" ]; then
	echo "" >&2
	echo "FOUT: sd-rpi5/hopos.cfg ontbreekt — config.txt laadt hem verplicht." >&2
	echo "  cp image/hopos-gui.cfg sd-rpi5/hopos.cfg" >&2
	echo "  \$EDITOR sd-rpi5/hopos.cfg   # minimaal hopos.apikey zetten" >&2
	echo "" >&2
	exit 1
fi

echo "sd-rpi5/hop-agent5.img ($(du -h sd-rpi5/hop-agent5.img | cut -f1)) + config.txt + hopos.cfg klaar." >&2
echo "flash: cp sd-rpi5/hop-agent5.img sd-rpi5/config.txt sd-rpi5/hopos.cfg /Volumes/bootfs/ && sync && diskutil eject" >&2

# 5. Het complete, dd-bare kaart-image (image/mkcard — zelfde vorm als de
#    LicheeRV en de Radxa): MBR + FAT16 met de firmware er al op, dus gunzip|dd
#    en de kaart boot — geen bestaande bootfs meer nodig. De Pi 5 heeft geen
#    start*.elf (firmware in de EEPROM) maar weigert zonder passende DTB; die
#    komt uit sd-rpi5/ (gitignored; herkomst in sd-rpi5/LEESMIJ.txt).
#    Ontbreekt er iets, dan slaan we dit LUID over en is de cp-flow hierboven
#    gewoon compleet. De config in het image is ALTIJD een template (of
#    CFG=...): nooit sd-rpi5/hopos.cfg, daar wonen de echte sleutels.
rm -f metal/out/hopos-rpi5.img
FW_MISSING=""
for f in bcm2712-rpi-5-b.dtb overlays/bcm2712d0.dtbo; do
	[ -f "sd-rpi5/$f" ] || FW_MISSING="$FW_MISSING $f"
done
if [ -z "$FW_MISSING" ]; then
	DEFCFG="$DIR/image/hopos-headless.cfg"
	[ "${GUI:-1}" = 1 ] && DEFCFG="$DIR/image/hopos-gui.cfg"
	go run "$DIR/image/mkcard/main.go" -o metal/out/hopos-rpi5.img -size 64 \
		-cfgwindow 1048576 \
		-start 8192 -label bootfs -vollabel \
		sd-rpi5/hop-agent5.img sd-rpi5/config.txt "${CFG:-$DEFCFG}=hopos.cfg" \
		sd-rpi5/bcm2712-rpi-5-b.dtb \
		sd-rpi5/overlays/bcm2712d0.dtbo=overlays/bcm2712d0.dtbo >&2
	echo "metal/out/hopos-rpi5.img (dd-baar, config = $(basename "${CFG:-$DEFCFG}"))" >&2
	echo "flash: diskutil unmountDisk /dev/diskN && sudo dd if=metal/out/hopos-rpi5.img of=/dev/rdiskN bs=4m" >&2
else
	echo "GEEN dd-baar image gebouwd — mist in sd-rpi5/:$FW_MISSING (zie sd-rpi5/LEESMIJ.txt)" >&2
fi
