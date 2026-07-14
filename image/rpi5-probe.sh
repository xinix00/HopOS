#!/bin/sh
# Bouw de fase-P-probe voor de Raspberry Pi 5 (PLAN.md fase P, "verifieer
# eerst") en zet alles voor de SD-kaart klaar in sd-rpi5/. Flash-instructies:
# sd-rpi5/LEESMIJ.txt (of docs/rpi5.md).

set -e

TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"

# 1. De probe-image: gelinkt op de WERKELIJKE load (0x80000, zie hieronder)
#    + 0x10000; de eerste 64 bytes van het bestand worden de header (mkkernel).
cd "$DIR/metal"
mkdir -p out
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags linkcpuinit -trimpath \
	-ldflags "-s -w -T 0x90000 -R 0x1000" -o out/probe5.elf ./cmd/probe5

# 2. ELF → kernel_2712.img, RAW (het Circle-recept): géén arm64-Image-magic,
#    plat bestand, code0-branch op byte 0; de firmware springt blind naar
#    kernel_address (expliciet in config.txt, zoals Circle doet). Boot-
#    metingen 2026-07-08: het Image-pad (magic, raw én gzip) weigerde onze
#    kernel geluidloos — nul levenstekens — terwijl ditzelfde board Linux
#    boot; het raw-pad is door Circle op de Pi 5 bewezen.
cd "$DIR"
mkdir -p sd-rpi5
go run "$DIR/image/mkkernel/main.go" "$DIR/image/mkkernel/pe.go" -elf metal/out/probe5.elf -o sd-rpi5/kernel_2712.img -load 0x80000 -raw

# 3. config-probe5.txt (gitignored; komt als config.txt op de kaart — het
#    getrackte config.txt is de agent-config) + instructies.
cat > sd-rpi5/config-probe5.txt <<'EOF'
# HopOS probe5 — Raspberry Pi 5 (zie docs/rpi5.md)
arm_64bit=1
kernel=kernel_2712.img
# DE sleutel voor bare metal op de Pi 5 (gevonden 2026-07-08 na 5 stille
# boots): zonder os_check=0 neemt de EEPROM-bootloader aan dat hij Linux
# laadt en valideert hij het image daarop — elk niet-Linux-image sneuvelt
# geluidloos vóór de eerste eigen instructie. Bestond niet op de Pi 4.
os_check=0
# Raw image (geen arm64-magic) → de firmware laadt op de Pi 5-default 0x80000
# en springt naar byte 0 (code0-branch). LET OP (gemeten 2026-07-09 sessie 2,
# XN-kooi): kernel_address wordt door de Pi 5-EEPROM-bootloader GENEGEERD —
# zet hem niet, link op 0x80000 (+0x10000 voor text). Drie dagen "MMU-wedge"
# was in werkelijkheid dit adres-verschil.
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

echo "sd-rpi5/ klaar: kernel_2712.img + config-probe5.txt + LEESMIJ.txt (kaart: config-probe5.txt AS config.txt)" >&2
