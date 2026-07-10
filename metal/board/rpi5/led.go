package rpi5

// De groene ACT-LED ("2712_STAT_LED") hangt op de always-on-GPIO-bank van de
// BCM2712 zelf — níét achter de RP1 — en is daarmee een blind levensteken voor
// de bring-up-probe (cmd/probe5) zolang de debug-UART geen kabel heeft. De
// productie-multikernel (pi5_main) gebruikt 'm niet: die logt via UART/ring.
// Bron: bcm2712-rpi-5-b.dtb — leds/led-act: gpios = <&gio_aon 9 ...>,
// gio_aon = gpio@7d517c00 (brcm,brcmstb-gpio, bank 0 = 17 pinnen).
//
// brcmstb-GIO-registerlayout per bank van 0x20 (Linux gpio-brcmstb.c):
// +0x00 ODEN (open-drain), +0x04 DATA, +0x08 IODIR (1 = input).

import "hop-os/metal/dev"

const (
	gioAonBase = 0x107d517c00 // bank 0; DTB: soc@107c000000/gpio@7d517c00
	gioODEN    = gioAonBase + 0x00
	gioDATA    = gioAonBase + 0x04
	gioIODIR   = gioAonBase + 0x08

	actLEDBit = uint32(1) << 9 // pin 9
)

// LEDInit zet de ACT-LED-pin als push-pull-output. Read-modify-write: op
// dezelfde bank leven o.a. RP1_RUN (pin 2) en de SD-power-pinnen — afblijven.
func LEDInit() {
	dev.Write32(gioODEN, dev.Read32(gioODEN)&^actLEDBit)
	dev.Write32(gioIODIR, dev.Read32(gioIODIR)&^actLEDBit)
}

// LED zet de groene ACT-LED aan of uit. LET OP: op het board (gemeten
// 2026-07-10 aan de hartslag) blijkt de pin niet-geïnverteerd te lichten —
// bit HÓÓG = aan, ondanks de DTB-`ACTIVE_LOW`-annotatie (de pad/driver keert
// 'm blijkbaar om). Dus aan = bit zetten.
func LED(on bool) {
	d := dev.Read32(gioDATA)
	if on {
		d |= actLEDBit
	} else {
		d &^= actLEDBit
	}
	dev.Write32(gioDATA, d)
}
