# Boards: de UEFI-machines (O6N en Altra)

Stand 29-09-2026, avond. Wat er voor de Radxa Orion O6N en de Ampere Altra
in v3 gebouwd is, wat je bij de eerste boot op de console hoort te zien, wat
er nog niet is, en wat je meet. De lat is de contractmatrix van de Go-kern
(`OLD/docs/support.md`): een v3-board is pas klaar als het op elk gemeten
punt minstens doet wat de Go-kern deed.

## Bouwen

```sh
BOARD=o6n   image/uefi-run.sh   # target/uefi-esp-o6n/EFI/BOOT/BOOTAA64.EFI
BOARD=altra image/uefi-run.sh   # target/uefi-esp-altra/EFI/BOOT/BOOTAA64.EFI
```

Op een stick: een FAT32-partitie met die boom erop (plus `hopos.cfg` als je
er een hebt), Secure Boot uit. Beide boards zijn het UEFI-board
(`board-uefi`, kernvenster `window-8000` op 0x8800_0000) plus een eigen crate:
`board-o6n` en `board-altra`. De binary kiest met `--features board-o6n` of
`board-altra`.

**Stand van de build:** de board-crates en alle drivers bouwen voor
`aarch64-unknown-none-softfloat` en zijn groen op host-test, clippy en fmt.
`hopos` met `board-o6n`/`board-altra` bouwt op dit moment níet, om dezelfde
reden als `board-uefi` zelf: de core-0-lijm in `hopos/src/slots.rs` vraagt
van het board `this_core()`, `os_bell()`, `kick_self()` en `plan(cores,
core)`, en `board-uefi` levert die nog niet. De O6N- en Altra-crates geven
alles wat `Uefi` heeft door (`Deref`), dus zodra `board-uefi` bouwt, bouwen
zij ook. Toets vóór je naar het board loopt: `BOARD=uefi image/uefi-run.sh`
boot op QEMU, en `BOARD=o6n` en `BOARD=altra` leveren een `BOOTAA64.EFI`.

## Radxa Orion O6N

### Wat er is

| Onderdeel | Crate | Hoe getest |
| --- | --- | --- |
| NIC: RTL8126A (5G, O6) en RTL8125B/D/CP/BP (2,5G, O6N), chip-XID kiest de variant | `driver-rtl8126` | 12 host-tests tegen een nep-chip: reset tot `hw_start`, ringen, MAC-terugval, EPHY-tabel en PHY-stappen van de 8125B, link via PHYSR, RX-grenzen, TX-padding en eigendom, IRQ-mask/rearm |
| NVMe: admin- en I/O-queue, identify, PRP-lijsten, flush, 512-byte-sectoren boven 4K-namespaces | `driver-nvme` | 10 host-tests tegen een nep-controller die echte SQ's/CQ's en PRP's uitvoert, time-out en vreemde CID maken de driver dood |
| Thermometer: SCP via SCMI op 0x065d0000 (het kanaal van de DSDT-`_TMP`) | `driver-scmi`, `board-o6n::thermal` | 6 + 2 host-tests; kiest CPU-sensoren, anders alle Celsius |
| Klok: `_CPC`-scanner (AML zonder interpreter), domeinen, `CpcKnob` | `board-o6n::{cpc, clock}`, `driver-dvfs` | 3 + 3 host-tests; het beleid 8 host-tests |
| Core-klassen: MADT, anders `_CPC` (25%-clustering), anders vast per MPIDR (aff1 0-3 small) | `board-o6n::class` | 4 host-tests, waaronder de meting van 17-09 (2232 ×4, 8192/7876/7246/6931 ×2 = small ×4, big ×8) |
| PCIe-zoektocht: eerste Realtek, eerste NVMe, INTx per root-poort | `board-o6n::probe` | 2 host-tests op een nep-config-space |

### Wat je hoort te zien (in deze volgorde)

- `uefi: booted at EL2 ... OEM "CIXTEK"` en `pcie: ...` per functie (van
  `board-uefi`). Kijk of beide Realteks en de NVMe erin staan, met hun BAR's.
- `o6n: 11 app cores, classes from Mpidr - small 3, mid 0, big 8` (of
  `Madt`). De kern-core is core 0; is dat een A520, dan staat er small 3.
- `boot: HopOS v3.0.0 on o6n, EL2, 12 cores (...) HOPOS_BOOT`.
- `net: RTL8125? 10ec:8125 at 31:00.0 xid 0x... link 2500 Mbps full duplex, polled (INTx 477 known, not wired)`
  en dan `HOPOS_NIC_UP mac=...` en na DHCP `HOPOS_NET_UP`.
- `disk: nvme <model> at ... HOPOS_NVME_UP`, dan `HOPOS_DISK_UP` en
  `HOPOS_FS_UP` (hopfs op de NVMe; het hele device is van HopOS, stateful).
- Bij de eerste temperatuurvraag: `hwmon: N SCMI sensors of M (first ...)`.

Gaat het mis, dan zegt de regel welke stap: `rtl8126: <stap> timed out (reg
0x..=0x..)`, `rtl8126: unsupported chip XID 0x...`, `nvme: CSTS.RDY never
became 1`, `HOPOS_NIC_FAIL`, `HOPOS_NVME_FAIL`.

### Wat er nog niet is

