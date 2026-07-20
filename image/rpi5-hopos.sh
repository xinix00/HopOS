#!/bin/sh
# Bouw de fase-P1-multikernel voor de Raspberry Pi 5 (metal/cmd/hopos-embed):
# HOP-kern + embedded canonieke app-image, als raw kernel voor de EEPROM-
# bootloader (os_check=0, raw op 0x80000, DTB op 0x0F000000); zie
# docs/archief/rpi5.md voor het dossier.
#
# Flashen (kaart-conventie: Linux-bestanden blijven staan). config-hopos.txt
# komt AS config.txt op de kaart; het getrackte sd-rpi5/config.txt is de
# agent-config en blijft ongemoeid:
#   cp sd-rpi5/hop-hopos5.img /Volumes/bootfs/ && cp sd-rpi5/config-hopos.txt /Volumes/bootfs/config.txt
#   sync && diskutil eject /Volumes/bootfs
# UART meekijken (kabeltje in de U-poort):
#   /bin/sh -c 'exec 4<>/dev/cu.usbmodem11302; stty -f /dev/cu.usbmodem11302 115200 raw; exec cat <&4'

set -e

TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"

cd "$DIR/metal"
mkdir -p out

# 1. De app-image: canoniek gelinkt (slot-1-IPA, zelfde -T als op QEMU) maar
#    met de hopslot-runtime-hooks (board-onafhankelijk). Zonder -s: de symboltabel is
#    nodig zodat slots.Start RamStart/RamSize kan patchen (job.MemoryLimit).
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags linkcpuinit -trimpath \
	-ldflags "-w -T 0x50010000 -R 0x1000" -o cmd/hopos-embed/app5.elf ./app/appspike

# 2. De HOP-kern (embed app5.elf): gelinkt op de werkelijke load 0x80000
#    (+0x10000 voor text) — de Pi 5-EEPROM negeert kernel_address.
#    Default gui; GUI=0 bouwt de kale (headless) smaak. (Zelfde knop overal.)
GUITAG=""
[ "${GUI:-1}" = 1 ] && GUITAG=" gui"
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags "rpi5 linkcpuinit$GUITAG" -trimpath \
	-ldflags "-s -w -T 0x90000 -R 0x1000" -o out/hopos5.elf ./cmd/hopos-embed

# 3. ELF → raw image (Circle-recept, incl. BSS-nullen t/m memEnd — mkkernel).
cd "$DIR"
mkdir -p sd-rpi5
go run "$DIR/image/mkkernel/main.go" "$DIR/image/mkkernel/pe.go" -elf metal/out/hopos5.elf -o sd-rpi5/hop-hopos5.img -load 0x80000 -raw

# 4. config-hopos.txt (gitignored) — zelfde poortwachters als de probe, kernel
#    wijst naar ons. Het getrackte config.txt is de agent-config; deze komt bij
#    het flashen als config.txt op de kaart.
cat > sd-rpi5/config-hopos.txt <<'EOF'
# HopOS multikernel (fase P1) — Raspberry Pi 5 (zie docs/archief/rpi5.md)
arm_64bit=1
kernel=hop-hopos5.img
# Verplicht voor élke niet-Linux-kernel op de Pi 5 (gemeten 2026-07-08):
# zonder os_check=0 weigert de EEPROM geluidloos.
os_check=0
# Raw image → load op de Pi 5-default 0x80000, entry op byte 0.
# kernel_address wordt door de EEPROM GENEGEERD — link op 0x80000.
# DTB buiten alle RAM-declaraties; HOP leest er /memory uit (MemTotal).
device_tree_address=0x0f000000
# Bootloader-logs op de debug-UART: bewijst meteen dat de kabel werkt.
uart_2ndstage=1
# Lagere idle-vloer voor het dvfs-klokbeleid (metal/driver/dvfs vraagt de min op
# en volgt): zonder deze regel klemt de Pi 5-firmware op 1500MHz (gemeten
# 2026-07-11). Accepteert de firmware 800 niet, dan meldt de dvfs-regel dat.
arm_freq_min=800
EOF

echo "sd-rpi5/hop-hopos5.img ($(du -h sd-rpi5/hop-hopos5.img | cut -f1)) + config-hopos.txt klaar."
echo "flash: cp sd-rpi5/hop-hopos5.img /Volumes/bootfs/ && cp sd-rpi5/config-hopos.txt /Volumes/bootfs/config.txt && sync && diskutil eject /Volumes/bootfs"
