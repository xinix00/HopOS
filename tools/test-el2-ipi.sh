#!/bin/sh
# Compile the actual switch in all three modes: VHE is not an Apple IPI capability.
set -eu
DIR="$(cd "$(dirname "$0")/.." && pwd)"
TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
GOROOT="$($TAMAGO env GOROOT)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM
for mode in nvhe vhe apple; do
	set --
	[ "$mode" = nvhe ] || set -- -D VHE
	[ "$mode" != apple ] || set -- "$@" -D APPLE_IPI
	GOARCH=arm64 "$TAMAGO" tool asm -I "$GOROOT/pkg/include" "$@" -o "$TMP/$mode.o" "$DIR/metal/cpu/el2/switch.s"
	"$TAMAGO" tool objdump "$TMP/$mode.o" > "$TMP/$mode.txt"
	if [ "$mode" = apple ]; then
		for opcode in d51df025 d53df120 d51df120 d503207f; do
			rg -q "$opcode" "$TMP/$mode.txt" || { echo "Apple switch missing $opcode" >&2; exit 1; }
		done
	else
		if rg -q 'd51df025|d53df120|d51df120|d503207f' "$TMP/$mode.txt"; then
			echo "$mode switch contains Apple IPI/WFI path" >&2; exit 1
		fi
		rg -q d503205f "$TMP/$mode.txt" # generic WFE sleep
	fi
done
echo 'EL2 IPI separation: nVHE, generic VHE and Apple passed'
