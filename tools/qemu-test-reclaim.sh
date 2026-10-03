#!/bin/sh
# Core-reclaim op een gedeelde app-core: een bewoner die nooit yieldt,
# houdt zijn core niet voor altijd vast.
#
# Op arm64 wisselt een app-core alleen bij een vrijwillige yield (de
# switcher in cpu/src/el2, geen tijdschijf). Een nieuw lid van een
# sharegroup staat boot-pending in de rotatie tot een buur yieldt; een buur
# die rekent, gijzelt de core (Go, 14/15-08: uren, en elke plaatsing dood).
# De lifecycle wacht daarom 2 s (kern::slots::RECLAIM_WAIT), offert dan de
# vasthouder (SCHED_CURRENT, zijn kooi ingetrokken) en wacht nog één ronde.
# QEMU met vier cores, Hop op de OS-core. Groen alleen als:
#
#   kern        de boot-markers van tools/qemu-test-hop.sh (tot HOP_UP);
#   de gijzelaar  burn (bench met BURN=1, werk zonder rust) in sharegroup
#               demo op core 1, en hij rekent (HOPOS_BENCH_UP role=burn);
#   de nieuwe   web (welcome) in dezelfde groep: hij wacht eerst
#               (HOPOS_SHARE_PENDING), de kern offert burn
#               (HOPOS_CORE_RECLAIM, slot 2), en web komt op
#               (HOPOS_WELCOME_UP) op core 1, zonder quarantaine.
#
# Zonder reclaim blijft web boot-pending terwijl de start gelukt heet, en
# komt HOPOS_WELCOME_UP nooit.
#
#   tools/qemu-test-reclaim.sh             TIMEOUT=120 standaard, in seconden
#   KEEP_LOG=pad tools/qemu-test-reclaim.sh  bewaart ook een groene console
#   SYSPORT/AGENTPORT/LEADERPORT/ARTPORT   de host-poorten; bezet = een vrije
#   HOP_DIR=pad                            de hop-repo (standaard ../hop/hop)
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TIMEOUT="${TIMEOUT:-120}"
HOP_DIR="${HOP_DIR:-$DIR/../hop/hop}"
TARGET=aarch64-unknown-none-softfloat
LOG="$(mktemp -t hopos-qemu-reclaim.XXXXXX)"
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
echo "== bouwen: hopos (qemuvirt), bench, welcome, en agentd-hopos in $HOP_DIR"
cargo build --quiet --release --target "$TARGET" -p hopos --features board-qemuvirt
cargo build --quiet --release --target "$TARGET" -p bench -p welcome
HOP_ELF="$(HOP_DIR="$HOP_DIR" sh "$DIR/tools/hop-build.sh" "$TARGET")"

OBJCOPY="$(ls "$(rustc --print sysroot)"/lib/rustlib/*/bin/rust-objcopy 2>/dev/null | head -1)"
for app in bench welcome; do
	if [ -n "$OBJCOPY" ]; then
		"$OBJCOPY" --strip-debug "$DIR/target/$TARGET/release/$app" "$ART/$app.elf"
	else
		cp "$DIR/target/$TARGET/release/$app" "$ART/$app.elf"
	fi
done
(cd "$ART" && exec python3 -m http.server "$ARTPORT" --bind 127.0.0.1) >"$ART/http.log" 2>&1 &
HPID=$!

echo "== booten op QEMU virt met Hop, 4 cores (tot ${TIMEOUT}s; leader :$LEADERPORT, artifacts :$ARTPORT)"
SMP=4 SYSPORT="$SYSPORT" AGENTPORT="$AGENTPORT" LEADERPORT="$LEADERPORT" HOP_DIR="$HOP_DIR" APP="$HOP_ELF" ROLE=1 DISK="$DISK" \
	sh "$DIR/image/qemu-run.sh" </dev/null >"$LOG" 2>&1 &
QPID=$!

