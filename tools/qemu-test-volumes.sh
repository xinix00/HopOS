#!/bin/sh
# De volumes van een jobspec op QEMU: van Hop's START_SLOT tot in hopfs, en
# over een herstart van de taak heen.
#
# De kern boot met Hop in slot 1 (zoals tools/qemu-test-hop.sh). Van buiten
# gaat er één jobspec naar de leader met een volume en de VOLUME-toets van
# appspike:
#
#   "volumes": {"/volumes/demo": "/data"}      gedeeld pad naar taakpad
#   "env":     {"VOLUME": "/data/spike.txt"}
#
# Groen alleen als:
#
#   de start     de kern zet het volume in de tabel van de hopfs-actor:
#                "slot 2: 1 volume(s) mounted: /data -> /volumes/demo
#                HOPOS_SLOT_MOUNTS";
#   leven 1      appspike vindt niets, schrijft (HOPOS_APPSPIKE_VOLUME ok
#                wrote), de kern ziet de schrijf in het volume landen
#                ("hopfs: slot 2 saved /data/spike.txt as
#                /volumes/demo/spike.txt" HOPOS_FS_SAVED), en de hele doorloop
#                is groen (HOPOS_APPSPIKE_DONE pass=10 fail=0);
#   leven 2      appspike stopt met code 0, Hop plaatst de service opnieuw
#                (een nieuwe levensduur, een verse lege root), en die vindt
#                het bestand terug met dezelfde bytes (HOPOS_APPSPIKE_VOLUME
#                ok found ... root=fresh): het volume overleefde, de root niet.
#
# Een HOPOS_APPSPIKE_VOLUME FAIL, HOPOS_FS_MOUNTS (volumes geweigerd bij de
# registratie), "persistent volumes require" (een Hop op de ABI van
# alpha.10), en de rode markers van qemu-test-hop.sh zijn meteen rood. Rood
# bewaart de console (en drukt hem af).
#
# Hop stuurt de volumes mee sinds hop op de ABI van HopOS alpha.11 staat
# (hopos-runner, start_mounts.rs).
#
#   tools/qemu-test-volumes.sh             TIMEOUT=90 standaard, in seconden
#   KEEP_LOG=pad tools/qemu-test-volumes.sh
#   SYSPORT/AGENTPORT/LEADERPORT/ARTPORT   de host-poorten (bezet = een vrije)
#   HOP_DIR=pad                            de hop-repo (standaard ../hop/hop)
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TIMEOUT="${TIMEOUT:-90}"
HOP_DIR="${HOP_DIR:-$DIR/../hop/hop}"
TARGET=aarch64-unknown-none-softfloat
LOG="$(mktemp -t hopos-qemu-vol.XXXXXX)"
ART="$(mktemp -d -t hopos-art.XXXXXX)"
DISK="$ART/disk.img"
QPID=""
HPID=""
cleanup() {
	[ -n "$QPID" ] && kill "$QPID" 2>/dev/null
	[ -n "$HPID" ] && kill "$HPID" 2>/dev/null
	rm -rf "$LOG" "$ART"
	true
}
trap cleanup EXIT INT TERM

. "$(dirname "$0")/lib.sh"
SYSPORT="$(port "${SYSPORT:-10100}" SYSPORT)"
AGENTPORT="$(port "${AGENTPORT:-8080}" AGENTPORT)"
LEADERPORT="$(port "${LEADERPORT:-9080}" LEADERPORT)"
ARTPORT="$(port "${ARTPORT:-8000}" ARTPORT)"

cd "$DIR"
echo "== bouwen: hopos (qemuvirt), appspike, en agentd-hopos in $HOP_DIR"
cargo build --quiet --release --target "$TARGET" -p hopos --features board-qemuvirt
cargo build --quiet --release --target "$TARGET" -p appspike
HOP_ELF="$(HOP_DIR="$HOP_DIR" sh "$DIR/tools/hop-build.sh" "$TARGET")"

OBJCOPY="$(ls "$(rustc --print sysroot)"/lib/rustlib/*/bin/rust-objcopy 2>/dev/null | head -1)"
if [ -n "$OBJCOPY" ]; then
	"$OBJCOPY" --strip-debug "$DIR/target/$TARGET/release/appspike" "$ART/appspike.elf"
else
	cp "$DIR/target/$TARGET/release/appspike" "$ART/appspike.elf"
fi
(cd "$ART" && exec python3 -m http.server "$ARTPORT" --bind 127.0.0.1) >"$ART/http.log" 2>&1 &
HPID=$!

