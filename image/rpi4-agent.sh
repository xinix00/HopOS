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

cd "$DIR/metal"
mkdir -p out

# 1. De app-image voor jobs: canoniek gelinkt (slot-1-IPA), hopslot-hooks
#    (board-onafhankelijk — zelfde binary als op de Pi 5/QEMU/Altra).
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags linkcpuinit -trimpath \
	-ldflags "-w -T 0x50010000 -R 0x1000" -o out/app4.elf ./app/appspike

# 1b. De universele apploader (hopslot-hooks) op de go:embed-plek: de node bakt
#     'm in (embedloader) en laadt 'm als fase 1 in élk slot — de app downloadt
#     dan zijn eigen image op zijn eigen core+netstack. Zonder ingebakken
#     loader start geen enkele job (de twee-fase-lading is de enige route).
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags linkcpuinit -trimpath \
	-ldflags "-w -T 0x50010000 -R 0x1000" -o kern/apploaderblob/apploader.elf ./app/apploader
# Gecomprimeerd inbakken (gzip -9: 8,4→3,1MB): de blob zit 6× in de Altra-PE
# en 1× per Pi-image; de node pakt 'm één keer lazy uit (kern/apploaderblob).
# -n: geen naam/tijdstempel in de gzip-header → deterministische builds.
gzip -9 -n -f kern/apploaderblob/apploader.elf

# 2. De agent-kern: cmd/hopos met het rpi4-board (build-tag kiest board_rpi4.go)
#    + de ingebakken apploader (embedloader). Default gui; GUI=0 bouwt de
#    kale (headless) smaak. (Zelfde knop in alle imagescripts.)
GUITAG=""
[ "${GUI:-1}" = 1 ] && GUITAG=" gui"
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags "rpi4 linkcpuinit embedloader$GUITAG" -trimpath \
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
EOF

echo "sd-rpi4/kernel8.img (HOP-agent, $(du -h sd-rpi4/kernel8.img | cut -f1)) + config.txt klaar." >&2
echo "flash: cp sd-rpi4/kernel8.img sd-rpi4/config.txt '/Volumes/NO NAME/'" >&2
