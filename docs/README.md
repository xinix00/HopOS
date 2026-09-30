# HopOS v3: de docs

Wat er is, hoe je het bouwt, en wat er per board te bewijzen valt. De
checklists per board zijn de lat voor een dag op het ijzer; elke stap noemt
de consoleregel die erbij hoort en wat een afwijking betekent.

## Bewezen op QEMU (30-09-2026)

| Test | Wat hij bewijst |
| --- | --- |
| `sh tools/qemu-test.sh` | boot op virt, netwerk, opslag, appspike in slot 1 en 2 in een stage-2-kooi, tien toetsen |
| `GUI=1 sh tools/qemu-test.sh` | hetzelfde met de console op een `ramfb`: de bunny, mem, datum en tijd op het glas, geschoten via de QEMU-monitor |
| `GUI=display sh tools/qemu-test.sh` | de hele gui-keten: qemu-xhci met toetsenbord en muis (`HOPOS_USB_UP`), de display-app krijgt het glas in zijn kooi (`HOPOS_FB_GRANT`, `HOPOS_DISPLAY_UP`), verbindt met de input-listener (`HOPOS_INPUT_CONN`), een `sendkey` en een `mouse_move` komen aan (`HOPOS_DISPLAY_INPUT keys=1 moves=1`), de screendump toont de app, en na de stop komt de console terug (`HOPOS_FB_RELEASE`) |
| `sh tools/qemu-test-hop.sh` | de kern start Hop met het token op de OS-core; een `POST /v1/jobs` van de host laat Hop appspike plaatsen; Hop bewaart zijn staat en leest hem na een herstart terug |
| `SMP=2 sh tools/qemu-test-hop.sh` | hetzelfde met twee cores: kern plus Hop op één core, de app op de andere |
| `SMP=2 OSCORE=1 sh tools/qemu-test-hop.sh` | de kern verhuist bij boot naar core 1 |
| `sh tools/qemu-test-welcome.sh` | Hop plaatst welcome met `"ports":{"http":80}`; de kern zet poort 80 door naar het slot, `curl` van de host krijgt de pagina met de bunny en `/health`, en na `DELETE /v1/jobs/welcome` is de poort weer dicht |
| `sh tools/qemu-test-smp.sh` | een SMP-app: appspike met `cores: 2` krijgt zijn tweede core in de eigen kooi, telt erop, en houdt beide cores na een herstart door Hop |
| `sh tools/qemu-test-share.sh` | een sharegroep: twee appspikes delen één app-core in coöperatieve rotatie, allebei groen, en een lid dat stopt komt terug naast het levende lid |
| `sh tools/qemu-test-bench.sh` | de meetketen: Hop plaatst `apps/bench` met poort 80, `tools/netmeter` meet van de host (rtt, storm, in, out), twee apps meten elkaar door de switch, BURN draait, `hopos.idlestat=1` drukt de meetlat, en een tweede boot doet `hopos.nvmebench=1`; de getallen staan in [measurements.md](measurements.md) |
| `sh tools/qemu-soak-hop.sh 30` | de soak van de hoplb-kring (Hop, welcome, hoplb met de hairpin door de switch, plus een poller op Hop): N runs achter elkaar, een waakhond op `HOPOS_TICK` die bij een tik die meer dan 3 s uitblijft `info registers -a` en de meetlat van de OS-core uit de monitor dumpt; ook met `SMP=2`, `SMP=8` en `OSCORE=1` |
| `sh tools/qemu-test-mcast.sh` | multicast in de node: een bench joint 224.0.0.251, een tweede zendt, de switch floodt, `HOPOS_BENCH_MCAST recv=3` |
| `sh tools/qemu-test-volumes.sh` | de volumes van een jobspec: een job met `"volumes":{"/data":"/volumes/demo"}` schrijft, hopfs commit, Hop herstart hem, en hij vindt zijn bestand terug (`HOPOS_SLOT_MOUNTS`) |
| `sh tools/qemu-test-store.sh` | de store-ops: appspike doet push, list, pull en drop op zijn map `apps/<cluster>/<job>/` in een S3-nep op de host, van app via kern naar Hop en terug (`HOPOS_APPSPIKE_STORE ok`) |
| `python3 tools/qemu-test-sync.py` | de bevestigde opslagbarrière (`OP_SYNC`, [storage-sync.md](storage-sync.md)): Hop plaatst `apps/syncprobe` met een eigen volume, die schrijft en synct een journal en 8192 databasebytes, haalt het journal weg met een barrière op de map, en QEMU krijgt SIGKILL; de koude boot van hetzelfde volume leest de bytes terug en mist het journal (`HOPOS_SYNC_WRITE`, `HOPOS_SYNC_READ`) |
| `sh tools/qemu-test-cluster.sh` (in de hop-repo) | een cluster van een QEMU-node en een host-agent met een hoplockserver als lock: beide nodes in `hop agents`, en na het hard doden van de host-leider wint de HopOS-node de lease en plaatst hij een job op de host |
| `sh tools/qemu-test-flip.sh` | de kern-flip: hopfs bevroren en gecommit, de NAT-flows gevangen, de sprong, Hop overleeft zonder herstart, en een uitgaande TCP-verbinding van een app (rol FLIPCONN) loopt door: drie antwoorden via kern A, drie via kern B, over één verbinding |
| `MISMATCH=1 sh tools/qemu-test-flip.sh` | een bundel met een andere switch-code wordt vóór de sprong geweigerd (Hop geeft 502) |
| `COLD=1 sh tools/qemu-test-flip.sh` | de koude flip (`"cold":true` op `POST /flip`): Hop stopt zijn taken, de kern zet de app-cores uit en springt zonder adoptie, Hop start koud en plaatst de job opnieuw |
| `OSCORE=1 sh tools/qemu-test-flip.sh` | de flip vanaf een verhuisde kern (ook met `COLD=1`) |
| `BOARD=rpi4 sh tools/qemu-test-flip.sh` | de flip-ingang van de Pi op raspi4b: een core met het merkteken komt tot de kern en leest de DTB opnieuw |
| `sh tools/qemu-uefi-test.sh` | de EFI-stub op EDK2, ACPI, PCIe, virtio over PCI met MSI-X via de ITS (`nic=` loopt op in de tik), de hele appspike-keten |
| `GUI=1 sh tools/qemu-uefi-test.sh` | hetzelfde met de GOP van EDK2 als console |
| `FEATURES=vhe CPU=neoverse-n1 sh tools/qemu-uefi-test.sh` | de VHE-switcher (E2H=1), de smaak die de O6N eist, op een VHE-model |
| `sh tools/qemu-uefi-flip-test.sh` | de kern-flip op EDK2: kern B landt op de PIE-basis van kern A (ook `COLD=1`, en onder VHE met `FEATURES=vhe CPU=neoverse-n1`) |
| `sh tools/qemu-rpi4-test.sh` | het Pi 4-board op QEMU's raspi4b tot de executor-tik |
| `sh tools/qemu-riscv-test.sh` | QEMU virt riscv64 in machine mode: boot, net, opslag, de zelftest van de PMP-kooi (met de kill-tick), en appspike twee keer door de lifecycle van de kern in een PMP-plus-Sv39-kooi |
| `sh tools/qemu-riscv-test-hop.sh` | Hop als bewoner op het hart van de kern op riscv64, een `POST /v1/jobs` plaatst appspike, en Hop komt na een herstart terug |

