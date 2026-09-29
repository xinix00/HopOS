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
#   de som      HOPOS_FLIP_SWITCHCODE_OK: de switch-code van de bundel is die
#               van de bewoners, getoetst vóór de sprong;
#   hopfs       HOPOS_FS_FROZEN generation=N vóór de sprong, en na de landing
#               HOPOS_FS_UP fresh=0 generation=N: dezelfde generatie, dus de
#               staat van Hop overleeft via de schijf én (Hop draait door)
#               via de adoptie;
#   conntrack   HOPOS_FLIP_NAT_CAPTURED flows=K vóór de sprong (K >= 1: de
#               download van de bundel ging door de masquerade), en na de
#               landing "HOPOS_FLIP_NAT restored=K of=K": elke flow terug;
#   een verbinding  een TCP-verbinding van de host naar de agent-poort van
#               Hop, geopend vóór de sprong en pas ná de landing afgemaakt
#               (GET /tasks in twee helften): de gepubliceerde poort en de
#               luisterende Hop overleven de flip met de verbinding erop.
#
# Rood is ook: HOPOS_PANIC, HOPOS_EXCEPTION, een fout van Hop, en elke
# flip-weigering (HOPOS_FLIP_REFUSED, _FAIL, _BLOB_BAD, _GUARD,
# _ADOPT_FAIL, HOP_FLIP_FAIL). Rood bewaart de console en drukt hem af.
#
#   tools/qemu-test-flip.sh                 TIMEOUT=90 standaard, in seconden
#   BOARD=uefi tools/qemu-test-flip.sh      dezelfde flip onder EDK2: de kern als
#                                           BOOTAA64.EFI (image/uefi-run.sh), Hop
#                                           als hopos-stage.elf op de ESP, de
#                                           bundel van image/flip-bundle.sh uefi
#                                           (tools/qemu-uefi-flip-test.sh)
#   MISMATCH=1 tools/qemu-test-flip.sh      de weigering: dezelfde bundel met een
#                                           andere switch-code-som (en dus een
#                                           andere sha256). Groen alleen als de
#                                           kern hem vóór de sprong weigert
#                                           ("HOPOS_FLIP_REFUSED switch code
#                                           mismatch"), Hop HOP_FLIP_FAIL meldt
#                                           en een 5xx geeft, kern A gewoon
#                                           doordraait (GET /tasks) en er geen
#                                           sprong, geen bevriezing en geen
#                                           landing op de console staat.
#   KEEP_LOG=pad tools/qemu-test-flip.sh    bewaart ook een groene console
#   SYSPORT/AGENTPORT/LEADERPORT/ARTPORT    de host-poorten (zoals qemu-test-hop)
#   HOP_DIR=pad                             de hop-repo (standaard ../hop/hop)
#   FEATURES=vhe CPU=neoverse-n1 BOARD=uefi ...  de flip met de kern onder
#                                           E2H = 1 (image/uefi-run.sh en
#                                           image/flip-bundle.sh nemen FEATURES
#                                           mee; alleen BOARD=uefi kent CPU)
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)"
BOARD="${BOARD:-virt}"
case "$BOARD" in
virt) TIMEOUT="${TIMEOUT:-90}" ;;
uefi) TIMEOUT="${TIMEOUT:-150}" ;;
*)
	echo "BOARD=$BOARD: virt of uefi" >&2
	exit 64
	;;
esac
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
echo "== bouwen ($BOARD): kern A (stempel A), bundel B (stempel B), appspike, agentd-hopos"
cargo build --quiet --release --target "$TARGET" -p appspike
(cd "$HOP_DIR" && cargo build --quiet --release --target "$TARGET" -p agentd-hopos)
if [ "$BOARD" = uefi ]; then
	# De ESP van kern A met Hop als gestagede bewoner (rol hop).
	ESP="$ART/esp"
	HOPOS_STAMP=A BUILD_ONLY=1 ESP="$ESP" APP="$HOP_DIR/target/$TARGET/release/agentd-hopos" ROLE=hop \
		sh "$DIR/image/uefi-run.sh" 2>&1 | sed 's/^/   /'
	[ -e "$ESP/EFI/BOOT/BOOTAA64.EFI" ] || {
		echo "ROOD: geen BOOTAA64.EFI"
		exit 1
	}
