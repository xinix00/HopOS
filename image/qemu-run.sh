#!/bin/sh
# Boot HopOS op QEMU -M virt — PLAN.md fase 1. Altijd virtualization=on:
# HopOS eist een EL2-boot (de stage-2-kooi is een invariant, geen optie);
# PSCI via SMC, GICv3, tot 12 cores — zelfde bouwstenen als de O6N.
#
#   image/qemu-run.sh          demo/regressie: cmd/hopos-embed (HOPOS_*-markers)
#   image/qemu-run.sh bench    meetbank core-deling (cmd/hopos-embed/schedbench.go)
#   image/qemu-run.sh agent    de echte HOP-agent + leader (cmd/hopos)
#
# Job submitten vanaf de Mac (agent-modus, poorten via hostfwd):
#   python3 -m http.server 8000 --directory metal/out &   # serveert app.elf
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
. "$DIR/image/lib.sh"

# De ingebakken blobs zijn build-input, geen bronbestand: na deze run horen ze
# weg te zijn (ook als hij halverwege faalt). Anders bouwt een volgende build
# ongemerkt met de resten van deze mee.
trap clean_embeds EXIT INT TERM

SMP="${SMP:-4}"
MODE="${1:-demo}"
[ $# -gt 0 ] && shift

cd "$DIR/metal"
mkdir -p out

# 1. De app-image: één canoniek gelinkt artifact (TEXT_START =
#    SlotBase(1) + 0x10000, zie metal/abi/layout). Zonder -s: de symboltabel is
#    nodig zodat de slot-manager RamStart/RamSize kan patchen (job.MemoryLimit).
#    Demo-modus bakt hem ín de kern (go:embed) — dan moet hij op de embed-plek
#    naast de main landen; agent-modus serveert hem los via http.server.
APP=out/app.elf
case "$MODE" in demo | bench | flip) APP=cmd/hopos-embed/app.elf ;; esac
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags "linkcpuinit" -trimpath \
	-ldflags "-w -T 0x50010000 -R 0x1000" -o "$APP" ./app/appspike

# 2. De kern + het poort-plan van de gekozen modus. Twee smaken: kaal
#    (headless) en gui (metal/gui + fb-grant). Default gui; GUI=0 bouwt de
#    kale smaak. (Zelfde knop in alle imagescripts.)
GUITAG=""
[ "${GUI:-1}" = 1 ] && GUITAG=" gui"
case "$MODE" in
demo|bench)
	# bench = dezelfde kern, maar de meetbank i.p.v. de demo
	# (cmd/hopos-embed/schedbench.go): core-deling meten in plaats van bewijzen.
	BENCHX=""
	[ "$MODE" = bench ] && BENCHX=" -X main.benchMode=1"
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
		"$TAMAGO" build -tags "qemuvirt linkcpuinit$GUITAG" -trimpath \
		-ldflags "-s -w -T 0x40010000 -R 0x1000$BENCHX" -o out/hopos-virt.elf ./cmd/hopos-embed
	KERNEL=out/hopos-virt.elf
	FWD="hostfwd=tcp:127.0.0.1:${HOPPORT:-8080}-10.0.2.15:80,hostfwd=tcp:127.0.0.1:${PORTPUB:-18080}-10.0.2.15:8080"
	echo "hopos-virt.elf ($(du -h out/hopos-virt.elf | cut -f1), incl. app.elf) gebouwd — QEMU -smp $SMP start..." >&2
	echo "HOP-kern HTTP: curl http://127.0.0.1:${HOPPORT:-8080}/ · poort-publicatie: nc 127.0.0.1 ${PORTPUB:-18080}" >&2
	;;
agent)
	# Werkbank-config (optioneel): HOPCFG="hopos.s3.endpoint=http://10.0.2.2:9000 ..."
	# gaat als -X de kern in (board_virt.go extraCfg) — QEMU heeft geen
	# bootmedium, dit is de regressie-knop voor config-gedreven paden.
	XCFG=""
	[ -n "${HOPCFG:-}" ] && XCFG=" -X 'main.extraCfg=$HOPCFG'"
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
		"$TAMAGO" build -tags "linkcpuinit$GUITAG" -trimpath \
		-ldflags "-s -w -T 0x40010000 -R 0x1000$XCFG" -o out/hopos-agent.elf ./cmd/hopos
	KERNEL=out/hopos-agent.elf
	FWD="hostfwd=tcp:127.0.0.1:${AGENTPORT:-8080}-10.0.2.15:8080,hostfwd=tcp:127.0.0.1:${LEADERPORT:-9080}-10.0.2.15:9080,hostfwd=tcp:127.0.0.1:${PORTPUB:-18080}-10.0.2.15:18080"
	echo "hopos-agent.elf ($(du -h out/hopos-agent.elf | cut -f1)) gebouwd — QEMU -smp $SMP start..." >&2
	echo "agent:  curl http://127.0.0.1:${AGENTPORT:-8080}/health" >&2
	echo "leader: curl http://127.0.0.1:${LEADERPORT:-9080}/health" >&2
	;;
