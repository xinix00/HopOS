#!/bin/sh
# De poort van HopOS v3 (handboek §9): host-tests, clippy met de harde set,
# rustfmt, en de target-builds. Rood is rood.
set -e
cd "$(dirname "$0")/.."
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
echo "poort groen"
