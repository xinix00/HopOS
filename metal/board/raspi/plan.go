package raspi

import (
	"fmt"

	"hop-os/metal/abi/layout"
	"hop-os/metal/fw/fdt"
)

// revokeVecAsm = de EL2-vectortabel (faultdump2, 0x8B000) die cpuinit.s
// (cpuinit_body.h, gedeeld door Pi 4 en Pi 5) al voor de boot-diagnostiek
// installeert en waar VBAR_EL2 van core 0 op staat. De revoke-HVC-handler
// wordt daar door stage2.InitVectors ingeplugd (offset 0x400 — sync vanuit
// lager EL); de andere 15 vectoren blijven de Y-dump.
const revokeVecAsm = 0x8B000

// SetupPlan is de gedeelde board-init van de Pi-familie: RNG200/watchdog-bases
// bekendmaken en het PA-plan zetten. Het plan is op de Pi 4 en Pi 5 numeriek
// identiek (zelfde lage-DRAM-indeling, zelfde cpuinit-vectortabel); alleen de
// MMIO-bases verschillen — dat zijn de parameters.
//
// Alleen de HOP-core (MPIDR-aff 0) doet het werk: het plan lezen vergt de DTB
// fysiek (0x7F008 + de blob), en dat adres bestaat niet in de kooi van een
// app-core (die draait onder stage-2). Een app-core heeft het plan ook niet
// nodig — HOP bezit het en gebruikt de *PA-accessors; de app kent alleen de
// IPA-constanten. Zonder deze guard faultt elke app bij zijn eigen board-init
// (gemeten 2026-07-10: far=0x7f008). De MMIO-bases staan óók achter de guard:
// appspike linkt het board, dus een app draait deze init ook — met RNG200Base
// gezet zou getRandomData in de app naar MMIO wijzen dat zijn stage-2-kooi
// niet mapt (fault bij de eerste crypto/rand); op 0 valt hij terug op de PRNG.
func SetupPlan(rng200, watchdog uintptr) {
	if MPIDR()&0xFFFFFF != 0 {
		return
	}
	RNG200Base = rng200
	WatchdogBase = watchdog
	p := layout.Plan{
		NodeCtrlPA:    0x10000000,
		Stage2PA:      0x12000000,
		RevokeVecPA:   revokeVecAsm,
		BootScratchPA: BootScratch, // 0x7F000, cpuinit-vast
		NetDMAPA:      0x14000000,  // NIC-DMA-ringen/buffers (buiten RAM-decl → ongecachet)
	}
	// De pool = het volledige DRAM (DTB /memory, ook boven 4GB) minus de vaste
	// regio's. Faalt de DTB-lezing, val terug op een conservatieve vaste pool
	// (512MB, past in élke Pi) — nooit fantoom-RAM uitdelen. Die terugval is
	// LUID: op een Pi met geldige DTB hoort dit pad nooit te lopen, dus als het
	// wél loopt (kromme/afwezige blob) mag het niet stil degraderen.
	dtb := DTB()
	p.Pool = DTBPool(dtb, p)
	if len(p.Pool) == 0 {
		fmt.Printf("WAARSCHUWING HOPOS_POOL_FALLBACK: geen bruikbare DTB /memory (dtb=%#x, geldig=%v) — partitie-pool valt terug op de vaste 512MB [0x20000000,0x40000000); de RAM-sanity draait dan op het layout, niet op gemeten RAM\n",
			dtb, fdt.Valid(dtb))
		p.Pool = []layout.Region{{Base: 0x20000000, Size: 0x20000000}}
	}
	layout.UsePlan(p)
}
