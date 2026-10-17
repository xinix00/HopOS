# The gui plane

What HopOS v3 does with a screen, a keyboard and a mouse, and what it does not.

## The decisions

1. **No GPU driver.** The kernel writes pixels into a picture that is already
   running: from the firmware (UEFI GOP, the Pi's VideoCore mailbox), from QEMU
   (`ramfb`), or on the Radxa from our own scanout chain (VOP2 and HDMI,
   because U-Boot leaves no picture there, measured 2026-08-05).
2. **The console belongs to the kernel, the screen to the display app.** At
   boot the console is on the screen: the bunny as a fixed header, three
   gauge lines on the right (kernel heap, date, time, every second), the log
   below. If a job asks for the screen, the display app gets the window in its
   cage and the console goes off; if the slot stops (a crash too), the console
   comes back. One holder at a time; the first one wins.
3. **A grant only for a device without DMA** (gui design §7). A
   framebuffer is a pixel buffer: no registers, no DMA, so it may go into
   a cage. An xHCI is a bus master and there is no IOMMU: it stays with
   the kernel, and the app gets the input as a stream (2026-08-06).
4. **Bare is bare.** Without the feature `gui` the binary links not one line
   of display code and every board is headless.

## Building

`--features gui` next to the board, for example
`cargo build --release --target aarch64-unknown-none-softfloat -p hopos --features board-rk3566,gui`.
The feature turns on `gui` on the chosen board (`board-x?/gui`) and links
`driver-fb`, `gui-fbgrant` and the USB input (`gui-usbin`, `driver-xhci`,
`driver-hid`, `driver-dwc3`).

| Crate | What it owns |
| --- | --- |
| `driver/fb` (`driver-fb`) | the console on a linear framebuffer: `Desc`, `Console` (header, log, `header_status`), no allocation |
| `kern::grants` | the `DeviceGrant` primitive: one window, one holder, `WindowMap` into stage 2, the adoption after a flip, the hooks (`Grants`) |
| `gui/fbgrant` (`gui-fbgrant`) | the policy: who gets the screen, the `FB_*` env, the console off and back |
| `gui/rkscan` (`gui-rkscan`) | the RK3566 chain: PD_VO, VOP2, DW-HDMI |
| `driver/usb/xhci`, `driver/usb/hid`, `gui/usbin` | the USB input: host, enumeration, boot HID, delivery to the display app |
| `driver/usb/dwc3` (`driver-dwc3`) | a Synopsys DWC3 core in host mode (the RK3566), in front of the xHCI |
| `board/<x>/src/usb.rs`, `Board::usb_hosts` | which controllers a board has: window, line, kind (xHCI or DWC3), and its own piece of DMA memory; PCIe and the firmware handshake are done by then |
| `hopos/src/gui.rs` | the wiring: console on the screen, gauge lines, the USB task (`usb`), the grant hooks |
| `hopos/src/net.rs` (`input`) | the input listener on `10.100.0.1:7879`: the lines to the holder of the screen |
| `applib/src/fb.rs` (`applib::fb`) | the app side: the `FB_*` env, the window Normal-NC in the app's stage 1 (`applib::mmu`), the line reader of `INPUT_ADDR` |
| `apps/display` | the display app: background, clock, the latest input and the cursor on the screen |

## Per board

Status as in the table of ALLES.md (as of 2026-10-04).

| Board | Picture | Where it comes from | Status |
| --- | --- | --- | --- |
| QEMU virt | `-device ramfb` | `board/qemuvirt/src/ramfb.rs`: fw_cfg `etc/ramfb`, 1280x800 at 0xC400_0000 | `GUI=1 sh tools/qemu-test.sh`: the console on the screen, screendump tested |
| UEFI (EDK2, QEMU) | GOP | `board/uefi/src/gop.rs`, before `ExitBootServices`; with `-device ramfb` EDK2 provides a GOP | `GUI=1 sh tools/qemu-uefi-test.sh`: GOP 800x600 at 0xfc7a_0000, the console on it, the whole chain green |
| Orion O6N | GOP from its own firmware | the one from the UEFI board (`board_uefi::On`, `board/uefi/src/gop.rs`) | console on the screen seen on hardware |
| Ampere Altra | GOP (BMC VGA) | same | console on the screen seen on hardware (A4, 2026-10-03, GOP 1024x768) |
| Raspberry Pi 4 and 5 | HDMI via the firmware | `board/raspi/src/vcfb.rs`: DTB simplefb, otherwise `FB_ALLOC` over the mailbox (1920x1080) | Pi 4: console on the screen seen on hardware. Pi 5: the firmware refuses every framebuffer since the reflash (0x80000001) |
| Radxa Zero 3E | HDMI via our own chain | `board/rk3566/src/display.rs` over `gui-rkscan` | console on the screen seen on hardware |

The USB input per board (`Board::usb_hosts`, `board/<x>/src/usb.rs`):

| Board | Controllers | How the board finds them | DMA | Status |
| --- | --- | --- | --- | --- |
| QEMU virt | `qemu-xhci` | the virt PCIe host (ECAM 0x3f00_0000, `highmem-ecam=off`), class 0x0c0330 on bus 0; we assign the BAR ourselves from the 32-bit MMIO window (no firmware) | top 2 MB of `BLK_DMA`, 0x4fe0_0000 | `GUI=display sh tools/qemu-test.sh` green |
| UEFI (EDK2, QEMU) | every xHCI on the MCFG segments | class 0x0c0330, BAR 0 from the firmware, above 1 TB via `map_device` | top 2 MB of `BLK_DMA`, split evenly | by hand 2026-09-29: two xHCIs (the ESP stick and `qemu-xhci`), keyboard, mouse, storage |
| Orion O6N | ten native xHCIs (XHC0..5, USB0..3) | platform devices in the DSDT (`PNP0D10`), on/off and host/device from the firmware's variable RAM (`GNVA`, `GNVL`); `usb_firmware.rs` reads the exact shape | top 2 MB of `BLK_DMA` (the NVMe takes the bottom 4), ten fixed pieces | seen on hardware |
| Raspberry Pi 5 | two xHCIs in the RP1 (`rp1-usb0`, `rp1-usb1`) | behind the PCIe link that the NIC probe trains; no link, no controller | 0x14a0_0000 (2 MB, `board_raspi::usb::USB_DMA`), `bus_off` 0x10_0000_0000 | built, not seen (nothing plugged in) |
| Raspberry Pi 4 | the VL805 | the BCM2711 RC up (gen 2), BAR 0 at PCIe 0xf800_0000 = CPU 0x6_0000_0000 (gigabyte 24 added as Device), with the endpoint closed, then `NOTIFY_XHCI_RESET` over the mailbox if config 0x50 is zero, wait until a version shows (at most 1 s, two chains), and only then memory decode on | 0x14a0_0000 (2 MB) | xHCI seen on hardware (P2g, 2026-10-03, enable slot with completion); HID and the display app still open |
| Radxa Zero 3E | two DWC3 cores (`usbdrd30`, `usbhost30`) | fixed SoC addresses; host mode via `driver-dwc3`, the clocks and PHYs from U-Boot | `USB_DMA` from the plan (0x06c0_0000), halved | built, not seen (nothing plugged in) |
| Ampere Altra | xHCI on PCIe, as on UEFI | the default of the UEFI board (`Platform::usb_hosts` in `board/uefi/src/on.rs`, every xHCI on the MCFG segments) | as UEFI | built, not seen (nothing plugged in) |

### What you should see on the screen

- **Radxa (HDMI).** 1920x1080p60 from our own chain: PD_VO on via the
  PMU, the HPLL and the VOP2 clocks, the VOP IOMMU off, VP0 with Smart0 scans
  `FB_RAM` (0x0700_0000, 8 MB, Normal-NC, the board's PA plan), then
  DW-HDMI with the PHY and the frame composer (DVI, no infoframes).
  The chain always drives 1080p60 and reads no EDID. The
  console gets 16x16 cells (120x67). Lines: `display:
  1920x1080p60 on HDMI (sink attached: true) HOPOS_DISPLAY_UP`, then `fb:
  console on 1920x1080 ... HOPOS_FB_CONSOLE`. If a layer fails, the line
  says which one, with the registers of that layer (`HOPOS_DISPLAY_FAIL`);
  the console then draws into a buffer nobody scans out. `HOPOS_DISPLAY_NOLATCH` = VP0 did
  not take over the config-done.
- **Pi 4 and Pi 5 (HDMI via the firmware).** The firmware turns the picture on;
  we request a buffer from it over the mailbox. Watch the depth: MEASURED
  2026-07-11 on the Pi 5 the scanout stayed at 16 bpp while the depth tag said 32;
  the pitch decides (`vcfb::from_mbox`). The buffer is outside the
  `/memory` banks; `vcfb::plan_map` sets its 2 MB blocks Normal-NC in the
  table of the first gigabyte (empty entries only). Lines: `fb:
  VideoCore framebuffer ... HOPOS_FB_UP`, `HOPOS_FB_CONSOLE`. Without an
  HDMI cable at boot the firmware may refuse: then `HOPOS_FB_NONE` and the
  node runs headless (discovery is once per boot, 2026-07-19).
- **O6N (GOP).** Whatever the firmware puts in the GOP, usually the resolution of
  the BIOS screen. Lines: `uefi: GOP WxH stride S format F @ ...
  HOPOS_UEFI_GOP` before the exit, `HOPOS_FB_CONSOLE` after it. The buffer goes
  Normal-NC into the identity map (`gop::map`, exact to 4 KB).

## The grant

A job asks for the screen with `GUI=display` in its env; that is what the
kernel reads (`FB=1` works too). Hop's `StartSpec` has no `gui` field of its
own (hop/runner, 2026-09-29): the jobspec sets it with
`"env":{"GUI":"display"}`, and Hop passes the env through unchanged. The
kernel gives the holder in the env:

| Key | Value |
| --- | --- |
| `FB_BASE` | the IPA of the buffer in the cage: `0x2000_0000` plus the offset in its 2 MB block (the buffer may sit physically above 4 GB: the ramfb finding of 2026-07-19, 0x1_bc7a_0000) |
| `FB_WIDTH`, `FB_HEIGHT` | pixels |
| `FB_STRIDE` | bytes per line |
| `FB_BPP` | 32 or 16 |
| `FB_SWAP` | `1` if red and blue swap (GOP format RGB) |
| `INPUT_ADDR` | where HOP serves the input, `10.100.0.1:7879`; only if the board names USB controllers (and withdrawn if none comes up) |

The keys, the port, the pixel rule (`FB_SWAP`, r5g6b5 at 16 bpp) and the
JSON lines of the input are a contract in `abi::glass`; the grant, the
deliverer, the console and applib read them from there.

The hooks in the lifecycle (`kern::grants::Grants`, implemented by
`hopos::gui::GuiGrants`):

| Step | Hook | Place |
| --- | --- | --- |
| after the claim, before the env on the control page | `env` | `Request::Env` to the lifecycle actor, just before `Stream::put_env` in `Stream::arm` (`kern/src/system.rs`), for a start from Hop and for the boot placement (`kern::system::place`) |
| after the cage is built, before dispatch | `arm` | `Lifecycle::arm` (`kern/src/slots.rs`); if it fails, it is a start error: ports closed, grant back |
| on adoption after a flip | `adopt` | `Lifecycle::adopt`, after all ownership claims and before the servicers |
| after a confirmed stop, and on an abort | `release` | `Lifecycle::stop` (not in quarantine) and `Lifecycle::abort` |

The actor owns the provider (`Lifecycle<…, G: Grants>`): `hopos/src/slots.rs`
passes `GuiGrants` in the gui flavor and `NoGrants` in a bare build. If the
env with the `FB_*` lines does not fit on the control page (`CTRL_ENV_MAX`),
the grant goes back and the app runs headless (`HOPOS_FB_ENV` or
`HOPOS_GRANT_ENV`).

On QEMU: `GUI=display sh tools/qemu-test.sh` puts the display app on the
staging with `hopos.appenv=GUI=display,DISPLAY_QUIT=27` and the USB devices
attached, and runs the whole life cycle: per slot `HOPOS_FB_GRANT` and
`HOPOS_FB_ARM`, the app on the screen and on the input, and after Escape
`HOPOS_FB_RELEASE` (the console back) and a second holder in slot 2 (see
the checklist).

## The display app

`apps/display` is an ordinary app on `applib`, cut down to what proves the screen and the input: a
background, a clock, the counters, the last ten input lines and a
cursor. No compositor, no `/screen.png`, no `/kvm`. What it uses from
`applib` (`applib::fb`):

1. **Reading the `FB_*` env** (`Glass::from_env`) and refusing what is wrong:
   a stride smaller than width times bytes per pixel, a BPP other than
   16 or 32, a window outside the cage's screen window.
2. **The window in the app's stage 1** (`fb::map`): since 2026-09-30 every
   app has a stage 1 (`applib::mmu`, set up in `_start`: the RAM
   declaration Normal-WB, the control page Normal-NC, the rings
   Normal-WB, SCTLR M/C/I), and `map` adds the screen as **Normal-NC** at
   4 KB (`FB_BASE` is not 2 MB aligned), in the same tables in the
   64 KB below the image (`LINK_TEXT_OFF`). Until 2026-09-30 `fb.rs` built its
   own table with SCTLR.C off and the tail as Device; why that is no longer
   needed and why the control page stays NC is in the module doc of
   `applib::mmu`. The switcher keeps the EL1 regime per resident, so a
   yield does not lose the map. Without a stage 1 (`HOPOS_APP_NO_MMU`) the
   app draws via Device, slower but correct (`HOPOS_DISPLAY_NOMAP`); if the
   MMU is on but the screen did not get mapped, it does not draw.
3. **The input**: a TCP client to `INPUT_ADDR` that reads lines
   (`LineReader`, no allocation), one JSON event per line, exactly the
   object that `POST /input` of the browser KVM accepts
   (`{"k":"key","c":..,"v":..}`, `{"k":"move","x":..,"y":..}`,
   `{"k":"btn",...}`, `{"k":"wheel",...}`, `Input::parse`); an empty line
   is a keepalive (every 5 s). Three missed keepalives or a broken
   stream means calling again with a growing pause (after a kernel flip),
   `HOPOS_DISPLAY_RECONNECT`.
4. **Its own painter** (`apps/display/src/paint.rs`) in the format of
   the env (16 or 32 bpp, `FB_SWAP`), with the font of the kernel console
   (`driver_fb::glyph`, `Desc::encode`).

Two tasks, each with one owner: the paint task owns the screen (waits for an
event or for the next second of the clock), the input task the
connection; between them an SPSC queue of values (full means drop and
count). The app stays alive; only with `DISPLAY_QUIT=<code>` in the env
does it stop on that key (for the QEMU gate, which tests the whole life
cycle of the grant that way; a jobspec does not set it). It does not wait on
the stop doorbell: on a stop from the kernel the applib heartbeat ends it
after the grace period (`HOPOS_APP_STOP`, then `HOPOS_APP_STOP_FORCED`).

Markers: `display: window 0x20000000+... mapped Normal-NC in stage-1`,
`display: glass WxH ... HOPOS_DISPLAY_UP w=W h=H`, `display: input stream
from 10.100.0.1:7879 HOPOS_DISPLAY_CONN`, and when the counters change
(at most five times per second) `display: keys=.. moves=.. buttons=..
wheel=.. dropped=.. HOPOS_DISPLAY_INPUT keys=K moves=M`.

Build and run:

```sh
cargo build --release --target aarch64-unknown-none-softfloat -p display
# QEMU with a window: click into it and type.
GUI=display APP=display APPENV=GUI=display image/qemu-run.sh
```

With Hop (the welcome jobspec, with the screen added):

```json
{"name":"display","driver":"hop",
 "artifacts":[{"url":"http://LAPTOP:8000/display.elf"}],
 "memory_limit":33554432,"env":{"GUI":"display"}}
```

## The USB input

HOP owns the controllers (an xHCI is a DMA master and there is no
IOMMU), reads boot protocol reports from keyboard and mouse and delivers them
as JSON lines to the holder of the screen. The display app calls, HOP
listens (`INPUT_ADDR` travels with the grant): that way HOP does not have to
find the app, and it checks on accept (and again per line) that the caller
is the holder's slot (the switch refuses a frame with someone else's
source MAC, 2026-08-06).

The chain, after the network (`hopos::gui::start_usb_input`):

1. `Board::usb_hosts` names the controllers: the window, the line (still
   unused: the driver polls), the kind (xHCI or DWC3) and its own piece of
   DMA memory (Normal-NC, outside kernel RAM). PCIe link, BARs and
   firmware handshake are done by then. In a bare build the list is empty.
2. The input address goes into the grant right away (`FbGrant::use_input`),
   before the USB task spawns and so before the first placement: the bring-up
   sleeps on the executor, and a display app that got the screen in the
   meantime must already have the address. If no controller comes up, the
   task withdraws it (`stop_input`, `HOPOS_USB_NONE`).
3. The USB task owns the `gui_usbin::Manager`. `Registry::bring_up` first
   quiesces every controller (probe, reset) and only then starts them, so
   that a controller left by the firmware or the previous kernel never writes
   into a piece that another one is just setting up; a DWC3 goes into host
   mode first (`driver-dwc3`). Then the round of `Manager::step`: scan (every
   500 ms), fetch reports (every 4 ms), sleep on the timer wheel.
4. Every waiting hardware step of the driver (a port reset up to 2 s, a
   command up to 1 s, the DWC3 reset of 225 ms) is `async` and sleeps on the
   task's `Timer` (`sync::Timer`, in the binary the executor's timer wheel,
   `executor::ExecTimer`). Until 2026-09-29 the driver spun on the clock, and
   plugging something in held the kernel's whole executor.
