#!/bin/sh
# Bouw de SD-kaart van HopOS v3 voor de Raspberry Pi 4 (board/rpi4):
# kernel8.img, config.txt, cmdline.txt en het image van Hop, en als de
# firmware er ligt het complete, dd-bare kaart-image. De knoppen (APP, GUI,
# FW, CFG, EXTRA) en het boot-recept staan in image/raspi.sh, dat de Pi 4
# en de Pi 5 samen bouwt.
#
# Uitvoer: target/sd-rpi4/ en target/hopos-rpi4.img (MBR + FAT16, dd-baar).
exec sh "$(dirname "$0")/raspi.sh" rpi4
