# Radxa Zero 3E (RK3566) op HopOS v3

De Radxa Zero 3E is geport van de Go-kern (`OLD/metal/board/rk3566`,
`driver/nic/dwmac4`, `driver/nic/mdio`, `OLD/image/radxa-zero3.sh`) naar
Rust. Alles hieronder is op de host getest en bouwt voor het target, maar
niets ervan heeft in Rust op ijzer gedraaid. De Go-metingen (05-08 tot
21-09) staan in het commentaar van de code; deze checklist zegt per stap
wat de console moet tonen en wat een afwijking betekent.

## Wat er is

| Deel | Waar | Getest |
| --- | --- | --- |
| Board: DTB uit x0 naar de heap, geheugen en cores uit de DTB, UART2 (ns16550, stap 4), GIC-600, de klok, de watchdog (gemeten, niet gewapend), het plan, de NIC-probe | `board/rk3566` | 11 host-tests: plan, pool uit de banken min de gaten, CRU-, GRF- en iomux-woorden, aff1-nummering, de map-tellingen |
| SoC-glue: klokgates, bronkeuze en snelheidsdeler (CRU), AXI-reset, RGMII-modus (GRF), de gmac1m1-pinmux, de PHY-reset op GPIO3 PC0, de DW-watchdog | `board/rk3566/src/soc.rs` | idem |
| Identity map: kern-RAM Normal, DMA Normal-NC, de rest Device | `board/rk3566/src/mmu.rs` | const-asserties tegen het plan |
| DWMAC4: registerblok, ringen, DMA, MTL, MDIO-master, `netdev::Device`, IRQ-ack en rearm | `driver/nic/dwmac4` | 19 host-tests op een nep-registerblok en nep-DMA in RAM |
| MDIO: scan, autonegotiatie, RTL8211F-delays | `driver/nic/mdio` | 10 host-tests (samen met de Pi-port) |
| Linkscript: Image op 0x0220_0000, tekst op 0x0221_0000 | `hopos/link-rk3566.ld` | het image-script leest het ELF terug |
| Kaart-image: build, ELF naar arm64-Image, donor, MBR plus FAT16 | `image/radxa-zero3.sh` | levert het kaart-image |

## Bouwen en flashen

```sh
sh image/radxa-zero3.sh                  # of CFG=~/node.cfg, NODE=radxa-2
diskutil unmountDisk /dev/diskN
sudo dd if=target/radxa-zero3/hopos-radxa-zero3.img of=/dev/rdiskN bs=4m
```

De donor (idbloader en u-boot.itb, LBA 64 tot 32767 van het Radxa
b6-image) komt uit `image/radxa/donor-boot.bin`, anders uit de Go-cache
`OLD/image/radxa/donor-boot.bin`, anders van GitHub; hij wordt bij elke
build gehasht. Console: USB-UART op de 40-pins header, pin 8 TX, 10 RX,
6 GND, 1500000 8N1.

## De checklist, in bootvolgorde

1. **U-Boot vindt de kern.** Verwacht: `Retrieving file: /hopos.img`,
   `/hopos.cfg`, dan `## Flattened Device Tree blob at ...` en
   `Starting kernel ...`. Ziet U-Boot `extlinux.conf` niet, dan leest hij
   onze FAT niet (de partitie is actief, type 0x0C, LFN voor
   `extlinux.conf`): mount de kaart op de Mac en kijk of de drie bestanden
   er staan.
2. **Het Image landt op 0x0220_0000.** U-Boot kan `Moving Image from X to
   0x2200000` zeggen; dat is goed. Een ander doeladres betekent dat
   `bi_dram[0].start` niet 0x20_0000 is (Go mat 0x20_0000): pas
   `DRAM_BASE` in het image-script aan, zodat text_offset klopt.
3. **De bunny op de UART.** Stilte na `Starting kernel` en het goede
   adres: de MMU-map of de UART. De UART is 0xFE66_0000 in het
   Device-gebied van de map; het image draait uit Normal op 0x0221_0000.
4. **`boot: ... EL2`.** Go mat dat `booti` op EL2 aflevert. EL1 geeft
   `HOPOS_BOOT_EL` en parkeren.
5. **`fdt: N bytes at 0x7c......, bootargs "hopos.node=radxa-1", hopos.cfg
   M bytes`.** Dit is de kopie van DTB en initrd naar de heap. Staat er
   `HOPOS_RAM_CHECK_SKIPPED`, dan lag x0 niet in het DRAM of was de header
   geen FDT; noteer x0. `GIC differs from the board plan` betekent dat de
   DTB andere GIC-adressen heeft dan 0xFD40_0000 en 0xFD46_0000.
6. **`fb: none from U-Boot`.** Go mat dat U-Boot hier geen scherm achterlaat.
   Staat er toch een framebuffer, noteer de geometrie: er is nog geen
   framebuffer-console in v3.
