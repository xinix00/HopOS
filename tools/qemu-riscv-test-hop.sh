#!/bin/sh
# De kring van HopOS v3 op riscv64 (QEMU virt, machine mode, twee harts): de
# kern start Hop op de OS-core, en Hop plaatst een app op het app-hart. De
# riscv-vorm van tools/qemu-test-hop.sh, met dezelfde toetsen:
#
#   kern        HOPOS_BOOT, HOPOS_CLOCK_FIXED, HOPOS_PRIVILEGE, HOPOS_NET_UP,
#               HOPOS_SYSTEM_UP, HOPOS_DISK_UP en HOPOS_FS_UP fresh=1,
#               HOPOS_OS_SELFTEST ok (de overgang van de kern-hart naar een
#               bewoner en terug: de wekker, de yield, de kick en de exit,
#               cpu/src/riscv/oscore.rs), HOPOS_HOP_START slot=1 core=0 cpu=0
#               (Hop deelt hart 0 met de kern, PORT.md beslissing 2), en
#               twee keer HOPOS_HOP_PUBLISH (8080 en 9080);
#   Hop         via de servicer van slot 1: HOP_UP en HOP_LEADER;
#   van buiten  POST http://127.0.0.1:$LEADERPORT/v1/jobs wordt aangenomen;
#   de plaatsing HOP_JOB_PLACED slot=2, HOPOS_SLOT_START slot=2 core=1 cpu=1
#               (het app-hart, onder de M-mode-switcher) en "slot 2:
#               HOPOS_APPSPIKE_DONE pass=9 fail=0";
#   Hop's staat HOPOS_FS_SAVED van /hop/agent-state.json en daarna een
#               HOPOS_FS_COMMIT;
#   van buiten  GET http://127.0.0.1:$AGENTPORT/tasks toont "spike" running;
#   de herstart  dezelfde schijf: HOPOS_FS_UP fresh=0 en HOP_ADOPTED.
#
# Hop zelf: agentd-hopos uit de hop-repo ($HOP_DIR, standaard ../hop/hop),
# gebouwd voor riscv64gc-unknown-none-elf. De hop-repo pint applib op een
# tag (v3.0.0-alpha.9) waarin de riscv-_start, de paniek en de timebase nog
# ontbreken; tot hij een tag met deze applib pint, bouwt dit script hem
# tegen de applib en abi van DEZE werkboom: een kopie van de hop-repo
# (`git archive`, HOP_REV, standaard HEAD) in target/hop-riscv, met een
# `[patch]` in de .cargo/config.toml van die kopie. De hop-repo zelf wordt
# niet aangeraakt. HOP_PATCH=0 bouwt in $HOP_DIR zelf, zonder patch (voor
# een hop-repo die al een geschikte tag pint).
#
# Rood is rood: HOPOS_PANIC, HOPOS_EXCEPTION, HOPOS_HOP_FAULT,
# HOPOS_HOP_EXIT, HOPOS_HOP_FAIL, HOP_STATE_SKIPPED, HOPOS_OS_SELFTEST_FAIL,
# HOPOS_OS_CORE_FAIL, HOPOS_OS_CORE_NONE of HOPOS_CAGE_FAIL is meteen rood,
# en rood bewaart en drukt de console af.
#
#   tools/qemu-riscv-test-hop.sh           TIMEOUT=90 standaard, in seconden
#   KEEP_LOG=pad tools/qemu-riscv-test-hop.sh   bewaart ook een groene console
#   SYSPORT/AGENTPORT/LEADERPORT/ARTPORT   de host-poorten; bezet = een vrije
#                                          poort van het OS, luid gemeld
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TIMEOUT="${TIMEOUT:-90}"
HOP_DIR="${HOP_DIR:-$DIR/../hop/hop}"
HOP_REV="${HOP_REV:-HEAD}"
HOP_PATCH="${HOP_PATCH:-1}"
TARGET=riscv64gc-unknown-none-elf
LOG="$(mktemp -t hopos-rv-hop.XXXXXX)"
ART="$(mktemp -d -t hopos-rv-art.XXXXXX)"
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
[ -d "$HOP_DIR" ] || {
	echo "FAIL: de hop-repo ontbreekt: $HOP_DIR (zet HOP_DIR)"
	exit 1
}
echo "== bouwen: hopos (qemuvirt-riscv), appspike, en agentd-hopos ($HOP_DIR $HOP_REV)"
cargo build --quiet --release --target "$TARGET" -p hopos --features board-qemuvirt-riscv
cargo build --quiet --release --target "$TARGET" -p appspike
if [ "$HOP_PATCH" = 1 ]; then
	HOP_SRC="$DIR/target/hop-riscv/src"
	rm -rf "$HOP_SRC"
	mkdir -p "$HOP_SRC/.cargo"
	git -C "$HOP_DIR" archive -o "$ART/hop.tar" "$HOP_REV"
	tar -x -C "$HOP_SRC" -f "$ART/hop.tar"
	cat >"$HOP_SRC/.cargo/config.toml" <<EOF
