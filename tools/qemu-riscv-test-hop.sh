#!/bin/sh
# De kring van HopOS v3 op riscv64 (QEMU virt, machine mode, twee harts): de
# kern start Hop op de OS-core, en Hop plaatst een app op het app-hart. De
# riscv-vorm van tools/qemu-test-hop.sh, met dezelfde toetsen (en daar
# beschreven), behalve:
#
#   kern        HOPOS_OS_SELFTEST ok is hier de overgang van de kern-hart
#               naar een bewoner en terug (de wekker, de yield, de kick en de
#               exit, cpu/src/riscv/oscore.rs), en HOPOS_OS_CORE_UP erbij;
#               HOPOS_HOP_START slot=1 core=0 cpu=0 (Hop deelt hart 0 met de
#               kern, PORT.md beslissing 2);
#   de plaatsing HOPOS_SLOT_START slot=2 core=1 cpu=1 (het app-hart, onder de
#               M-mode-switcher);
#   geen zaad   en geen canary-markers; rood is ook HOPOS_OS_CORE_NONE.
#
# Hop zelf: agentd-hopos uit de hop-repo, gebouwd door tools/hop-build.sh
# voor riscv64gc-unknown-none-elf, tegen de applib, abi en sync van deze
# werkboom (HOP_DIR, HOP_REV en HOP_PATCH: zie daar).
#
#   tools/qemu-riscv-test-hop.sh           TIMEOUT=90 standaard, in seconden
#   KEEP_LOG, de poorten en HOP_DIR        zoals tools/qemu-test-hop.sh
exec env HOP_ARCH=riscv64 sh "$(dirname "$0")/qemu-test-hop.sh"