5. What the task reads goes as a value through an SPSC queue (256, full means
   drop and count) to the input listener in `net.rs`. It listens
   on `10.100.0.1:7879` as soon as the node stack is up, lets only the holder
   in, lets a new connection from the holder replace the old one, and
   writes every line with a deadline of 1 s. Without a connection the lines
   are dropped and counted (`HOPOS_INPUT_DROP`, a line the first three
   times).

The 64-bit registers (CRCR, DCBAAP, ERSTBA, ERDP) are two 32-bit halves,
written low then high (`Reg64` in `driver/usb/xhci/src/lib.rs`, like Linux
`lo_hi_writeq`), never one native 64-bit access. The lesson of 2026-09-29 on
QEMU: `qemu-xhci` latches CRCR and ERSTBA on the high word, and with the
high word written first the controller fetched its first command from
address 0 and set HCE. High, low, high then fixed QEMU, but lost the ring
state on the CIX (the O6N). On 2026-10-03 the Pi 4's VL805 showed that it
treats a native 64-bit access as one dword (CRCR read back the low word twice,
Enable Slot got no completion). Low then high works on all three: QEMU
latches on the high word, and that comes last.

## Checklist per board

Each step with the console line that goes with it. On hardware so far
(ALLES.md, as of 2026-10-04): the console on the screen on Pi 4, Radxa,
Altra and O6N; the xHCI on Pi 4 and O6N. The boxes below are the detailed
steps.