- **De NIC-interrupt.** De lijn staat in `probe::nic_intid` (477 voor LAN 2
  achter bus 0x30, gemeten L80) en de driver kent de les van 17-20/09 (masker
  dicht bij de ack, W1C van alle bits, rearm plus eigen blik op de ring in
  `flush`). Maar `board-uefi` geeft geen weg om een SPI scherp te zetten en
  in `dispatch_interrupts` te herkennen: de O6N pollt. Nodig in `board-uefi`:
  `enable_line(intid, ack)` en een haak in de dispatch, zoals `NIC_IRQ` op virt.
- **De klok.** Beleid, knop en `_CPC`-scanner zijn er; `board-uefi` geeft de
  DSDT/SSDT niet door, dus er zijn geen fastchannel-adressen en er draait geen
  governor-taak. Zonder dit blijven de grote cores op de boot-OPP (Go: 1,5
  GHz). Nodig: een tabel-toegang in `board-uefi` (`acpi_table(sig)`) en een
  taak in `hopos` die `Governor::step` elke 10 ms voedt.
- **De `_CPC`-klassenbron**, om dezelfde reden: nu MADT, anders de MPIDR-tabel.
- **De OEM-toets** (`CIXTEK`) vóór de SCMI-toegang: het O6N-image is O6N-eigen.
- **De header-UART** (0x040d0000) als spiegel: de console is die van de SPCR.
- De temperatuur gaat nog niet op de heartbeat: `O6n::temp_milli_c` bestaat,
  een aanroeper in `hopos` niet.
- Twee poorten tegelijk (de eerste ondersteunde wint), VPU en USB (media/gui).

### Wat je meet (lat: L74-L83 van de Go-kern)

1. Boot tot `HOPOS_NET_UP` en `HOPOS_FS_UP`; noteer de variant (XID) en de
   linksnelheid.
2. Netwerk gepold: rtt p50 (Go gepold 160-168 µs) en doorvoer M4 ↔ O6N (Go
   met IRQ 112-117 MB/s). Pomp-rondes per seconde idle (Go gepold 3333/s).
3. NVMe: 1 GiB schrijven en teruglezen met positie-afhankelijke inhoud;
   MB/s beide kanten; `slowest_ns`. Herstart: hopfs herstelt (`restored`).
4. Core-klassen: klopt de indeling met het ijzer (4× A520, 8× A720)? Is
   core 0 een A520 of een A720?
5. Temperatuur: één lezing, en of die bij belasting stijgt.
6. Watchdog, FLIP, lifecycle: zoals `board-uefi` ze levert.

## Ampere Altra

### Wat er is

| Onderdeel | Crate | Hoe getest |
| --- | --- | --- |
| NIC: Intel igb (I210 8086:1533 en familie; QEMU's 82576 8086:10c9), advanced descriptors, RX 256 / TX 64, doorbell per burst | `driver-igb` | 11 host-tests tegen een nep-igb: reset, MAC uit RAL0/RAH0, ringen, link via MDIC en STATUS, RX-grenzen, zelf-flush per 32, TX-eigendom en de TDT≠TDH-rem |
| NVMe | `driver-nvme` | zie de O6N |
| Thermometer: SMpro via PCC-kanaal 14 (xgene-hwmon), PCCT-parser | `driver-smpro` | 4 host-tests, waaronder "een onafgemaakt commando laat de buffer van de firmware" |
| Klassen: homogeen big | `board-altra` | host-test |
| PCIe-zoektocht over tot acht segmenten, hoge BAR's via `map_device` | `board-altra` | host-test op twee nep-segmenten |

### Wat je hoort te zien

- `uefi: booted at EL2 ...`, `pcie: segment ...` voor elk segment.
- `boot: HopOS v3.0.0 on altra, EL2, 128 cores (big) ... HOPOS_BOOT` (of 80,
  afhankelijk van de SKU).
- `net: igb 8086:1533 at ... link 1000 Mbps full duplex, polled`, dan
  `HOPOS_NIC_UP` en `HOPOS_NET_UP`.
- `disk: nvme ... HOPOS_NVME_UP`, `HOPOS_DISK_UP`, `HOPOS_FS_UP`.

### Wat er nog niet is

- **De thermometer staat niet aan**: `Altra::open_hwmon(pcct)` wil de
  PCCT-bytes, en `board-uefi` geeft geen tabellen door. Nodig: dezelfde
  `acpi_table(sig)` als voor de O6N.
- **Gepold, en dat blijft zo**: de `_PRT`-INTx doodt de SoC (L83); MSI-X via
  de ITS valt buiten de scope.
- Het geheugenplan is dat van `board-uefi`: of de pool de ~300 GB boven de
  512 GB haalt (Go 15-07: 1,62 GB zonder de hoge map), zegt de bootregel van
  `board-uefi`.

### Wat je meet

1. Boot tot `HOPOS_NET_UP`/`HOPOS_FS_UP`; de igb-variant en de link.
2. Doorvoer gepold (Go: inbound 5 MB/s met een ring van 64, dus nu met 256
   opnieuw meten) en rtt p50/p99.
3. NVMe zoals op de O6N; de BAR ligt hoog: klopt `map_device`?
4. Het aantal cores en de pool-grootte tegen de firmware-memory-map.

## QEMU `-device igb`

De igb-driver is QEMU-testbaar (82576). De stap: `board-altra` op QEMU/EDK2
booten met `-device igb` in plaats van virtio-net. Dat kan pas als `hopos`
met `board-altra` bouwt (zie "Stand van de build"); dan is het een
`NIC=igb`-variant van de EDK2-test (`tools/qemu-uefi-test.sh`, van het
UEFI-spoor) die `HOPOS_NIC_UP` en `HOPOS_NET_UP` moet halen. Niet gedaan.
