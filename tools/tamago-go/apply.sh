#!/bin/sh
# Legt onze patches op de tamago-go-fork (README.md hiernaast). Eerst een
# droge controle van álle patches, dan pas toepassen — een half toegepaste
# toolchain is erger dan geen. Al toegepast = overslaan, niet falen.
set -e
DIR="$(cd "$(dirname "$0")" && pwd)"
FORK="${TAMAGO_SRC:-$HOME/tamago-go}"
[ -d "$FORK/.git" ] || { echo "geen tamago-go-clone op $FORK (zet TAMAGO_SRC)" >&2; exit 1; }
cd "$FORK"
for p in "$DIR"/*.patch; do
	if git apply --check --reverse "$p" >/dev/null 2>&1; then
		echo "al toegepast: $(basename "$p")"
		continue
	fi
	git apply --check "$p" || { echo "past niet: $(basename "$p")" >&2; exit 1; }
	git apply "$p"
	echo "toegepast:    $(basename "$p")"
done