**QEMU virt**

- [x] `GUI=1 sh tools/qemu-test.sh`: `fb: ramfb 1280x800 @ 0xc4000000
  HOPOS_FB_UP`, `fb: console on 1280x800 ... (80x50 cells)
  HOPOS_FB_CONSOLE`, and the screendump (via the monitor) has well over 170,000
  text and 849,000 background pixels: the bunny, the gauge lines and the log.
  `SHOT_KEEP=pad.ppm` keeps it.
- [x] USB (`GUI=display sh tools/qemu-test.sh`, 2026-09-29, green twice):
  `usb: xHCI 1b36:000d at 00:01.0 on PCIe, registers 0x10000000+0x4000`,
  `usb: qemu-xhci xHCI 1.0, 64 slots, 8 ports`, `HOPOS_USB_UP`,
  `usb: qemu-xhci port 5: keyboard 0627:0001`, `port 6: mouse`,
  `input: listening on 10.100.0.1:7879 ... HOPOS_INPUT_UP`.
- [x] The display app: `slot 1: fb granted ... HOPOS_FB_GRANT`,
  `HOPOS_FB_ARM`, `slot 1: display: window 0x20000000+0x3e8000 mapped
  Normal-NC in stage-1`, `HOPOS_DISPLAY_UP w=1280 h=800`, `input:
  10.100.0.2 connected ... HOPOS_INPUT_CONN`, `HOPOS_DISPLAY_CONN`; then
  `sendkey a` and `mouse_move 40 30` via the monitor: `HOPOS_DISPLAY_INPUT
  keys=1 moves=1`; the screendump has well over 1,012,000 pixels in the
  app's background, 8,500 text, 55 cursor and no console.
