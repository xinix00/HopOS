#!/bin/sh
# Sharegroups op een app-core: twee kooien op één core, in coöperatieve
# rotatie, via Hop.
#
# QEMU met vier cores: de kern en Hop op de OS-core, drie app-cores. Van
# buiten gaan twee jobspecs naar de leader van Hop, allebei met
# `"tags":{"sharegroup":"demo"}` en `ROLE=SHARE` in de env (Hop maakt er
# `share: "demo"`, `pool_cores: 1` van, runner/hopos.rs). De kern reserveert
# voor de groep één app-core; het eerste lid krijgt hem met een gewoon
# startschot, het tweede komt erbij in de rotatie van die draaiende core
# (boot-pending, `el2::join`), met een eigen kooi en partitie. Beide leden
# yielden in hun idle, en de switcher geeft de core om de beurt. Groen alleen
# als:
#
#   kern        de boot-markers van tools/qemu-test-hop.sh (tot HOP_UP);
#   plaatsing   HOP_JOB_PLACED slot=2 en slot=3, beide HOPOS_SLOT_START op
#               dezelfde app-core (core=1), en het tweede lid in de rotatie
#               naast het eerste (HOPOS_SHARE_JOIN);
#   de apps     elk lid HOPOS_APPSPIKE_SHARE ok (CTRL_SHARED gezien, en zijn
#               beurten geteld: yields=...) en DONE pass=10 fail=0;
#   de stop     share-a stopt na zijn toetsen (exit 0; Hop herstart hem),
#               share-b blijft leven (HOLD=1): de stop van het ene lid laat
#               het andere en de groepsreservering staan, en share-a komt
#               terug in de rotatie naast share-b (een tweede
#               HOPOS_SHARE_JOIN, een derde SHARE ok, share-b één start).
#
# Rood is meteen: een paniek, een fault, een quarantaine, een lid dat niet
# opgepikt werd (HOPOS_SHARE_PENDING), of een toets die faalt.
#
#   tools/qemu-test-share.sh               TIMEOUT=120 standaard, in seconden
#   KEEP_LOG=pad tools/qemu-test-share.sh  bewaart ook een groene console
#   SYSPORT/AGENTPORT/LEADERPORT/ARTPORT   de host-poorten; bezet = een vrije
#   HOP_DIR=pad                            de hop-repo (standaard ../hop/hop)
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TIMEOUT="${TIMEOUT:-120}"
HOP_DIR="${HOP_DIR:-$DIR/../hop/hop}"
TARGET=aarch64-unknown-none-softfloat
LOG="$(mktemp -t hopos-qemu-share.XXXXXX)"
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
echo "== bouwen: hopos (qemuvirt), appspike, en agentd-hopos in $HOP_DIR"
cargo build --quiet --release --target "$TARGET" -p hopos --features board-qemuvirt
cargo build --quiet --release --target "$TARGET" -p appspike
(cd "$HOP_DIR" && cargo build --quiet --release --target "$TARGET" -p agentd-hopos)

OBJCOPY="$(ls "$(rustc --print sysroot)"/lib/rustlib/*/bin/rust-objcopy 2>/dev/null | head -1)"
if [ -n "$OBJCOPY" ]; then
	"$OBJCOPY" --strip-debug "$DIR/target/$TARGET/release/appspike" "$ART/appspike.elf"
else
	cp "$DIR/target/$TARGET/release/appspike" "$ART/appspike.elf"
fi
(cd "$ART" && exec python3 -m http.server "$ARTPORT" --bind 127.0.0.1) >"$ART/http.log" 2>&1 &
HPID=$!

echo "== booten op QEMU virt met Hop, 4 cores (tot ${TIMEOUT}s; leader :$LEADERPORT, artifacts :$ARTPORT)"
SMP=4 SYSPORT="$SYSPORT" AGENTPORT="$AGENTPORT" LEADERPORT="$LEADERPORT" HOP_DIR="$HOP_DIR" APP=hop DISK="$DISK" \
	sh "$DIR/image/qemu-run.sh" </dev/null >"$LOG" 2>&1 &
QPID=$!

has() { tr -d '\r' <"$LOG" | grep -q -E "$1"; }
count() { tr -d '\r' <"$LOG" | grep -c -E "$1" || true; }
all() {
	(
		IFS='|'
		for m in $1; do has "$m" || exit 1; done
	)
}

