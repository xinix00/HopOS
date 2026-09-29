# HopOS v3: de docs

Wat er is, hoe je het bouwt, en wat er per board te bewijzen valt. De
checklists per board zijn de lat voor een dag op het ijzer; elke stap noemt
de consoleregel die erbij hoort en wat een afwijking betekent.

## Bewezen op QEMU (30-09-2026)

| Test | Wat hij bewijst |
| --- | --- |
| `sh tools/qemu-test.sh` | boot op virt, netwerk, opslag, appspike in slot 1 en 2 in een stage-2-kooi, negen toetsen |
| `sh tools/qemu-test-hop.sh` | de kern start Hop met het token op de OS-core; een `POST /v1/jobs` van de host laat Hop appspike plaatsen; Hop bewaart zijn staat en leest hem na een herstart terug |
| `SMP=2 sh tools/qemu-test-hop.sh` | hetzelfde met twee cores: kern plus Hop op één core, de app op de andere |
| `SMP=2 OSCORE=1 sh tools/qemu-test-hop.sh` | de kern verhuist bij boot naar core 1 |
| `sh tools/qemu-test-welcome.sh` | Hop plaatst welcome met `"ports":{"http":80}`; de kern zet poort 80 door naar het slot, `curl` van de host krijgt de pagina met de bunny en `/health`, en na `DELETE /v1/jobs/welcome` is de poort weer dicht |
| `sh tools/qemu-test-flip.sh` | de kern-flip: Hop overleeft de wissel van de kern zonder herstart |
| `sh tools/qemu-uefi-test.sh` | de EFI-stub op EDK2, ACPI, PCIe, virtio over PCI, de hele appspike-keten |
| `sh tools/qemu-rpi4-test.sh` | het Pi 4-board op QEMU's raspi4b tot de executor-tik |

`sh tools/gate.sh` is de poort vóór elke commit: host-tests, clippy met
`-D warnings`, rustfmt, en de target-builds van alle boards.

## Images per board

| Board | Bouwen | Uitvoer | Checklist |
| --- | --- | --- | --- |
| QEMU virt | `sh image/qemu-run.sh` | draait meteen | de tests hierboven |
| UEFI generiek (EDK2, QEMU) | `BOARD=uefi sh image/uefi-run.sh` | `target/uefi-esp-uefi/` | [boards.md](boards.md) |
| Orion O6N | `BOARD=o6n sh image/uefi-run.sh` | `target/uefi-esp-o6n/` (naar een FAT32-stick, met `hopos.cfg` naast `EFI/`) | [boards.md](boards.md) |
| Ampere Altra | `BOARD=altra sh image/uefi-run.sh` | `target/uefi-esp-altra/` | [boards.md](boards.md) |
| Raspberry Pi 4 | `sh image/rpi4.sh` | `target/hopos-rpi4.img` (dd) | [boards-pi.md](boards-pi.md) |
| Raspberry Pi 5 | `sh image/rpi5.sh` | `target/hopos-rpi5.img` (dd) | [boards-pi.md](boards-pi.md) |
| Radxa Zero 3E | `sh image/radxa-zero3.sh` | `target/radxa-zero3/hopos-radxa-zero3.img` (dd) | [boards-radxa.md](boards-radxa.md) |
| Kern-flip-bundel | `sh image/flip-bundle.sh virt` | `target/hopos-virt.flip` plus `.sha256` | `tools/qemu-test-flip.sh` |

Hop zelf komt uit de hop-repo (`agentd-hopos`); de image-scripts bouwen
hem via `HOP_DIR` en bakken hem in als bewoner. Secure Boot moet uit op de
UEFI-boards.

Een devicedag eindigt met een pagina in de browser: bouw
[`apps/welcome`](../apps/welcome/README.md), zet `welcome.elf` op een
HTTP-server op de laptop, en `curl -X POST -d '{"name":"welcome","driver":"hop","artifacts":[{"url":"http://LAPTOP:8000/welcome.elf"}],"memory_limit":33554432,"ports":{"http":80}}' http://NODE:9080/v1/jobs`
(met `hopos.insecure=1`); daarna toont `http://NODE/` de bunny, de node, het
slot, de uptime, de verzoeken en de heap.

## Nog niet, en op ijzer nog nooit gedraaid

- De kick van de OS-core op de GIC-400 van de Pi's (in uitvoering).
- Interrupts over PCI op UEFI: INTx vraagt de `_PRT` uit AML, MSI-X de ITS;
  NIC en schijf pollen elke 300 µs.
- De O6N-console na de exit: de SPCR wijst naar een UART die de SCP dicht
  houdt; Go gebruikte een vroege UART op 0x040d0000.
- VHE op de A720 van de O6N: op QEMU met het Neoverse-model bewezen, op
  silicium niet.
- NVMe op de Pi 5, een SD-driver op de Radxa, de framebuffer-consoles.
- De flip op andere boards dan virt (de adressen in `hopos/src/flip.rs`).
- SMP-apps, de IPv6-baan van leannet, LicheeRV (RISC-V) en de Mac mini.

Hoe de code geschreven is: het Rust-handboek van haas.software
(`rustdoc/README.md`) en de port-notities (`rustdoc/PORT.md`).
