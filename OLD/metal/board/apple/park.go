//go:build tamago && arm64

package apple

import "github.com/xinix00/HopOS/metal/v2/dev"

// ParkEntryPC is het fysieke startadres van de parkeerlus (park_arm64.s) dat
// als entry naar Release gaat — zelfde vorm als rk3566.ParkEntryPC.
func ParkEntryPC() uint64

// Levenstekenwoorden: per core één, op WakeBase — buiten de RAM-declaratie en
// dus device-gemapt (zie de toelichting bij WakeBase). De HOP-core nult het
// woord vóór Release en pollt daarna; de gewekte core schrijft er zijn MPIDR
// in. Eén woord dat twee vragen beantwoordt: kwam hij op, en klopt m1n1's
// MPIDR-tabel met wat de core zelf zegt.
func WakeSlot(core int) uintptr { return uintptr(WakeBase) + uintptr(core)*8 }

// ClearWake nult het levenstekenwoord van een core.
func ClearWake(core int) { dev.Write64(WakeSlot(core), 0) }

// Wake leest het levenstekenwoord (0 = de core is nooit aangekomen).
func Wake(core int) uint64 { return dev.Read64(WakeSlot(core)) }

// In regs_arm64.s: systeemregisters die de probe rapporteert.
func ReadTCR() uint64
func ReadTTBR0() uint64
func ReadSCTLR() uint64
func ReadMMFR0() uint64
func ReadMMFR1() uint64
func ReadMMFR4() uint64
func CNTFRQ() uint64
func CNTPCT() uint64
func ReadCNTKCTL() uint64

// ESR/FAR van EL1: de reden en het adres van de laatste exceptie.
func ReadESR() uint64
func ReadFAR() uint64

func timerFires(ticks uint64) uint64

// TimerProbe zet de fysieke timer op `ticks`, wacht POLLEND tot hij afgaat en
// leest dan ISR_EL1. fired = de timer ging af; isr = wat er op dat moment aan
// deze core pending stond (bit 6 = FIQ, bit 7 = IRQ).
//
// Dit is de veilige versie van de vraag "wekt WFI hier eigenlijk?". Die vraag
// rechtstreeks stellen kost een eeuwige slaap als het antwoord nee is; ISR_EL1
// geeft hem zonder te slapen, want die staat los van DAIF.
func TimerProbe(ticks uint64) (fired bool, isr uint64) {
	v := timerFires(ticks)
	return v>>32&1 != 0, v & 0xFFFFFFFF
}

func ReadSPRRConfig() uint64

// Idle-mechanica (meting, zie regs_arm64.s).
func wfeBurst(n uint64) uint64
func wfiTimer(ticks uint64) uint64

// WFEBurst/WFITimer zijn de geëxporteerde meetfuncties voor de probe.
func WFEBurst(n uint64) uint64 { return wfeBurst(n) }
func WFITimer(t uint64) uint64 { return wfiTimer(t) }

// hvcKick: zie regs_arm64.s.
func hvcKick(target uint64)

// Kick wekt cpu met een fast IPI — m1n1's wek op precies dit silicium
// (smp.c: deep_wfi + IPI_RR_GLOBAL + IPI_SR-ack). De IPI komt op die core
// binnen als FIQ; de app-core heeft HCR.FMO en cpu/el2/switch.s ackt hem op
// EL2 en keert terug, zodat de app alleen zijn WFI ziet terugkeren. Dit is
// board.Cores.Kick; wie slaapt en wanneer beslist HOP's wekker.
func Kick(cpu int) {
	cpus := CPUs()
	if cpu < 0 || cpu >= len(cpus) {
		return
	}
	hvcKick(uint64(cpus[cpu].Core) | uint64(cpus[cpu].Cluster)<<16)
}