- [x] The life cycle: `sendkey esc`, `HOPOS_DISPLAY_QUIT`, `HOPOS_SLOT_DONE
  slot=1 exit=0`, `HOPOS_FB_RELEASE`; the kernel places it again in slot
  2: `HOPOS_FB_GRANT`, `input: 10.100.0.3 connected`, and after a key and
  a move `slot 2: ... HOPOS_DISPLAY_INPUT keys=1 moves=1`.

**UEFI on QEMU (EDK2)**

- [x] `GUI=1 sh tools/qemu-uefi-test.sh` (`image/uefi-run.sh` then gives QEMU
  `-device ramfb`): `uefi: GOP 800x600 stride 3200 format 1 @ 0xfc7a0000
  HOPOS_UEFI_GOP` before the exit, `HOPOS_FB_CONSOLE` after it. The buffer is
  not 2 MB aligned, so `cpu::memattr` leaves it as `gop::map` set it
  (Normal-NC at 4 KB); the line `fb: left as the board mapped it` is then
  expected. No screendump test in this script yet.
- [x] USB, by hand (2026-09-29): `GUI=1 BOARD=uefi sh image/uefi-run.sh
  -device qemu-xhci,id=xhci -device usb-kbd,bus=xhci.0 -device
  usb-mouse,bus=xhci.0`: `usb: xhci0: xHCI 1b36:000d at 00:01.0 on PCIe,
  registers 0x800000c000+0x4000` (the ESP stick) and `xhci1` (the
  keyboard and mouse), `HOPOS_USB_UP`, `xhci0 port 1: mass storage`,
  `xhci1 port 5: keyboard`, `port 6: mouse`, `HOPOS_INPUT_UP`. No
  test in `qemu-uefi-test.sh` yet.

