#!/bin/sh
# Boot HopOS multikernel-proof op QEMU -M virt (PLAN.md fase 1, stap 4).
# virt levert PSCI (HVC), GICv3 en tot 12 cores — zelfde bouwstenen als de O6N.

set -e

TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"
SMP="${SMP:-4}"

cd "$DIR/metal"

# 1. App-images. Op EL2 volstaat één canoniek gelinkte image (slot-1-bereik;
#    de stage-2-map is de relocatie) — de per-slot varianten hier bestaan
#    alleen nog voor de EL1-steiger. Zonder -s: de symboltabel is nodig
#    zodat de slot-manager RamStart/RamSize kan patchen (job.MemoryLimit).
for i in 1 2 3; do
	base=$(printf '%#x' $((0x50000000 + (i - 1) * 0x20000000 + 0x10000)))
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
		"$TAMAGO" build -tags linkcpuinit -trimpath \
		-ldflags "-w -T $base -R 0x1000" -o "app$i.elf" ./appspike
done

# 2. De HOP-kern (core 0), met de app-images embedded.
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags "qemuvirt linkcpuinit" -trimpath \
	-ldflags "-s -w -T 0x40010000 -R 0x1000" -o hopos-virt.elf .

echo "hopos-virt.elf ($(du -h hopos-virt.elf | cut -f1), incl. 3 app-images) gebouwd — QEMU -smp $SMP start..." >&2
echo "HOP-kern HTTP: curl http://127.0.0.1:${HOPPORT:-8080}/" >&2

# NVMe-scratchdisk (raw, wegwerp — HopOS beschouwt hem bij boot als leeg).
[ -f nvme.img ] || dd if=/dev/zero of=nvme.img bs=1m count=16 2>/dev/null

# virtio-net expliciet op de mmio-bus (virt zet 'm anders op PCIe) + modern
# (force-legacy=false → version 2). hostfwd mapt de HOP-poort naar de host.
# highmem-ecam=off houdt de PCIe-ECAM op 0x3f000000 (32-bit; zie metal/pcie).
VIRT="${EL2:+,virtualization=on}"
exec qemu-system-aarch64 -M "virt,gic-version=3,highmem-ecam=off$VIRT" -cpu cortex-a53 -smp "$SMP" -m 2G \
	-nographic -monitor none -serial stdio \
	-global virtio-mmio.force-legacy=false \
	-device virtio-net-device,netdev=n0,bus=virtio-mmio-bus.0 \
	-netdev "user,id=n0,hostfwd=tcp:127.0.0.1:${HOPPORT:-8080}-10.0.2.15:80,hostfwd=tcp:127.0.0.1:${PORTPUB:-18080}-10.0.2.15:8080" \
	-drive file=nvme.img,if=none,format=raw,id=nvm \
	-device nvme,serial=hopos-scratch,drive=nvm \
	-kernel hopos-virt.elf "$@"
