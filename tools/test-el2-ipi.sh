#!/bin/sh
# Assemble the real EL2 code in every board variant: the switcher's sleep and
# wake path is chosen by the board's build tag (cpu/el2/el2_*.s), and this
# checks that each variant carries exactly its own mechanism.
set -eu
DIR="$(cd "$(dirname "$0")/.." && pwd)"
TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
GOROOT="$($TAMAGO env GOROOT)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM
for v in nvhe o6n apple; do
	GOARCH=arm64 "$TAMAGO" tool asm -I "$GOROOT/pkg/include" -I "$DIR/metal/cpu/el2" -o "$TMP/$v.o" "$DIR/metal/cpu/el2/el2_$v.s"
	"$TAMAGO" tool objdump "$TMP/$v.o" > "$TMP/$v.txt"
	case "$v" in
	apple)
		for opcode in d51df025 d53df120 d51df120 d503207f; do
			rg -q "$opcode" "$TMP/$v.txt" || { echo "apple switch missing $opcode" >&2; exit 1; }
		done ;;
	*)
		if rg -q 'd51df025|d53df120|d51df120|d503207f|d518cba4' "$TMP/$v.txt"; then echo "$v switch contains an IPI path" >&2; exit 1; fi
		rg -q d503205f "$TMP/$v.txt" ;; # generic WFE sleep
	esac
done
echo 'EL2 variants: nvhe, o6n (VHE) and apple passed'
