#!/bin/sh
# De kern-flip onder EDK2: kern A boot als BOOTAA64.EFI met Hop (rol hop,
# hopos-stage.elf op de ESP), Hop haalt de UEFI-bundel van kern B
# (image/flip-bundle.sh uefi) en de kern springt, zonder firmware, naar B
# op de basis die de firmware voor A koos. Dezelfde toets en dezelfde
# markers als de flip op virt (tools/qemu-test-flip.sh), plus wat alleen
# hier kan misgaan: de feitenpagina van de stub (board/uefi/src/flip.rs)
# en de PIE-basis (docs/flip.md).
#
#   tools/qemu-uefi-flip-test.sh          TIMEOUT=150 standaard, in seconden
#   KEEP_LOG=pad tools/qemu-uefi-flip-test.sh   bewaart ook een groene console
set -eu
DIR="$(cd "$(dirname "$0")/.." && pwd)"
BOARD=uefi exec sh "$DIR/tools/qemu-test-flip.sh" "$@"
