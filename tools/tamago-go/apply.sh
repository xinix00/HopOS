#!/bin/sh
# Preflight the complete pending set before changing the checkout.
set -eu
DIR="$(cd "$(dirname "$0")" && pwd)"
FORK="${TAMAGO_SRC:-$HOME/tamago-go}"
git -C "$FORK" rev-parse --git-dir >/dev/null 2>&1 || {
    echo "No tamago-go checkout at $FORK (set TAMAGO_SRC)" >&2
    exit 1
}
cd "$FORK"
set --
for patch in "$DIR"/*.patch; do
    if git apply --check --reverse "$patch" >/dev/null 2>&1; then
        echo "Already applied: $(basename "$patch")"
    else
        set -- "$@" "$patch"
    fi
done
[ "$#" -gt 0 ] || exit 0
git apply --check "$@"
git apply "$@"
for patch do
    echo "Applied: $(basename "$patch")"
done
