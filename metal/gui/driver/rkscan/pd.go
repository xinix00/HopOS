package rkscan

import (
	"time"

	"github.com/xinix00/HopOS/metal/dev"
)

// Power domains van de RK3566. Nodig voor het beeld: PD_VO draagt de VOP2 en de
// HDMI-TX, en zonder dat domein aan zijn hun registers dood — niet fout, dóód.
// Dat is een andere faalmodus dan een dichte klok (die leest nullen); een
// afgeschakeld domein kan de bus vasthouden.
//
// REFERENTIE (opgehaald 05-08): Linux v6.13 drivers/pmdomain/rockchip/pm-domains.c
// (rk3568_pmu-offsets, rk3568_pm_domains-tabel, rockchip_pd_power en de twee
// helpers eronder) plus rk356x-base.dtsi voor het PMU-basisadres.
const (
	pmuBase = 0xFDD90000 // rk356x-base.dtsi: power-management@fdd90000

	// De vier registers die één domein aan- of uitzetten (rk3568_pmu).
	pmuPwr    = 0xA0 // schrijven: 0 = aan, 1 = uit (hiword-masked)
	pmuStatus = 0x98 // lezen: 0 = aan, 1 = uit
	pmuReq    = 0x50 // NIU-idle-request (hiword-masked)
	pmuAck    = 0x60 // bevestiging van de request
	pmuIdle   = 0x68 // de werkelijke idle-stand

	// PD_VO (rk3568_pm_domains: DOMAIN_RK3568("vo", BIT(7), BIT(4), false)).
	// DOMAIN_RK3568 → DOMAIN_M, dus pwr en req zijn hiword-masked: het
	// maskerbit zit 16 posities hoger. status gebruikt hetzelfde bit als pwr,
	// idle en ack hetzelfde als req.
	pdVOPwrBit = 7
	pdVOReqBit = 4
)

// PowerOnVO zet het video-domein aan. Idempotent: staat het al aan, dan doet
// deze functie niets behalve de klokken openzetten.
//
// De volgorde komt uit rockchip_pd_power(pd, true) en is niet vrij:
//
//  1. de klokken van het domein moeten LOPEN tijdens het schakelen — dat is
//     waarom clk_bulk_enable er in de driver omheen staat en waarom we hier
//     VOPClockOn() eerst doen;
//  2. het domein aanzetten (pwr-bit naar 0) en wachten tot status het meldt;
//  3. de NIU-idle-request LOSLATEN en wachten op ack + idle. Sla je die stap
//     over, dan staat het domein aan maar blijft de interconnect het isoleren:
//     registers lezen dan nullen en de DMA komt nooit bij DRAM.
func PowerOnVO() error {
	VOPClockOn()

	if dev.Read32(pmuBase+pmuStatus)&(1<<pdVOPwrBit) != 0 {
		// Uit → aanzetten. Schrijven is hiword-masked: alleen het maskerbit
		// zetten en de waarde op 0 laten betekent "aan".
		dev.Write32(pmuBase+pmuPwr, 1<<(pdVOPwrBit+16))
		dev.MB()
		if err := pmuWait("PD_VO power", pmuStatus, 1<<pdVOPwrBit, 0); err != nil {
			return err
		}
	}

	// Idle-request los (waarde 0, masker gezet), dan wachten op ack én idle.
	dev.Write32(pmuBase+pmuReq, 1<<(pdVOReqBit+16))
	dev.MB()
	if err := pmuWait("PD_VO idle-ack", pmuAck, 1<<pdVOReqBit, 0); err != nil {
		return err
	}
	return pmuWait("PD_VO idle", pmuIdle, 1<<pdVOReqBit, 0)
}

// pmuWait pollt begrensd tot een veld de verwachte waarde heeft. Begrensd en
// niet eeuwig: een domein dat niet schakelt mag de boot niet ophouden, en de
// foutmelding draagt de rauwe registerinhoud zodat één boot genoeg is om te
// weten wélke stap bleef hangen.
func pmuWait(what string, off uintptr, mask, want uint32) error {
	deadline := time.Now().Add(10 * time.Millisecond)
	for {
		v := dev.Read32(pmuBase + off)
		if v&mask == want {
			return nil
		}
		if time.Now().After(deadline) {
			return &pmuError{what: what, off: off, got: v, mask: mask, want: want}
		}
	}
}

type pmuError struct {
	what            string
	off             uintptr
	got, mask, want uint32
}

func (e *pmuError) Error() string {
	return "rk3566: " + e.what + " did not settle (pmu+" + hex(uint32(e.off)) +
		" = " + hex(e.got) + ", masked " + hex(e.got&e.mask) + ", want " + hex(e.want) + ")"
}

// hex zonder fmt: een foutmelding is geen reden om fmt dit pakket in te
// slepen (geërfd uit board/rk3566, waar de app-kant meelinkte; de gewoonte
// blijft goedkoop).
func hex(v uint32) string {
	const d = "0123456789abcdef"
	b := []byte("0x00000000")
	for i := 0; i < 8; i++ {
		b[9-i] = d[v&0xF]
		v >>= 4
	}
	return string(b)
}

// PowerVOInfo geeft de drie relevante PMU-registers terug voor het
// meetinstrument.
func PowerVOInfo() (status, ack, idle uint32) {
	return dev.Read32(pmuBase + pmuStatus),
		dev.Read32(pmuBase + pmuAck),
		dev.Read32(pmuBase + pmuIdle)
}
