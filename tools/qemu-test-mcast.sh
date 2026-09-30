#!/bin/sh
# MCAST tussen twee benches in één node: Hop plaatst MCAST=listen en
# MCAST=send; de switch floodt de probe, de luisteraar joint 224.0.0.251.
# Groen: "HOPOS_BENCH_UP role=mcast-listen" en "HOPOS_BENCH_MCAST recv=3".
# Een kopie van de vorm van tools/qemu-test-bench.sh (boot 1, zonder netmeter).
set -eu
DIR="$(cd "$(dirname "$0")/.." && pwd)"
TIMEOUT="${TIMEOUT:-120}"
HOP_DIR="${HOP_DIR:-$DIR/../hop/hop}"
TARGET=aarch64-unknown-none-softfloat
LOG="$(mktemp -t hopos-qemu-mcast.XXXXXX)"
ART="$(mktemp -d -t hopos-art.XXXXXX)"
QPID=""; HPID=""
cleanup() { [ -n "$QPID" ] && kill "$QPID" 2>/dev/null; [ -n "$HPID" ] && kill "$HPID" 2>/dev/null; rm -rf "$ART"; true; }
trap cleanup EXIT INT TERM
port() { python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])'; }
SYSPORT=$(port); AGENTPORT=$(port); LEADERPORT=$(port); ARTPORT=$(port); WEBPORT=$(port)
cd "$DIR"
cargo build --quiet --release --target "$TARGET" -p hopos --features board-qemuvirt
cargo build --quiet --release --target "$TARGET" -p bench
HOP_ELF="$(HOP_DIR="$HOP_DIR" sh "$DIR/tools/hop-build.sh" "$TARGET")"
OBJCOPY="$(ls "$(rustc --print sysroot)"/lib/rustlib/*/bin/rust-objcopy 2>/dev/null | head -1)"
"$OBJCOPY" --strip-debug "$DIR/target/$TARGET/release/bench" "$ART/bench.elf"
(cd "$ART" && exec python3 -m http.server "$ARTPORT" --bind 127.0.0.1) >"$ART/http.log" 2>&1 &
HPID=$!
SYSPORT="$SYSPORT" AGENTPORT="$AGENTPORT" LEADERPORT="$LEADERPORT" WEBPORT="$WEBPORT" \
	HOP_DIR="$HOP_DIR" APP="$HOP_ELF" ROLE=1 DISK="$ART/disk.img" OSCORE= \
	sh "$DIR/image/qemu-run.sh" </dev/null >"$LOG" 2>&1 &
QPID=$!
has() { tr -d '\r' <"$LOG" | grep -q -E "$1"; }
START=$(date +%s)
job() {
	curl -s -m 20 -w ' HTTP %{http_code}' -X POST -H 'Content-Type: application/json' \
		-d '{"name":"'"$1"'","driver":"hop","artifacts":[{"url":"http://10.0.2.2:'"$ARTPORT"'/bench.elf"}],"memory_limit":67108864,"env":{'"$2"'}}' \
		"http://127.0.0.1:$LEADERPORT/v1/jobs" 2>&1 || true
}
RED="HOPOS_PANIC|HOPOS_EXCEPTION|HOPOS_APP_PANIC|HOPOS_HOP_FAULT|HOPOS_HOP_EXIT|HOPOS_BENCH_FAIL"
alive() { ! has "$RED" && kill -0 "$QPID" 2>/dev/null && [ $(($(date +%s) - START)) -lt "$TIMEOUT" ]; }
while alive && ! has 'HOP_LEADER'; do sleep 0.3; done
sleep 1
echo "listen: $(job mlisten '"MCAST":"listen"')"
while alive && ! has 'role=mcast-listen'; do sleep 0.3; done
echo "send:   $(job msend '"MCAST":"send"')"
while alive && ! has 'HOPOS_BENCH_MCAST recv=3'; do sleep 0.3; done
kill "$QPID" 2>/dev/null || true; wait "$QPID" 2>/dev/null || true; QPID=""
tr -d '\r' <"$LOG" | grep -E 'MCAST|JOB_PLACED' | sed 's/^/   /'
if has 'HOPOS_BENCH_MCAST recv=3' && ! has "$RED"; then
	echo "mcast groen in $(($(date +%s) - START)) s"
	[ -n "${KEEP_LOG:-}" ] && tr -d '\r' <"$LOG" >"$KEEP_LOG"
	rm -f "$LOG"
	exit 0
fi
echo "ROOD; console in $LOG"; tail -40 "$LOG"; exit 1
