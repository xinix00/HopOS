#!/bin/sh
# Bouw de fase-P1-multikernel voor de Raspberry Pi 4 (metal/cmd/hopos-embed):
# HOP-kern + embedded canonieke app-image, als raw kernel8.img (raw op
# 0x80000, TF-A bl31.bin als armstub); zie docs/archief/rpi4.md.
#
# TF-A bl31.bin is VERPLICHT als armstub (de stock armstub8 heeft geen PSCI):
#   cd ~/arm-trusted-firmware
#   gmake PLAT=rpi4 CROSS_COMPILE=aarch64-elf- OC=aarch64-elf-objcopy \
#         OD=aarch64-elf-objdump DEBUG=0 bl31
#   cp build/rpi4/release/bl31.bin <dit repo>/sd-rpi4/
#
# Flashen (kaart-conventie: Linux-bestanden blijven staan). config-hopos.txt
# komt AS config.txt op de kaart; het getrackte sd-rpi4/config.txt is de
# agent-config en blijft ongemoeid:
#   cp sd-rpi4/kernel8.img sd-rpi4/bl31.bin /Volumes/bootfs/
#   cp sd-rpi4/config-hopos.txt /Volumes/bootfs/config.txt
#   sync && diskutil eject /Volumes/bootfs
# UART meekijken (GPIO14/15 → USB-serieel):
#   /bin/sh -c 'exec 4<>/dev/cu.usbserial-XXXX; stty -f /dev/cu.usbserial-XXXX 115200 raw; exec cat <&4'

set -e

TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"

cd "$DIR/metal"
mkdir -p out

# 1. De app-image: canoniek gelinkt (slot-1-IPA), hopslot-hooks (board-
#    onafhankelijk). Zonder
#    -s: de symboltabel is nodig voor de RamStart/RamSize-patch (job.MemoryLimit).
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags linkcpuinit -trimpath \
	-ldflags "-w -T 0x50010000 -R 0x1000" -o cmd/hopos-embed/app4.elf ./app/appspike

# 2. De HOP-kern (embed app4.elf): gelinkt op de werkelijke load 0x80000 (+0x10000).
#    Default gui; GUI=0 bouwt de kale (headless) smaak. (Zelfde knop overal.)
GUITAG=""
[ "${GUI:-1}" = 1 ] && GUITAG=" gui"
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags "rpi4 linkcpuinit$GUITAG" -trimpath \
	-ldflags "-s -w -T 0x90000 -R 0x1000" -o out/hopos4.elf ./cmd/hopos-embed

# 3. ELF → RAW kernel8.img (géén arm64-Image-header: die triggert het
#    relocatie-protocol en de Pi 4-firmware verplaatst ons dan naar 0x200000
#    — gemeten 2026-07-10 aan de 'p'-marker, terwijl we op 0x90000 linken. Met
#    -raw laadt de firmware plat op 0x80000 en springt naar byte 0, precies
#    zoals de Pi 5. Incl. BSS-nullen t/m memEnd.).
cd "$DIR"
mkdir -p sd-rpi4
go run "$DIR/image/mkkernel/main.go" "$DIR/image/mkkernel/pe.go" -elf metal/out/hopos4.elf -o sd-rpi4/kernel8.img -load 0x80000 -raw

# 4. config-hopos.txt (gitignored) — kernel wijst naar ons; bl31.bin als armstub
#    (PSCI). Het getrackte config.txt is de agent-config; die overschrijven we
#    niet, deze komt bij het flashen als config.txt op de kaart.
cat > sd-rpi4/config-hopos.txt <<'EOF'
# HopOS multikernel (fase P1) — Raspberry Pi 4 (zie docs/archief/rpi4.md)
arm_64bit=1
kernel=kernel8.img
device_tree_address=0x0f000000
# TF-A BL31 als armstub: levert PSCI (CPU_ON voor de cold bring-up). De stock
# armstub8 heeft dat NIET — dan hangt de eerste SMC.
armstub=bl31.bin
uart_2ndstage=1
# Houd de PL011 bij GPIO14/15 (anders claimt Bluetooth hem).
dtoverlay=disable-bt
EOF

# 5. bl31.bin meenemen als hij naast de build ligt (anders zelf kopiëren, zie kop).
if [ -f "$HOME/arm-trusted-firmware/build/rpi4/release/bl31.bin" ]; then
	cp "$HOME/arm-trusted-firmware/build/rpi4/release/bl31.bin" sd-rpi4/bl31.bin
fi

echo "sd-rpi4/kernel8.img ($(du -h sd-rpi4/kernel8.img | cut -f1)) + config-hopos.txt + bl31.bin klaar."
echo "flash: cp sd-rpi4/kernel8.img sd-rpi4/bl31.bin /Volumes/bootfs/ && cp sd-rpi4/config-hopos.txt /Volumes/bootfs/config.txt && sync && diskutil eject /Volumes/bootfs"
