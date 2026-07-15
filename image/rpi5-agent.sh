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

cd "$DIR/metal"
mkdir -p out

# 1. De app-image voor jobs: canoniek gelinkt (slot-1-IPA), hopslot-hooks
#    (board-onafhankelijk — zelfde binary als op de Pi 4/QEMU/Altra).
#    Zonder -s: slots patcht RamStart/RamSize via de symboltabel.
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags linkcpuinit -trimpath \
	-ldflags "-w -T 0x50010000 -R 0x1000" -o out/app5.elf ./app/appspike

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

# 2. De agent-kern: cmd/hopos met het rpi5-board (build-tag kiest board_rpi5.go)
#    + de ingebakken apploader (embedloader).
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags "rpi5 linkcpuinit embedloader" -trimpath \
	-ldflags "-s -w -T 0x90000 -R 0x1000" -o out/agent5.elf ./cmd/hopos

# 3. ELF → raw image (Circle-recept, mkkernel).
cd "$DIR"
mkdir -p sd-rpi5
go run "$DIR/image/mkkernel/main.go" "$DIR/image/mkkernel/pe.go" -elf metal/out/agent5.elf -o sd-rpi5/hop-agent5.img -load 0x80000 -raw

# 4. config.txt — zelfde poortwachters, kernel wijst naar de agent.
cat > sd-rpi5/config.txt <<'EOF'
# HopOS: de echte HOP-agent (fase P2) — Raspberry Pi 5 (zie docs/rpi5.md)
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
EOF

echo "sd-rpi5/hop-agent5.img ($(du -h sd-rpi5/hop-agent5.img | cut -f1)) + config.txt klaar." >&2
echo "flash: cp sd-rpi5/hop-agent5.img sd-rpi5/config.txt /Volumes/bootfs/ && sync && diskutil eject" >&2
