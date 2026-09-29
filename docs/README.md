# HopOS v3: de docs

Wat er is, hoe je het bouwt, en wat er per board te bewijzen valt. De
checklists per board zijn de lat voor een dag op het ijzer; elke stap noemt
de consoleregel die erbij hoort en wat een afwijking betekent.

## Bewezen op QEMU (30-09-2026)

| Test | Wat hij bewijst |
| --- | --- |
| `sh tools/qemu-test.sh` | boot op virt, netwerk, opslag, appspike in slot 1 en 2 in een stage-2-kooi, tien toetsen |
| `GUI=1 sh tools/qemu-test.sh` | hetzelfde met de console op een `ramfb`: de bunny, mem, datum en tijd op het glas, geschoten via de QEMU-monitor |
| `GUI=display sh tools/qemu-test.sh` | de framebuffer-grant: een app met `GUI=display` in zijn env krijgt het glas in zijn kooi (`HOPOS_FB_GRANT`, `HOPOS_FB_ARM`), en na de stop komt de console terug (`HOPOS_FB_RELEASE`) |
| `sh tools/qemu-test-hop.sh` | de kern start Hop met het token op de OS-core; een `POST /v1/jobs` van de host laat Hop appspike plaatsen; Hop bewaart zijn staat en leest hem na een herstart terug |
| `SMP=2 sh tools/qemu-test-hop.sh` | hetzelfde met twee cores: kern plus Hop op één core, de app op de andere |
| `SMP=2 OSCORE=1 sh tools/qemu-test-hop.sh` | de kern verhuist bij boot naar core 1 |
| `sh tools/qemu-test-welcome.sh` | Hop plaatst welcome met `"ports":{"http":80}`; de kern zet poort 80 door naar het slot, `curl` van de host krijgt de pagina met de bunny en `/health`, en na `DELETE /v1/jobs/welcome` is de poort weer dicht |
| `sh tools/qemu-test-smp.sh` | een SMP-app: appspike met `cores: 2` krijgt zijn tweede core in de eigen kooi, telt erop, en houdt beide cores na een herstart door Hop |
| `sh tools/qemu-test-share.sh` | een sharegroep: twee appspikes delen één app-core in coöperatieve rotatie, allebei groen, en een lid dat stopt komt terug naast het levende lid |
| `sh tools/qemu-test-bench.sh` | de meetketen: Hop plaatst `apps/bench` met poort 80, `tools/netmeter` meet van de host (rtt, storm, in, out), twee apps meten elkaar door de switch, BURN draait, `hopos.idlestat=1` drukt de meetlat, en een tweede boot doet `hopos.nvmebench=1`; de getallen staan in [measurements.md](measurements.md) |
| `sh tools/qemu-test-flip.sh` | de kern-flip: hopfs bevroren en gecommit, de NAT-flows gevangen, de sprong, Hop overleeft zonder herstart, een TCP-verbinding die op kern A openging wordt door kern B beantwoord |
| `MISMATCH=1 sh tools/qemu-test-flip.sh` | een bundel met een andere switch-code wordt vóór de sprong geweigerd (Hop geeft 502) |
| `sh tools/qemu-uefi-test.sh` | de EFI-stub op EDK2, ACPI, PCIe, virtio over PCI met MSI-X via de ITS (`nic=` loopt op in de tik), de hele appspike-keten |
| `GUI=1 sh tools/qemu-uefi-test.sh` | hetzelfde met de GOP van EDK2 als console |
| `FEATURES=vhe CPU=neoverse-n1 sh tools/qemu-uefi-test.sh` | de VHE-switcher (E2H=1), de smaak die de O6N eist, op een VHE-model |
| `sh tools/qemu-uefi-flip-test.sh` | de kern-flip op EDK2: kern B landt op de PIE-basis van kern A |
| `sh tools/qemu-rpi4-test.sh` | het Pi 4-board op QEMU's raspi4b tot de executor-tik |
| `sh tools/qemu-riscv-test.sh` | QEMU virt riscv64 in machine mode: boot, net, opslag, en de zelftest van de PMP-kooi |

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

## Nog niet, en op ijzer nog nooit gedraaid