else
	HOPOS_STAMP=A cargo build --quiet --release --target "$TARGET" -p hopos --features board-qemuvirt
fi
HOPOS_STAMP=B sh "$DIR/image/flip-bundle.sh" "$BOARD"
BUNDLE="hopos-$BOARD.flip"
cp "$DIR/target/$BUNDLE" "$ART/$BUNDLE"
SHA="$(cat "$DIR/target/$BUNDLE.sha256")"
MISMATCH="${MISMATCH:+1}"
if [ -n "$MISMATCH" ]; then
	# De som van de switch-code staat op kop + 56 (kern::kernflip::Bundle,
	# versie 2); één bit om, en de sha256 opnieuw: de bundel is verder echt.
	SHA="$(python3 - "$ART/$BUNDLE" <<'TAMPER'
import hashlib, struct, sys
p = sys.argv[1]
b = bytearray(open(p, "rb").read())
head = struct.unpack_from("<Q", b, len(b) - 16)[0]
v = struct.unpack_from("<Q", b, head + 56)[0]
struct.pack_into("<Q", b, head + 56, v ^ 1)
open(p, "wb").write(b)
print(hashlib.sha256(b).hexdigest())
TAMPER
)"
	echo "   MISMATCH: de switch-code-som van de bundel is omgezet, sha256 $SHA"
fi
OBJCOPY="$(ls "$(rustc --print sysroot)"/lib/rustlib/*/bin/rust-objcopy 2>/dev/null | head -1)"
if [ -n "$OBJCOPY" ]; then
	"$OBJCOPY" --strip-debug "$DIR/target/$TARGET/release/appspike" "$ART/appspike.elf"
else
	cp "$DIR/target/$TARGET/release/appspike" "$ART/appspike.elf"
fi
cat >"$ART/half.py" <<'HALF'
import os, socket, sys, time
port, base = int(sys.argv[1]), sys.argv[2]
out = open(base + ".out", "w")
try:
    s = socket.create_connection(("127.0.0.1", port), timeout=10)
    s.sendall(b"GET /tasks HTTP/1.1\r\n")
    open(base + ".open", "w").close()
    deadline = time.time() + 120
    while not os.path.exists(base + ".go") and time.time() < deadline:
        time.sleep(0.1)
    s.sendall(b"Host: node\r\nConnection: close\r\n\r\n")
    s.settimeout(20)
    got = b""
    while True:
        b = s.recv(4096)
        if not b:
            break
        got += b
    out.write(got.decode("latin-1").split("\r\n", 1)[0])
except Exception as e:
    out.write("ERROR " + repr(e))
out.close()
open(base + ".done", "w").close()
HALF
(cd "$ART" && exec python3 -m http.server "$ARTPORT" --bind 127.0.0.1) >"$ART/http.log" 2>&1 &
HPID=$!

echo "== booten op QEMU $BOARD met Hop, kern A (tot ${TIMEOUT}s; agent :$AGENTPORT, leader :$LEADERPORT, artifacts :$ARTPORT)"
if [ "$BOARD" = uefi ]; then
	# De QEMU-regel van image/uefi-run.sh, met de hostfwd's van Hop erbij
	# (dat script zet alleen de system-API door).
	QEMU_SHARE="${QEMU_SHARE:-/opt/homebrew/share/qemu}"
	VARS="$ART/vars.fd"
	dd if=/dev/zero of="$VARS" bs=1048576 count=64 2>/dev/null
	dd if=/dev/zero of="$DISK" bs=1048576 count=0 seek=64 2>/dev/null
	FWD="hostfwd=tcp:127.0.0.1:${SYSPORT}-:10100"
	FWD="$FWD,hostfwd=tcp:127.0.0.1:${AGENTPORT}-:8080,hostfwd=tcp:127.0.0.1:${LEADERPORT}-:9080"
	qemu-system-aarch64 -M virt,gic-version=3,virtualization=on \
		-cpu "${CPU:-cortex-a57}" -smp 4 -m 3G \
		-nographic -monitor none -serial stdio \
		-drive "if=pflash,format=raw,readonly=on,file=$QEMU_SHARE/edk2-aarch64-code.fd" \
		-drive "if=pflash,format=raw,file=$VARS" \
		-device qemu-xhci \
		-drive "file=fat:$ESP,format=raw,if=none,id=esp,readonly=on" \
		-device usb-storage,drive=esp,bootindex=0 \
		-device virtio-net-pci,netdev=n0,romfile= \
		-netdev "user,id=n0,$FWD" \
		-drive "if=none,format=raw,file=$DISK,id=disk0" \
		-device virtio-blk-pci,drive=disk0 \
		</dev/null >"$LOG" 2>&1 &
