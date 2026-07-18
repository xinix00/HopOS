// Package smpro leest de SoC-sensoren van Ampere's systeembeheer-processor
// (SMpro) via een ACPI PCC-kanaal — het Altra-equivalent van de Pi's
// VideoCore-mailbox (vcmail.Temp). Linux-referenties:
// drivers/hwmon/xgene-hwmon.c (het berichtformaat: de Altra-SMpro spreekt de
// SLIMpro-taal van zijn APM X-Gene-voorouder) en drivers/mailbox/pcc.c (de
// doorbell-handshake). Eén bewuste afwijking: wij pollen op CMD_COMPLETE in
// plaats van de platform-interrupt af te wachten — één temperatuur per minuut
// rechtvaardigt geen GIC-bedrading.
package smpro

import (
	"time"

	"hop-os/metal/dev"
	"hop-os/metal/fw/acpi"
)

// HwmonChannel is het PCC-kanaal van de hardware-monitor op socket 0 — vast
// in alle Altra-firmware (edk2-platforms Dsdt.asl: device APMC0D29 met _DSD
// "pcc-channel"=14; identiek bij ADLINK/Jade/ASRock. Kanaal 29 = socket 1).
// Wij lezen géén AML, dus dit is de ene firmware-constante; een platform
// waar hij niet klopt is onschuldig — de proef-read faalt en telemetrie
// blijft uit.
const HwmonChannel = 14

const (
	pccSignature  = 0x50434300 // "PCC" | kanaalnummer (include/acpi/pcc.h)
	cmdGenDBInt   = 1 << 15    // command: "interrupt het OS bij antwoord" — het door SMpro geteste Linux-pad; de SPI verzuipt in onze uitstaande GIC
	stCmdComplete = 1 << 0     // status: platform is klaar met dit commando

	sensorRdMsg = 0x04FFE902 // DBG | SENSOR_READ | handle (xgene-hwmon)
	socTempReg  = 0x10       // de SoC-temperatuursensor
	invalidBit  = 1 << 15    // antwoorddata: sensor (nog) niet geldig
	msgTypeErr  = 7          // antwoordtype in bits 31:28: SMpro-fout
)

// Dev is één open PCC-kanaal naar de SMpro. De aanroeper heeft shmem en
// doorbell al bereikbaar gemaakt (uefi.MapHigh) — dit package kent het
// board niet.
type Dev struct {
	ch uint32
	ss acpi.PCCSubspace
}

func New(ch int, ss acpi.PCCSubspace) *Dev { return &Dev{ch: uint32(ch), ss: ss} }

// SoCTemp geeft de SoC-temperatuur in milligraden Celsius (de vcmail.Temp-
// vorm). De sensor meldt hele graden als 9-bit two's-complement.
func (d *Dev) SoCTemp() (mC int, ok bool) {
	r0, r1, ok := d.call(sensorRdMsg, socTempReg, 0)
	if !ok || r0>>28 == msgTypeErr || r1&invalidBit != 0 {
		return 0, false
	}
	t := int32(r1<<23) >> 23 // tekenbit is bit 8 (xgene TEMP_NEGATIVE_BIT)
	return int(t) * 1000, true
}

// call doet één synchroon bericht: header+payload in het gedeelde geheugen,
// doorbell, pollen op CMD_COMPLETE. Het gedeelde geheugen ligt buiten onze
// RAM-declaratie en is dus device-gemapt: uitgelijnde dev-accesses verplicht
// (zelfde regel als fw/acpi). Eén aanroeper (de telemetrie-goroutine), dus
// geen lock.
func (d *Dev) call(m0, m1, m2 uint32) (r0, r1 uint32, ok bool) {
	sh := uintptr(d.ss.ShmemBase)
	dev.Write32(sh, pccSignature|d.ch)
	// command (u16 @4) en status (u16 @6) in één uitgelijnde 32-bit write:
	// commandtype = bits 31:28 van het bericht, CMD_COMPLETE gewist, de
	// overige status-bits bewaard.
	keep := dev.Read32(sh+4) &^ (0xFFFF | stCmdComplete<<16)
	dev.Write32(sh+4, keep|m0>>28|cmdGenDBInt)
	dev.Write32(sh+8, m0)
	dev.Write32(sh+12, m1)
	dev.Write32(sh+16, m2)
	d.ring()

	// Linux budgetteert 500× de PCCT-latentie; zelfde budget met een vloer,
	// en een 1ms-slaap per poll zodat de HOP-core coöperatief blijft (de
	// SMpro antwoordt in praktijk in microseconden).
	budget := time.Duration(d.ss.LatencyUS) * 500 * time.Microsecond
	if budget < 50*time.Millisecond {
		budget = 50 * time.Millisecond
	}
	deadline := time.Now().Add(budget)
	for dev.Read32(sh+4)&(stCmdComplete<<16) == 0 {
		if time.Now().After(deadline) {
			return 0, 0, false
		}
		time.Sleep(time.Millisecond)
	}
	return dev.Read32(sh + 8), dev.Read32(sh + 12), true
}

// ring belt het doorbell-register: read-modify-write met de PCCT-maskers
// (de pcc.c-semantiek). De Altra meldt een 32-bit register; 64 voor de
// volledigheid van de GAS.
func (d *Dev) ring() {
	db := uintptr(d.ss.Doorbell)
	if d.ss.DBWidth == 64 {
		dev.Write64(db, dev.Read64(db)&d.ss.Preserve|d.ss.Write)
		return
	}
	dev.Write32(db, uint32(uint64(dev.Read32(db))&d.ss.Preserve|d.ss.Write))
}
