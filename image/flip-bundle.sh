#!/bin/sh
# Bouwt de KERN-FLIP-BUNDEL van één board (docs/kern-flip.md): het artifact
# waarmee een draaiende HopOS zichzelf vervangt zónder de node te herstarten.
#
#   image/flip-bundle.sh rpi5          → metal/out/hopos-rpi5.flip
#   image/flip-bundle.sh licheerv      → metal/out/hopos-licheerv.flip
#   image/flip-bundle.sh apple         → metal/out/hopos-apple.flip
#
# Een bundel is de kern-ELF zoals de linker hem maakte, plus een
# HOPRELO1-staart met de relocatietabel. Die tabel komt uit een DIFF: dezelfde
# build op twee linkadressen, waarbij elk verschillend 8-byte-woord exact de
# linkbasis-delta moet dragen (mkkernel -elfreloc faalt hard als dat niet zo
# is). Daarom bouwt dit script elk board twee keer.
#
# Twee build-eisen, allebei niet-onderhandelbaar:
#   -w ZONDER -s   de symboltabel moet mee: de flip patcht RamStart/RamSize en
#                  vergelijkt de switch-code-symbolen van beide kernen;
#   -buildid=      anders verschillen de varianten in méér dan hun linkbasis en
#                  weigert de diff terecht.
#
# De node krijgt hem op AANVRAAG, via de agent-API (de enige trigger, achter
# dezelfde HMAC als job-dispatch): POST /flip {"url","sha256"} — de som die
# dit script hieronder print. Die som is het
# vertrouwensanker: hij staat op het bootmedium dat jij schrijft, dus een
# webserver of mirror die iets anders serveert komt er niet doorheen. Geen
# handtekening: de sleutel zou in dezelfde repo wonen als de release, en dekt
# dan geen aanval die deze som niet al dekt.
set -e

TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"
BOARD="${1:-}"

# Per board: GOARCH · build-tags · primair linkadres · schaduwadres · extra.
# Het schaduwadres doet niets in het eindproduct — het is puur diff-bewijs —
# maar moet wél een adres zijn waar de kern zou kúnnen liggen (zelfde vorm,
# ver genoeg weg om een echte delta te geven).
case "$BOARD" in
rpi5)      ARCH=arm64;   TAGS="rpi5 linkcpuinit";     T1=0x90000;        T2=0x10090000 ;;
rpi4)      ARCH=arm64;   TAGS="rpi4 linkcpuinit";     T1=0x90000;        T2=0x10090000 ;;
radxa)     ARCH=arm64;   TAGS="rk3566 linkcpuinit";   T1=0x02210000;     T2=0x12210000 ;;
virt)      ARCH=arm64;   TAGS="linkcpuinit";          T1=0x40010000;     T2=0x70010000 ;;
uefi)      ARCH=arm64;   TAGS="uefi linkcpuinit";     T1=0x50010000;     T2=0x88010000 ;;
apple)     ARCH=arm64;   TAGS="apple linkcpuinit highram"
           T1=0x10100010000; T2=0x10180010000
           ASM="-asmflags all=-D=VHE"; WORK="$DIR/image/apple/go.work" ;;
licheerv)  ARCH=riscv64; TAGS="licheerv linkcpuinit"; T1=0x84010000;     T2=0x88010000 ;;
*)
	echo "gebruik: $0 [rpi5|rpi4|radxa|virt|uefi|apple|licheerv]" >&2
	exit 64
	;;
esac

cd "$DIR/metal"
mkdir -p out
OUT="out/hopos-$BOARD.flip"

# CFG=<pad>: de platform-config MEE IN DE BUNDEL (zelfde mechaniek en om
# dezelfde reden als in apple-m4.sh) — een board zonder loader léést zijn
# config uit het image, en een geflipte kern is zo'n image. Zonder CFG zou de
# nieuwe kern zijn naam, API-key en console-knop kwijt zijn en headless
# booten. Geef hier dus DEZELFDE config als die van het geïnstalleerde image.
if [ -n "${CFG:-}" ]; then
	[ -f "$DIR/$CFG" ] || { echo "CFG=$CFG bestaat niet (pad vanaf de repo-wortel)" >&2; exit 1; }
	cp "$DIR/$CFG" "$DIR/metal/cmd/hopos/cfgblob/hopos.cfg"
	TAGS="$TAGS embedcfg"
	echo "config ingebakken: $CFG ($(wc -c <"$DIR/$CFG" | tr -d ' ') bytes)" >&2
fi
# Zonder CFG boot de geflipte kern zonder naam, API-key en console-knop en
# parkeert hij vóór de agent op HOPOS_API_NO_AUTH: ping doet het, de
# system-poort staat open, en verder is er niets te lezen — 03-09 twee keer een
# "hang" gejaagd die precies dit was. Voor een board dat zijn config uit het
# image leest is dat nooit de bedoeling; wie het écht wil, zegt NOCFG=1.
case "$BOARD" in
apple)
	if [ -z "${CFG:-}" ] && [ "${NOCFG:-}" != 1 ]; then
		echo "$BOARD: geen CFG= opgegeven — de geflipte kern zou zonder config booten en parkeren (HOPOS_API_NO_AUTH)." >&2
		echo "    geef CFG=image/apple/hopos-m4.cfg (dezelfde config als het geïnstalleerde image), of NOCFG=1 als dat bewust is." >&2
		exit 1
	fi
	;;
esac

for V in "1:$T1" "2:$T2"; do
	GOWORK="${WORK:-off}" GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH="$ARCH" \
		"$TAMAGO" build -tags "$TAGS" -trimpath $ASM \
		-ldflags "-w -buildid= -T ${V#*:} -R 0x1000" -o "out/flip-v${V%%:*}.elf" ./cmd/hopos
done

GO111MODULE=off go run "$DIR"/image/mkkernel/*.go -elfreloc -o "$OUT" \
	-elf out/flip-v1.elf -elf out/flip-v2.elf
rm -f out/flip-v1.elf out/flip-v2.elf

SHA="$(shasum -a 256 "$OUT" | awk '{print $1}')"
echo "" >&2
echo "$OUT gebouwd. Zet hem op een webserver en vraag de flip aan op de agent-API:" >&2
echo "  curl -X POST http://<node>:8080/flip -d '{\"url\":\"<url naar $OUT>\",\"sha256\":\"$SHA\"}'" >&2
