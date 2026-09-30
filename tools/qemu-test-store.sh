#!/bin/sh
# De store-ops op QEMU: een app kopieert tussen zijn eigen map in een
# object-store en zijn hopfs-zicht, via de kern en Hop.
#
# Op de host draait een kleine S3-nep (tools/fakes3.py: één bucket,
# path-style, PUT/GET/DELETE/LIST, de Authorization-kop geëist, SigV4 niet
# nagerekend, wel de payload-hash), voor de gast op 10.0.2.2 (slirp), over
# http: de nep heeft geen certificaat, en Hop zegt dat luid
# (HOP_STORE_PLAIN_HTTP). De kern boot met Hop in slot 1 en hopos.s3.* in de
# bootargs (image/qemu-run.sh BOOTARGS, hopos/src/config.rs geeft ze als
# HOPOS_S3_* aan Hop). Met een bucket is dat ook de lock van Hop's cluster,
# en een lease vraagt een gezette klok: daarom ook een SNTP-antwoorder op de
# host (hopos.ntp=10.0.2.2:$NTPPORT, HOP_CLOCK_SYNCED). Van buiten gaat er
# één jobspec naar de leader:
# appspike met ROLE=STORE (appspike/src/main.rs, Go's store_demo.go):
#
#   pull van iets dat er nooit was   een nette fout (NotFound)
#   push state.json                  de nep ziet PUT apps/hopos/spike/state.json
#   list ""                          precies [state.json], relatief aan de map
#   pull state.json -> copy.json     een vuil, langer bestand wordt vervangen
#   drop state.json, list ""         leeg
#
# Groen alleen als:
#
#   Hop       "HOP_STORE_UP" (de bucket onder apps/hopos/) en
#             HOP_STORE_PLAIN_HTTP;
#   de app    "slot 2: HOPOS_APPSPIKE_STORE ok" en de hele doorloop groen
#             (HOPOS_APPSPIKE_DONE pass=10 fail=0);
#   de kern   "store: slot 2 push done (size 30) HOPOS_STORE_DONE" en de
#             pull en drop idem, en de miss (HOPOS_STORE_MISS);
#   de nep    PUT, GET en DELETE van apps/hopos/spike/state.json, de 404 van
#             apps/hopos/spike/never-pushed.json, en geen 403 (elke call
#             getekend) en geen 400 (elke hash klopte).
#
# Rood is ook: HOPOS_APPSPIKE_STORE FAIL, HOP_STORE_FAIL, HOP_STORE_KERNEL,
# HOPOS_STORE_NO_SERVICE, HOPOS_STORE_FULL, en de rode markers van
# qemu-test-hop.sh. Rood bewaart de console (en drukt hem af).
#
#   tools/qemu-test-store.sh                TIMEOUT=90 standaard, in seconden
#   KEEP_LOG=pad tools/qemu-test-store.sh
#   SYSPORT/AGENTPORT/LEADERPORT/ARTPORT/S3PORT/NTPPORT  de host-poorten
#   HOP_DIR=pad                             de hop-repo (standaard ../hop/hop)
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TIMEOUT="${TIMEOUT:-90}"
HOP_DIR="${HOP_DIR:-$DIR/../hop/hop}"
TARGET=aarch64-unknown-none-softfloat
LOG="$(mktemp -t hopos-qemu-store.XXXXXX)"
ART="$(mktemp -d -t hopos-art.XXXXXX)"
DISK="$ART/disk.img"
S3LOG="$ART/s3.log"
QPID=""
HPID=""
SPID=""
NPID=""
cleanup() {
	[ -n "$QPID" ] && kill "$QPID" 2>/dev/null
	[ -n "$NPID" ] && kill "$NPID" 2>/dev/null
	[ -n "$HPID" ] && kill "$HPID" 2>/dev/null
	[ -n "$SPID" ] && kill "$SPID" 2>/dev/null
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
S3PORT="$(port "${S3PORT:-9000}" S3PORT)"
NTPPORT="$(port "${NTPPORT:-10123}" NTPPORT)"

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
python3 "$DIR/tools/fakes3.py" "$S3PORT" hop >"$S3LOG" 2>&1 &
SPID=$!
# De tijd van de host als SNTP-server (modus 4, stratum 2; het transmit-veld
# van de vraag terug als originate, dat toetst Hop).
python3 - "$NTPPORT" >"$ART/ntp.log" 2>&1 <<'PY' &
import socket, struct, sys, time
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(("127.0.0.1", int(sys.argv[1])))
def stamp(t):
    t += 2208988800
    return struct.pack("!II", int(t), int((t % 1) * (1 << 32)))
while True:
    req, addr = s.recvfrom(512)
    if len(req) < 48:
        continue
    now = time.time()
    resp = bytes([0x24, 2, 6, 0xEC]) + bytes(8) + b"LOCL" + stamp(now) + req[40:48] + stamp(now) + stamp(time.time())
    s.sendto(resp, addr)
PY
NPID=$!

S3ARGS="hopos.s3.endpoint=http://10.0.2.2:$S3PORT hopos.s3.bucket=hop hopos.s3.region=us-east-1 hopos.s3.key=hopkey hopos.s3.secret=hopsecret hopos.s3.pathstyle=1 hopos.ntp=10.0.2.2:$NTPPORT"
echo "== booten op QEMU virt met Hop en een S3-nep (tot ${TIMEOUT}s; agent :$AGENTPORT, leader :$LEADERPORT, artifacts :$ARTPORT, s3 :$S3PORT)"
SYSPORT="$SYSPORT" AGENTPORT="$AGENTPORT" LEADERPORT="$LEADERPORT" HOP_DIR="$HOP_DIR" APP="$HOP_ELF" ROLE=1 DISK="$DISK" \
	BOOTARGS="$S3ARGS" sh "$DIR/image/qemu-run.sh" </dev/null >"$LOG" 2>&1 &
QPID=$!

has() { tr -d '\r' <"$LOG" | grep -q -E "$1"; }
s3has() { grep -q -E "$1" "$S3LOG" 2>/dev/null; }
all() {
	(
		IFS='|'
		for m in $1; do has "$m" || exit 1; done
	)
}
s3all() {
	(
		IFS='|'
		for m in $1; do s3has "$m" || exit 1; done
	)
}

BOOT_MARKS="HOPOS_BOOT|HOPOS_FS_UP fresh=1|HOPOS_SYSTEM_UP|slot 1: .*HOP_CLOCK_SYNCED|slot 1: .*HOP_STORE_PLAIN_HTTP|slot 1: .*HOP_STORE_UP|slot 1: .*HOP_LEADER|slot 1: .*HOP_UP"
RUN_MARKS="slot 1: .*HOP_JOB_PLACED slot=2|store: slot 2 pull: no such object HOPOS_STORE_MISS|store: slot 2 push done \\(size 30\\) HOPOS_STORE_DONE|store: slot 2 list done \\(size 1\\) HOPOS_STORE_DONE|store: slot 2 pull done \\(size 30\\) HOPOS_STORE_DONE|store: slot 2 drop done|store: slot 2 list done \\(size 0\\) HOPOS_STORE_DONE|slot 2: HOPOS_APPSPIKE_STORE ok|slot 2: HOPOS_APPSPIKE_DONE pass=10 fail=0"
S3_MARKS="S3 GET apps/hopos/spike/never-pushed.json 404|S3 PUT apps/hopos/spike/state.json 200|S3 GET \\?list prefix=apps/hopos/spike/ 200|S3 GET apps/hopos/spike/state.json 200|S3 DELETE apps/hopos/spike/state.json 204"
RED="HOPOS_PANIC|HOPOS_EXCEPTION|HOPOS_HOP_FAULT|HOPOS_HOP_EXIT|HOPOS_HOP_FAIL|HOPOS_CAGE_FAIL|HOPOS_APPSPIKE_STORE FAIL|HOP_STORE_FAIL|HOP_STORE_KERNEL|HOPOS_STORE_NO_SERVICE|HOPOS_STORE_FULL"
S3RED="S3 [A-Z]+ .* (403|400)$"

JOB='{"name":"spike","driver":"hop","artifacts":[{"url":"http://10.0.2.2:'"$ARTPORT"'/appspike.elf"}],"memory_limit":33554432,"env":{"ROLE":"STORE"}}'
POSTED=""
START=$(date +%s)
elapsed=0
step() {
	sleep 0.2
	elapsed=$(($(date +%s) - START))
}
while :; do
	has "$RED" && break
	s3has "$S3RED" && break
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
	all "$RUN_MARKS" && s3all "$S3_MARKS" && break
	step
done
kill "$QPID" 2>/dev/null || true
wait "$QPID" 2>/dev/null || true
QPID=""

fail=0
IFS_WAS="$IFS"
IFS='|'
for m in $BOOT_MARKS $RUN_MARKS; do
	if has "$m"; then
		echo "   ok  $m: $(tr -d '\r' <"$LOG" | grep -m1 -E "$m")"
	else
		echo "   ROOD $m ontbreekt"
		fail=1
	fi
done
for m in $S3_MARKS; do
	if s3has "$m"; then
		echo "   ok  nep-S3: $(grep -m1 -E "$m" "$S3LOG")"
	else
		echo "   ROOD nep-S3: $m ontbreekt"
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
if s3has "$S3RED"; then
	echo "   ROOD nep-S3: $(grep -m1 -E "$S3RED" "$S3LOG")"
	fail=1
fi
echo "   nep-S3: $(grep -c '^S3 ' "$S3LOG") regels; alle keys: $(grep -o 'apps/[^ ]*' "$S3LOG" | sort -u | tr '\n' ' ')"
echo "   tijd: $(($(date +%s) - START)) s na de start van QEMU"
if [ "$fail" != 0 ]; then
	KEEP="$(mktemp -t hopos-qemu-store-rood.XXXXXX)"
	tr -d '\r' <"$LOG" >"$KEEP"
	echo "== console bewaard in $KEEP"
	echo "== nep-S3:"
	cat "$S3LOG"
	echo "== console:"
	cat "$KEEP"
	exit 1
fi
[ -n "${KEEP_LOG:-}" ] && tr -d '\r' <"$LOG" >"$KEEP_LOG"
echo "qemu-store groen"
