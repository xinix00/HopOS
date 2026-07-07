#!/bin/sh
# Bouw de fase-P-probe voor de Raspberry Pi 5 (PLAN.md fase P, "verifieer
# eerst") en zet alles voor de SD-kaart klaar in sd-rpi5/. Flash-instructies:
# sd-rpi5/LEESMIJ.txt (of docs/rpi5.md).

set -e

TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"

# 1. De probe-image: canoniek gelinkt op load (0x200000) + 0x10000; de eerste
#    64 bytes van het bestand worden de arm64 Image-header (mkkernel).
cd "$DIR/metal"
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags linkcpuinit -trimpath \
	-ldflags "-s -w -T 0x210000 -R 0x1000" -o probe5.elf ./cmd/probe5

# 2. ELF → kernel_2712.img (raw + arm64 Image-header). Als los bestand
#    gedraaid: stdlib-only, geen module nodig.
cd "$DIR"
mkdir -p sd-rpi5
go run "$DIR/image/mkkernel/main.go" -elf metal/probe5.elf -o sd-rpi5/kernel_2712.img -load 0x200000

# 3. config.txt + instructies.
cat > sd-rpi5/config.txt <<'EOF'
# HopOS probe5 — Raspberry Pi 5 (zie docs/rpi5.md)
arm_64bit=1
kernel=kernel_2712.img
# DTB buiten onze RAM-declaratie (de firmware eist een DTB op de kaart,
# de probe gebruikt hem niet).
device_tree_address=0x0f000000
# Bootloader-logs op de debug-UART: bewijst meteen dat de kabel werkt.
uart_2ndstage=1
EOF

cat > sd-rpi5/LEESMIJ.txt <<'EOF'
HopOS probe5 op de Raspberry Pi 5 — flashen en meten
====================================================

SD-kaart (FAT32, eerste partitie) — zet erop:
  1. config.txt en kernel_2712.img (uit deze map);
  2. bcm2712-rpi-5-b.dtb  en  overlays/bcm2712d0.dtbo, allebei uit
     https://github.com/raspberrypi/firmware/tree/master/boot
     (de firmware weigert te booten zonder passende DTB).

Debug-UART: de 3-pins JST-SH-connector tussen de HDMI-poorten (Raspberry Pi
Debug Probe of een 3V3 USB-UART), 115200 8N1:
  screen /dev/tty.usbmodem* 115200      # of tty.usbserial-*

Verwachte volgorde op de UART:
  1. bootloader-logs (uart_2ndstage);
  2. "P2" — ons vroegste levensteken: cpuinit draait, boot-EL = 2;
  3. de probe-banner + metingen, afgesloten met
     HOPOS_PI5_PROBE_OK        → alles klopt, fase P1 kan los;
     HOPOS_PI5_PROBE_DEELS     → lees de regels erboven: meetdata
       (bv. CPU_ON = ALREADY_ON ⇒ we bouwen TF-A bl31.bin als armstub).

Komt er níks (ook geen bootloader-logs): check de kabel/poort; zet zo nodig
BOOT_UART=1 in de EEPROM-config (rpi-eeprom-config -e op een Linux-Pi).
Komt er wel bootloader-log maar geen "P2": stuur de laatste regels door —
dan faalt het laden van de kernel (config.txt/DTB-probleem).
EOF

echo "sd-rpi5/ klaar: kernel_2712.img + config.txt + LEESMIJ.txt" >&2
