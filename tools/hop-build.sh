#!/bin/sh
# Bouwt agentd-hopos (Hop, de bewoner uit de hop-repo) voor een target en
# zegt op stdout waar de ELF staat. De bouwuitvoer gaat naar stderr.
#
#   HOP_ELF="$(sh tools/hop-build.sh aarch64-unknown-none-softfloat)"
#
# Waarom tegen DEZE werkboom (HOP_PATCH=1, de standaard): de hop-repo pint
# applib, abi en sync op een tag, en wat Hop op ijzer laat leven zit in
# applib. Les van 30-09, de eerste Pi 5-boot: de fix voor de alignment-val
# (applib::mmu, elke app een stage-1) en de vectortabel die een fault op
# EL1 meldt, bereiken Hop alleen als hij tegen deze applib bouwt. Dus: een
# kopie van de hop-repo (`git archive`, HOP_REV, standaard HEAD) in
# target/hop-patched-<target>/src, met een `[patch]` in de
# .cargo/config.toml van die kopie, zoals tools/qemu-riscv-test-hop.sh het
# al deed. De hop-repo zelf wordt niet aangeraakt (ook zijn Cargo.lock en
# target/ niet). HOP_PATCH=0 bouwt in $HOP_DIR zelf, zonder patch, voor een
# hop-repo die al een tag met deze applib pint.
#
#   HOP_DIR=pad    de hop-repo (standaard ../hop/hop)
#   HOP_REV=rev    wat er uit de hop-repo gekopieerd wordt (standaard HEAD)
#   HOP_PATCH=0|1  tegen deze werkboom (1) of de tag van de hop-repo (0)
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TARGET="${1:-aarch64-unknown-none-softfloat}"
HOP_DIR="${HOP_DIR:-$DIR/../hop/hop}"
HOP_REV="${HOP_REV:-HEAD}"
HOP_PATCH="${HOP_PATCH:-1}"

[ -d "$HOP_DIR" ] || {
	echo "hop-build: de hop-repo ontbreekt: $HOP_DIR (zet HOP_DIR)" >&2
	exit 1
}
if [ "$HOP_PATCH" != 1 ]; then
	(cd "$HOP_DIR" && cargo build --quiet --release --target "$TARGET" -p agentd-hopos) >&2
	echo "$HOP_DIR/target/$TARGET/release/agentd-hopos"
	exit 0
fi

WORK="$DIR/target/hop-patched-$TARGET"
SRC="$WORK/src"
rm -rf "$SRC"
mkdir -p "$SRC/.cargo"
git -C "$HOP_DIR" archive "$HOP_REV" | tar -x -C "$SRC"
# applib en abi uit deze werkboom, en sync erbij: Hop gebruikt sync ook
# zelf, en twee sync's in één image (één van de tag, één van hier) is een
# stille dubbele `Local`. De rest van applib's afhankelijkheden (dev,
# executor, bounded, netdev) volgt applib vanzelf over zijn paden.
cat >"$SRC/.cargo/config.toml" <<EOF
# Gezet door tools/hop-build.sh: Hop tegen de applib, abi en sync van
# $DIR.
[patch."https://github.com/xinix00/HopOS.git"]
applib = { path = "$DIR/applib" }
abi = { path = "$DIR/abi" }
sync = { path = "$DIR/sync" }
EOF
echo "hop-build: agentd-hopos from $HOP_DIR ($HOP_REV $(git -C "$HOP_DIR" rev-parse --short "$HOP_REV")) against applib of $DIR" >&2
(cd "$SRC" && cargo build --quiet --release --target "$TARGET" --target-dir "$WORK/target" -p agentd-hopos) >&2
echo "$WORK/target/$TARGET/release/agentd-hopos"
