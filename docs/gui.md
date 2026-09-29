# Het gui-vlak

Wat HopOS v3 met een scherm, een toetsenbord en een muis doet, en wat niet.
De Go-generatie is de specificatie (`OLD/metal/driver/fb`, `OLD/metal/gui`,
`OLD/docs/v1/archief/gui-ontwerp.md`); dit is de stand van de Rust-port.

## De beslissingen

1. **Geen GPU-driver.** De kern schrijft pixels in een beeld dat al loopt:
   van de firmware (UEFI GOP, de VideoCore-mailbox van de Pi), van QEMU
   (`ramfb`), of op de Radxa van onze eigen scanout-keten (VOP2 en HDMI,
   want U-Boot laat daar geen beeld achter, gemeten 05-08).
2. **De console is van de kern, het glas van de display-app.** Bij boot
   staat de console op het scherm: de bunny als vaste kop, rechts drie
   meetregels (kern-heap, datum, tijd, elke seconde), de log eronder. Vraagt
   een job het glas, dan krijgt de display-app het venster in zijn kooi en
   gaat de console eraf; stopt het slot (ook een crash), dan komt de console
   terug. Eén houder tegelijk; de eerste wint.
3. **Een grant alleen voor een device zonder DMA** (gui-ontwerp §7). Een
   framebuffer is een pixelbuffer: geen registers, geen DMA, dus hij mag in
   een kooi. Een xHCI is een bus-master en er is geen IOMMU: die blijft van
   de kern, en de app krijgt de invoer als stroom (06-08).
4. **Kaal is kaal.** Zonder de feature `gui` linkt de binary geen regel
   display-code en is elk board headless (Go: 183 KB voor de hele
   gui-smaak op de Radxa, 06-08).

## Bouwen

`--features gui` naast het board, bijvoorbeeld
`cargo build --release --target aarch64-unknown-none-softfloat -p hopos --features board-rk3566,gui`.
De feature zet `gui` aan op het gekozen board (`board-x?/gui`) en linkt
`driver-fb` en `gui-fbgrant`.

| Crate | Wat het bezit |
| --- | --- |
| `driver/fb` (`driver-fb`) | de console op een lineaire framebuffer: `Desc`, `Console` (kop, log, `header_status`), geen allocatie |
| `kern::grants` | het `DeviceGrant`-primitief: één venster, één houder, `WindowMap` naar de stage-2, de adoptie na een flip, de haken (`Grants`) |
| `gui/fbgrant` (`gui-fbgrant`) | het beleid: wie het glas krijgt, de `FB_*`-env, de console eraf en terug |
| `gui/rkscan` (`gui-rkscan`) | de RK3566-keten: PD_VO, VOP2, DW-HDMI, EDID |
| `driver/usb/xhci`, `driver/usb/hid`, `gui/usbin` | de USB-invoer: host, enumeratie, boot-HID, de bezorging aan de display-app |
| `hopos/src/gui.rs` | de bedrading: console op het glas, meetregels, USB na het netwerk, de grant-haken |

## Per board

| Board | Beeld | Waar het vandaan komt | Status |
| --- | --- | --- | --- |
| QEMU virt | `-device ramfb` | `board/qemuvirt/src/ramfb.rs`: fw_cfg `etc/ramfb`, 1280x800 op 0xC400_0000 | `GUI=1 sh tools/qemu-test.sh`: de console op het glas, screendump getoetst |
| UEFI (EDK2, QEMU) | GOP | `board/uefi/src/gop.rs`, vóór `ExitBootServices`; met `-device ramfb` geeft EDK2 een GOP | `GUI=1 sh tools/qemu-uefi-test.sh`: GOP 800x600 op 0xfc7a_0000, de console erop, de hele keten groen |
| Orion O6N | GOP van de eigen firmware | `board_uefi::gop_framebuffer` | nooit op ijzer |
| Ampere Altra | GOP (BMC-VGA) | idem | niet bedraad: `board/altra` geeft `framebuffer()` nog niet door |
| Raspberry Pi 4 en 5 | HDMI via de firmware | `board/raspi/src/vcfb.rs`: DTB-simplefb, anders `FB_ALLOC` over de mailbox (1920x1080) | nooit op ijzer |
| Radxa Zero 3E | HDMI via onze eigen keten | `board/rk3566/src/display.rs` over `gui-rkscan` | nooit op ijzer |