Alles hieronder is op QEMU bewezen waar QEMU het kan, en op geen enkel
board ooit gestart. De lijst is de eerlijke stand vóór de devicedag.

- **O6N.** De VHE-switcher is op QEMU met het Neoverse-model bewezen, op de
  A720 niet. De console na de exit: de SPCR wijst naar een UART die de SCP
  dicht houdt (Go gebruikte een vroege UART op 0x040d0000). MSI-X op de
  RTL8125 en de Cix-IORT zijn onbewezen; `hopos.nicirq=intx` meet de
  terugval los. De VPU: het contract, de driver en de kern-kant staan, maar
  een arena buiten de partitie-pool en de firmware-lezing uit hopfs
  ontbreken nog, dus de codec blijft uit (`HOPOS_CODEC_OFF`); `codecdemo`
  is niet geport; er is nog geen Rust-app die decodeert.
- **Altra.** De NIC blijft gepold (de INTx-les L83; igb zonder MSI-X), de
  SBSA-watchdog is zijn eerste echte proef.
- **Overal.** NVMe pollt (het blokcontract is synchroon); INTx voor
  virtio-pci is er niet.
- **Pi 4 en 5.** Watchdog, thermiek en klok via de mailbox zijn alleen
  haken; NVMe op de Pi 5 ontbreekt; de USB-bedrading (VL805, RP1) ontbreekt,
  dus geen HID-invoer; de flip is gebouwd, niet gesprongen (`_pi_start` op
  een andere core dan 0?).
- **Radxa.** Geen SD-driver; de EDID-lezer, de DDC-pinmux en de GRF zijn
  ongemeten; DWC3-USB is niet geport; de flip is gebouwd, niet gesprongen.
- **Kern-flip.** Op de UEFI-boards en de Pi's alleen gebouwd. De koude flip
  (`cold` op `POST /flip`) is gedocumenteerd, niet gebouwd. Een uitgaande
  TCP-verbinding van een app over de flip is alleen op de host getoetst.
- **SMP en sharegroepen.** Niet op Apple, RISC-V of ijzer. De join-wacht van
  5 ms is een spin op de kern-core.
- **Gui.** Er is nog geen display-app in Rust; de input-listener op
  10.100.0.1:7879 voor de HID-bezorging ontbreekt; de xHCI-driver wacht
  blokkerend op de klok (tot 2 s per poortreset); de log wrapt en scrollt
  niet.
- **RISC-V.** appspike draait nog niet in een slot via de lifecycle
  (`cage_riscv.rs`, de applib-runtime voor riscv64); geen OS-core-rotatie,
  lottery of flip; de LicheeRV bouwt (FIP uit de donor) en heeft nooit
  gedraaid.
- **Mac mini M4.** Het board, ADT, xnuboot, GPT, AIC, tg3, rtkit, de ANS
  (lezen én schrijven, in het gat na de macOS-partities) en de SMC zijn
  geport en host-getest; de OS-core-rotatie voor `AppleVhe` (fast IPI,
  timer-FIQ) en de CPU_ON-haak (er is geen PSCI) ontbreken, dus het
  slot-plan weigert standaard luid (`hopos.cages=on` haalt de rem eraf).
  Nooit gedraaid.
- **Hop op de host.** Geen SIGTERM-afhandeling in `agentd` (std heeft geen
  signaal-API: een gedode daemon laat zijn lease via de TTL verlopen), geen
  SSE `/v1/events`, geen live log-tail, geen streaming door de proxy; de
  Linux-isolatie is in Alpine als root bewezen, niet op een echte host.
- **applib.** Geen `join_group` (multicast), geen tellerfrequentie, de
  timebase staat vast op 10 MHz (de LicheeRV heeft 25 MHz), en de
  TcpConn-adapter staat dubbel (hop-http en welcome).
- **ABI.** `CTRL_TEMP` (de thermiek op de control-page van Hop) is nieuw en
  verkleint `CTRL_ENV_MAX`; Hop hoort op de tag van deze kern te staan.
- De IPv6-baan van leannet.

Hoe de code geschreven is: het Rust-handboek van haas.software
(`rustdoc/README.md`) en de port-notities (`rustdoc/PORT.md`).