# Gezet door tools/qemu-riscv-test-hop.sh: applib en abi uit de werkboom
# van hop-os, tot de hop-repo een tag met de riscv-applib pint.
[patch."https://github.com/xinix00/HopOS.git"]
applib = { path = "$DIR/applib" }
abi = { path = "$DIR/abi" }
EOF
	(cd "$HOP_SRC" && cargo build --quiet --release --target "$TARGET" \
		--target-dir "$DIR/target/hop-riscv/target" -p agentd-hopos)
	HOP_ELF="$DIR/target/hop-riscv/target/$TARGET/release/agentd-hopos"
else
	(cd "$HOP_DIR" && cargo build --quiet --release --target "$TARGET" -p agentd-hopos)
	HOP_ELF="$HOP_DIR/target/$TARGET/release/agentd-hopos"
fi

OBJCOPY="$(ls "$(rustc --print sysroot)"/lib/rustlib/*/bin/rust-objcopy 2>/dev/null | head -1)"
strip() {
	if [ -n "$OBJCOPY" ]; then "$OBJCOPY" --strip-debug "$1" "$2"; else cp "$1" "$2"; fi
}
strip "$DIR/target/$TARGET/release/appspike" "$ART/appspike.elf"
strip "$HOP_ELF" "$ART/hop.elf"
HOP_SIZE=$(wc -c <"$ART/hop.elf" | tr -d ' ')
[ "$HOP_SIZE" -le 14680064 ] || {
	echo "FAIL: agentd-hopos is $HOP_SIZE bytes, the staging holds 14680064"
	exit 1
}
(cd "$ART" && exec python3 -m http.server "$ARTPORT" --bind 127.0.0.1) >"$ART/http.log" 2>&1 &
HPID=$!
truncate -s 64m "$DISK"

# QEMU met Hop op de staging (board/qemuvirt-riscv/src/slots.rs: het image
# op STAGE_PA, zijn maat op STAGE_HDR_PA, rol 1 = Hop op STAGE_ROLE_PA).
boot() {
	qemu-system-riscv64 -M virt -m 1G -smp 2 -bios none -nographic \
		-kernel "$DIR/target/$TARGET/release/hopos" \
		-global virtio-mmio.force-legacy=false \
		-netdev "user,id=n0,hostfwd=tcp:127.0.0.1:${SYSPORT}-:10100,hostfwd=tcp:127.0.0.1:${AGENTPORT}-:8080,hostfwd=tcp:127.0.0.1:${LEADERPORT}-:9080" \
		-device virtio-net-device,netdev=n0 \
		-drive file="$DISK",if=none,format=raw,id=d0 -device virtio-blk-device,drive=d0 \
		-device loader,file="$ART/hop.elf",addr=0xa8200000,force-raw=on \
		-device loader,addr=0xa8100000,data="$HOP_SIZE",data-len=8 \
		-device loader,addr=0xa8100008,data=1,data-len=8 \
		</dev/null >"$LOG" 2>&1 &
	QPID=$!
}

