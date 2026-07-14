// Package raspi is de gedeelde board-laag voor de Raspberry Pi-familie
// (BCM2711 = Pi 4, BCM2712 = Pi 5). Beide boards booten identiek: de
// firmware laadt een raw image op 0x80000 (de Pi 5-EEPROM negeert
// kernel_address — gemeten 2026-07-09; de Pi 4 laadt daar per default) en
// levert ons op EL2 af met TF-A/armstub op EL3 → PSCI via SMC.
//
// Taakverdeling met de dunne board-pakketten (board/rpi4, board/rpi5):
//
//   - hier: PSCI/SMCCC (+ psci.s), MPIDR-read, generic timers + idle, de
//     runtime-hooks RamStackOffset/Hwinit1/Nanotime, het park/scratch-
//     geheugenplan en de park-code-generator;
//   - board: UART (printk + cpuinit.s — die moeten vóór init() al werken,
//     dus geen parametrisatie via variabelen), MPIDR-nummering (A72: aff0,
//     A76: aff1 → target/CoreID), GIC-basis, RNG en de board.Board-glue.
//
// Alleen voor GOOS=tamago GOARCH=arm64.
package raspi

import (
	"strings"

	_ "unsafe"

	"github.com/usbarmory/tamago/arm64"

	"hop-os/metal/abi/layout"
	"hop-os/metal/cpu/idle"
	"hop-os/metal/dev"
	"hop-os/metal/fw/fdt"
)

// Gedeeld geheugenplan ONDER 0x80000 — want (gemeten 2026-07-09): de Pi 5-
// EEPROM-bootloader negeert kernel_address en laadt raw images op 0x80000;
// de Pi 4 laadt op 0x200000 (kernel_address gehonoreerd) dus voor die board
// ligt dit gebied nog ruimer onder de load. Boven het TF-A/armstub-gebied
// (< ~0x20000) blijven.
const (
	// ParkBase/ParkCount: park-code voor secundaire cores en hun
	// levensteken-tellers (geplant door de probes, zie ParkCode).
	ParkBase  = 0x70000
	ParkCount = 0x78000

	// VCMailBuf: de property-buffer voor de firmware-mailbox (metal/driver/vcmail).
	// 16-byte-gealigneerd, laag DRAM (VC-bereik), buiten elke RAM-declaratie
	// (ongecachet), en vrij van park/scratch (0x70000-0x78040, 0x7F000+).
	VCMailBuf = 0x7E000

	// BootScratch: cpuinit.s van het board schrijft er het boot-EL (vóór
	// de drop naar EL1); BootEL() leest het. Moet gelijk zijn aan de
	// BOOT_SCRATCH-#define in de cpuinit.s van beide boards.
	BootScratch = 0x7F000

	// HopKernelStart/HopKernelSize: het laadadres en de grootte van de
	// HOP-kern-RAM-declaratie (mem_raspi.go: raw load op 0x80000, 128MB). De
	// go:linkname ramStart/ramSize-vars in élk Pi-main-pakket (agent,
	// multikernel, probe4/5/6/7) initialiseren hierop — één bron voor de
	// kern-omvang i.p.v. de losse 0x00080000/0x08000000-literals.
	HopKernelStart = 0x80000
	HopKernelSize  = 0x8000000 // 128MB

	// HopKernelEnd: het einde van de HOP-kern-regio (= start + size). Alles
	// eronder — de HOP-runtime, TF-A/armstub (laadt op 0x0), park/scratch
	// (0x70000-0x7F008) — is voor de partitie-pool verboden terrein.
	HopKernelEnd = HopKernelStart + HopKernelSize
)

// DTB geeft de DTB-pointer die de firmware in x0 meegaf: cpuinit.s legde die op
// het scratch-woord (BootScratch+8) — dus eerst dereferencen. Board-neutraal
// (de scratch-plek is gelijk op Pi 4 en Pi 5, zie de cpuinit.s-#defines).
func DTB() uintptr { return uintptr(dev.Read64(BootScratch + 8)) }

// DTBPool berekent de partitie-pool uit het volledige DRAM van dit board (de
// DTB /memory-banken, ook boven 4GB) minus alle vaste regio's: de HOP-kern,
// de control-regio's van het plan (ctrl/ring/stage-2/net-ring), de DTB zelf en
// de firmware-/memreserve/-blokken. Board-neutraal (Pi 4 en Pi 5 identiek).
// Leeg als de DTB onleesbaar is — de aanroeper valt dan terug op een
// conservatieve vaste pool i.p.v. fantoom-RAM uit te delen.
func DTBPool(dtbPtr uintptr, p layout.Plan) []layout.Region {
	memregs, ok := fdt.MemRegions(dtbPtr)
	if !ok {
		return nil
	}
	banks := make([]layout.Region, 0, len(memregs))
	for _, r := range memregs {
		banks = append(banks, layout.Region{Base: r.Addr, Size: r.Size})
	}

	holes := []layout.Region{
		{Base: 0, Size: HopKernelEnd}, // HOP-kern + laag geheugen (TF-A/scratch/park)
		{Base: p.CtrlPA, Size: uint64(layout.MaxSlots+1) * layout.CtrlStride},
		{Base: p.RingPA, Size: uint64(layout.MaxSlots) * layout.RingStride},
		{Base: p.Stage2PA, Size: uint64(layout.MaxSlots+1) * layout.Stage2Stride},
	}
	if p.NetDMAPA != 0 {
		holes = append(holes, layout.Region{Base: p.NetDMAPA, Size: layout.NetDMASize})
	}
	if sz := fdt.BlobSize(dtbPtr); sz > 0 {
		holes = append(holes, layout.Region{Base: uint64(dtbPtr), Size: sz})
	}
	for _, r := range fdt.MemReserve(dtbPtr) {
		holes = append(holes, layout.Region{Base: r.Addr, Size: r.Size})
	}
	return layout.CarvePool(banks, holes, 2<<20)
}

