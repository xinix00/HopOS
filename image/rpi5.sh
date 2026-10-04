#!/bin/sh
# Bouw de SD-kaart van HopOS v3 voor de Raspberry Pi 5 (board/rpi5):
# hop-agent5.img, config.txt, cmdline.txt en het image van Hop, en als de
# DTB er ligt het complete, dd-bare kaart-image. De knoppen (APP, GUI, FW,
# CFG, EXTRA) en het boot-recept staan in image/raspi.sh, dat de Pi 4 en
# de Pi 5 samen bouwt.
#
# Uitvoer: target/sd-rpi5/ en target/hopos-rpi5.img (MBR + FAT16, dd-baar).
exec sh "$(dirname "$0")/raspi.sh" rpi5
