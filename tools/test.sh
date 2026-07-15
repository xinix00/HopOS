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
	./fw/fdt ./kern/hopfs ./driver/vcmail ./driver/nic/mdio ./kern/slots

TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
if [ ! -x "$TAMAGO" ]; then
	echo "tamago-gate OVERGESLAGEN ($TAMAGO ontbreekt)" >&2
	exit 0
fi
for tags in "linkcpuinit" "rpi4 linkcpuinit" "rpi5 linkcpuinit" "uefi linkcpuinit"; do
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
		"$TAMAGO" build -tags "$tags" -o /dev/null ./app/appspike
	# De apploader is de enige startroute (twee-fase-lading): een board waarvoor
	# hij niet bouwt kan geen enkele job starten — dus per board mee in de gate.
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
		"$TAMAGO" build -tags "$tags" -o /dev/null ./app/apploader
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
		"$TAMAGO" build -tags "$tags" -o /dev/null ./cmd/hopos
done
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags "uefi linkcpuinit" -o /dev/null ./cmd/probeuefi
echo "OK: host-tests groen, tamago-gate (virt/rpi4/rpi5/uefi) gebouwd" >&2