echo "== booten op QEMU virt met Hop (tot ${TIMEOUT}s; system :$SYSPORT, agent :$AGENTPORT, leader :$LEADERPORT, artifacts :$ARTPORT)"
SYSPORT="$SYSPORT" AGENTPORT="$AGENTPORT" LEADERPORT="$LEADERPORT" HOP_DIR="$HOP_DIR" APP="$HOP_ELF" ROLE=1 DISK="$DISK" \
	sh "$DIR/image/qemu-run.sh" </dev/null >"$LOG" 2>&1 &
QPID=$!

BOOT_MARKS="HOPOS_BOOT|HOPOS_FS_UP fresh=1|HOPOS_SYSTEM_UP|slot 1: .*HOP_LEADER|slot 1: .*HOP_UP"
LIFE1_MARKS="slot 1: .*HOP_JOB_PLACED slot=2|slot 2: 1 volume\\(s\\) mounted: /data -> /volumes/demo HOPOS_SLOT_MOUNTS|slot 2: HOPOS_APPSPIKE_VOLUME ok wrote path=/data/spike.txt|hopfs: slot 2 saved /data/spike.txt as /volumes/demo/spike.txt .*HOPOS_FS_SAVED|slot 2: HOPOS_APPSPIKE_DONE pass=10 fail=0"
LIFE2_MARKS="slot [0-9]+: HOPOS_APPSPIKE_VOLUME ok found path=/data/spike.txt bytes=[0-9]+ root=fresh"
RED="HOPOS_PANIC|HOPOS_EXCEPTION|HOPOS_HOP_FAULT|HOPOS_HOP_EXIT|HOPOS_HOP_FAIL|HOP_STATE_SKIPPED|HOPOS_CAGE_FAIL|HOPOS_APPSPIKE_VOLUME FAIL|HOPOS_FS_MOUNTS|persistent volumes require"

JOB='{"name":"spike","driver":"hop","artifacts":[{"url":"http://10.0.2.2:'"$ARTPORT"'/appspike.elf"}],"memory_limit":33554432,"env":{"VOLUME":"/data/spike.txt"},"volumes":{"/volumes/demo":"/data"}}'
POSTED=""
START=$(date +%s)
elapsed=0
step() {
	sleep 0.2
	elapsed=$(($(date +%s) - START))
}
while :; do
	has "$RED" && break
	kill -0 "$QPID" 2>/dev/null || break
	[ "$elapsed" -ge "$TIMEOUT" ] && break
	if [ -z "$POSTED" ]; then
		if all "$BOOT_MARKS"; then
			if out="$(curl -s -m 20 -w ' HTTP %{http_code}' -X POST \
				-H 'Content-Type: application/json' -d "$JOB" \
				"http://127.0.0.1:$LEADERPORT/v1/jobs" 2>&1)"; then
				POSTED="$out"
			else
				POSTED="ROOD curl: $out"
				break
			fi
		fi
		step
		continue
	fi
	all "$LIFE1_MARKS" && all "$LIFE2_MARKS" && break
	step
done
kill "$QPID" 2>/dev/null || true
wait "$QPID" 2>/dev/null || true
QPID=""

fail=0
IFS_WAS="$IFS"
IFS='|'
for m in $BOOT_MARKS $LIFE1_MARKS $LIFE2_MARKS; do
	if has "$m"; then
		echo "   ok  $m: $(tr -d '\r' <"$LOG" | grep -m1 -E "$m")"
	else
		echo "   ROOD $m ontbreekt"
		fail=1
	fi
done
IFS="$IFS_WAS"
case "$POSTED" in
*"HTTP 2"*) echo "   ok  POST /v1/jobs: $POSTED" ;;
"") echo "   ROOD POST /v1/jobs nooit gedaan (Hop niet op tijd op)"; fail=1 ;;
*) echo "   ROOD POST /v1/jobs: $POSTED"; fail=1 ;;
esac
if has "$RED"; then
	echo "   ROOD $(tr -d '\r' <"$LOG" | grep -m1 -E "$RED")"
	fail=1
fi
echo "   tijd: $(($(date +%s) - START)) s na de start van QEMU"
if [ "$fail" != 0 ]; then
	KEEP="$(mktemp -t hopos-qemu-vol-rood.XXXXXX)"
	tr -d '\r' <"$LOG" >"$KEEP"
	echo "== console bewaard in $KEEP"
	echo "== console:"
	cat "$KEEP"
	exit 1
fi
[ -n "${KEEP_LOG:-}" ] && tr -d '\r' <"$LOG" >"$KEEP_LOG"
echo "qemu-volumes groen"
