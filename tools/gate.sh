#!/bin/sh
# De poort van HopOS v3 (handboek §9): host-tests, clippy met de harde set,
# rustfmt, de host-meetbank netmeter, de target-builds van de apps
# (appspike, welcome, bench) en van ELK board. Rood is rood.
#
# De boards: virt, rpi4, rpi5, rk3566 en de twee riscv64-boards (QEMU virt
# in machine mode en de LicheeRV, docs/boards-riscv.md) als debug-build in de gedeelde
# target-map (compileert en linkt tegen hun linkscript), en de drie
# UEFI-boards (QEMU/EDK2, O6N, Altra) via image/uefi-run.sh: een release-build
# als PIE in target/uefi plus de relocatie-toets en de PE-verpakking van dat
# script. Dat laatste is het deel dat op een stick stuk kan gaan zonder dat
# `cargo build` het ziet, dus de gate loopt het echte bouwpad.
#
# Het gui-vlak (docs/gui.md) bouwt voor elk board dat een framebuffer kan
# leveren ook met `--features gui`; de console op het glas bewijst
# `GUI=1 sh tools/qemu-test.sh` (ramfb en een screendump).
#
# Het media-vlak (docs/media.md): de codec-dienst van de kern, de client
# van applib en het aanzetten van de VPU bestaan alleen met hun feature
# `media`; hun tests en clippy krijgen een eigen ronde, en hopos bouwt met
# media voor virt (de dienst zonder VPU) en de O6N (de VPU, als PIE zoals
# uefi-run.sh, in een eigen target-map).
#
# De duur staat onderaan, per stap: de grens is tien minuten.
set -e
cd "$(dirname "$0")/.."
T0=$(date +%s)
echo "== host: cargo test"
cargo test --quiet
echo "== host: cargo clippy"
cargo clippy --all-targets --quiet -- -D warnings
echo "== host: netmeter (test en clippy)"
# De meetbank op de host is std en dus geen default-member (de target-build
# van de bibliotheken slaat hem over); hier zijn eigen ronde.
cargo test --quiet -p netmeter
cargo clippy --all-targets --quiet -p netmeter -- -D warnings
echo "== host: de gui-smaak van de boards (test en clippy)"
# De framebuffer-code van de boards (vcfb, gop, ramfb, de rk3566-keten)
# bestaat alleen met hun feature `gui`; de werkruimte-ronde hierboven ziet
# hem dus niet (docs/gui.md).
GUI_BOARDS="-p board-qemuvirt -p board-raspi -p board-uefi -p board-rk3566"
GUI_FEATS="board-qemuvirt/gui,board-raspi/gui,board-uefi/gui,board-rk3566/gui"
cargo test --quiet $GUI_BOARDS --features "$GUI_FEATS"
cargo clippy --all-targets --quiet $GUI_BOARDS --features "$GUI_FEATS" -- -D warnings
echo "== host: de VHE-vorm van het UEFI-board (test en clippy)"
# De kern onder E2H = 1 (board/uefi/src/el2.rs) bestaat alleen met de
# feature `vhe` van board-uefi; de werkruimte-ronde ziet de nVHE-vorm. De
# target-build van de VHE-vorm is de O6N hieronder (board-o6n zet hem altijd
# aan); de proef op QEMU is `FEATURES=vhe CPU=neoverse-n1 sh
# tools/qemu-uefi-test.sh`.
cargo test --quiet -p board-uefi --features vhe
cargo clippy --all-targets --quiet -p board-uefi --features vhe -- -D warnings
echo "== host: het media-vlak (test en clippy)"
MEDIA_PKGS="-p kern -p applib -p board-o6n"
MEDIA_FEATS="kern/media,applib/media,board-o6n/media"
cargo test --quiet $MEDIA_PKGS --features "$MEDIA_FEATS"
cargo clippy --all-targets --quiet $MEDIA_PKGS --features "$MEDIA_FEATS" -- -D warnings
echo "== rustfmt"
cargo fmt --check
echo "== target: bibliotheken (aarch64)"
cargo build --quiet --target aarch64-unknown-none-softfloat
echo "== target: apps (appspike, welcome, bench)"
cargo build --quiet --target aarch64-unknown-none-softfloat -p appspike -p welcome -p bench
echo "== target: hopos (qemuvirt)"
cargo build --quiet --target aarch64-unknown-none-softfloat -p hopos --features board-qemuvirt
echo "== target: hopos (rpi4, rpi5)"
cargo build --quiet --target aarch64-unknown-none-softfloat -p hopos --features board-rpi4
cargo build --quiet --target aarch64-unknown-none-softfloat -p hopos --features board-rpi5
echo "== target: hopos (rk3566)"
cargo build --quiet --target aarch64-unknown-none-softfloat -p hopos --features board-rk3566
echo "== target: hopos (apple: de Mac mini M4 onder m1n1, docs/boards-apple.md)"
cargo build --quiet --target aarch64-unknown-none-softfloat -p hopos --features board-apple
echo "== target: hopos (riscv64: qemuvirt-riscv, licheerv)"
cargo build --quiet --target riscv64gc-unknown-none-elf -p hopos --features board-qemuvirt-riscv
cargo build --quiet --target riscv64gc-unknown-none-elf -p hopos --features board-licheerv
echo "== target: hopos met gui (qemuvirt, rpi4, rpi5, rk3566)"
for b in qemuvirt rpi4 rpi5 rk3566; do
	cargo build --quiet --target aarch64-unknown-none-softfloat -p hopos --features "board-$b,gui"
done
echo "== target: hopos met media (qemuvirt, o6n)"
cargo build --quiet --target aarch64-unknown-none-softfloat -p hopos --features board-qemuvirt,media
RUSTFLAGS="-C relocation-model=pie" cargo build --quiet --release --target aarch64-unknown-none-softfloat \
	-p hopos --features board-o6n,media --target-dir target/uefi-media
echo "== image: UEFI met gui (uefi, o6n, altra)"
GUI=1 BUILD_ONLY=1 BOARD=uefi sh image/uefi-run.sh
GUI=1 BOARD=o6n sh image/uefi-run.sh
GUI=1 BOARD=altra sh image/uefi-run.sh
echo "== image: UEFI (uefi, o6n, altra), kaal: de laatste ESP is de kale"
BUILD_ONLY=1 BOARD=uefi sh image/uefi-run.sh
BOARD=o6n sh image/uefi-run.sh
BOARD=altra sh image/uefi-run.sh
echo "poort groen in $(($(date +%s) - T0)) s"
