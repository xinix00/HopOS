#!/bin/sh
# Bouw en boot HopOS voor UEFI/ACPI-platforms op QEMU -M virt met échte
# EDK2-firmware — de proeftuin voor de Ampere Altra: zelfde firmware-familie,
# zelfde weg (FAT-medium → EFI/BOOT/BOOTAA64.EFI → PE-stub → relocatie →
# tamago). Wat hier boot, hoort op de Altra van een USB-stick te booten.
#
#   image/uefi-run.sh              probe: metal/cmd/probeuefi (discovery)
#   image/uefi-run.sh probe        idem
#   image/uefi-run.sh agent        de échte HOP-node (cmd/hopos + app-image)
#
# Eén script, twee modi (qemu-run.sh-precedent): het zelfkiezende venster,
# de mkkernel-verpakking en het QEMU-recept zijn identiek; alleen de payload
# en de netwerk-forwards verschillen. De -cpu is neoverse-n1 (Altra-silicium);
# virtualization=on levert ons op EL2 af (de HopOS-eis).
#
# Job submitten in agent-modus (poorten via hostfwd):
#   python3 -m http.server 8000 --directory metal/out &   # serveert app-uefi.elf
#   curl -X POST http://127.0.0.1:9080/v1/jobs -d '{
#     "name":"werkje","driver":"hop","tags":{"core-class":"big"},
#     "artifacts":[{"url":"http://10.0.2.2:8000/app-uefi.elf"}],
#     "memory_limit":100663296}'

set -e

TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"
QEMU_SHARE="${QEMU_SHARE:-/opt/homebrew/share/qemu}"
SMP="${SMP:-4}"
MEM="${MEM:-6G}"
# -cpu neoverse-n1 = het Altra-silicium (geen FEAT_RNG → SMCCC/jitter-pad).
# CPU=max forceert een core mét FEAT_RNG (RNDR) om het hardware-RNG-pad in
# QEMU te bewijzen; de Altra zelf blijft neoverse-n1.
CPU="${CPU:-neoverse-n1}"
MODE="${1:-probe}"
[ $# -gt 0 ] && shift # rest gaat door naar QEMU (zie "$@" aan het eind)

# De venster-kandidaten: de stub kiest bij boot de eerste waar AllocatePages
# slaagt (zie metal/board/uefi). Elke kandidaat heeft 160MB aaneengesloten
# vrij RAM nodig vanaf zijn basis: Go-RAM 128MB (0x08000000; 64MB bleek op
# 15-07 te krap voor 127 gelijktijdige twee-fase-starts) + carve 32MB
# (0x02000000, de net-ringen leven in de partitie-staart) = 0x0A000000. De
# slot-pool komt daarná uit ál het resterende bruikbare DRAM (usablePool).
#
# Herijking (review golf-2 #5): AFGEROND door de krimp — de lijst werd ooit
# voor een 128MB-claim gekozen en de claim is nu 96MB, dus elke kandidaat
# vraagt mínder dan waarvoor hij is geprikt. Faalt er tóch een (bezet RAM,
# Altra: o.a. rond 0x90000000), dan slaat de stub 'm over; falen álle zes,
# dan print hij "RAM WINDOW BUSY" + de vrije regio's — voeg dan een kandidaat
# toe. Gespreid over het lage Altra-DRAM (0x80000000..0xFFFFFFFF) + een lage
# QEMU-terugvaller.
SLOTS="${SLOTS:-0xB0000000 0xA0000000 0xC8000000 0x88000000 0xE8000000 0x50000000}"

case "$MODE" in
probe)
	PKG=./cmd/probeuefi
	ESP="$DIR/uefi-esp"
	FWD=""
	;;
agent)
	PKG=./cmd/hopos
	ESP="$DIR/uefi-esp-agent"
	FWD="hostfwd=tcp:127.0.0.1:8080-10.0.2.15:8080,hostfwd=tcp:127.0.0.1:9080-10.0.2.15:9080,hostfwd=tcp:127.0.0.1:18080-10.0.2.15:18080"
	;;
*)
	echo "gebruik: $0 [probe|agent]" >&2
	exit 64
	;;
esac

cd "$DIR/metal"
mkdir -p out

# Tags: de node-image bakt in agent-modus de apploader ín (embedloader) — geen
# externe URL, self-contained. De probe heeft 'm niet nodig.
TAGS="uefi linkcpuinit"
[ "$MODE" = agent ] && TAGS="$TAGS embedloader"

