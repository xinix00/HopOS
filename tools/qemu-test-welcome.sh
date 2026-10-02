#!/bin/sh
# De kring van een devicedag op QEMU: de kern start Hop, Hop plaatst
# welcome met een gepubliceerde poort, en van buiten staat er een pagina.
#
# De kern boot met agentd-hopos in slot 1 (zoals tools/qemu-test-hop.sh).
# Van buiten gaat er één jobspec naar de leader van Hop: welcome van een
# artifact-server op de host (voor de gast 10.0.2.2), met "ports":{"http":80}.
# Hop zet ER_PORT_HTTP=80 in de env van de app en geeft de poort mee in
# START_SLOT; de kern zet uplink-poort 80 door naar het slot (DNAT, tcp en
# udp) vóór het startschot, en QEMU's hostfwd brengt 127.0.0.1:$WEBPORT naar
# poort 80 van de gast. Groen alleen als:
#
#   kern        HOPOS_BOOT, HOPOS_NET_UP, HOPOS_SYSTEM_UP, HOPOS_HOP_START
#               slot=1 en Hop's twee poorten (HOPOS_HOP_PUBLISH);
#   Hop         HOP_UP en HOP_LEADER via de servicer van slot 1;
#   van buiten  POST http://127.0.0.1:$LEADERPORT/v1/jobs wordt aangenomen;
#   de plaatsing HOP_JOB_PLACED slot=2, "slot 2: 1 port(s) published tcp+udp
#               on the uplink: :80 HOPOS_SLOT_PUBLISH" en
#               "slot 2: ... HOPOS_WELCOME_UP port=80";
#   van buiten  GET http://127.0.0.1:$WEBPORT/ geeft 200 met de bunny
#               ("( -.-)") en "slot 2", en GET /health geeft 200 "ok";
#   de stop     DELETE /v1/jobs/welcome: de kern trekt de poort in
#               ("slot 2: ports withdrawn from the uplink
#               HOPOS_SLOT_UNPUBLISH"), en daarna antwoordt
#               127.0.0.1:$WEBPORT niet meer met de pagina.
#
# Een HOPOS_PANIC, HOPOS_EXCEPTION, HOPOS_APP_PANIC, HOPOS_HOP_FAULT,
# HOPOS_HOP_EXIT, HOPOS_HOP_FAIL of HOPOS_SLOT_PUBLISH_FAIL is meteen rood.
# Rood bewaart de console (en drukt hem af).
#
#   tools/qemu-test-welcome.sh             TIMEOUT=60 standaard, in seconden
#   KEEP_LOG=pad tools/qemu-test-welcome.sh   bewaart ook een groene console
#   KEEP_PAGE=pad tools/qemu-test-welcome.sh  bewaart de pagina zoals curl hem gaf
#   GO_ELF=pad tools/qemu-test-welcome.sh   een tamago-welcome uit OLD/apps
#                                          als artifact (docs/go-apps.md): de
#                                          Go-app zegt "serving http://" en
#                                          kent /healthz in plaats van /health
#   SYSPORT/AGENTPORT/LEADERPORT/ARTPORT/WEBPORT   de host-poorten; bezet =
#                                          een vrije poort van het OS, luid
#   HOP_DIR=pad                            de hop-repo (standaard ../hop/hop)
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TIMEOUT="${TIMEOUT:-60}"
HOP_DIR="${HOP_DIR:-$DIR/../hop/hop}"
TARGET=aarch64-unknown-none-softfloat
LOG="$(mktemp -t hopos-qemu-welcome.XXXXXX)"
ART="$(mktemp -d -t hopos-art.XXXXXX)"
DISK="$ART/disk.img"
PAGE="$ART/page.html"
QPID=""
HPID=""
cleanup() {
	[ -n "$QPID" ] && kill "$QPID" 2>/dev/null
	[ -n "$HPID" ] && kill "$HPID" 2>/dev/null
	rm -rf "$LOG" "$ART"
	true
}
trap cleanup EXIT INT TERM

# Een host-poort: de gevraagde als hij vrij is, anders een vrije van het OS.
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
WEBPORT="$(port "${WEBPORT:-8081}" WEBPORT)"

