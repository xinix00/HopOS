#!/bin/sh
# De kern-flip op QEMU: een draaiende node vervangt zijn kern zonder
# herstart, en Hop (slot 1) draait door.
#
# Kern A (stempel A) boot met Hop, zoals tools/qemu-test-hop.sh. Kern B is
# dezelfde bron met stempel B, gebouwd als flip-bundel
# (image/flip-bundle.sh: twee links, de relocatietabel uit het verschil).
# Van buiten gaat POST /flip naar Hop; Hop haalt de bundel van de
# artifact-server op de host (10.0.2.2), stroomt hem rauw de kern in en
# vraagt PrivOp::FLIP. Groen alleen als:
#
#   kern A      HOPOS_BOOT gen=1 stamp=A, Hop op (HOP_UP, de poorten door);
#   de flip     POST /flip geeft 202; HOPOS_FLIP_STAGED (som getoetst,
#               beeld gerelokeerd) en HOPOS_FLIP_JUMP gen=2;
#   kern B      HOPOS_BOOT gen=2 stamp=B, HOPOS_FLIP_BOOT gen=2,
#               "HOPOS_FLIP_ADOPT 1 of 1 resident(s)", HOPOS_HOP_RESUMED en
#               HOPOS_FLIP_SETTLED (de guard: adoptie en net binnen de gratie);
#   dezelfde Hop  precies één "HOP_BOOT" en één HOPOS_HOP_START in de hele
#               console (Hop is niet herstart, zijn kooi is geadopteerd), en
#               GET /tasks antwoordt na de flip;
#   werk        daarna een jobspec naar de leader: HOP_JOB_PLACED slot=2,
#               HOPOS_SLOT_START slot=2 en "slot 2: HOPOS_APPSPIKE_DONE
#               pass=9 fail=0", allemaal ná de landing: de nieuwe kern plaatst.
#
# Rood is ook: HOPOS_PANIC, HOPOS_EXCEPTION, een fout van Hop, en elke
# flip-weigering (HOPOS_FLIP_REFUSED, _FAIL, _BLOB_BAD, _GUARD,
# _ADOPT_FAIL, HOP_FLIP_FAIL). Rood bewaart de console en drukt hem af.
#
#   tools/qemu-test-flip.sh                 TIMEOUT=90 standaard, in seconden
#   KEEP_LOG=pad tools/qemu-test-flip.sh    bewaart ook een groene console
#   SYSPORT/AGENTPORT/LEADERPORT/ARTPORT    de host-poorten (zoals qemu-test-hop)
#   HOP_DIR=pad                             de hop-repo (standaard ../hop/hop)
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TIMEOUT="${TIMEOUT:-90}"
HOP_DIR="${HOP_DIR:-$DIR/../hop/hop}"
TARGET=aarch64-unknown-none-softfloat
LOG="$(mktemp -t hopos-qemu-flip.XXXXXX)"
ART="$(mktemp -d -t hopos-flip-art.XXXXXX)"
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

port() {
	python3 - "$1" "$2" <<'PY'
import socket, sys
want, name = int(sys.argv[1]), sys.argv[2]
s = socket.socket()
try:
    s.bind(("127.0.0.1", want))
    print(want)
except OSError:
    s.close()
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    got = s.getsockname()[1]
    print(f"   {name} {want} is taken, using {got}", file=sys.stderr)
    print(got)
s.close()
PY
}
SYSPORT="$(port "${SYSPORT:-10100}" SYSPORT)"
AGENTPORT="$(port "${AGENTPORT:-8080}" AGENTPORT)"
LEADERPORT="$(port "${LEADERPORT:-9080}" LEADERPORT)"
ARTPORT="$(port "${ARTPORT:-8000}" ARTPORT)"