**Orion O6N**

- [ ] HDMI or DP on, boot from the stick with a gui image:
  `HOPOS_UEFI_GOP` with a plausible resolution.
- [ ] The bunny and the log on the screen; the time at the top right runs.
- [ ] After the exit: does the picture keep running? (The GOP buffer belongs to
  the firmware; a firmware that stops its scanout at the exit is a finding.)
- [ ] USB: `usb: N of 10 native xHCI hosts enabled by the firmware`, and
  a line per host that is off (`disabled by the firmware, or a
  device-role port`). If the parser refuses the DSDT, the line says
  why (`unsupported CIX USB firmware description: ...`): a new
  firmware is then a new measurement.
- [ ] Per host: `usb: XHCn xHCI ...` and a PORTSC line; a keyboard
  in a USB-A port gives `keyboard ...` on the port it is in.
- [ ] The display app with Hop (`"env":{"GUI":"display"}`):
  `HOPOS_DISPLAY_UP`, `HOPOS_DISPLAY_CONN`, typing gives
  `HOPOS_DISPLAY_INPUT`, and the cursor follows the mouse on the screen.

**Raspberry Pi 4 and 5**

- [ ] HDMI in port 0 before power: `fb: VideoCore framebuffer ...
  HOPOS_FB_UP` with 16 or 32 bpp.
