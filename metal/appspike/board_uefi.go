//go:build uefi

package main

// UEFI/ACPI-variant van de app-image (Ampere Altra; image/uefi-agent.sh,
// -tags uefi): zelfde app, zelfde canonieke link (slot-1-IPA), alleen de
// runtime-hooks komen van het uefi-board. Op een app-core onder stage-2 is
// er geen SystemTable/ACPI (de globals zijn leeg in een vers image): de
// console blijft dan stil (hop-ABI-ringen zijn het kanaal) en CoreID valt
// terug op MPIDR — de Altra-nummering wordt tegen de MADT-dump geijkt.
import _ "hop-os/metal/board/uefi"
