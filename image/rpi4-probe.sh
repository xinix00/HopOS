#!/bin/sh
# Bouw de fase-P-probe voor de Raspberry Pi 4 (zelfde draaiboek als
# rpi5-probe.sh) en zet alles voor de SD-kaart klaar in sd-rpi4/.
# Flash-instructies: sd-rpi4/LEESMIJ.txt (of docs/rpi4.md).

set -e

TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"

# 1. De probe-image: gelinkt op de Pi-DEFAULT load 0x80000 + 0x10000 — de
#    laadadres-les van de Pi 5 (2026-07-09): kernel_address wordt daar door
#    de EEPROM-bootloader genegeerd; door overal op 0x80000 te laden zijn
#    beide boards identiek en is die optie nergens meer nodig. De eerste
#    64 bytes worden de arm64 Image-header.
cd "$DIR/metal"
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags linkcpuinit -trimpath \
	-ldflags "-s -w -T 0x90000 -R 0x1000" -o probe4.elf ./cmd/probe4

# 2. ELF → kernel8.img (raw + arm64 Image-header, incl. BSS als nullen —
#    mkkernel schrijft t/m memEnd). Als los bestand gedraaid: stdlib-only.
cd "$DIR"
mkdir -p sd-rpi4
go run "$DIR/image/mkkernel/main.go" -elf metal/probe4.elf -o sd-rpi4/kernel8.img -load 0x80000

# 3. config.txt + instructies.
cat > sd-rpi4/config.txt <<'EOF'
# HopOS probe4 — Raspberry Pi 4 (zie docs/rpi4.md)
arm_64bit=1
kernel=kernel8.img
# Kernel op de Pi-default 0x80000 (geen kernel_address: de Pi 5 negeert die
# optie toch — laadadres-les 2026-07-09 — en zo laden beide boards identiek;
# het image is op 0x90000 gelinkt en de p-marker op de UART bewijst het).
# DTB buiten onze RAM-declaratie (de firmware eist een DTB op de kaart; de
# probe gebruikt hem niet, TF-A patcht hem alleen).
device_tree_address=0x0f000000
# TF-A BL31 als armstub: levert PSCI — de stock armstub8 heeft dat NIET
# (spin-table) en dan hangt onze eerste SMC. Bouwinstructie: LEESMIJ.txt.
armstub=bl31.bin
# Bootloader-logs op de PL011 (GPIO14/15): bewijst meteen dat de kabel werkt
# én configureert de UART voor onze printk.
uart_2ndstage=1
# Houd de PL011 bij GPIO14/15 (anders claimt Bluetooth hem).
dtoverlay=disable-bt
EOF

cat > sd-rpi4/LEESMIJ.txt <<'EOF'
HopOS probe4 op de Raspberry Pi 4 — flashen en meten
====================================================

SD-kaart (FAT32, eerste partitie) — zet erop:
  1. config.txt en kernel8.img (uit deze map);
  2. start4.elf en fixup4.dat uit
     https://github.com/raspberrypi/firmware/tree/master/boot
     (de Pi 4 heeft die nodig — de Pi 5 niet);
  3. bcm2711-rpi-4-b.dtb uit dezelfde repo;
  4. bl31.bin — zelfgebouwde upstream-TF-A (VERPLICHT: de stock armstub8
     heeft geen PSCI; onze eerste SMC zou in een lege EL3-vector hangen):
       git clone --depth 1 https://github.com/ARM-software/arm-trusted-firmware
       cd arm-trusted-firmware
       make PLAT=rpi4 CROSS_COMPILE=aarch64-none-elf- DEBUG=0 bl31
       cp build/rpi4/release/bl31.bin <SD-kaart>/
     (macOS: brew install --cask gcc-aarch64-embedded, of aarch64-elf-gcc
      → dan CROSS_COMPILE=aarch64-elf-)

UART: PL011 op de 40-pins header — pin 8 (TXD), pin 10 (RXD), pin 6 (GND),
3V3-niveau, 115200 8N1:
  screen /dev/tty.usbserial-* 115200      # of tty.usbmodem-*

Verwachte volgorde op de UART:
  1. bootloader-logs (uart_2ndstage);
  2. "P2" — ons vroegste levensteken: cpuinit draait, boot-EL = 2 (TF-A);
  3. "Rp00000000000exxxx" — de PC-dump: bewijs dat de firmware ons écht op
     0x80000 laadde (moet ~0xE-Fxxxx zijn; iets ánders = laadadres-probleem,
     zie docs/handoff-pi5-mmu.md voor het Pi 5-verhaal);
  4. de MMU-ladder "@$abc t MNCId hH eEZ" (two-stage: blokkenkaart → echte
     kaart) en dan de probe-banner + metingen, afgesloten met
     HOPOS_PI4_PROBE_OK        → alles klopt, P1 kan los;
     HOPOS_PI4_PROBE_DEELS     → lees de regels erboven: meetdata.
  Een fault verschijnt als XE<esr>L<elr>F<far>R<lr>S<sp>+stack (EL1) of
  YE... (EL2) — decodeerbaar, geen stille hang.

Blijft het stil ná de regel "PSCI: versie opvragen via SMC...": bl31.bin
ontbreekt of wordt niet geladen (armstub-regel in config.txt) — de SMC
verdwijnt dan in een lege EL3-vector.
Komt er níks (ook geen bootloader-logs): check de kabel/poort; zet zo nodig
BOOT_UART=1 in de EEPROM-config (rpi-eeprom-config -e op een Linux-Pi).
Komt er wel bootloader-log maar geen "P2": stuur de laatste regels door —
dan faalt het laden van de kernel (config.txt/DTB/armstub-probleem).
EOF

echo "sd-rpi4/ klaar: kernel8.img + config.txt + LEESMIJ.txt (bl31.bin/DTB/start4.elf zelf toevoegen, zie LEESMIJ)" >&2