- [ ] The bunny and the log on the screen; no stripes (stripes = a
  depth mix-up, 2026-07-11).
- [ ] No bus fault on the first sweep (the freeze hunt of 2026-08-04: a
  wrongly read address).
- [ ] Pi 5, USB: without `usb: the RP1 link is down` (the NIC probe trains
  it); `usb: rp1-usb0 xHCI ...` and `rp1-usb1`, their PORTSC lines, and a
  keyboard (2026-08-06: a Logi Bolt gave keyboard and mouse on one
  dongle) as `keyboard` and `mouse`.
- [ ] Pi 4, USB, booted COLD: `usb: vl805 firmware 0x... loaded by the
  VideoCore after N us (attempt 1, reply 0x...)` (or `already loaded` after
  a warm flip), then `usb: vl805 on PCIe (status 0xb0)`, the self-test
  `ok`, `usb: vl805 xHCI ...` and the PORTSC line. An error is
  `HOPOS_USB_VL805` (the version stayed 0; the endpoint then stays closed and the
  xHCI registers untouched) and an SError names its step with
  `HOPOS_USB_SERROR`; the full list is in docs/boards-pi.md, step 21.
- [ ] The display app with Hop: `HOPOS_DISPLAY_UP`, typing gives
  `HOPOS_DISPLAY_INPUT`; at 16 bpp (the Pi 5 depth of 2026-07-11) the
  background colour is right (dark green).

