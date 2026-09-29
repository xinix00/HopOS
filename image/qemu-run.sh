#!/bin/sh
# Boot HopOS v3 op QEMU -M virt. Altijd virtualization=on: HopOS eist een
# EL2-boot (de stage-2-kooi is een invariant, geen optie); PSCI via SMC,
# GICv3, tot 12 cores: dezelfde bouwstenen als de O6N. De QEMU-regel is die
# van de Go-generatie (OLD/image/qemu-run.sh), zonder de NVMe-schijf (nog
# geen driver). Eén hostfwd: de system-API (10.0.2.15:10100) op
# 127.0.0.1:$SYSPORT (standaard 10100).
#
#   image/qemu-run.sh            bouwen en starten (Ctrl-A X stopt QEMU)
#   image/qemu-run.sh -s -S      extra argumenten gaan naar QEMU (hier: gdb)
#   APP= image/qemu-run.sh       zonder app-image (standaard: appspike)
#
# Het app-image gaat rauw in RAM op de staging van het board
# (board/qemuvirt/src/slots.rs: STAGE_PA), met zijn maat in het woord op
# STAGE_HDR_PA; de kern plaatst het bij boot in slot 1.
#
# QEMU laadt het ELF rechtstreeks op zijn fysieke adressen (hopos/link.ld);
# een raw image met objcopy is niet nodig.
set -e

DIR="$(cd "$(dirname "$0")/.." && pwd)"
SMP="${SMP:-4}"
SYSPORT="${SYSPORT:-10100}"
TARGET=aarch64-unknown-none-softfloat

cd "$DIR"
cargo build --quiet --release --target "$TARGET" -p hopos --features board-qemuvirt
KERNEL="$DIR/target/$TARGET/release/hopos"

# De staging van board/qemuvirt/src/slots.rs (STAGE_HDR_PA, STAGE_PA).
APP="${APP-appspike}"
if [ -n "$APP" ]; then
	cargo build --quiet --release --target "$TARGET" -p "$APP"
	IMAGE="$DIR/target/$TARGET/release/$APP"
	SIZE=$(wc -c <"$IMAGE" | tr -d ' ')
	set -- -device "loader,file=$IMAGE,addr=0xb0200000,force-raw=on" \
		-device "loader,addr=0xb0100000,data=$SIZE,data-len=8" "$@"
fi

# virtio-net expliciet op de mmio-bus (virt zet hem anders op PCIe) en
# modern (force-legacy=false: transportversie 2). -m 3G: het PA-plan van
# virt legt de slot-pool tot voorbij 0xC000_0000.
exec qemu-system-aarch64 -M virt,gic-version=3,highmem-ecam=off,virtualization=on \
	-cpu cortex-a53 -smp "$SMP" -m 3G \
	-nographic -monitor none -serial stdio \
	-global virtio-mmio.force-legacy=false \
	-device virtio-net-device,netdev=n0,bus=virtio-mmio-bus.0 \
	-netdev "user,id=n0,hostfwd=tcp:127.0.0.1:${SYSPORT}-:10100" \
	-kernel "$KERNEL" "$@"