BOOT_MARKS="HOPOS_BOOT|HOPOS_NET_UP|HOPOS_SYSTEM_UP|HOPOS_HOP_START slot=1 core=0|slot 1: .*HOP_LEADER|slot 1: .*HOP_UP"
PLACE_MARKS="slot 1: .*HOP_JOB_PLACED slot=2|slot 1: .*HOP_JOB_PLACED slot=3|HOPOS_SLOT_START slot=2 core=1 |HOPOS_SLOT_START slot=3 core=1 |joined shared core 1 next to 1 resident.*HOPOS_SHARE_JOIN|slot 2: HOPOS_APPSPIKE_SHARE ok .*shared=1|slot 3: HOPOS_APPSPIKE_SHARE ok .*shared=1|slot 2: HOPOS_APPSPIKE_DONE pass=10 fail=0|slot 3: HOPOS_APPSPIKE_DONE pass=10 fail=0"
AGAIN="HOPOS_APPSPIKE_SHARE ok"
JOIN="joined shared core 1 next to 1 resident.*HOPOS_SHARE_JOIN"
RED="HOPOS_PANIC|HOPOS_EXCEPTION|HOPOS_HOP_FAULT|HOPOS_HOP_EXIT|HOPOS_HOP_FAIL|HOPOS_APP_PANIC|HOPOS_PART_QUARANTINE|HOPOS_SHARE_PENDING|HOPOS_CAGE_ROSTER|HOPOS_APPSPIKE_[A-Z_]* FAIL|HOPOS_CAGE_FAIL"

# share-b blijft na zijn toetsen leven (HOLD=1): de stop van share-a (exit 0,
# Hop herstart hem) gebeurt dan naast een levende buur.
job() {
	hold=""
	[ "$1" = share-b ] && hold=',"HOLD":"1"'
	printf '{"name":"%s","driver":"hop","artifacts":[{"url":"http://10.0.2.2:%s/appspike.elf"}],"memory_limit":33554432,"cpu_shares":1024,"tags":{"sharegroup":"demo"},"env":{"ROLE":"SHARE"%s}}' "$1" "$ARTPORT" "$hold"
}
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
			for name in share-a share-b; do
				if out="$(curl -s -m 20 -w ' HTTP %{http_code}' -X POST \
					-H 'Content-Type: application/json' -d "$(job "$name")" \
					"http://127.0.0.1:$LEADERPORT/v1/jobs" 2>&1)"; then
					POSTED="$POSTED $name: $out"
				else
					POSTED="ROOD curl $name: $out"
					break
				fi
			done
			case "$POSTED" in ROOD*) break ;; esac
		fi
		step
		continue
	fi
	if all "$PLACE_MARKS" && [ "$(count "$AGAIN")" -ge 3 ] && [ "$(count "$JOIN")" -ge 2 ]; then
		break
	fi
	step
done
kill "$QPID" 2>/dev/null || true
wait "$QPID" 2>/dev/null || true
QPID=""

fail=0
IFS_WAS="$IFS"
IFS='|'
for m in $BOOT_MARKS $PLACE_MARKS; do
	if has "$m"; then
		echo "   ok  $m: $(tr -d '\r' <"$LOG" | grep -m1 -E "$m")"
	else
		echo "   ROOD $m ontbreekt"
		fail=1
	fi
done
IFS="$IFS_WAS"
n="$(count "$AGAIN")"
j="$(count "$JOIN")"
b="$(count "HOPOS_SLOT_START slot=3 ")"
if [ "$n" -ge 3 ] && [ "$j" -ge 2 ] && [ "$b" = 1 ]; then
	echo "   ok  share-a stopte en kwam terug naast de levende share-b: $n keer SHARE ok, $j keer HOPOS_SHARE_JOIN, share-b $b start"
else
	echo "   ROOD de stop van één lid: $n keer SHARE ok (>= 3), $j keer HOPOS_SHARE_JOIN (>= 2), share-b $b start(s) (moet 1)"
	fail=1
fi
case "$POSTED" in
*"share-a: "*"HTTP 2"*"share-b: "*"HTTP 2"*) echo "   ok  POST /v1/jobs:$POSTED" ;;
"") echo "   ROOD POST /v1/jobs nooit gedaan (Hop niet op tijd op)"; fail=1 ;;
*) echo "   ROOD POST /v1/jobs: $POSTED"; fail=1 ;;
esac
if has "$RED"; then
	echo "   ROOD $(tr -d '\r' <"$LOG" | grep -m1 -E "$RED")"
	fail=1
fi
echo "   tijd: $(($(date +%s) - START)) s na de start van QEMU"
echo "   marker: $(tr -d '\r' <"$LOG" | grep -m1 -E 'slot 2: HOPOS_APPSPIKE_SHARE ok')"
echo "   marker: $(tr -d '\r' <"$LOG" | grep -m1 -E 'slot 3: HOPOS_APPSPIKE_SHARE ok')"
echo "   kern:   $(tr -d '\r' <"$LOG" | grep -m1 -E 'HOPOS_SHARE_JOIN')"
if [ "$fail" != 0 ]; then
	KEEP="$(mktemp -t hopos-qemu-share-rood.XXXXXX)"
	tr -d '\r' <"$LOG" >"$KEEP"
	echo "== console bewaard in $KEEP"
	echo "== console (staart):"
	tail -150 "$KEEP"
	exit 1
fi
[ -n "${KEEP_LOG:-}" ] && tr -d '\r' <"$LOG" >"$KEEP_LOG"
echo "qemu-share groen"