// BootParam leest een boot-parameter (key=value) van de cmdline: op de Pi
// is dat cmdline.txt op de SD-kaart, door de firmware in /chosen/bootargs
// gezet — node-configuratie zonder rebuild (Derek, 2026-07-11). Sleutels
// zijn hopos.*-geprefixt zodat Linux-restanten op de kaart onschadelijk
// zijn. Leeg = niet aanwezig.
func BootParam(dtb uintptr, key string) string {
	args, ok := fdt.Bootargs(dtb)
	if !ok {
		return ""
	}
	for _, tok := range strings.Fields(args) {
		if v, found := strings.CutPrefix(tok, key+"="); found {
			return v
		}
	}
	return ""
}

// SerialSuffix geeft de laatste 8 hexcijfers van het board-serial uit de
// DTB (/serial-number) — de stabiele node-identiteit (node-ID, MAC). Leeg
// bij een onleesbaar serial; de aanroeper kiest dan zijn terugval.
func SerialSuffix(dtb uintptr) string {
	s, ok := fdt.RootString(dtb, "serial-number")
	if !ok || len(s) < 8 {
		return ""
	}
	return s[len(s)-8:]
}

// MACFromSerial bouwt een stabiel, lokaal beheerd MAC-adres (02:48 = "H")
// uit het board-serial dat de firmware in de DTB zet (/serial-number,
// "10000000xxxxxxxx"): uniek per board, gelijk over elke boot — precies wat
// een DHCP-server nodig heeft om dezelfde lease terug te geven. Bij een
// onleesbaar serial: een vaste terugval met het gegeven slotbyte.
func MACFromSerial(dtb uintptr, fallback byte) [6]byte {
	mac := [6]byte{0x02, 0x48, 0x4f, 0x50, 0x00, fallback} // "HOP" + terugval
	s, ok := fdt.RootString(dtb, "serial-number")
	if !ok || len(s) < 8 {
		return mac
	}
	// De laatste 8 hexcijfers → 4 bytes (mac[2:6]); één krom teken = terugval.
	var b [4]byte
	for i, c := range s[len(s)-8:] {
		var v byte
		switch {
		case c >= '0' && c <= '9':
			v = byte(c - '0')
		case c >= 'a' && c <= 'f':
			v = byte(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = byte(c-'A') + 10
		default:
			return mac
		}
		b[i/2] = b[i/2]<<4 | v
	}
	copy(mac[2:], b[:])
	return mac
}

// ARM64 core-instantie (zelfde constructie als board/qemuvirt).
var ARM64 = &arm64.CPU{
	TimerOffset: 1,
}

//go:linkname ramStackOffset runtime/goos.RamStackOffset
var ramStackOffset uint = 0x100

// hwinit1: post-World lagere-laag-init. CNTFRQ is door de firmware/TF-A
// gezet; InitGenericTimers(0, 0) berekent alleen de TimerMultiplier.
//
//go:linkname hwinit1 runtime/goos.Hwinit1
func hwinit1() {
	// 'H'-marker (rauwe DR-poke, geen FIFO-poll): de runtime leeft en is
	// door rt0 + Hwinit0 (tamago's vroege MMU-init) heen — bisect-punt
	// tussen 'R' (cpuinit) en de main-banner. ALLEEN op de primaire core
	// (MPIDR-affiniteit 0 — dekt A72-aff0 én A76-aff1): een app-core draait
	// onder stage-2 zonder mapping voor het UART-adres, dus dezelfde poke zou
	// een app daar bij boot meteen zijn core kosten (fault → CPU_OFF).
	if MPIDR()&0xFFFFFF == 0 {
		dev.Write32(0x107d001000, 'H')
	}

	ARM64.Init()
	ARM64.EnableCache()
	ARM64.InitGenericTimers(0, 0)
	idle.Enable() // ná Init (die zet de default governor)
}

//go:linkname nanotime runtime/goos.Nanotime
func nanotime() int64 {
	return ARM64.GetTime()
}

// mpidr leest MPIDR_EL1 (cpu_arm64.s).
func mpidr() uint64

// MPIDR geeft het rauwe register; de nummering (aff0 op de A72, aff1 op de
// A76) is boardspecifiek — zie CoreID in het board-pakket.
func MPIDR() uint64 { return mpidr() }

// cntfrq/cntpct lezen de generic-timer-registers (cpu_arm64.s).
func cntfrq() uint64
func cntpct() uint64

// CNTFRQ geeft de counterfrequentie die de firmware zette (verwacht 54MHz op
// de Pi; 0 = niet gezet → tamago's timers en time.Sleep zijn dan dood).
func CNTFRQ() uint64 { return cntfrq() }

// CNTPCT geeft de rauwe fysieke counter. Kan trappen als EL1PCTEN uit staat
// (zie cpu_arm64.s) — kondig de read aan vóór je hem doet.
func CNTPCT() uint64 { return cntpct() }

// spin (cpu_arm64.s) draait n afhankelijke SUBS-iteraties.
func spin(n uint64)

// Spin brandt n CPU-cycli (±dual-issue-marge) — met CNTPCT eromheen is dat
// de kloksnelheidsmeting van de probes.
func Spin(n uint64) { spin(n) }
