#!/bin/sh
# Host-tests + tamago-compile-gate.
#
#   1. go test op de ontwikkelmachine: de logica-packages compileren daar
#      dankzij de host-stubs in metal/dev en metal/kern/stage2 (barrières/cache-
#      onderhoud zijn no-ops; het protocol is wat de tests bewijzen, de
#      barrière-plaatsing bewijst het board). Packages zonder tests draaien
#      mee als compile-check.
#   2. de tamago-gate: appspike + cmd/hopos voor virt/rpi4/rpi5 moeten
#      blijven bouwen, zodat de host-splitsing nooit stiekem het target
#      breekt. Zonder tamago-toolchain wordt de gate overgeslagen.
#
# Extra argumenten gaan naar go test door: tools/test.sh -run Isolatie -v
set -e
cd "$(dirname "$0")/../metal"

# Importrichting van docs/indeling.md — een verkeerde import is een buildfout,
# geen reviewtaak (tools/importcheck.go leest ook code achter build-tags).
go run ../tools/importcheck.go

go test "$@" \
	./abi/ring ./net/hopswitch ./kern/stage2 ./abi/layout ./net/dhcp ./abi/hopabi ./abi/checksum \
	./fw/fdt ./fw/acpi ./kern/hopfs ./driver/vcmail ./driver/nic/mdio ./kern/slots

TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
if [ ! -x "$TAMAGO" ]; then
	echo "tamago-gate OVERGESLAGEN ($TAMAGO ontbreekt)" >&2
	exit 0
fi
# App-images zijn board-onafhankelijk (board/hopslot via applib): één build
# dekt alle boards. De apploader is de enige startroute (twee-fase-lading):
# bouwt hij niet, dan start geen enkele job — dus hard in de gate. De
# lnetonet-variant is de opt-in netstack-backend (appnet/up_lneto.go); die
# moet blijven bouwen tot hij na NETDEMO+soak de default wordt.
for tags in "linkcpuinit" "lnetonet linkcpuinit"; do
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
		"$TAMAGO" build -tags "$tags" -o /dev/null ./app/appspike ./app/apploader
done
for tags in "linkcpuinit" "rpi4 linkcpuinit" "rpi5 linkcpuinit" "uefi linkcpuinit"; do
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
		"$TAMAGO" build -tags "$tags" -o /dev/null ./cmd/hopos
done
# De demo/regressie-mains (cmd/hopos-embed) horen óók in de gate: ze
# compileerden nergens en konden dus stilletjes breken bij elke refactor
# (Derek, 18-07). go:embed eist de app-blobs — één canonieke appspike-build
# (gitignored) dekt alle drie de varianten (images zijn board-onafhankelijk).
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags linkcpuinit -trimpath \
	-ldflags "-w -T 0x50010000 -R 0x1000" -o cmd/hopos-embed/app.elf ./app/appspike
cp cmd/hopos-embed/app.elf cmd/hopos-embed/app4.elf
cp cmd/hopos-embed/app.elf cmd/hopos-embed/app5.elf
for tags in "qemuvirt linkcpuinit" "rpi4 linkcpuinit" "rpi5 linkcpuinit"; do
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
		"$TAMAGO" build -tags "$tags" -o /dev/null ./cmd/hopos-embed
done
# probeuefi is de enige overgebleven probe: default-modus van uefi-run.sh en
# het meetinstrument voor de O6N-bring-up die nog komt. probe4/5/6 zijn
# gesloopt (opruimronde 18-07): hun functie is geproductiseerd (PSCI/CPU_ON in
# de mains, PCIe→RP1→GEM→DHCP in hopnet.Up) — terughalen kan uit git history.
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags "uefi linkcpuinit" -o /dev/null ./cmd/probeuefi
echo "OK: host-tests groen, tamago-gate (virt/rpi4/rpi5/uefi + embed-mains + probeuefi) gebouwd" >&2