cd "$DIR"
echo "== bouwen: hopos (qemuvirt), welcome, en agentd-hopos in $HOP_DIR"
cargo build --quiet --release --target "$TARGET" -p hopos --features board-qemuvirt
HOP_ELF="$(HOP_DIR="$HOP_DIR" sh "$DIR/tools/hop-build.sh" "$TARGET")"

# De artifact-server: welcome zonder debug-info, de symbolen blijven voor
# de plaatsing. Met GO_ELF het tamago-image, dat zijn eigen regel en pad heeft.
UP_MARK="HOPOS_WELCOME_UP port=80"
HEALTH_PATH=/health
if [ -n "${GO_ELF:-}" ]; then
	cp "$GO_ELF" "$ART/welcome.elf"
	UP_MARK="serving http://"
	HEALTH_PATH=/healthz
else
	cargo build --quiet --release --target "$TARGET" -p welcome
	OBJCOPY="$(ls "$(rustc --print sysroot)"/lib/rustlib/*/bin/rust-objcopy 2>/dev/null | head -1)"
	if [ -n "$OBJCOPY" ]; then
		"$OBJCOPY" --strip-debug "$DIR/target/$TARGET/release/welcome" "$ART/welcome.elf"
	else
		cp "$DIR/target/$TARGET/release/welcome" "$ART/welcome.elf"
	fi
fi
[ -n "${GO_ELF:-}" ] && cp "$GO_ELF" "$ART/welcome.elf"
(cd "$ART" && exec python3 -m http.server "$ARTPORT" --bind 127.0.0.1) >"$ART/http.log" 2>&1 &
HPID=$!

echo "== booten op QEMU virt met Hop (tot ${TIMEOUT}s; leader :$LEADERPORT, artifacts :$ARTPORT, web :$WEBPORT -> gast :80)"
SYSPORT="$SYSPORT" AGENTPORT="$AGENTPORT" LEADERPORT="$LEADERPORT" WEBPORT="$WEBPORT" \
	HOP_DIR="$HOP_DIR" APP="$HOP_ELF" ROLE=1 DISK="$DISK" \
	sh "$DIR/image/qemu-run.sh" </dev/null >"$LOG" 2>&1 &
QPID=$!

has() { tr -d '\r' <"$LOG" | grep -q -E "$1"; }
all() {
	(
		IFS='|'
		for m in $1; do has "$m" || exit 1; done
	)
}

BOOT_MARKS="HOPOS_BOOT|HOPOS_NET_UP|HOPOS_SYSTEM_UP|HOPOS_HOP_START slot=1 |uplink tcp :8080 -> slot 1 :8080 HOPOS_HOP_PUBLISH|uplink tcp :9080 -> slot 1 :9080 HOPOS_HOP_PUBLISH|slot 1: .*HOP_LEADER|slot 1: .*HOP_UP"
PLACE_MARKS="slot 1: .*HOP_JOB_PLACED slot=2|slot 2: 1 port\\(s\\) published tcp\\+udp on the uplink: :80 HOPOS_SLOT_PUBLISH|slot 2: .*$UP_MARK"
STOP_MARKS="slot 2: ports withdrawn from the uplink HOPOS_SLOT_UNPUBLISH"
RED="HOPOS_PANIC|HOPOS_EXCEPTION|HOPOS_APP_PANIC|HOPOS_HOP_FAULT|HOPOS_HOP_EXIT|HOPOS_HOP_FAIL|HOPOS_SLOT_PUBLISH_FAIL"

JOB='{"name":"welcome","driver":"hop","artifacts":[{"url":"http://10.0.2.2:'"$ARTPORT"'/welcome.elf"}],"memory_limit":33554432,"ports":{"http":80}}'
POSTED=""
PAGE_OK=""
HEALTH=""
DELETED=""
GONE=""
START=$(date +%s)
elapsed=0
step() {
	sleep 0.2
	elapsed=$(($(date +%s) - START))
}
alive() {
	! has "$RED" && kill -0 "$QPID" 2>/dev/null && [ "$elapsed" -lt "$TIMEOUT" ]
}

# 1. Boot, dan de jobspec naar de leader.
while alive && [ -z "$POSTED" ]; do
	if all "$BOOT_MARKS"; then
		if out="$(curl -s -m 20 -w ' HTTP %{http_code}' -X POST \
			-H 'Content-Type: application/json' -d "$JOB" \
			"http://127.0.0.1:$LEADERPORT/v1/jobs" 2>&1)"; then
			POSTED="$out"
		else
			POSTED="ROOD curl: $out"
		fi
	fi
	step