**Radxa Zero 3E**

- [ ] HDMI on: `HOPOS_DISPLAY_UP` and `sink attached: true`.
- [ ] A picture at 1080p60; the bunny and the log.
- [ ] Without a monitor: `sink attached: false`, no hang.
- [ ] USB: per core `usb: usbdrd30 dwc3 id=5533.... gctl=...` (GSNPSID zero
  = not clocked: the CRU gates and the PHYs are from U-Boot, and then that is
  the measurement), then the xHCI line and PORTSC. A keyboard in the USB-A
  (`usbhost30`) gives `keyboard`.

**Every board, with Hop**

- [ ] A job with `"env":{"GUI":"display"}`: `HOPOS_FB_GRANT` and the console
  leaves the screen; with the display app `HOPOS_DISPLAY_UP` and, with USB,
  `HOPOS_DISPLAY_CONN`.
- [ ] Stopping the job: `HOPOS_FB_RELEASE` and the bunny comes back; the
  input listener closes the old slot's stream on the next line or
  keepalive (within 5 s), or as soon as a new holder calls
  (`HOPOS_INPUT_GONE`).

## Not done

- On hardware only the console on the screen (Pi 4, Radxa, Altra, O6N) and
  the xHCI (Pi 4, O6N) are marked as seen in ALLES.md; the rest is built and
  not run. The Pi 5 screen is blocked by its firmware (0x80000001 since the
  reflash).
- Interrupts: every controller has an `irq` in its `UsbHost`, but the
  driver polls (every 4 ms); the lines (RP1 MSI-X via the MIP, the
  GIC SPIs of the Radxa, the `_CRS` of the O6N) are not wired yet.
- Storage on USB: the manager reports a drive (`mass storage vid:pid`), and
  with the feature `media` the USB sink hands the bulk transfers to the
  optical owner (`hopos/src/optical.rs`, see docs/media.md); without `media`
  those hooks do nothing.
- The test of `INPUT_ADDR` after a kernel flip on hardware: the display calls
  again after 15 s of silence; on QEMU only the change of holder is tested.
- `tools/qemu-uefi-test.sh` has no `GUI=display` mode yet; the
  UEFI chain was run by hand (see the checklist).
- The O6N check that the firmware's variable RAM lies in the ACPI RAM of the
  EFI memory map; the UEFI board does not pass that map on.
