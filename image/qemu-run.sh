#!/bin/sh
# Boot HopOS v3 op QEMU -M virt. Altijd virtualization=on: HopOS eist een
# EL2-boot (de stage-2-kooi is een invariant, geen optie); PSCI via SMC,
# GICv3, tot 12 cores: dezelfde bouwstenen als de O6N. De QEMU-regel is die
# van de Go-generatie (OLD/image/qemu-run.sh), met een virtio-blk-schijf in
# plaats van de NVMe: hopfs mount hem bij boot, en Hop bewaart er zijn staat
# op (`/hop/`). Drie hostfwd's: de system-API (10.0.2.15:10100) op
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
#   SMP=2 image/qemu-run.sh          twee cores: de kern en Hop delen de
#                                    OS-core, de andere is een volle
#                                    app-core (PORT.md beslissing 2);
#                                    standaard 4
#   OSCORE=1 image/qemu-run.sh       de OS-core (bootparameter
#                                    hopos.oscore=<small|mid|big|N> in de
#                                    FDT-bootargs); de kern boot op core 0
#                                    en verhuist er vóór de eerste bewoner
#                                    heen. Standaard geen: de boot-core
#   APP=appspike APPENV=GUI=display image/qemu-run.sh
#                                    de env van de gestagede app
#                                    (bootparameter hopos.appenv; meer
#                                    sleutels met komma's). Het glas zelf
#                                    vraagt een gui-kern en ramfb: zie
#                                    GUI=display in tools/qemu-test.sh
#   DISK=pad image/qemu-run.sh       de schijf (raw); standaard
#                                    target/hopos-disk.img, 64 MiB, aangemaakt
#                                    (ijl) als hij ontbreekt. Een verse schijf
#                                    is een lege hopfs; een bestaande houdt de
#                                    volumes over een herstart (stateful).
#   WEBPORT=8081 image/qemu-run.sh   een vierde hostfwd: 127.0.0.1:$WEBPORT
#                                    naar poort 80 van de gast, de poort die
#                                    een jobspec met "ports":{"http":80}
#                                    publiceert (de kern zet hem door naar
#                                    het slot van de app)
#   ARTIFACT=welcome image/qemu-run.sh
#                                    een app-ELF voor Hop: een crate uit deze
#                                    werkruimte (of een pad naar een ELF),
#                                    zonder debug-info op een artifact-server
#                                    op 127.0.0.1:$ARTPORT (standaard 8000),
#                                    voor de gast
#                                    http://10.0.2.2:$ARTPORT/<naam>.elf. Het
#                                    script drukt de URL en het curl-commando
#                                    met de jobspec af; de server stopt met
#                                    QEMU.
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
DISK="${DISK:-$DIR/target/hopos-disk.img}"
DISK_MIB="${DISK_MIB:-64}"

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

# De bootparameters: QEMU legt -append in /chosen/bootargs van de DTB, en
# daar leest het board ze (board/qemuvirt `os_core`, hopos `bootparam`).
# Eén -append: een tweede zou de eerste vervangen.
ARGS=""
[ -n "${OSCORE:-}" ] && ARGS="hopos.oscore=$OSCORE"
[ -n "${APPENV:-}" ] && ARGS="${ARGS:+$ARGS }hopos.appenv=$APPENV"
if [ -n "$ARGS" ]; then
	set -- -append "$ARGS" "$@"
fi

# De schijf: ijl aangemaakt als hij er niet is (dd met seek schrijft niets).
if [ ! -e "$DISK" ]; then
	mkdir -p "$(dirname "$DISK")"
	dd if=/dev/zero of="$DISK" bs=1048576 count=0 seek="$DISK_MIB" 2>/dev/null
	echo "qemu-run: new disk $DISK ($DISK_MIB MiB)" >&2
fi

# De artifact-server voor een app die Hop ophaalt (ARTIFACT). Zonder
# debug-info, zoals de staging hierboven; de symbolen blijven voor de
# plaatsing. QEMU draait dan als kind (geen exec), zodat de server met hem
# stopt.
ARTPID=""
if [ -n "${ARTIFACT:-}" ]; then
	ARTPORT="${ARTPORT:-8000}"
	case "$ARTIFACT" in
	*/*) ARTELF="$ARTIFACT" ;;
	*)
		cargo build --quiet --release --target "$TARGET" -p "$ARTIFACT"
		ARTELF="$DIR/target/$TARGET/release/$ARTIFACT"
		;;
	esac
	ARTDIR="$(mktemp -d -t hopos-artifact.XXXXXX)"
	ARTNAME="$(basename "$ARTELF" .elf).elf"
	OBJCOPY="$(ls "$(rustc --print sysroot)"/lib/rustlib/*/bin/rust-objcopy 2>/dev/null | head -1)"
	if [ -n "$OBJCOPY" ]; then
		"$OBJCOPY" --strip-debug "$ARTELF" "$ARTDIR/$ARTNAME"
	else
		cp "$ARTELF" "$ARTDIR/$ARTNAME"
	fi
	(cd "$ARTDIR" && exec python3 -m http.server "$ARTPORT" --bind 127.0.0.1) >/dev/null 2>&1 &
	ARTPID=$!
	trap 'kill "$ARTPID" 2>/dev/null; rm -rf "$ARTDIR"' EXIT INT TERM
	{
		echo "qemu-run: artifact http://10.0.2.2:$ARTPORT/$ARTNAME (host 127.0.0.1:$ARTPORT)"
		echo "qemu-run: curl -X POST -d '{\"name\":\"$(basename "$ARTNAME" .elf)\",\"driver\":\"hop\",\"artifacts\":[{\"url\":\"http://10.0.2.2:$ARTPORT/$ARTNAME\"}],\"memory_limit\":33554432,\"ports\":{\"http\":80}}' http://127.0.0.1:$LEADERPORT/v1/jobs"
	} >&2
fi

# virtio-net expliciet op de mmio-bus (virt zet hem anders op PCIe) en
# modern (force-legacy=false: transportversie 2). -m 3G: het PA-plan van
# virt legt de slot-pool tot voorbij 0xC000_0000.
FWD="hostfwd=tcp:127.0.0.1:${SYSPORT}-:10100"
FWD="$FWD,hostfwd=tcp:127.0.0.1:${AGENTPORT}-:8080,hostfwd=tcp:127.0.0.1:${LEADERPORT}-:9080"
if [ -n "${WEBPORT:-}" ]; then
	FWD="$FWD,hostfwd=tcp:127.0.0.1:${WEBPORT}-:80"
fi
set -- -M virt,gic-version=3,highmem-ecam=off,virtualization=on \
	-cpu cortex-a53 -smp "$SMP" -m 3G \
	-nographic -monitor none -serial stdio \
	-global virtio-mmio.force-legacy=false \
	-device virtio-net-device,netdev=n0,bus=virtio-mmio-bus.0 \
	-netdev "user,id=n0,$FWD" \
	-drive "if=none,format=raw,file=$DISK,id=disk0" \
	-device virtio-blk-device,drive=disk0,bus=virtio-mmio-bus.1 \
	-kernel "$KERNEL" "$@"
if [ -n "$ARTPID" ]; then
	qemu-system-aarch64 "$@"
else
	exec qemu-system-aarch64 "$@"
fi