done

# 2. De plaatsing, dan de pagina en /health van buiten.
while alive && [ -z "$PAGE_OK" ]; do
	if all "$PLACE_MARKS"; then
		code="$(curl -s -m 5 -o "$PAGE" -w '%{http_code}' "http://127.0.0.1:$WEBPORT/" 2>/dev/null || true)"
		if [ "$code" = 200 ] && grep -q -F '( -.-)' "$PAGE" && grep -q 'slot 2' "$PAGE"; then
			PAGE_OK="HTTP $code, $(wc -c <"$PAGE" | tr -d ' ') bytes"
			HEALTH="$(curl -s -m 5 -w ' HTTP %{http_code}' "http://127.0.0.1:$WEBPORT$HEALTH_PATH" 2>&1 || true)"
			break
		fi
	fi
	step
done
[ -n "${KEEP_PAGE:-}" ] && [ -s "$PAGE" ] && cp "$PAGE" "$KEEP_PAGE"

# 3. De stop: de job weg, de poort dicht.
if [ -n "$PAGE_OK" ]; then
	DELETED="$(curl -s -m 20 -w ' HTTP %{http_code}' -X DELETE \
		"http://127.0.0.1:$LEADERPORT/v1/jobs/welcome" 2>&1 || true)"
	while alive && ! all "$STOP_MARKS"; do step; done
	if all "$STOP_MARKS"; then
		# Na de intrekking bereikt 127.0.0.1:$WEBPORT het slot niet meer:
		# geen pagina (een lege reply, een reset of een timeout).
		sleep 0.5
		code="$(curl -s -m 3 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$WEBPORT/" 2>/dev/null || true)"
		case "$code" in
		200) GONE="" ;;
		*) GONE="no page any more (curl code ${code:-none})" ;;
		esac
	fi
fi

kill "$QPID" 2>/dev/null || true
wait "$QPID" 2>/dev/null || true
QPID=""

fail=0
IFS_WAS="$IFS"
IFS='|'
for m in $BOOT_MARKS $PLACE_MARKS $STOP_MARKS; do
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
if [ -n "$PAGE_OK" ]; then
	echo "   ok  GET http://127.0.0.1:$WEBPORT/: $PAGE_OK, met de bunny"
else
	echo "   ROOD GET http://127.0.0.1:$WEBPORT/: geen pagina met de bunny"
	fail=1
fi
case "$HEALTH" in
"ok"*"HTTP 200") echo "   ok  GET $HEALTH_PATH: $(printf '%s' "$HEALTH" | tr '\n' ' ')" ;;
*) echo "   ROOD GET $HEALTH_PATH: ${HEALTH:-nooit gevraagd}"; fail=1 ;;
esac
case "$DELETED" in
*"HTTP 2"*) echo "   ok  DELETE /v1/jobs/welcome: $DELETED" ;;
*) echo "   ROOD DELETE /v1/jobs/welcome: ${DELETED:-nooit gedaan}"; fail=1 ;;
esac
if [ -n "$GONE" ]; then
	echo "   ok  na de stop: $GONE"
else
	echo "   ROOD na de stop: de poort gaf nog de pagina, of de stop kwam nooit"
	fail=1
fi
if grep -q "GET /welcome.elf" "$ART/http.log" 2>/dev/null; then
	echo "   ok  artifact-server: $(grep -c 'GET /welcome.elf' "$ART/http.log") download(s) van welcome.elf"
else
	echo "   ROOD artifact-server: nooit gevraagd"
	fail=1
fi
if has "$RED"; then
	echo "   ROOD $(tr -d '\r' <"$LOG" | grep -m1 -E "$RED")"
	fail=1
fi
echo "   tijd: $(($(date +%s) - START)) s na de start van QEMU"
if [ "$fail" != 0 ]; then
	KEEP="$(mktemp -t hopos-qemu-welcome-rood.XXXXXX)"
	tr -d '\r' <"$LOG" >"$KEEP"
	echo "== console bewaard in $KEEP"
	echo "== console:"
	cat "$KEEP"
	exit 1
fi
[ -n "${KEEP_LOG:-}" ] && tr -d '\r' <"$LOG" >"$KEEP_LOG"
echo "welcome-kring groen"
