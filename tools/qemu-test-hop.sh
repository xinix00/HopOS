#!/bin/sh
# De kring van HopOS v3 op QEMU: de kern start Hop, en Hop plaatst een app.
#
# De kern boot met agentd-hopos (de bewoner uit de hop-repo, gestaged door
# image/qemu-run.sh met de rol Hop) in slot 1, met het Privilege-token, de
# HOPOS_*-env, de wandklok en de poorten 8080/9080 van de uplink doorgezet
# naar het slot. Van buiten gaat er daarna één jobspec naar de leader van
# Hop; Hop haalt appspike van een artifact-server op de host (voor de gast
# 10.0.2.2), stroomt hem via de bevoegde system-API de kern in, en de kern
# plaatst hem in slot 2. Groen alleen als:
#
#   kern        HOPOS_BOOT, HOPOS_CLOCK_FIXED, HOPOS_PRIVILEGE, HOPOS_NET_UP,
#               HOPOS_SYSTEM_UP, HOPOS_DISK_UP en HOPOS_FS_UP fresh=1 (een
#               verse schijf per run), HOPOS_HOP_START slot=1, en twee keer
#               HOPOS_HOP_PUBLISH (8080 en 9080);
#   Hop         via de servicer van slot 1: HOP_UP en HOP_LEADER, en
#               het zaad van de kern op zijn control-page: HOPOS_RNG_SLOTS
#               source=jitter (virt heeft geen TRNG) en "applib: rng seed
#               from the kernel ... HOPOS_APP_RNG source=jitter" (applib
#               van deze werkboom, dus ook met een Hop van vóór het zaad);
#   van buiten  POST http://127.0.0.1:$LEADERPORT/v1/jobs wordt aangenomen
#               (onbeveiligd: de kern geeft Hop HOPOS_INSECURE=1);
#   de plaatsing HOP_JOB_PLACED slot=2, HOPOS_SLOT_START slot=2 en
#               "slot 2: HOPOS_APPSPIKE_DONE pass=9 fail=0" (met de
#               FS-toets in de eigen root van slot 2);
#   Hop's staat de kern ziet slot 1 zijn agent-state.json in zijn volume
#               bewaren ("hopfs: slot 1 saved /hop/agent-state.json as
#               /volumes/hop/..." HOPOS_FS_SAVED), en HOP_STATE_SKIPPED komt
#               nergens voor (dan kon Hop zijn staat niet kwijt); daarna een
#               HOPOS_FS_COMMIT (de boom op de schijf vastgelegd);
#   de herstart  een tweede boot op dezelfde schijf: HOPOS_FS_UP fresh=0
#               ("hopfs: tree restored"), en Hop leest zijn staat terug
#               (Node::restore) en neemt de kooi van spike over: HOP_ADOPTED;
#   van buiten  GET http://127.0.0.1:$AGENTPORT/tasks toont de taak van
#               "spike" als running. Een exit bestaat daar niet als staat:
#               appspike stopt met code 0 (applib: shutdown code=0) en Hop
#               herstart een service, dus de toets pollt tot hij running ziet.
#
# De OS-core (PORT.md beslissing 2): Hop deelt de core van de kern
# (HOPOS_HOP_START slot=1 core=0, cpu = de OS-core), de overgang bewees
# zichzelf bij boot (HOPOS_OS_SELFTEST ok: terug op de timer, een yield en
# de kick-SGI), en appspike landt op de eerste app-core (HOPOS_SLOT_START
# slot=2 core=1). Met SMP=2 is dat de enige andere core; met OSCORE=1
# verhuist de kern eerst naar core 1 en wordt core 0 die app-core
# (HOPOS_OSCORE_PARKED, en cpu=0 voor slot 2).
#
# Een HOPOS_PANIC, HOPOS_EXCEPTION, HOPOS_HOP_FAULT, HOPOS_HOP_EXIT,
# HOP_STATE_SKIPPED, of een OS-core die niet deelt (HOPOS_OS_SELFTEST_FAIL,
# HOPOS_OS_CORE_FAIL, HOPOS_OSCORE_FALLBACK, HOPOS_CAGE_FAIL) is meteen rood.
# Rood bewaart de console (en drukt hem af).
#
#   tools/qemu-test-hop.sh                 TIMEOUT=60 standaard, in seconden
#   SMP=2 tools/qemu-test-hop.sh           twee cores (standaard 4)
#   SMP=2 OSCORE=1 tools/qemu-test-hop.sh  de kern en Hop op core 1
#   KEEP_LOG=pad tools/qemu-test-hop.sh    bewaart ook een groene console
#   SYSPORT/AGENTPORT/LEADERPORT/ARTPORT   de host-poorten; bezet = een vrije
#                                          poort van het OS, luid gemeld
#   HOP_DIR=pad                            de hop-repo (standaard ../hop/hop)
#   HOP_PATCH=0                            Hop tegen de tag van de hop-repo in
#                                          plaats van de applib van deze
#                                          werkboom (tools/hop-build.sh)
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TIMEOUT="${TIMEOUT:-60}"
HOP_DIR="${HOP_DIR:-$DIR/../hop/hop}"
TARGET=aarch64-unknown-none-softfloat
LOG="$(mktemp -t hopos-qemu-hop.XXXXXX)"
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