flip)
	# Kern-flip-regressie (docs/kern-flip.md): dezelfde demo-kern, maar mét de
	# flip-stap — kern A haalt de bundel van de host en springt erin; de
	# geflipte kern B (zelfde build, herbaseerd naar een geleend pool-venster)
	# meldt HOPOS_FLIP_BOOT en draait de volle demo af. De bundel is deze
	# build op het canonieke linkadres + één schaduw-variant op een ander -T
	# als diff-bewijs (mkkernel -elfreloc). Eisen aan de varianten: -w zónder
	# -s (kernflip patcht RamStart/RamSize via de symboltabel) en -buildid=
	# (anders is de diff geen zuivere linkbasis-delta).
	FLIPX=" -X main.flipMode=1 -X main.flipURL=http://10.0.2.2:8071/flip.img"
	for VAR in "0x40010000:out/hopos-flipA.elf" "0x70010000:out/hopos-flipB.elf"; do
		GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
			"$TAMAGO" build -tags "qemuvirt linkcpuinit$GUITAG" -trimpath \
			-ldflags "-w -buildid= -T ${VAR%%:*} -R 0x1000$FLIPX" -o "${VAR#*:}" ./cmd/hopos-embed
	done
	GO111MODULE=off go run "$DIR"/image/mkkernel/*.go -elfreloc -o out/flip.img \
		-elf out/hopos-flipA.elf -elf out/hopos-flipB.elf
	KERNEL=out/hopos-flipA.elf
	FWD="hostfwd=tcp:127.0.0.1:${HOPPORT:-8080}-10.0.2.15:80,hostfwd=tcp:127.0.0.1:${PORTPUB:-18080}-10.0.2.15:8080"
	# Bundel-server voor de gast (slirp: 10.0.2.2 = host-loopback).
	python3 -m http.server 8071 --directory out --bind 127.0.0.1 >/dev/null 2>&1 &
	FLIPHTTP=$!
	trap 'kill $FLIPHTTP 2>/dev/null; clean_embeds' EXIT INT TERM
	echo "hopos-flipA.elf ($(du -h out/hopos-flipA.elf | cut -f1)) + flip.img gebouwd — QEMU -smp $SMP start (flip-regressie)..." >&2
	;;
*)
	echo "gebruik: $0 [demo|bench|agent|flip]" >&2
	exit 64
	;;
esac

# NVMe-scratchdisk (raw, wegwerp — HopOS beschouwt hem bij boot als leeg).
[ -f out/nvme.img ] || dd if=/dev/zero of=out/nvme.img bs=1m count=16 2>/dev/null

# virtio-net expliciet op de mmio-bus (virt zet 'm anders op PCIe) + modern
# (force-legacy=false → versie 2). highmem-ecam=off houdt de PCIe-ECAM op
# 0x3f000000 (32-bit; zie metal/driver/pcie).
# -m 3G: het qemuvirt-PA-plan legt ctrl/ringen/stage-2 bewust op 0xC0000000+
# (non-identity t.o.v. de IPA's — bewijst de IPA/PA-splitsing), dus de RAM
# moet tot voorbij 0xC4600000 reiken.
exec qemu-system-aarch64 -M virt,gic-version=3,highmem-ecam=off,virtualization=on \
	-cpu cortex-a53 -smp "$SMP" -m 3G \
	-nographic -monitor none -serial stdio \
	-global virtio-mmio.force-legacy=false \
	-device virtio-net-device,netdev=n0,bus=virtio-mmio-bus.0 \
	-netdev "user,id=n0,$FWD" \
	-drive file=out/nvme.img,if=none,format=raw,id=nvm \
	-device nvme,serial=hopos-scratch,drive=nvm \
	-kernel "$KERNEL" "$@"
