#!/bin/sh
# Boot HopOS mét de echte HOP-agent (PLAN.md fase 1, stap 3): hop's agent +
# leader draaien bare-metal op core 0, jobs met driver "hop" starten native
# app-images op cores 1..N via metal/slots.
#
# Job submitten vanaf de Mac (poorten via hostfwd):
#   python3 -m http.server 8000 --directory metal &   # serveert app-images
#   curl -X POST http://127.0.0.1:9080/v1/jobs -d '{
#     "name": "werkje", "driver": "hop", "tags": {"core-class": "small"},
#     "artifacts": [{"url": "http://10.0.2.2:8000/app1.elf"}],
#     "memory_limit": 100663296, "env": {"BUCKET": "hop-apps"}}'
#
# Bij een EL2-boot (EL2=1) is één artifact genoeg: images zijn canoniek
# gelinkt (slot-1-bereik) en de stage-2-map legt ze in elk slot — gebruik
# app1.elf voor álle jobs. Alleen de EL1-steiger (geen stage-2) eist nog
# per-slot varianten (app<i>.elf op slot i).

set -e

TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"
SMP="${SMP:-4}"

cd "$DIR/metal"

# 1. App-images: per slot gelinkt, symboltabel intact (RamStart/RamSize-patch).
for i in 1 2 3; do
	base=$(printf '%#x' $((0x50000000 + (i - 1) * 0x20000000 + 0x10000)))
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
		"$TAMAGO" build -tags linkcpuinit -trimpath \
		-ldflags "-w -T $base -R 0x1000" -o "app$i.elf" ./appspike
done

# 2. De HOP-agent-kern (core 0).
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags linkcpuinit -trimpath \
	-ldflags "-s -w -T 0x40010000 -R 0x1000" -o hopos-agent.elf ./cmd/hopos

echo "hopos-agent.elf ($(du -h hopos-agent.elf | cut -f1)) gebouwd — QEMU -smp $SMP start..." >&2
echo "agent:  curl http://127.0.0.1:${AGENTPORT:-8080}/health" >&2
echo "leader: curl http://127.0.0.1:${LEADERPORT:-9080}/health" >&2

# NVMe-scratchdisk (raw, wegwerp) — de storage voor volumes.
[ -f nvme.img ] || dd if=/dev/zero of=nvme.img bs=1m count=16 2>/dev/null

# highmem-ecam=off houdt de PCIe-ECAM op 0x3f000000 (32-bit; zie metal/pcie).
VIRT="${EL2:+,virtualization=on}"
exec qemu-system-aarch64 -M "virt,gic-version=3,highmem-ecam=off$VIRT" -cpu cortex-a53 -smp "$SMP" -m 2G \
	-nographic -monitor none -serial stdio \
	-global virtio-mmio.force-legacy=false \
	-device virtio-net-device,netdev=n0,bus=virtio-mmio-bus.0 \
	-netdev "user,id=n0,hostfwd=tcp:127.0.0.1:${AGENTPORT:-8080}-10.0.2.15:8080,hostfwd=tcp:127.0.0.1:${LEADERPORT:-9080}-10.0.2.15:9080,hostfwd=tcp:127.0.0.1:${PORTPUB:-18080}-10.0.2.15:18080" \
	-drive file=nvme.img,if=none,format=raw,id=nvm \
	-device nvme,serial=hopos-scratch,drive=nvm \
	-kernel hopos-agent.elf "$@"