else
	HOPOS_STAMP=A SYSPORT="$SYSPORT" AGENTPORT="$AGENTPORT" LEADERPORT="$LEADERPORT" HOP_DIR="$HOP_DIR" APP=hop DISK="$DISK" \
		sh "$DIR/image/qemu-run.sh" </dev/null >"$LOG" 2>&1 &
fi
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
FLIP_MARKS="HOPOS_FLIP_SWITCHCODE_OK|HOPOS_FLIP_STAGED|HOPOS_FS_FROZEN generation=|HOPOS_FLIP_NAT_CAPTURED flows=|HOPOS_FLIP_JUMP gen=2|HOPOS_BOOT gen=2 stamp=B|HOPOS_FLIP_BOOT gen=2|HOPOS_FLIP_ADOPT 1 of 1 resident\\(s\\)|HOPOS_HOP_RESUMED|HOPOS_FLIP_NAT restored=|HOPOS_FLIP_SETTLED"
WORK_MARKS="slot 1: .*HOP_JOB_PLACED slot=2|HOPOS_SLOT_START slot=2|slot 2: HOPOS_APPSPIKE_DONE pass=9 fail=0"
RED="HOPOS_PANIC|HOPOS_EXCEPTION|HOPOS_HOP_FAULT|HOPOS_HOP_EXIT|HOPOS_HOP_FAIL|HOPOS_FLIP_REFUSED|HOPOS_FLIP_FAIL|HOPOS_FLIP_BLOB_BAD|HOPOS_FLIP_GUARD|HOPOS_FLIP_ADOPT_FAIL|HOPOS_FLIP_NAT_FAIL|HOPOS_FS_FREEZE_FAIL|HOPOS_CAGE_FAIL|HOP_FLIP_FAIL"
if [ -n "$MISMATCH" ]; then
	FLIP_MARKS="HOPOS_FLIP_REFUSED switch code mismatch|slot 1: .*HOP_FLIP_FAIL"
	WORK_MARKS=""
	RED="HOPOS_PANIC|HOPOS_EXCEPTION|HOPOS_HOP_FAULT|HOPOS_HOP_EXIT|HOPOS_HOP_FAIL|HOPOS_FLIP_STAGED|HOPOS_FS_FROZEN|HOPOS_FLIP_JUMP|HOPOS_FLIP_BOOT|HOPOS_FLIP_FAIL|HOPOS_CAGE_FAIL"
fi

JOB='{"name":"spike","driver":"hop","artifacts":[{"url":"http://10.0.2.2:'"$ARTPORT"'/appspike.elf"}],"memory_limit":33554432}'
FLIPREQ='{"url":"http://10.0.2.2:'"$ARTPORT"'/'"$BUNDLE"'","sha256":"'"$SHA"'"}'
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
				# De verbinding over de flip: nu openen, met de eerste
				# helft van een GET /tasks; de rest pas na de landing.
				[ -n "$MISMATCH" ] || python3 "$ART/half.py" "$AGENTPORT" "$ART/half" &
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
			# Eerst de halve verbinding van vóór de sprong afmaken.
			if [ -e "$ART/half.open" ] && [ ! -e "$ART/half.go" ]; then
				: >"$ART/half.go"
				n=0
				while [ ! -e "$ART/half.done" ] && [ "$n" -lt 150 ]; do
					sleep 0.2
					n=$((n + 1))
				done
			fi
			# Dezelfde Hop antwoordt, over de uplink van de nieuwe kern.
			if t="$(curl -s -m 5 -w ' HTTP %{http_code}' "http://127.0.0.1:$AGENTPORT/tasks" 2>&1)"; then
				case "$t" in *"HTTP 200"*) TASKS="$t" ;; esac
			fi
		fi
		step
		continue
	fi
	[ -n "$MISMATCH" ] && break
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
case "$MISMATCH:$FLIPPED" in
1:*"HTTP 5"*) echo "   ok  POST /flip geweigerd: $FLIPPED" ;;
1:*) echo "   ROOD POST /flip had geweigerd moeten worden: ${FLIPPED:-nooit gedaan}"; fail=1 ;;
:*"HTTP 202"*) echo "   ok  POST /flip: $FLIPPED" ;;
:) echo "   ROOD POST /flip nooit gedaan (kern A of Hop niet op tijd op)"; fail=1 ;;
*) echo "   ROOD POST /flip: $FLIPPED"; fail=1 ;;
esac
case "$TASKS" in
*"HTTP 200"*) echo "   ok  GET /tasks na de flip: $TASKS" ;;
*) echo "   ROOD GET /tasks na de flip: ${TASKS:-nooit beantwoord}"; fail=1 ;;
esac
if [ -n "$MISMATCH" ]; then
	# Geweigerd: er is geen kern B, dus ook geen werk, verbinding, hopfs of
	# conntrack om na de flip te toetsen.
	POSTED="n.v.t. HTTP 2"
	: >"$ART/half.out"
	echo " 200 (n.v.t.)" >"$ART/half.out"
