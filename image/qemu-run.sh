#!/bin/sh
# Boot HopOS v3 op QEMU -M virt. Altijd virtualization=on: HopOS eist een
# EL2-boot (de stage-2-kooi is een invariant, geen optie); PSCI via SMC,
# GICv3, tot 12 cores: dezelfde bouwstenen als de O6N. De QEMU-regel is die
# van de Go-generatie (OLD/image/qemu-run.sh), zonder de NVMe-schijf (nog
# geen driver). Drie hostfwd's: de system-API (10.0.2.15:10100) op
# 127.0.0.1:$SYSPORT, en de API van Hop: de agent (:8080) op
# 127.0.0.1:$AGENTPORT en de leader (:9080) op 127.0.0.1:$LEADERPORT. De
# kern zet 8080 en 9080 van de uplink door naar het slot van Hop (DNAT).
#
#   image/qemu-run.sh                Hop in slot 1 (standaard)
#   APP=appspike image/qemu-run.sh   een app uit deze werkruimte, twee keer
#                                    door de kern geplaatst (het ABI-bewijs)
#   APP=/pad/naar/elf ROLE=0|1 image/qemu-run.sh   een kant-en-klare ELF
#   APP= image/qemu-run.sh           zonder app-image
#   image/qemu-run.sh -s -S          extra argumenten gaan naar QEMU (gdb)
#
# APP=hop bouwt `agentd-hopos` in de hop-repo ($HOP_DIR, standaard
# ../hop/hop naast deze repo) met cargo en neemt alleen het bestand: geen
# pad-dependency over de repo-grens (PORT.md beslissing 6).
#
# Het image gaat rauw in RAM op de staging van het board
# (board/qemuvirt/src/slots.rs: STAGE_PA), met zijn maat in het woord op
# STAGE_HDR_PA en zijn rol op STAGE_ROLE_PA (0 = app, 1 = Hop). De staging
# is 14 MiB; de debug-info gaat eraf (rust-objcopy --strip-debug), de
# symbolen blijven, want de plaatsing leest __hopos_* uit de symbooltabel.
#
# QEMU laadt de kern-ELF rechtstreeks op zijn fysieke adressen
# (hopos/link.ld); een raw image met objcopy is niet nodig.
set -e

DIR="$(cd "$(dirname "$0")/.." && pwd)"
SMP="${SMP:-4}"
SYSPORT="${SYSPORT:-10100}"
AGENTPORT="${AGENTPORT:-8080}"
LEADERPORT="${LEADERPORT:-9080}"
HOP_DIR="${HOP_DIR:-$DIR/../hop/hop}"
TARGET=aarch64-unknown-none-softfloat
STAGE_MAX=14680064 # 0xB100_0000 - 0xB020_0000

cd "$DIR"
cargo build --quiet --release --target "$TARGET" -p hopos --features board-qemuvirt
KERNEL="$DIR/target/$TARGET/release/hopos"

APP="${APP-hop}"
IMAGE=""
case "$APP" in
"") ;;
hop)
	(cd "$HOP_DIR" && cargo build --quiet --release --target "$TARGET" -p agentd-hopos)
	IMAGE="$HOP_DIR/target/$TARGET/release/agentd-hopos"
	ROLE="${ROLE:-1}"
	;;
*/*)
	IMAGE="$APP"
	ROLE="${ROLE:-0}"
	;;
*)
	cargo build --quiet --release --target "$TARGET" -p "$APP"
	IMAGE="$DIR/target/$TARGET/release/$APP"
	ROLE="${ROLE:-0}"
	;;
esac

if [ -n "$IMAGE" ]; then
	# Zonder debug-info: de release-profielen dragen `debug = true` voor de
	# zwarte doos, en agentd-hopos is daarmee 12 MB (gemeten 29-09) tegen
	# 594 KiB laadbaar.
	OBJCOPY="$(ls "$(rustc --print sysroot)"/lib/rustlib/*/bin/rust-objcopy 2>/dev/null | head -1)"
	if [ -n "$OBJCOPY" ]; then
		STAGED="$DIR/target/$TARGET/release/staged-$(basename "$IMAGE").elf"
		"$OBJCOPY" --strip-debug "$IMAGE" "$STAGED"
		IMAGE="$STAGED"
	fi
	SIZE=$(wc -c <"$IMAGE" | tr -d ' ')
	if [ "$SIZE" -gt "$STAGE_MAX" ]; then
		echo "qemu-run: $IMAGE is $SIZE bytes, the staging holds $STAGE_MAX" >&2
		exit 1
	fi
	set -- -device "loader,file=$IMAGE,addr=0xb0200000,force-raw=on" \
		-device "loader,addr=0xb0100000,data=$SIZE,data-len=8" \
		-device "loader,addr=0xb0100008,data=$ROLE,data-len=8" "$@"
fi

# virtio-net expliciet op de mmio-bus (virt zet hem anders op PCIe) en
# modern (force-legacy=false: transportversie 2). -m 3G: het PA-plan van
# virt legt de slot-pool tot voorbij 0xC000_0000.
FWD="hostfwd=tcp:127.0.0.1:${SYSPORT}-:10100"
FWD="$FWD,hostfwd=tcp:127.0.0.1:${AGENTPORT}-:8080,hostfwd=tcp:127.0.0.1:${LEADERPORT}-:9080"
exec qemu-system-aarch64 -M virt,gic-version=3,highmem-ecam=off,virtualization=on \
	-cpu cortex-a53 -smp "$SMP" -m 3G \
	-nographic -monitor none -serial stdio \
	-global virtio-mmio.force-legacy=false \
	-device virtio-net-device,netdev=n0,bus=virtio-mmio-bus.0 \
	-netdev "user,id=n0,$FWD" \
	-kernel "$KERNEL" "$@"