# In agent-modus de app-image (die de apps zelf downloaden, via de http.server
# geserveerd als HOP_IMAGE_URL) én de universele apploader. De apploader wordt
# NIET geserveerd maar íngebakken in de node: hij landt gecomprimeerd op de
# go:embed-plek (kern/apploaderblob/apploader.elf.gz) zodat de node-build
# (embedloader) 'm meeneemt.
# Canoniek gelinkt (slot-1-IPA; zonder -s: slots patcht RamStart/RamSize/slotHint).
if [ "$MODE" = agent ]; then
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
		"$TAMAGO" build -tags linkcpuinit -trimpath \
		-ldflags "-w -T 0x50010000 -R 0x1000" -o out/app-uefi.elf ./app/appspike
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
		"$TAMAGO" build -tags linkcpuinit -trimpath \
		-ldflags "-w -T 0x50010000 -R 0x1000" -o kern/apploaderblob/apploader.elf ./app/apploader
	# Gecomprimeerd inbakken (gzip -9: 8,4→3,1MB — de blob zit 6× in deze PE);
	# de node pakt 'm één keer lazy uit (kern/apploaderblob). -n: geen naam/
	# tijdstempel in de gzip-header → deterministische builds.
	gzip -9 -n -f kern/apploaderblob/apploader.elf
fi

# 1. Eén ELF per venster-kandidaat (zelfde build, ander -T; -buildid= zodat
#    de varianten byte-identiek zijn op de adressen na — de eis van de
#    reloc-diff). mkkernel -reloc verpakt er straks ÉÉN als payload en
#    gebruikt de rest alleen als diff-bewijs (docs/pe-relocatie.md).
#    Parallel linken:
#    zes onafhankelijke builds, wall-clock ≈ één i.p.v. zes. PID's verzamelen
#    en per stuk waiten: een kaal `wait` returnt onder `set -e` ALTIJD 0, dus
#    een gefaalde background-link zou stil doorglippen en mkkernel de STALE
#    .elf van de vorige run verpakken (namen zijn stabiel + gitignored) —
#    dan boot je ongemerkt code van gisteren (op de Altra: een verloren cyclus).
ELFS=""
PIDS=""
for base in $SLOTS; do
	text=$(printf '0x%X' $((base + 0x10000)))
	out="hopos-uefi-$MODE-$base.elf"
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
		"$TAMAGO" build -tags "$TAGS" -trimpath \
		-ldflags "-buildid= -w -T $text -R 0x1000" -o "out/$out" "$PKG" &
	PIDS="$PIDS $!"
	ELFS="$ELFS -elf metal/out/$out"
done
for pid in $PIDS; do
	wait "$pid" || { echo "FOUT: venster-link (pid $pid) faalde — build afgebroken" >&2; exit 1; }
done

# 2. ELFs → één zelfkiezende BOOTAA64.EFI in de ESP-boom. GO111MODULE=off:
#    mkkernel is puur stdlib en de repo-root heeft geen go.mod (de module
#    woont in metal/) — GOPATH-modus draait de losse .go-bestanden zonder
#    module-gezeur, ongeacht de shell-omgeving.
cd "$DIR"
mkdir -p "$ESP/EFI/BOOT"
GO111MODULE=off go run "$DIR/image/mkkernel/main.go" "$DIR/image/mkkernel/pe.go" \
	$ELFS -o "$ESP/EFI/BOOT/BOOTAA64.EFI" -pe -reloc

# 3. Verse varstore (boot-entries verouderen bij een topologie-wijziging →
#    EDK2 valt anders in de Shell i.p.v. onze BOOTAA64 te booten).
dd if=/dev/zero of=metal/out/uefi-vars.fd bs=1m count=64 2>/dev/null

echo "BOOTAA64.EFI ($(du -h "$ESP/EFI/BOOT/BOOTAA64.EFI" | cut -f1), mode=$MODE) klaar — EDK2 boot..." >&2
[ "$MODE" = agent ] && echo "agent: curl http://127.0.0.1:8080/health · leader: curl http://127.0.0.1:9080/health" >&2

# USB-semantiek zoals op de Altra (xhci + usb-storage op de vvfat-boom) +
# igb achter een root-port (de Altra's i210-opstelling). Geen virtio-rng: de
# stub roept het EFI_RNG_PROTOCOL bewust niet aan (zie board/uefi, jitter-DRBG).
exec qemu-system-aarch64 -M virt,gic-version=3,virtualization=on \
	-cpu "$CPU" -smp "$SMP" -m "$MEM" \
	-boot menu=on,splash-time=0 \
	-nographic -monitor none -serial stdio \
	-drive if=pflash,format=raw,readonly=on,file="$QEMU_SHARE/edk2-aarch64-code.fd" \
	-drive if=pflash,format=raw,file=metal/out/uefi-vars.fd \
	-device qemu-xhci \
	-drive file=fat:rw:"$ESP",format=raw,if=none,id=esp \
	-device usb-storage,drive=esp \
	-device pcie-root-port,id=rp1,chassis=1 \
	-device igb,bus=rp1,netdev=n1 \
	-netdev "user,id=n1${FWD:+,$FWD}" \
	"$@"
