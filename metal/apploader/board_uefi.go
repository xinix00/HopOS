//go:build uefi

package main

// UEFI/ACPI-variant (Ampere Altra, QEMU virt + EDK2): de board-import levert de
// tamago runtime-hooks. Zelfde loader, zelfde canonieke link — alleen de hooks
// verschillen per board.
import _ "hop-os/metal/board/uefi"
