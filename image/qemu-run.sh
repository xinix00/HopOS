#!/bin/sh
# Boot HopOS op QEMU -M virt — PLAN.md fase 1. Altijd virtualization=on:
# HopOS eist een EL2-boot (de stage-2-kooi is een invariant, geen optie);
# PSCI via SMC, GICv3, tot 12 cores — zelfde bouwstenen als de O6N.
#
#   image/qemu-run.sh          demo/regressie: virt_main (HOPOS_*-markers)
#   image/qemu-run.sh agent    de echte HOP-agent + leader (cmd/hopos)
#
# Job submitten vanaf de Mac (agent-modus, poorten via hostfwd):
#   python3 -m http.server 8000 --directory metal &   # serveert app.elf
#   curl -X POST http://127.0.0.1:9080/v1/jobs -d '{
#     "name": "werkje", "driver": "hop", "tags": {"core-class": "big"},
#     "artifacts": [{"url": "http://10.0.2.2:8000/app.elf"}],
#     "memory_limit": 100663296, "env": {"BUCKET": "hop-apps"}}'
#
# Eén artifact voor elk slot: images zijn canoniek gelinkt (slot-1-bereik),
# de stage-2-map is de relocatie.

set -e

TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"
SMP="${SMP:-4}"
MODE="${1:-demo}"
[ $# -gt 0 ] && shift

cd "$DIR/metal"

# 1. De app-image: één canoniek gelinkt artifact (TEXT_START =
#    SlotBase(1) + 0x10000, zie metal/layout). Zonder -s: de symboltabel is
#    nodig zodat de slot-manager RamStart/RamSize kan patchen (job.MemoryLimit).
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags linkcpuinit -trimpath \
	-ldflags "-w -T 0x50010000 -R 0x1000" -o app.elf ./appspike

# 2. De kern + het poort-plan van de gekozen modus.
case "$MODE" in
demo)
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
		"$TAMAGO" build -tags "qemuvirt linkcpuinit" -trimpath \
		-ldflags "-s -w -T 0x40010000 -R 0x1000" -o hopos-virt.elf .
	KERNEL=hopos-virt.elf
	FWD="hostfwd=tcp:127.0.0.1:${HOPPORT:-8080}-10.0.2.15:80,hostfwd=tcp:127.0.0.1:${PORTPUB:-18080}-10.0.2.15:8080"
	echo "hopos-virt.elf ($(du -h hopos-virt.elf | cut -f1), incl. app.elf) gebouwd — QEMU -smp $SMP start..." >&2
	echo "HOP-kern HTTP: curl http://127.0.0.1:${HOPPORT:-8080}/ · poort-publicatie: nc 127.0.0.1 ${PORTPUB:-18080}" >&2
	;;
agent)
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
		"$TAMAGO" build -tags linkcpuinit -trimpath \
		-ldflags "-s -w -T 0x40010000 -R 0x1000" -o hopos-agent.elf ./cmd/hopos
	KERNEL=hopos-agent.elf
	FWD="hostfwd=tcp:127.0.0.1:${AGENTPORT:-8080}-10.0.2.15:8080,hostfwd=tcp:127.0.0.1:${LEADERPORT:-9080}-10.0.2.15:9080,hostfwd=tcp:127.0.0.1:${PORTPUB:-18080}-10.0.2.15:18080"
	echo "hopos-agent.elf ($(du -h hopos-agent.elf | cut -f1)) gebouwd — QEMU -smp $SMP start..." >&2
	echo "agent:  curl http://127.0.0.1:${AGENTPORT:-8080}/health" >&2
	echo "leader: curl http://127.0.0.1:${LEADERPORT:-9080}/health" >&2
	;;
*)
	echo "gebruik: $0 [demo|agent]" >&2
	exit 64
	;;
esac

# NVMe-scratchdisk (raw, wegwerp — HopOS beschouwt hem bij boot als leeg).
[ -f nvme.img ] || dd if=/dev/zero of=nvme.img bs=1m count=16 2>/dev/null

# virtio-net expliciet op de mmio-bus (virt zet 'm anders op PCIe) + modern
# (force-legacy=false → versie 2). highmem-ecam=off houdt de PCIe-ECAM op
# 0x3f000000 (32-bit; zie metal/pcie).
# -m 3G: het qemuvirt-PA-plan legt ctrl/ringen/stage-2 bewust op 0xC0000000+
# (non-identity t.o.v. de IPA's — bewijst de IPA/PA-splitsing), dus de RAM
# moet tot voorbij 0xC4600000 reiken.
exec qemu-system-aarch64 -M virt,gic-version=3,highmem-ecam=off,virtualization=on \
	-cpu cortex-a53 -smp "$SMP" -m 3G \
	-nographic -monitor none -serial stdio \
	-global virtio-mmio.force-legacy=false \
	-device virtio-net-device,netdev=n0,bus=virtio-mmio-bus.0 \
	-netdev "user,id=n0,$FWD" \
	-drive file=nvme.img,if=none,format=raw,id=nvm \
	-device nvme,serial=hopos-scratch,drive=nvm \
	-kernel "$KERNEL" "$@"