`sh tools/gate.sh` is de poort vóór elke commit: host-tests, clippy met
`-D warnings`, rustfmt, en de target-builds van alle boards (ook met `gui`
en `media`, en de riscv64-boards).

## De vlakken

| Vlak | Doc | Wat erin staat |
| --- | --- | --- |
| Boards | [boards.md](boards.md), [boards-pi.md](boards-pi.md), [boards-radxa.md](boards-radxa.md), [boards-riscv.md](boards-riscv.md), [boards-apple.md](boards-apple.md) | per board de checklist met de verwachte regels, de interrupts, de watchdog, de klok en de thermiek |
| Kern-flip | [flip.md](flip.md) | de procedure per board, de markers van kern A en kern B, de faalmodi, de boot-guard, de koude weg |
| Gui | [gui.md](gui.md) | de console op het glas, de framebuffer-grant aan een display-app, de USB-invoer, de beeldketen van de Radxa |
| Media | [media.md](media.md) | de videocodec van de O6N (Linlon V8), de codec-dienst, de optische drive, de checklist en de meting (24 fps 4K P010) |
| Meten | [measurements.md](measurements.md) | de lat van de Go-generatie per meting, het commando en de marker, en de lege v3-kolommen per board; `tools/soak.sh` voor uren |

## Images per board

