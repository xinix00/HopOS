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

# -tags gui: de surface-grant (kern/slots, kern/stage2) is gui-werk en zijn
# tests dus ook; zonder de tag zouden die stil overgeslagen worden. De kale
# stub-kant is een compilegate, geen logica — die dekken de tamago-builds
# hieronder (elke smaak zonder gui).
go test -tags gui "$@" \
	./abi/ring ./abi/frameq ./net/hopswitch ./kern/stage2 ./abi/layout ./abi/hopabi ./abi/systemapi ./abi/checksum \
	./fw/fdt ./fw/adt ./fw/xnuboot ./fw/acpi ./fw/bootcfg ./kern/hopfs ./driver/vcmail ./driver/nic/mdio ./kern/slots ./kern/kernflip \
	./gui/fbgrant ./gui/driver/usb/hid ./kern/cage ./driver/nic/dwmac ./driver/nic/dwmac4 ./cmd/hopos/cfgblob ./driver/conlog \
	./kern/cagestub ./net/nodemac

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
# dekt alle boards.
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags "linkcpuinit" -o /dev/null ./app/appspike ./app/hello
# Elke board-smaak kaal; plus de gui-smaak (metal/gui achter -tags gui) op
# virt (bewijst de bedrading zonder Display-board), rpi5 (mét) en rpi4 (de
# VL805-USB achter de BCM2711-root-complex — de enige plek waar dat pad
# compileert).
for tags in "linkcpuinit" "rpi4 linkcpuinit" "rpi5 linkcpuinit" "uefi linkcpuinit" "gui linkcpuinit" "rpi4 gui linkcpuinit" "rpi5 gui linkcpuinit"; do
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
		"$TAMAGO" build -tags "$tags" -o /dev/null ./cmd/hopos
done
# De demo/regressie-mains (cmd/hopos-embed) horen óók in de gate: ze
# compileerden nergens en konden dus stilletjes breken bij elke refactor
# (Derek, 18-07). go:embed eist de app-blobs — één canonieke appspike-build
# (gitignored) dekt alle drie de varianten (images zijn board-onafhankelijk).
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags "linkcpuinit" -trimpath \
	-ldflags "-w -T 0x50010000 -R 0x1000" -o cmd/hopos-embed/app.elf ./app/appspike
cp cmd/hopos-embed/app.elf cmd/hopos-embed/app4.elf
cp cmd/hopos-embed/app.elf cmd/hopos-embed/app5.elf
GATE_MADE="$GATE_MADE cmd/hopos-embed/app.elf cmd/hopos-embed/app4.elf cmd/hopos-embed/app5.elf"
for tags in "qemuvirt linkcpuinit" "rpi4 linkcpuinit" "rpi5 linkcpuinit" "rpi5 gui linkcpuinit" "rk3566 linkcpuinit"; do
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
		"$TAMAGO" build -tags "$tags" -o /dev/null ./cmd/hopos-embed
done
# RISC-V (LicheeRV Nano / SG2002): de tweede architectuur hoort net zo hard in
# de gate als de ARM-boards, anders breekt een refactor hem stil. De kooi is
# hier locked-PMP (kern/cage) i.p.v. stage-2, dus dit dekt een ánder pad door de
# board-laag: board.Board's riscv64-helft, de dev-primitieven en de app-kant met
# zijn eigen RAM-plan.
# De app-kant voor riscv64: de slot-demo en de switchtest. Dit dekt
# board/hopslot's riscv64-helft, applib's park en de app-kant van SMP — het
# spiegelbeeld van de HOP-kant hierboven.
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=riscv64 \
	"$TAMAGO" build -tags "linkramsize linkcpuinit" -o /dev/null ./app/slotdemo ./app/switchtest
# En de app-selectie expliciet: de S-mode-entry (cpu/slotstart, achter
# linkcpuinit) en de app-kant van Push/Pull (dev/share_riscv64_app.go, achter
# linkramsize — NIET linkcpuinit: die tag draagt de kern sinds de loterij óók,
# en precies die drift maakte de no-ops ooit stil kern-breed, 17-08). Ze zitten
# in de app-builds hierboven, maar een import die wegvalt zou dat stil maken —
# dus ook rechtstreeks, zodat een fout de gate breekt en niet pas een boot-cyclus.
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=riscv64 \
	"$TAMAGO" build -tags "linkramsize linkcpuinit" -o /dev/null ./cpu/slotstart ./dev