cd "$DIR"
echo "== bouwen: kern A (stempel A), bundel B (stempel B), appspike, agentd-hopos"
HOPOS_STAMP=A cargo build --quiet --release --target "$TARGET" -p hopos --features board-qemuvirt
HOPOS_STAMP=B sh "$DIR/image/flip-bundle.sh" virt
cargo build --quiet --release --target "$TARGET" -p appspike
(cd "$HOP_DIR" && cargo build --quiet --release --target "$TARGET" -p agentd-hopos)
cp "$DIR/target/hopos-virt.flip" "$ART/hopos-virt.flip"
SHA="$(cat "$DIR/target/hopos-virt.flip.sha256")"
OBJCOPY="$(ls "$(rustc --print sysroot)"/lib/rustlib/*/bin/rust-objcopy 2>/dev/null | head -1)"
if [ -n "$OBJCOPY" ]; then
	"$OBJCOPY" --strip-debug "$DIR/target/$TARGET/release/appspike" "$ART/appspike.elf"
else
	cp "$DIR/target/$TARGET/release/appspike" "$ART/appspike.elf"
fi
(cd "$ART" && exec python3 -m http.server "$ARTPORT" --bind 127.0.0.1) >"$ART/http.log" 2>&1 &
HPID=$!

echo "== booten op QEMU virt met Hop, kern A (tot ${TIMEOUT}s; agent :$AGENTPORT, leader :$LEADERPORT, artifacts :$ARTPORT)"
HOPOS_STAMP=A SYSPORT="$SYSPORT" AGENTPORT="$AGENTPORT" LEADERPORT="$LEADERPORT" HOP_DIR="$HOP_DIR" APP=hop DISK="$DISK" \
	sh "$DIR/image/qemu-run.sh" </dev/null >"$LOG" 2>&1 &
QPID=$!

has() { tr -d '\r' <"$LOG" | grep -q -E "$1"; }
# Alleen wat ná de landing van kern B op de console kwam.
after() { tr -d '\r' <"$LOG" | awk '/HOPOS_FLIP_BOOT/ { f = 1 } f' | grep -q -E "$1"; }
count() { tr -d '\r' <"$LOG" | grep -c -E "$1" || true; }
all() {
	(
		IFS='|'
		for m in $1; do has "$m" || exit 1; done
	)
}
all_after() {
	(
		IFS='|'
		for m in $1; do after "$m" || exit 1; done
	)
}

A_MARKS="HOPOS_BOOT gen=1 stamp=A|HOPOS_HOP_START slot=1|uplink tcp :8080 -> slot 1 :8080 HOPOS_HOP_PUBLISH|slot 1: .*HOP_UP"
FLIP_MARKS="HOPOS_FLIP_STAGED|HOPOS_FLIP_JUMP gen=2|HOPOS_BOOT gen=2 stamp=B|HOPOS_FLIP_BOOT gen=2|HOPOS_FLIP_ADOPT 1 of 1 resident\\(s\\)|HOPOS_HOP_RESUMED|HOPOS_FLIP_SETTLED"
WORK_MARKS="slot 1: .*HOP_JOB_PLACED slot=2|HOPOS_SLOT_START slot=2|slot 2: HOPOS_APPSPIKE_DONE pass=9 fail=0"
RED="HOPOS_PANIC|HOPOS_EXCEPTION|HOPOS_HOP_FAULT|HOPOS_HOP_EXIT|HOPOS_HOP_FAIL|HOPOS_FLIP_REFUSED|HOPOS_FLIP_FAIL|HOPOS_FLIP_BLOB_BAD|HOPOS_FLIP_GUARD|HOPOS_FLIP_ADOPT_FAIL|HOPOS_CAGE_FAIL|HOP_FLIP_FAIL"