# Een host-poort: de gevraagde als hij vrij is, anders een vrije van het
# OS. Zo draait de toets naast een andere QEMU of een andere server.
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
HOP_ELF="$(HOP_DIR="$HOP_DIR" sh "$DIR/tools/hop-build.sh" "$TARGET")"

# De artifact-server: appspike zonder debug-info (1,8 MB naar 226 KB,
# gemeten 29-09), symbolen blijven voor de plaatsing.
OBJCOPY="$(ls "$(rustc --print sysroot)"/lib/rustlib/*/bin/rust-objcopy 2>/dev/null | head -1)"
if [ -n "$OBJCOPY" ]; then
	"$OBJCOPY" --strip-debug "$DIR/target/$TARGET/release/appspike" "$ART/appspike.elf"
else
	cp "$DIR/target/$TARGET/release/appspike" "$ART/appspike.elf"
fi
(cd "$ART" && exec python3 -m http.server "$ARTPORT" --bind 127.0.0.1) >"$ART/http.log" 2>&1 &
HPID=$!

OSCPU="${OSCORE:-0}"
APPCPU=1
[ "$OSCPU" = 0 ] || APPCPU=0
echo "== booten op QEMU virt met Hop, ${SMP:-4} cores, OS-core $OSCPU (tot ${TIMEOUT}s; system :$SYSPORT, agent :$AGENTPORT, leader :$LEADERPORT, artifacts :$ARTPORT)"
SYSPORT="$SYSPORT" AGENTPORT="$AGENTPORT" LEADERPORT="$LEADERPORT" HOP_DIR="$HOP_DIR" APP="$HOP_ELF" ROLE=1 DISK="$DISK" \
	sh "$DIR/image/qemu-run.sh" </dev/null >"$LOG" 2>&1 &
QPID=$!

has() { tr -d '\r' <"$LOG" | grep -q -E "$1"; }

