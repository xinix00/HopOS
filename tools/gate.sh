#!/bin/sh
# De poort van HopOS v3 (handboek §9): host-tests, clippy met de harde set,
# rustfmt, en de target-builds van ELK board. Rood is rood.
#
# De boards: virt, rpi4, rpi5 en rk3566 als debug-build in de gedeelde
# target-map (compileert en linkt tegen hun linkscript), en de drie
# UEFI-boards (QEMU/EDK2, O6N, Altra) via image/uefi-run.sh: een release-build
# als PIE in target/uefi plus de relocatie-toets en de PE-verpakking van dat
# script. Dat laatste is het deel dat op een stick stuk kan gaan zonder dat
# `cargo build` het ziet, dus de gate loopt het echte bouwpad.
#
# De duur staat onderaan, per stap: de grens is tien minuten.
set -e
cd "$(dirname "$0")/.."
T0=$(date +%s)
echo "== host: cargo test"
cargo test --quiet
echo "== host: cargo clippy"
cargo clippy --all-targets --quiet -- -D warnings
echo "== rustfmt"
cargo fmt --check
echo "== target: bibliotheken (aarch64)"
cargo build --quiet --target aarch64-unknown-none-softfloat
echo "== target: hopos (qemuvirt)"
cargo build --quiet --target aarch64-unknown-none-softfloat -p hopos --features board-qemuvirt
echo "== target: hopos (rpi4, rpi5)"
cargo build --quiet --target aarch64-unknown-none-softfloat -p hopos --features board-rpi4
cargo build --quiet --target aarch64-unknown-none-softfloat -p hopos --features board-rpi5
echo "== target: hopos (rk3566)"
cargo build --quiet --target aarch64-unknown-none-softfloat -p hopos --features board-rk3566
echo "== image: UEFI (uefi, o6n, altra)"
BUILD_ONLY=1 BOARD=uefi sh image/uefi-run.sh
BOARD=o6n sh image/uefi-run.sh
BOARD=altra sh image/uefi-run.sh
echo "poort groen in $(($(date +%s) - T0)) s"