# DE echte agent-main voor riscv64: dit is het bewijs dat de kooi-naad
# (kern/slots cage_<arch>.go) en de node-naad (cmd/hopos node_<arch>.go) houden —
# één main voor élk board, ARM of RISC-V.
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=riscv64 \
	"$TAMAGO" build -tags "licheerv linkcpuinit" -o /dev/null ./cmd/hopos
# Mét ingebakken config én kooi-stub (dit board kan zijn eigen bootmedium niet
# lezen, dus dít is de smaak die er écht op gaat). De blobs maakt de gate zelf
# (gate_stub), zodat deze builds altijd meelopen en niet alleen op een machine
# waar een image-build resten achterliet.
gate_stub cmd/hopos/cfgblob/hopos.cfg 'hopos.node=gate
'
gate_stub kern/cagestub/stub-slot.bin
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=riscv64 \
	"$TAMAGO" build -tags "licheerv linkcpuinit embedcfg embedcagestub" -o /dev/null ./cmd/hopos

# probeuefi is de enige overgebleven probe: default-modus van uefi-run.sh en
# het meetinstrument voor de O6N-bring-up die nog komt. probe4/5/6 zijn
# gesloopt (opruimronde 18-07): hun functie is geproductiseerd (PSCI/CPU_ON in
# de mains, PCIe→RP1→GEM→DHCP in hopnet.Up) — terughalen kan uit git history.
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags "uefi linkcpuinit" -o /dev/null ./cmd/probeuefi
# Radxa Zero 3E (RK3566): de agent én het meetinstrument van zijn bring-up
# (docs/archief/radxa-zero3.md). De probe blijft in de gate omdat hij een andere
# vraag beantwoordt dan de agent — hij meet het silicium zonder van netwerk of
# plan af te hangen.
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags "linkcpuinit" -o /dev/null ./cmd/proberk3566
for tags in "rk3566 linkcpuinit" "rk3566 gui linkcpuinit"; do
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
		"$TAMAGO" build -tags "$tags" -o /dev/null ./cmd/hopos
done
# Apple silicon (Mac mini M4, t8132): de derde boot-route in de boom — geen
# kaart en geen U-Boot, wij zijn zelf het bootobject van iBoot. board/apple
# hangt aan cpu/memattr, driver/rtkit, driver/nvme en driver/smc — packages die
# elders gewijzigd worden — dus deze smaak hoort in de gate. Hij bouwt alleen
# tegen de tamago-fork met de L0-tabel voor RAM boven 512GB
# (image/apple/go.work → ../../tamago, branch hopos-highram), en waar die fork
# niet staat slaat de gate hem over i.p.v. rood te worden.
APPLE_GATE=""
if [ -f ../image/apple/go.work ] && [ -d ../../tamago ]; then
	# tags/target per smaak; VHE omdat Apple's EL2 E2H vast op 1 heeft en het
	# linkadres omdat dit board zijn venster op 1TiB+4GB heeft (apple.RamBase).
	for spec in "linkcpuinit highram:./cmd/probeapple" "apple linkcpuinit highram:./cmd/hopos" "apple linkcpuinit highram:./cmd/hopos-embed"; do
		GOWORK="$PWD/../image/apple/go.work" GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
			"$TAMAGO" build -tags "${spec%%:*}" -asmflags "all=-D=VHE" \
			-ldflags "-T 0x10100010000 -R 0x1000" -o /dev/null "${spec#*:}"
	done
	APPLE_GATE=" + apple probe/agent/embed"
else
	echo "gate: apple-smaak overgeslagen (tamago-fork ../../tamago ontbreekt)" >&2
fi

echo "OK: host-tests groen, tamago-gate (arm64: virt/rpi4/rpi5/uefi kaal + gui-smaken + embed-mains incl. rk3566 + probeuefi + proberk3566 + rk3566-agent kaal én gui${APPLE_GATE}; riscv64: cmd/hopos kaal én embedcfg/embedcagestub + slot-demo + slot-app + switchtest) gebouwd" >&2