# De vaste markers (grep -E), in de volgorde waarin ze horen te komen.
BOOT_MARKS="HOPOS_BOOT|HOPOS_CLOCK_FIXED|HOPOS_PRIVILEGE|HOPOS_DISK_UP model=virtio-blk|HOPOS_FS_UP fresh=1|HOPOS_NET_UP|HOPOS_SYSTEM_UP|HOPOS_OS_SELFTEST ok|HOPOS_HOP_START slot=1 core=0 cpu=$OSCPU |uplink tcp :8080 -> slot 1 :8080 HOPOS_HOP_PUBLISH|uplink tcp :9080 -> slot 1 :9080 HOPOS_HOP_PUBLISH|slot 1: .*HOP_LEADER|slot 1: .*HOP_UP|HOPOS_RNG_SLOTS source=jitter|slot 1: applib: rng seed from the kernel .*HOPOS_APP_RNG source=jitter"
[ "$OSCPU" = 0 ] || BOOT_MARKS="$BOOT_MARKS|HOPOS_OSCORE_PARKED"
PLACE_MARKS="slot 1: .*HOP_JOB_PLACED slot=2|HOPOS_SLOT_START slot=2 core=1 cpu=$APPCPU |slot 2: HOPOS_APPSPIKE_FS ok|slot 2: HOPOS_APPSPIKE_DONE pass=9 fail=0|hopfs: slot 1 saved /hop/agent-state.json as /volumes/hop/agent-state.json .*HOPOS_FS_SAVED"
RED="HOPOS_PANIC|HOPOS_EXCEPTION|HOPOS_HOP_FAULT|HOPOS_HOP_EXIT|HOPOS_HOP_FAIL|HOP_STATE_SKIPPED|HOPOS_OS_SELFTEST_FAIL|HOPOS_OS_CORE_FAIL|HOPOS_OSCORE_FALLBACK|HOPOS_CAGE_FAIL"

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
			# De job van buiten, naar de leader via de hostfwd en de DNAT.
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
	if all "$PLACE_MARKS"; then
		# De taak van buiten: running (Hop herstart de service na zijn
		# exit 0, dus even pollen).
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

# De herstart hieronder leest terug wat er VASTGELEGD is: wacht tot de
# committer (elke 10 s, kern::rpc::COMMIT_EVERY) de boom na de eerste
# bewaarde staat van Hop wegschreef.
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
"") echo "   ROOD POST /v1/jobs nooit gedaan (Hop niet op tijd op)"; fail=1 ;;
*) echo "   ROOD POST /v1/jobs: $POSTED"; fail=1 ;;
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
# De meetlat: de rtt van appspike's NET-toets en de laatste tik met de
# overgangen van de OS-core (in/irq/ipi/timer/yield en de tijd van Hop).
echo "   meting: $(tr -d '\r' <"$LOG" | grep -o 'dial_us=[0-9]*' | tr '\n' ' ')"
echo "   meting: $(tr -d '\r' <"$LOG" | grep -o 'os(in=.*' | tail -1)"
if [ "$fail" != 0 ]; then
	KEEP="$(mktemp -t hopos-qemu-hop-rood.XXXXXX)"
	tr -d '\r' <"$LOG" >"$KEEP"
	echo "== console bewaard in $KEEP"
	echo "== console:"
	cat "$KEEP"
	exit 1
fi
[ -n "${KEEP_LOG:-}" ] && tr -d '\r' <"$LOG" >"$KEEP_LOG"

# De herstart: dezelfde schijf, een nieuwe boot. hopfs vindt de boom terug
# (fresh=0), Hop leest zijn agent-state.json uit /hop/ (Node::restore) en
# neemt de kooi van zijn taak over: HOP_ADOPTED. Dat de kooi na een koude
# boot leeg is, merkt Hop daarna zelf; hier telt dat hij zijn staat terugvond.
echo "== herstart op dezelfde schijf (tot ${TIMEOUT}s)"
LOG1="$LOG"
LOG="$(mktemp -t hopos-qemu-hop2.XXXXXX)"
SYSPORT="$SYSPORT" AGENTPORT="$AGENTPORT" LEADERPORT="$LEADERPORT" HOP_DIR="$HOP_DIR" APP="$HOP_ELF" ROLE=1 DISK="$DISK" \
	sh "$DIR/image/qemu-run.sh" </dev/null >"$LOG" 2>&1 &
QPID=$!
RESTART_MARKS="HOPOS_FS_UP fresh=0|hopfs: tree restored|HOPOS_HOP_START slot=1 core=0 cpu=$OSCPU |slot 1: .*HOP_ADOPTED"
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
	KEEP="$(mktemp -t hopos-qemu-hop-rood.XXXXXX)"
	tr -d '\r' <"$LOG" >"$KEEP"
	echo "== console van de herstart bewaard in $KEEP"
	cat "$KEEP"
	rm -f "$LOG"
	exit 1
fi
[ -n "${KEEP_LOG:-}" ] && tr -d '\r' <"$LOG" >"$KEEP_LOG.restart"
rm -f "$LOG"
LOG="$LOG1"
echo "qemu-kring groen"