### Wat je op het scherm hoort te zien

- **Radxa (HDMI).** 1920x1080p60 uit de eigen keten: PD_VO aan via de
  PMU, de HPLL en de VOP2-klokken, de VOP-IOMMU uit, VP0 met Smart0 scant
  `FB_RAM` (0x0700_0000, 8 MB, Normal-NC, het PA-plan van het board), dan
  DW-HDMI met de PHY en de frame composer (DVI, geen infoframes, zoals
  Go). De EDID wordt over de DDC gelezen en alleen gemeld: de keten drijft
  altijd 1080p60 (`HOPOS_DISPLAY_MODE` als de monitor iets anders
  prefereert). De console krijgt 16x16-cellen (120x67). Regels: `display:
  1920x1080p60 on HDMI (sink attached: true) HOPOS_DISPLAY_UP`, dan `fb:
  console on 1920x1080 ... HOPOS_FB_CONSOLE`. Faalt een laag, dan zegt de
  regel welke, met de registers van die laag (`HOPOS_DISPLAY_FAIL`); de
  console tekent dan in een buffer die niemand uitscant (Go: de buffer
  blijft bruikbaar voor `/kvm`). `HOPOS_DISPLAY_NOLATCH` = VP0 nam de
  config-done niet over. De EDID-lezer, de DDC-pinmux en de GRF-bits
  daarvoor zijn nieuw ten opzichte van Go en ongemeten.
- **Pi 4 en Pi 5 (HDMI via de firmware).** De firmware zet het beeld aan;
  wij vragen er een buffer van via de mailbox. Let op de diepte: GEMETEN
  11-07 op de Pi 5 bleef de scanout op 16 bpp terwijl de depth-tag 32 zei;
  de pitch beslist (`vcfb::from_mbox`). De buffer ligt buiten de
  `/memory`-banken; `vcfb::plan_map` zet zijn 2 MB-blokken Normal-NC in de
  tabel van de eerste gigabyte (alleen lege regels). Regels: `fb:
  VideoCore framebuffer ... HOPOS_FB_UP`, `HOPOS_FB_CONSOLE`. Zonder
  HDMI-kabel bij boot kan de firmware weigeren: dan `HOPOS_FB_NONE` en de
  node draait headless (de ontdekking is één keer per boot, 19-07).
- **O6N (GOP).** Wat de firmware in de GOP zet, meestal de resolutie van
  het BIOS-scherm. Regels: `uefi: GOP WxH stride S format F @ ...
  HOPOS_UEFI_GOP` vóór de exit, `HOPOS_FB_CONSOLE` erna. De buffer gaat
  Normal-NC in de identity map (`gop::map`, op 4 KB precies). Nooit op
  ijzer gezien; de O6N-console na de exit is zelf nog open (docs/boards.md).

## De grant

Een job vraagt het glas met `gui: display` in zijn jobspec; Hop zet dat als
`GUI=display` in de env van de job (`FB=1`, de Go-vorm, werkt ook). De kern
geeft de houder in de env:

| Sleutel | Waarde |
| --- | --- |
| `FB_BASE` | het IPA van de buffer in de kooi: `0x2000_0000` plus de offset in zijn 2 MB-blok (de buffer mag fysiek boven 4 GB liggen: de ramfb-vondst van 19-07, 0x1_bc7a_0000) |
| `FB_WIDTH`, `FB_HEIGHT` | pixels |
| `FB_STRIDE` | bytes per regel |
| `FB_BPP` | 32 of 16 |
| `FB_SWAP` | `1` als rood en blauw ruilen (GOP-formaat RGB) |
| `INPUT_ADDR` | waar HOP de invoer uitserveert, `10.100.0.1:7879`; alleen als het board werkende USB heeft |