echo "== booten op QEMU virt riscv64 met Hop op hart 0 (tot ${TIMEOUT}s; system :$SYSPORT, agent :$AGENTPORT, leader :$LEADERPORT, artifacts :$ARTPORT)"
boot

has() { tr -d '\r' <"$LOG" | grep -q -E "$1"; }

BOOT_MARKS="HOPOS_BOOT|HOPOS_CLOCK_FIXED|HOPOS_PRIVILEGE|HOPOS_DISK_UP model=virtio-blk|HOPOS_FS_UP fresh=1|HOPOS_NET_UP|HOPOS_SYSTEM_UP|HOPOS_OS_SELFTEST ok|HOPOS_OS_CORE_UP|HOPOS_HOP_START slot=1 core=0 cpu=0 |uplink tcp :8080 -> slot 1 :8080 HOPOS_HOP_PUBLISH|uplink tcp :9080 -> slot 1 :9080 HOPOS_HOP_PUBLISH|slot 1: .*HOP_LEADER|slot 1: .*HOP_UP"
PLACE_MARKS="slot 1: .*HOP_JOB_PLACED slot=2|HOPOS_SLOT_START slot=2 core=1 cpu=1 |slot 2: HOPOS_APPSPIKE_FS ok|slot 2: HOPOS_APPSPIKE_DONE pass=9 fail=0|hopfs: slot 1 saved /hop/agent-state.json as /volumes/hop/agent-state.json .*HOPOS_FS_SAVED"
RED="HOPOS_PANIC|HOPOS_EXCEPTION|HOPOS_HOP_FAULT|HOPOS_HOP_EXIT|HOPOS_HOP_FAIL|HOP_STATE_SKIPPED|HOPOS_OS_SELFTEST_FAIL|HOPOS_OS_CORE_FAIL|HOPOS_OS_CORE_NONE|HOPOS_CAGE_FAIL"

all() {
	(
		IFS='|'
		for m in $1; do has "$m" || exit 1; done
	)
}

JOB='{"name":"spike","driver":"hop","artifacts":[{"url":"http://10.0.2.2:'"$ARTPORT"'/appspike.elf"}],"memory_limit":33554432}'
POSTED=""
TASKS=""
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
			if out="$(curl -s -m 30 -w ' HTTP %{http_code}' -X POST \
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
	if all "$PLACE_MARKS"; then
		t="$(curl -s -m 5 "http://127.0.0.1:$AGENTPORT/tasks" 2>&1 || true)"
		if printf '%s' "$t" | python3 -c '
import json, sys
tasks = json.load(sys.stdin)
sys.exit(0 if any(t.get("job_name") == "spike" and t.get("state") == "running" for t in tasks) else 1)
' 2>/dev/null; then
			TASKS="$t"
			break
		fi
		TASKS="(nog niet running) $t"
	fi
	step
done

committed() {
	tr -d '\r' <"$LOG" | awk '/HOPOS_FS_SAVED/ { s = 1 } s && /HOPOS_FS_COMMIT($| )/ { c = 1 } END { exit !c }'
}
if [ -n "$TASKS" ] && [ "${TASKS#(nog niet running)}" = "$TASKS" ]; then
	i=0
	while ! committed && [ "$i" -lt 150 ] && ! has "$RED"; do
		sleep 0.1
		i=$((i + 1))
	done
fi
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
case "$POSTED" in
*"HTTP 2"*) echo "   ok  POST /v1/jobs: $POSTED" ;;
"")
	echo "   ROOD POST /v1/jobs nooit gedaan (Hop niet op tijd op)"
	fail=1
	;;
*)
	echo "   ROOD POST /v1/jobs: $POSTED"
	fail=1
	;;