fi
case "$POSTED" in
"n.v.t."*) ;;
*"HTTP 2"*) echo "   ok  POST /v1/jobs op kern B: $POSTED" ;;
*) echo "   ROOD POST /v1/jobs op kern B: ${POSTED:-nooit gedaan}"; fail=1 ;;
esac
HALF="$(cat "$ART/half.out" 2>/dev/null || true)"
case "$HALF" in
*"n.v.t."*) ;;
*" 200"*) echo "   ok  een verbinding over de flip: geopend op kern A, beantwoord door kern B: $HALF" ;;
*) echo "   ROOD een verbinding over de flip: ${HALF:-geen antwoord}"; fail=1 ;;
esac
# hopfs: de generatie die kern A vastlegde, is die welke kern B mount.
FROZEN="$(tr -d '\r' <"$LOG" | sed -n 's/.*HOPOS_FS_FROZEN generation=\([0-9]*\).*/\1/p' | head -1)"
MOUNTED="$(tr -d '\r' <"$LOG" | awk '/HOPOS_FLIP_BOOT/ { f = 1 } f' | sed -n 's/.*HOPOS_FS_UP fresh=0 generation=\([0-9]*\).*/\1/p' | head -1)"
if [ -n "$MISMATCH" ]; then
	:
elif [ -n "$FROZEN" ] && [ "$FROZEN" = "$MOUNTED" ]; then
	echo "   ok  hopfs: generatie $FROZEN vastgelegd en bevroren door kern A, gemount door kern B (fresh=0)"
else
	echo "   ROOD hopfs: kern A bevroor generatie '${FROZEN:-?}', kern B mountte '${MOUNTED:-geen fresh=0}'"
	fail=1
fi
# De conntrack: gevangen is hersteld, en minstens de download van de bundel.
CAUGHT="$(tr -d '\r' <"$LOG" | sed -n 's/.*HOPOS_FLIP_NAT_CAPTURED flows=\([0-9]*\).*/\1/p' | head -1)"
BACK="$(tr -d '\r' <"$LOG" | sed -n 's/.*HOPOS_FLIP_NAT restored=\([0-9]*\) of=\([0-9]*\).*/\1 \2/p' | head -1)"
if [ -n "$MISMATCH" ]; then
	:
elif [ -n "$CAUGHT" ] && [ "$CAUGHT" -ge 1 ] && [ "$BACK" = "$CAUGHT $CAUGHT" ]; then
	echo "   ok  conntrack: $CAUGHT flow(s) gevangen door kern A, alle $CAUGHT hersteld door kern B"
else
	echo "   ROOD conntrack: gevangen '${CAUGHT:-?}', hersteld/van '${BACK:-?}'"
	fail=1
fi
boots="$(count 'slot 1: .*HOP_BOOT')"
starts="$(count 'HOPOS_HOP_START slot=1')"
if [ "$boots" = 1 ] && [ "$starts" = 1 ]; then
	echo "   ok  dezelfde Hop: 1x HOP_BOOT en 1x HOPOS_HOP_START over twee kernen"
else
	echo "   ROOD Hop herstart? HOP_BOOT ${boots}x, HOPOS_HOP_START ${starts}x"
	fail=1
fi
if grep -q "GET /$BUNDLE" "$ART/http.log" 2>/dev/null; then
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
echo "qemu-flip groen ($BOARD)"
