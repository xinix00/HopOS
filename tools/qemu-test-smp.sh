#!/bin/sh
# SMP-apps op QEMU: een jobspec met twee cores, via Hop, in één kooi.
#
# QEMU met vier cores: de kern en Hop op de OS-core (core 0), drie
# app-cores. Van buiten gaat één jobspec naar de leader van Hop met
# `"cpu_shares": 2048` (Hop maakt daar `cores: 2` van, runner/hopos.rs) en
# `ROLE=SMP` in de env. Hop haalt appspike van de artifact-server, stroomt
# hem de kern in, en de kern plaatst hem op een span van twee aaneengesloten
# app-cores. De runtime van de app vraagt zijn tweede core via de
# control-page (CTRL_SMP_REQ), de servicer ziet het, en de kern dispatcht
# hem in de kooi van de app (prepare_smp, de SMP-trampoline, PSCI de eerste
# keer). Groen alleen als:
#
#   kern        de boot-markers van tools/qemu-test-hop.sh (tot HOP_UP);
#   plaatsing   HOP_JOB_PLACED slot=2, HOPOS_SLOT_START slot=2 core=1, de
#               contexten geketend over twee cores (HOPOS_CAGE_SMP), en de
#               tweede core in de kooi (HOPOS_SMP_DISPATCH_OK core 2,
#               HOPOS_SMP_CORE);
#   de app      "applib: core 1 of 2 up" (HOPOS_APP_SMP_UP), de SMP-toets
#               (HOPOS_APPSPIKE_SMP ok cores=2: een taak op core 1 telde
#               terwijl core 0 zijn outbox schreef) en DONE pass=10 fail=0;
#   de stop     appspike stopt met code 0 en Hop herstart hem: de stop moet
#               BEIDE cores stil zien (E9) en de herstart haalt dezelfde
#               span weer op, dus een tweede HOPOS_APPSPIKE_SMP ok.
#
# Rood is meteen: een paniek, een fault, een quarantaine, een geweigerd of
# mislukt SMP-verzoek (HOPOS_SMP_REJECT, HOPOS_SMP_DISPATCH_FAIL,
# HOPOS_APP_SMP_FAIL). Rood bewaart de console (en drukt hem af).
#
#   tools/qemu-test-smp.sh                 TIMEOUT=90 standaard, in seconden
#   KEEP_LOG=pad tools/qemu-test-smp.sh    bewaart ook een groene console
#   SYSPORT/AGENTPORT/LEADERPORT/ARTPORT   de host-poorten; bezet = een vrije
#   HOP_DIR=pad                            de hop-repo (standaard ../hop/hop)
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TIMEOUT="${TIMEOUT:-90}"
HOP_DIR="${HOP_DIR:-$DIR/../hop/hop}"
TARGET=aarch64-unknown-none-softfloat
LOG="$(mktemp -t hopos-qemu-smp.XXXXXX)"
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
PLACE_MARKS="slot 1: .*HOP_JOB_PLACED slot=2|HOPOS_SLOT_START slot=2 core=1 |slot 2: 2 cores from core 1, contexts chained HOPOS_CAGE_SMP|slot 2: SMP core 2 dispatched HOPOS_SMP_DISPATCH_OK|HOPOS_SMP_CORE|slot 2: applib: core 1 of 2 up .*HOPOS_APP_SMP_UP|slot 2: HOPOS_APPSPIKE_SMP ok cores=2|slot 2: HOPOS_APPSPIKE_DONE pass=10 fail=0"
AGAIN="HOPOS_APPSPIKE_SMP ok cores=2"
RED="HOPOS_PANIC|HOPOS_EXCEPTION|HOPOS_HOP_FAULT|HOPOS_HOP_EXIT|HOPOS_HOP_FAIL|HOPOS_APP_PANIC|HOPOS_PART_QUARANTINE|HOPOS_SMP_REJECT|HOPOS_SMP_DISPATCH_FAIL|HOPOS_APP_SMP_FAIL|HOPOS_APPSPIKE_[A-Z_]* FAIL|HOPOS_CAGE_FAIL"

JOB='{"name":"smp","driver":"hop","artifacts":[{"url":"http://10.0.2.2:'"$ARTPORT"'/appspike.elf"}],"memory_limit":33554432,"cpu_shares":2048,"env":{"ROLE":"SMP"}}'
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
	if all "$PLACE_MARKS" && [ "$(count "$AGAIN")" -ge 2 ]; then
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
if [ "$n" -ge 2 ]; then
	echo "   ok  de stop over beide cores en de herstart: $n keer $AGAIN"
else
	echo "   ROOD na de stop kwam de SMP-toets niet terug ($n keer $AGAIN)"
	fail=1
fi
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
echo "   marker: $(tr -d '\r' <"$LOG" | grep -m1 -o 'HOPOS_APPSPIKE_SMP ok.*')"
if [ "$fail" != 0 ]; then
	KEEP="$(mktemp -t hopos-qemu-smp-rood.XXXXXX)"
	tr -d '\r' <"$LOG" >"$KEEP"
	echo "== console bewaard in $KEEP"
	echo "== console (staart):"
	tail -150 "$KEEP"
	exit 1
fi
[ -n "${KEEP_LOG:-}" ] && tr -d '\r' <"$LOG" >"$KEEP_LOG"
echo "qemu-smp groen"