JOB='{"name":"spike","driver":"hop","artifacts":[{"url":"http://10.0.2.2:'"$ARTPORT"'/appspike.elf"}],"memory_limit":33554432}'
FLIPREQ='{"url":"http://10.0.2.2:'"$ARTPORT"'/hopos-virt.flip","sha256":"'"$SHA"'"}'
FLIPPED=""
TASKS=""
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
	if [ -z "$FLIPPED" ]; then
		if all "$A_MARKS"; then
			if out="$(curl -s -m 30 -w ' HTTP %{http_code}' -X POST \
				-H 'Content-Type: application/json' -d "$FLIPREQ" \
				"http://127.0.0.1:$AGENTPORT/flip" 2>&1)"; then
				FLIPPED="$out"
			else
				FLIPPED="ROOD curl: $out"
				break
			fi
		fi
		step
		continue
	fi
	if [ -z "$TASKS" ]; then
		if all "$FLIP_MARKS"; then
			# Dezelfde Hop antwoordt, over de uplink van de nieuwe kern.
			if t="$(curl -s -m 5 -w ' HTTP %{http_code}' "http://127.0.0.1:$AGENTPORT/tasks" 2>&1)"; then
				case "$t" in *"HTTP 200"*) TASKS="$t" ;; esac
			fi
		fi
		step
		continue
	fi
	if [ -z "$POSTED" ]; then
		if out="$(curl -s -m 20 -w ' HTTP %{http_code}' -X POST \
			-H 'Content-Type: application/json' -d "$JOB" \
			"http://127.0.0.1:$LEADERPORT/v1/jobs" 2>&1)"; then
			POSTED="$out"
		else
			POSTED="ROOD curl: $out"
			break
		fi
		step
		continue
	fi
	all_after "$WORK_MARKS" && break
	step
done
kill "$QPID" 2>/dev/null || true
wait "$QPID" 2>/dev/null || true
QPID=""

fail=0
IFS_WAS="$IFS"
IFS='|'
for m in $A_MARKS $FLIP_MARKS; do
	if has "$m"; then
		echo "   ok  $m: $(tr -d '\r' <"$LOG" | grep -m1 -E "$m")"
	else
		echo "   ROOD $m ontbreekt"
		fail=1
	fi
done
for m in $WORK_MARKS; do
	if after "$m"; then
		echo "   ok  na de flip: $(tr -d '\r' <"$LOG" | awk '/HOPOS_FLIP_BOOT/ { f = 1 } f' | grep -m1 -E "$m")"
	else
		echo "   ROOD $m ontbreekt na de flip"
		fail=1
	fi
done
IFS="$IFS_WAS"
case "$FLIPPED" in
*"HTTP 202"*) echo "   ok  POST /flip: $FLIPPED" ;;
"") echo "   ROOD POST /flip nooit gedaan (kern A of Hop niet op tijd op)"; fail=1 ;;
*) echo "   ROOD POST /flip: $FLIPPED"; fail=1 ;;
esac
case "$TASKS" in
*"HTTP 200"*) echo "   ok  GET /tasks na de flip: $TASKS" ;;
*) echo "   ROOD GET /tasks na de flip: ${TASKS:-nooit beantwoord}"; fail=1 ;;
esac
case "$POSTED" in
*"HTTP 2"*) echo "   ok  POST /v1/jobs op kern B: $POSTED" ;;
*) echo "   ROOD POST /v1/jobs op kern B: ${POSTED:-nooit gedaan}"; fail=1 ;;
esac
boots="$(count 'slot 1: .*HOP_BOOT')"
starts="$(count 'HOPOS_HOP_START slot=1')"
if [ "$boots" = 1 ] && [ "$starts" = 1 ]; then
	echo "   ok  dezelfde Hop: 1x HOP_BOOT en 1x HOPOS_HOP_START over twee kernen"
else
	echo "   ROOD Hop herstart? HOP_BOOT ${boots}x, HOPOS_HOP_START ${starts}x"
	fail=1
fi
if grep -q "GET /hopos-virt.flip" "$ART/http.log" 2>/dev/null; then
	echo "   ok  artifact-server: de bundel is opgehaald"
else
	echo "   ROOD artifact-server: de bundel is nooit gevraagd"
	fail=1
fi
if has "$RED"; then
	echo "   ROOD $(tr -d '\r' <"$LOG" | grep -m1 -E "$RED")"
	fail=1
fi
echo "   tijd: $(($(date +%s) - START)) s na de start van QEMU"
if [ "$fail" != 0 ]; then
	KEEP="$(mktemp -t hopos-qemu-flip-rood.XXXXXX)"
	tr -d '\r' <"$LOG" >"$KEEP"
	echo "== console bewaard in $KEEP"
	echo "== console:"
	cat "$KEEP"
	exit 1
fi
[ -n "${KEEP_LOG:-}" ] && tr -d '\r' <"$LOG" >"$KEEP_LOG"
echo "qemu-flip groen"
