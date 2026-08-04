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

# Importrichting van docs/archief/indeling.md — een verkeerde import is een buildfout,
# geen reviewtaak (tools/importcheck.go leest ook code achter build-tags).
go run ../tools/importcheck.go

go test "$@" \
	./abi/ring ./net/hopswitch ./kern/stage2 ./abi/layout ./net/dhcp ./abi/hopabi ./abi/checksum \
	./fw/fdt ./fw/acpi ./fw/bootcfg ./kern/hopfs ./driver/vcmail ./driver/nic/mdio ./kern/slots \
	./gui/fbgrant ./app/applib/apphttp ./kern/cage ./driver/nic/dwmac ./cmd/hopos/cfgblob ./driver/conlog \
	./kern/cagestub

TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
if [ ! -x "$TAMAGO" ]; then
	echo "tamago-gate OVERGESLAGEN ($TAMAGO ontbreekt)" >&2
	exit 0
fi

# De gate maakt zijn eigen go:embed-blobs en ruimt ze op. Twee redenen, en de
# tweede is de belangrijkste:
#
#   1. Ze zijn build-output en horen niet in de sourceboom te blijven liggen (de
#      config kan een échte apikey bevatten, en hij is gitignored).
#   2. De embed-varianten werden alléén gebouwd als zulke resten TOEVALLIG
#      bestonden — wie nooit een board-image bouwde, testte die builds dus nooit.
#      Nu maakt de gate ze zelf en is de dekking deterministisch.
#
# De inhoud doet niet mee: dit is een compile-gate (-o /dev/null) en de blobs
# worden pas bij een echte jobstart uitgepakt. Alleen bestanden die WIJ maken
# worden opgeruimd, zodat een lopende image-build van een ontwikkelaar intact
# blijft.
GATE_MADE=""
gate_stub() { # gate_stub <pad> [inhoud]
	if [ ! -f "$1" ]; then
		mkdir -p "$(dirname "$1")"
		printf '%s' "${2:-}" > "$1"
		GATE_MADE="$GATE_MADE $1"
	fi
}
#
# LET OP: twee gates tegelijk in dezelfde worktree delen deze bestanden, dus die
# racen op elkaars opruimronde. Eén gate per boom (net als de image-scripts).
gate_clean() {
	if [ -n "$GATE_MADE" ]; then
		rm -f $GATE_MADE
	fi
	return 0 # de trap mag de exitstatus van de gate nooit beïnvloeden
}
trap gate_clean EXIT INT TERM
# App-images zijn board-onafhankelijk (board/hopslot via applib): één build
# dekt alle boards. De apploader is de enige startroute (twee-fase-lading):
# bouwt hij niet, dan start geen enkele job — dus hard in de gate.
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags linkcpuinit -o /dev/null ./app/appspike ./app/apploader ./app/hello
# Elke board-smaak kaal; plus de gui-smaak (metal/gui achter -tags gui) op
# virt (bewijst de bedrading zonder Display-board) en rpi5 (mét). En één smaak
# mét embedloader: dát is wat élk echt board-image bouwt, en die tag was nergens
# gedekt omdat de blob alleen bestond als iemand net een image gebouwd had.
gate_stub kern/apploaderblob/apploader.elf.gz
for tags in "linkcpuinit" "rpi4 linkcpuinit" "rpi5 linkcpuinit" "uefi linkcpuinit" "gui linkcpuinit" "rpi5 gui linkcpuinit" "rpi5 linkcpuinit embedloader"; do
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
GATE_MADE="$GATE_MADE cmd/hopos-embed/app.elf cmd/hopos-embed/app4.elf cmd/hopos-embed/app5.elf"
for tags in "qemuvirt linkcpuinit" "rpi4 linkcpuinit" "rpi5 linkcpuinit" "rpi5 gui linkcpuinit"; do
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
		"$TAMAGO" build -tags "$tags" -o /dev/null ./cmd/hopos-embed
done
# RISC-V (LicheeRV Nano / SG2002): de tweede architectuur hoort net zo hard in
# de gate als de ARM-boards, anders breekt een refactor hem stil. De kooi is
# hier locked-PMP (kern/cage) i.p.v. stage-2, dus dit dekt een ánder pad door de
# board-laag: board.Board's riscv64-helft, de dev-primitieven en de app-kant met
# zijn eigen RAM-plan.
# De app-kant voor riscv64: de apploader (fase 1 van élke job) en de
# slot-demo. Dit dekt board/hopslot's riscv64-helft, applib's park/self-place
# en de app-kant van SMP — het spiegelbeeld van de HOP-kant hierboven.
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=riscv64 \
	"$TAMAGO" build -tags "linkramsize linkcpuinit" -o /dev/null ./app/slotdemo ./app/apploader ./app/switchtest
# En de twee bestanden die ALLEEN achter linkcpuinit staan, expliciet: de
# S-mode-entry (cpu/slotstart/cpuinit_riscv64.s) en de app-kant van Push/Pull
# (dev/share_riscv64_app.go). Ze zitten in de app-builds hierboven, maar een
# import die wegvalt zou dat stil maken — dus ook rechtstreeks, zodat een fout in
# die assembly de gate breekt en niet pas een boot-cyclus.
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=riscv64 \
	"$TAMAGO" build -tags linkcpuinit -o /dev/null ./cpu/slotstart ./dev
# DE echte agent-main voor riscv64: dit is het bewijs dat de kooi-naad
# (kern/slots cage_<arch>.go) en de node-naad (cmd/hopos node_<arch>.go) houden —
# één main voor élk board, ARM of RISC-V.
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=riscv64 \
	"$TAMAGO" build -tags licheerv -o /dev/null ./cmd/hopos
# Mét ingebakken config én kooi-stub (dit board kan zijn eigen bootmedium niet
# lezen, dus dít is de smaak die er écht op gaat). De blobs maakt de gate zelf
# (gate_stub), zodat deze builds altijd meelopen en niet alleen op een machine
# waar een image-build resten achterliet.
gate_stub cmd/hopos/cfgblob/hopos.cfg 'hopos.node=gate
'
gate_stub kern/cagestub/stub-slot.bin
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=riscv64 \
	"$TAMAGO" build -tags "licheerv embedcfg embedcagestub" -o /dev/null ./cmd/hopos

# probeuefi is de enige overgebleven probe: default-modus van uefi-run.sh en
# het meetinstrument voor de O6N-bring-up die nog komt. probe4/5/6 zijn
# gesloopt (opruimronde 18-07): hun functie is geproductiseerd (PSCI/CPU_ON in
# de mains, PCIe→RP1→GEM→DHCP in hopnet.Up) — terughalen kan uit git history.
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags "uefi linkcpuinit" -o /dev/null ./cmd/probeuefi
echo "OK: host-tests groen, tamago-gate (arm64: virt/rpi4/rpi5/uefi kaal + gui-smaken + embedloader + embed-mains + probeuefi; riscv64: cmd/hopos kaal én embedcfg/embedcagestub + slot-demo + slot-app + switchtest) gebouwd" >&2