| Board | Bouwen | Uitvoer | Checklist |
| --- | --- | --- | --- |
| QEMU virt | `sh image/qemu-run.sh` | draait meteen | de tests hierboven |
| UEFI generiek (EDK2, QEMU) | `BOARD=uefi sh image/uefi-run.sh` | `target/uefi-esp-uefi/` | [boards.md](boards.md) |
| Orion O6N | `BOARD=o6n sh image/uefi-run.sh` (met `GUI=1` voor het glas) | `target/uefi-esp-o6n/` (naar een FAT32-stick, met `hopos.cfg` naast `EFI/`) | [boards.md](boards.md) |
| Ampere Altra | `BOARD=altra sh image/uefi-run.sh` | `target/uefi-esp-altra/` | [boards.md](boards.md) |
| Raspberry Pi 4 | `sh image/rpi4.sh` | `target/hopos-rpi4.img` (dd) | [boards-pi.md](boards-pi.md) |
| Raspberry Pi 5 | `sh image/rpi5.sh` | `target/hopos-rpi5.img` (dd) | [boards-pi.md](boards-pi.md) |
| Radxa Zero 3E | `sh image/radxa-zero3.sh` | `target/radxa-zero3/hopos-radxa-zero3.img` (dd) | [boards-radxa.md](boards-radxa.md) |
| QEMU virt riscv64 | `sh tools/qemu-riscv-test.sh` | draait meteen (machine mode, `-bios none`) | [boards-riscv.md](boards-riscv.md) |
| LicheeRV Nano (SG2002) | `sh image/licheerv-agent.sh` | `target/licheerv/fip-licheerv.bin` (naar de FAT-bootpartitie) | [boards-riscv.md](boards-riscv.md) |
| Mac mini M4 (t8132) | `sh image/apple-m4.sh` | `target/apple-m4/hopos-apple.img` (via m1n1: `image/apple/boot-cycle.sh`) | [boards-apple.md](boards-apple.md) |
| Kern-flip-bundel | `HOPOS_STAMP=B sh image/flip-bundle.sh <virt\|uefi\|o6n\|altra\|rpi4\|rpi5\|radxa>` | `target/hopos-<board>.flip` plus `.sha256` | [flip.md](flip.md) |

Hop zelf komt uit de hop-repo (`agentd-hopos`); de image-scripts bouwen
hem via `HOP_DIR` en bakken hem in als bewoner. Secure Boot moet uit op de
UEFI-boards. De host-kant van Hop (de `hop`-CLI, `agentd`, de runners en de
stores) staat in dezelfde hop-repo en bouwt met `cargo build --release -p
agentd -p cli`.

Een devicedag eindigt met een pagina in de browser: bouw
[`apps/welcome`](../apps/welcome/README.md), zet `welcome.elf` op een
HTTP-server op de laptop, en `curl -X POST -d '{"name":"welcome","driver":"hop","artifacts":[{"url":"http://LAPTOP:8000/welcome.elf"}],"memory_limit":33554432,"ports":{"http":80}}' http://NODE:9080/v1/jobs`
(met `hopos.insecure=1`); daarna toont `http://NODE/` de bunny, de node, het
slot, de uptime, de verzoeken en de heap. Daarna de meetreeks uit
[measurements.md](measurements.md) en een flip uit [flip.md](flip.md).

## Wat er nog moet

Eén lijst, per node en voor alles tegelijk: [ALLES.md](../ALLES.md) in de
root van de repo. Wat af is gaat eruit. De eerste groene node was de Pi 5 op
30-09-2026, inclusief de eerste kern-flip op ijzer.

Hoe de code geschreven is: het Rust-handboek van haas.software
(`rustdoc/README.md`) en de port-notities (`rustdoc/PORT.md`).