7. **`watchdog: ... measured N ms at TOP 15`.** Verwacht rond 89478 ms
   (2^31 cycli op 24 MHz). Nul betekent dat de teller niet laadt zolang hij
   niet gewapend is; dat is ook een meting. Hij wordt NIET gewapend.
8. **`boot: HopOS v3.0.0 on rk3566, EL2, 4 cores (big), 2046 MB DRAM
   HOPOS_BOOT`.** Het aantal cores en MB komen uit de DTB.
9. **`irq: ...` en daarna `HOPOS_TICK 1`, `2`, ...** De tik loopt alleen als
   de slaap wekt. Het board slaapt met WFI op de timer-PPI 30 (bewezen op
   QEMU, niet op dit silicium). Blijft de tik staan: zet in
   `board/rk3566/src/lib.rs` `sleeper()` op `Mode::Wfe`. `irq(timer=...)`
   moet oplopen. `irq: FAIL ... HOPOS_IRQ_FAIL` noemt de reden
   (redistributor-frame, slapende redistributor).
10. **De NIC-keten.** Elke stap faalt met een eigen regel en `HOPOS_NIC_FAIL`:
    - `no DWMAC4 at GMAC1 (version 0x0...)`: de pclk staat dicht; de regel
      geeft clksel33, clkgate17 en softrst14.
    - `DMA soft reset does not clear (bus mode 0x00000001)`: de AXI-kant
      staat in reset (de meting van 05-08); de AXI-reset-puls ging mis.
    - `no PHY on the MDIO bus (... mdc/mdio mux 3/3 ...)`: de mux moet 3/3
      zijn; anders landde de pinmux niet. Een PHY op adres 0 en 1 met
      dezelfde id is normaal: de kern kiest 1 (de DTS).
    - `RTL8211F at 1 rgmii delays (tx, rx) Ok((..)) -> Ok((true, true))`:
      ná de schrijf moeten beide `true` zijn. Zonder TX-delay komt er een
      link, werkt RX en verdwijnt elk verzonden frame (06-08).
    - `phy: no link within 8000 ms`: kabel, of de PHY-reset.
11. **`net: dwmac4 at 0xfe010000 version 0x3051, PHY 1, link 1000 Mbps full
    duplex, intid 64`** en een diag-regel met `rxdesc[0] 0xc1000000`
    (OWN|IOC|BUF1V: de ring staat klaar). Dan `HOPOS_NIC_UP mac=02:48:4f:50:..`.
    Het adres volgt uit `hopos.node` (net::nodemac); `HOPOS_MAC_FIXED`
    betekent dat er geen node-naam was.
12. **DHCP en de lijn.** De kern vraagt een lease (net.rs). In
    `HOPOS_TICK` moet `nic=` oplopen bij verkeer: de lijn is SPI 32 (INTID
    64), NIE op bit 15 (de 4.10-indeling; 20-09 bewezen). Blijft `nic=0`
    terwijl er frames binnenkomen, dan pollt de pomp op de vangrail van
    10 ms: werkt, maar traag.
13. **Doorvoer.** Go haalde met Normal-NC op de NIC-DMA 56,6 MB/s in en 99 uit
    (was 15,5 op Device). De map zet die regio vanaf de eerste instructie
    Normal-NC; meet met de netmeter als DHCP er is.
14. **De slots.** Er is geen staging op dit board: verwacht
    `slots: no staged image, nothing placed HOPOS_SLOT_NONE`. De kooi zelf
    (`HOPOS_CAGE_UP`) staat in het Device-venster op 0x0622_0000. De app-cores
    heten 0x100, 0x200, 0x300 (aff1, gemeten 05-08).
15. **De kick van de OS-core.** Het board gebruikt SGI 7, niet SGI 8 zoals QEMU
    virt: TF-A op Rockchip houdt SGI 8 tot en met 15 als Secure. De zelftest
    van de OS-core (`HOPOS_OS_SELFTEST`) moet `kick=` met een tijd tonen;
    `HOPOS_OS_SELFTEST_FAIL` met een lege kick is dit punt.

## Niet gedaan

- Geen schijf: de SD/eMMC-controller heeft geen v3-driver. `probe_disk` geeft
  `Ok(None)`, dus `HOPOS_DISK_NONE` en geen hopfs.
- De watchdog wordt gemeten maar niet gewapend: v3 heeft nog geen
  aai-beleid (Go: elke 20 s vanuit `cmd/hopos/watchdog.go`).
- Geen framebuffer-console en geen VOP2/HDMI-scanout (Go: gui-werk).
- Geen TRNG-reseed, geen temperatuursensor (Go: `trng.go`, `tsadc.go`).
- Geen `hopos.cfg`-venster voor raw patchen (Go's `-cfgwindow`).
- De pool eindigt op 0xF000_0000: een bord met 8 GB gebruikt alleen de onderste
  3,75 GB.
- `hopos/src` noemt het board nog `board_qemuvirt`; main.rs laat die naam
  naar `board_rk3566` wijzen, net als voor de Pi's. Een neutrale naam in
  slots.rs, cage.rs en flip.rs is de nette vervolgstap.