BOOT_MARKS="HOPOS_BOOT|HOPOS_NET_UP|HOPOS_SYSTEM_UP|HOPOS_HOP_START slot=1 core=0|slot 1: .*HOP_LEADER|slot 1: .*HOP_UP"
HOG_MARKS="HOPOS_SLOT_START slot=2 core=1 |slot 2: .*HOPOS_BENCH_UP role=burn"
RECLAIM_MARKS="slot 3 waits on shared core 1.*HOPOS_SHARE_PENDING|slot 3: core 1 never yielded in 2 s, sacrificing resident slot 2 HOPOS_CORE_RECLAIM|HOPOS_SLOT_START slot=3 core=1 |slot 3: .*HOPOS_WELCOME_UP"
RED="HOPOS_PANIC|HOPOS_EXCEPTION|HOPOS_HOP_FAULT|HOPOS_HOP_EXIT|HOPOS_HOP_FAIL|HOPOS_APP_PANIC|HOPOS_PART_QUARANTINE|HOPOS_CORE_RECLAIM_FAILED|HOPOS_CAGE_ROSTER|HOPOS_CAGE_FAIL"

job() {
	printf '{"name":"%s","driver":"hop","artifacts":[{"url":"http://10.0.2.2:%s/%s.elf"}],"memory_limit":33554432,"cpu_shares":1024,"tags":{"sharegroup":"demo"},"env":{%s}}' "$1" "$ARTPORT" "$2" "$3"
}
post() {
	curl -s -m 20 -w ' HTTP %{http_code}' -X POST -H 'Content-Type: application/json' \
		-d "$1" "http://127.0.0.1:$LEADERPORT/v1/jobs" 2>&1 || true
}
POSTED=""
STAGE=boot
START=$(date +%s)
elapsed=0
while :; do
	has "$RED" && break
	kill -0 "$QPID" 2>/dev/null || break
	[ "$elapsed" -ge "$TIMEOUT" ] && break
	case "$STAGE" in
	boot)
		if all "$BOOT_MARKS"; then
			POSTED="burn: $(post "$(job burn bench '"BURN":"1","BURN_WORK":"600","BURN_REST":"0"')")"
			STAGE=hog
		fi
		;;
	hog)
		# Het tweede lid pas als de gijzelaar rekent.
		if all "$HOG_MARKS"; then
			POSTED="$POSTED web: $(post "$(job web welcome '')")"
			STAGE=reclaim
		fi
		;;
	reclaim) all "$RECLAIM_MARKS" && break ;;
	esac
	sleep 0.2
	elapsed=$(($(date +%s) - START))
done
kill "$QPID" 2>/dev/null || true
wait "$QPID" 2>/dev/null || true
QPID=""

fail=0
IFS_WAS="$IFS"
IFS='|'
for m in $BOOT_MARKS $HOG_MARKS $RECLAIM_MARKS; do
	if has "$m"; then
		echo "   ok  $m: $(tr -d '\r' <"$LOG" | grep -m1 -E "$m")"
	else
		echo "   ROOD $m ontbreekt"
		fail=1
	fi
done
IFS="$IFS_WAS"
case "$POSTED" in
"burn: "*"HTTP 2"*"web: "*"HTTP 2"*) echo "   ok  POST /v1/jobs: $POSTED" ;;
"") echo "   ROOD POST /v1/jobs nooit gedaan (Hop niet op tijd op)"; fail=1 ;;
*) echo "   ROOD POST /v1/jobs: $POSTED"; fail=1 ;;
esac
if has "$RED"; then
	echo "   ROOD $(tr -d '\r' <"$LOG" | grep -m1 -E "$RED")"
	fail=1
fi
echo "   tijd: $(($(date +%s) - START)) s na de start van QEMU"
if [ "$fail" != 0 ]; then
	KEEP="$(mktemp -t hopos-qemu-reclaim-rood.XXXXXX)"
	tr -d '\r' <"$LOG" >"$KEEP"
	echo "== console bewaard in $KEEP"
	echo "== console (staart):"
	tail -150 "$KEEP"
	exit 1
fi
[ -n "${KEEP_LOG:-}" ] && tr -d '\r' <"$LOG" >"$KEEP_LOG"
echo "qemu-reclaim groen"