esac
case "$TASKS" in
"(nog niet running)"* | "")
	echo "   ROOD GET /tasks: geen running spike: ${TASKS:-nooit gevraagd}"
	fail=1
	;;
*) echo "   ok  GET /tasks: $TASKS" ;;
esac
if grep -q "GET /appspike.elf" "$ART/http.log" 2>/dev/null; then
	echo "   ok  artifact-server: $(grep -c 'GET /appspike.elf' "$ART/http.log") download(s) van appspike.elf"
else
	echo "   ROOD artifact-server: nooit gevraagd"
	fail=1
fi
if has "$RED"; then
	echo "   ROOD $(tr -d '\r' <"$LOG" | grep -m1 -E "$RED")"
	fail=1
fi
if committed; then
	echo "   ok  vastgelegd na de bewaarde staat: $(tr -d '\r' <"$LOG" | grep -E 'HOPOS_FS_COMMIT($| )' | tail -1)"
else
	echo "   ROOD geen HOPOS_FS_COMMIT na HOPOS_FS_SAVED"
	fail=1
fi
echo "   tijd: $(($(date +%s) - START)) s na de start van QEMU"
echo "   meting: $(tr -d '\r' <"$LOG" | grep -o 'dial_us=[0-9]*' | tr '\n' ' ')"
echo "   meting: $(tr -d '\r' <"$LOG" | grep -o 'os(in=.*' | tail -1)"
if [ "$fail" != 0 ]; then
	KEEP="$(mktemp -t hopos-rv-hop-rood.XXXXXX)"
	tr -d '\r' <"$LOG" >"$KEEP"
	echo "== console bewaard in $KEEP"
	echo "== console:"
	cat "$KEEP"
	exit 1
fi
[ -n "${KEEP_LOG:-}" ] && tr -d '\r' <"$LOG" >"$KEEP_LOG"

# De herstart: dezelfde schijf, een nieuwe boot. hopfs vindt de boom terug
# (fresh=0), Hop leest zijn agent-state.json terug en neemt de kooi van
# zijn taak over: HOP_ADOPTED.
echo "== herstart op dezelfde schijf (tot ${TIMEOUT}s)"
LOG1="$LOG"
LOG="$(mktemp -t hopos-rv-hop2.XXXXXX)"
boot
RESTART_MARKS="HOPOS_FS_UP fresh=0|hopfs: tree restored|HOPOS_HOP_START slot=1 core=0 cpu=0 |slot 1: .*HOP_ADOPTED"
START=$(date +%s)
elapsed=0
while ! all "$RESTART_MARKS"; do
	has "$RED" && break
	kill -0 "$QPID" 2>/dev/null || break
	[ "$elapsed" -ge "$TIMEOUT" ] && break
	step
done
kill "$QPID" 2>/dev/null || true
wait "$QPID" 2>/dev/null || true
QPID=""
IFS='|'
for m in $RESTART_MARKS; do
	if has "$m"; then
		echo "   ok  $m: $(tr -d '\r' <"$LOG" | grep -m1 -E "$m")"
	else
		echo "   ROOD $m ontbreekt"
		fail=1
	fi
done
IFS="$IFS_WAS"
if has "$RED"; then
	echo "   ROOD $(tr -d '\r' <"$LOG" | grep -m1 -E "$RED")"
	fail=1
fi
echo "   tijd: $(($(date +%s) - START)) s na de herstart"
if [ "$fail" != 0 ]; then
	KEEP="$(mktemp -t hopos-rv-hop-rood.XXXXXX)"
	tr -d '\r' <"$LOG" >"$KEEP"
	echo "== console van de herstart bewaard in $KEEP"
	cat "$KEEP"
	rm -f "$LOG"
	exit 1
fi
[ -n "${KEEP_LOG:-}" ] && tr -d '\r' <"$LOG" >"$KEEP_LOG.restart"
rm -f "$LOG"
LOG="$LOG1"
echo "qemu-kring riscv64 groen"