De haken in de lifecycle (`kern::grants::Grants`, geïmplementeerd door
`hopos::gui::GuiGrants`):

| Stap | Haak | Plek |
| --- | --- | --- |
| na de claim, vóór de env op de control-page | `env` | `Request::Env` aan de lifecycle-actor, vlak vóór `Stream::put_env` (`kern/src/system.rs`) en `write_env` van de boot-plaatsing (`hopos/src/slots.rs`) |
| na de kooibouw, vóór de dispatch | `arm` | `Lifecycle::arm` (`kern/src/slots.rs`); faalt hij, dan is het een startfout: poorten dicht, grant terug |
| bij de adoptie na een flip | `adopt` | `Lifecycle::adopt`, na alle eigendomsclaims en vóór de servicers |
| na een bevestigde stop, en bij een abort | `release` | `Lifecycle::stop` (niet in quarantaine) en `Lifecycle::abort` |

De actor bezit de aanbieder (`Lifecycle<…, G: Grants>`): `hopos/src/slots.rs`
geeft `GuiGrants` mee in de gui-smaak en `NoGrants` kaal. Past de env met de
`FB_*`-regels niet op de control-page (`CTRL_ENV_MAX`), dan gaat de grant
terug en draait de app headless (`HOPOS_FB_ENV` of `HOPOS_GRANT_ENV`).

Op QEMU: `GUI=display sh tools/qemu-test.sh` start appspike met
`hopos.appenv=GUI=display` en eist per slot `HOPOS_FB_GRANT`, `HOPOS_FB_ARM`
en na de stop `HOPOS_FB_RELEASE`, en daarna de console terug op het glas.

## De display-app

In Go is het `cmd/display` in hop-os-surf: de compositor die `/screen.png`,
`/kvm` en `POST /input` serveert, en met `FB_*` in de env naar het glas
blit (`fbblit.go`). In Rust hoort hij een gewone app op `applib` te zijn.
Wat hij van `applib` nodig heeft:

1. **De `FB_*`-env lezen** (de env-blob op de control-page, zoals elke
   sleutel) en weigeren wat niet klopt (stride kleiner dan breedte maal
   bytes per pixel, een BPP anders dan 16 of 32).
2. **Het venster mappen in zijn stage-1**: `[FB_BASE, FB_BASE +
   FB_STRIDE * FB_HEIGHT)` als **Normal-NC** (`memattr::normal_nc` in de
   app-runtime, de tegenhanger van `cpu::memattr` in de kern). De stage-2
   van de kern zegt al Normal-NC; een stage-1 op Device zou elke store een
   losse transactie maken (een 1080p-frame is een miljoen stores), op
   Normal-WB zou de scanout oude cachelijnen zien. `FB_BASE` staat niet op
   2 MB: de app mapt op 4 KB.
3. **De invoer**: een TCP-client naar `INPUT_ADDR` die regels leest, één
   JSON-event per regel, precies het object dat `POST /input` van de
   browser-KVM aanneemt (`{"k":"key","c":..,"v":..}`, `{"k":"move","x":..,"y":..}`,
   `{"k":"btn",...}`, `{"k":"wheel",...}`); een lege regel is een
   keepalive (elke 5 s), en een verbroken stroom betekent opnieuw bellen
   (na een kern-flip). HOP laat alleen het slot van de houder binnen.
4. **Een eigen blit** in het formaat van de env (16 of 32 bpp, `FB_SWAP`).

## De USB-invoer

HOP bezit de controllers (een xHCI is een DMA-master en er is geen
IOMMU), leest boot-protocol-rapporten van toetsenbord en muis en levert ze
als JSON-regels aan de houder van het glas. De display-app belt, HOP
luistert (`INPUT_ADDR` reist mee in de grant): zo hoeft HOP de app niet te
vinden, en controleert hij op accept alleen dat de beller het slot van de
houder is (de switch weigert een frame met andermans bron-MAC, 06-08).

## Checklist per board

Elke stap met de consoleregel die erbij hoort.

**QEMU virt**

- [x] `GUI=1 sh tools/qemu-test.sh`: `fb: ramfb 1280x800 @ 0xc4000000
  HOPOS_FB_UP`, `fb: console on 1280x800 ... (80x50 cells)
  HOPOS_FB_CONSOLE`, en de screendump (via de monitor) heeft ruim 170.000
  tekst- en 849.000 achtergrondpixels: de bunny, de meetregels en de log.
  `SHOT_KEEP=pad.ppm` bewaart hem.

**UEFI op QEMU (EDK2)**

- [x] `GUI=1 sh tools/qemu-uefi-test.sh` (`image/uefi-run.sh` geeft QEMU dan
  `-device ramfb`): `uefi: GOP 800x600 stride 3200 format 1 @ 0xfc7a0000
  HOPOS_UEFI_GOP` vóór de exit, `HOPOS_FB_CONSOLE` erna. De buffer valt
  niet op 2 MB, dus `cpu::memattr` laat hem staan zoals `gop::map` hem zette
  (Normal-NC op 4 KB); de regel `fb: left as the board mapped it` is dan
  verwacht. Nog geen screendump-toets in dit script.

**Orion O6N**

- [ ] HDMI of DP aan, boot van de stick met een gui-image:
  `HOPOS_UEFI_GOP` met een plausibele resolutie.
- [ ] De bunny en de log op het scherm; de tijd rechtsboven loopt.
- [ ] Na de exit: loopt het beeld door? (De GOP-buffer is van de firmware;
  een firmware die zijn scanout bij de exit stopt, is een bevinding.)

**Raspberry Pi 4 en 5**

- [ ] HDMI in poort 0 vóór de stroom: `fb: VideoCore framebuffer ...
  HOPOS_FB_UP` met 16 of 32 bpp.
- [ ] De bunny en de log op het scherm; geen strepen (strepen = een
  diepte-verwisseling, 11-07).
- [ ] Geen bus-fault bij de eerste veeg (de freeze-jacht van 04-08: een
  verkeerd gelezen adres).

**Radxa Zero 3E**

- [ ] HDMI aan: `HOPOS_DISPLAY_UP` en `sink attached: true`.
- [ ] Een beeld op 1080p60; de bunny en de log.
- [ ] Zonder monitor: `sink attached: false`, geen hang.

**Elk board, met Hop**

- [ ] Een job met `gui: display`: `HOPOS_FB_GRANT` en de console gaat van
  het glas.
- [ ] De job stoppen: `HOPOS_FB_RELEASE` en de bunny komt terug.

## Niet gedaan

- De USB-bedrading per board: welke xHCI's een board heeft (de RP1 op de
  Pi 5 achter de PCIe-link van de GEM, de VL805 op de Pi 4, de twee
  DWC3-cores in hostmodus op de Radxa) en de DWC3-hostmodus zelf.
  `hopos::gui::start_usb_input` meldt `HOPOS_USB_NONE`.
- De listener op 7879 op de node-stack: de socket-glue zit in
  `hopos/src/net.rs` (van een ander spoor); `gui-usbin` levert de
  transport-onafhankelijke bezorging.
- De USB-taak in de binary: `gui_usbin::Manager::step` in een lus met
  `select(verzoekenrij, after(wacht))` (de doc van `step`), de
  `Registry::bring_up` met een `make` die de `unsafe Hc::new` doet, en de
  `Deliverer::serve` achter de listener. Let op: `driver-xhci` wacht op de
  klok (zoals `driver-nvme`), dus een insteek houdt de executor tot 2 s per
  poortreset vast; Go sliep daar. Een async attach-pad lost dat op als het
  op ijzer hindert.
- De `prepare` van de O6N-hosts deelde in Go staat over alle hosts; in
  `gui-usbin` is `Prepare` een kale `fn`, dus daar komt een static of een
  trait.
- De Altra: `Board::framebuffer` doorgeven naar `board_uefi::gop_framebuffer`.
- De display-app in Rust.
