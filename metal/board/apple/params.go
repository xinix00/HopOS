package apple

import (
	"sync"

	"github.com/xinix00/HopOS/metal/dev"
	"github.com/xinix00/HopOS/metal/fw/bootcfg"
)

// Het param-blok: wat de LOADER weet en de firmware niet vertelt.
//
// Dat is sinds 29-08 nog maar twee dingen, en dat is met opzet. Alles wat in
// iBoot's boot_args of in de device tree staat leest het board zelf
// (firmware.go, adt.go) — UART-bases, DRAM, framebuffer, de opslag, de cores.
// Eerder stond dat allemaal óók in dit blok, met per accessor een terugval, en
// dan zijn er twee antwoorden op elke vraag. Wat de loader als enige weet:
//
//   - m1n1's spin-table: de release-adressen van de cores die HIJ startte. Die
//     tabel staat in zijn geheugen en er is geen andere manier om hem te
//     vinden dan zijn ELF-symbolen. Zodra wij het bootobject zijn is er geen
//     spin-table meer en starten we cores zelf (cpustart.go).
//   - de hop: wélke core HopOS hoort te bewonen. Dat is een keuze, geen feit.
//
// Ontbreekt het blok, dan is dat de normale toestand van een node die zonder
// loader boot. Pariteit: PARAM_* in cpuinit.s, en de struct.pack in
// image/apple/load-probe.py.
const (
	// "HOPAPPLE", little-endian als één woord.
	ParamMagic   = 0x454C505041504F48
	ParamVersion = 5

	paramMagic   = 0x00
	paramVersion = 0x08
	paramBootCPU = 0x10 // m1n1's index van de core waar de firmware ons afleverde
	// De hop. Release 0 = de loader wees er geen aan; dan blijft HOP waar de
	// firmware hem startte. Het MPIDR kan die vraag niet beantwoorden: dat van
	// cpu0 is echt 0, en cpuinit.s vergelijkt ermee vóór de eerste Go-regel.
	paramHopCPU     = 0x18
	paramHopMPIDR   = 0x20
	paramHopRelease = 0x28
	paramRelease    = 0x30 // + 8×cpu: spin-table-release-adres (0 = niet gestart)
	paramEnd        = 0xB0

	// MaxCPUs is de breedte van de per-core tabel in het blok.
	MaxCPUs = 16

	// noHopCPU: "geen core aangewezen". Ook de waarde die de loader schrijft.
	noHopCPU = 0xFFFF
)

// CfgBase/CfgSize: de platform-config als tekst (`key=waarde`-regels, het
// hopos.cfg-formaat van fw/bootcfg), door de loader neergelegd — dit board heeft
// geen bootargs en geen initrd. Zonder loader reist de config mee in het image
// (cmd/hopos/cfgblob). 4KB is ruim voor node/cluster/apikey/jobspecs.
const (
	CfgBase = RamBase + 0xF000
	CfgSize = 0x1000
)

// P is het gelezen param-blok.
type P struct {
	BootCPU int
	Hop     struct {
		CPU     int
		MPIDR   uint64
		Release uint64
	}
	Release [MaxCPUs]uint64
}

// Params leest het blok; ok=false als het magic ontbreekt — een node zonder
// loader, en dat is geen fout.
func Params() (p P, ok bool) {
	if dev.Read64(ParamBase+paramMagic) != ParamMagic || dev.Read64(ParamBase+paramVersion) != ParamVersion {
		return p, false
	}
	r := func(off uintptr) uint64 { return dev.Read64(ParamBase + off) }
	p.BootCPU = int(r(paramBootCPU))
	p.Hop.CPU, p.Hop.MPIDR, p.Hop.Release = int(r(paramHopCPU)), r(paramHopMPIDR), r(paramHopRelease)
	for i := 0; i < MaxCPUs; i++ {
		p.Release[i] = r(paramRelease + uintptr(i)*8)
	}
	return p, true
}

// released: welke cores wij al losgelaten hebben. m1n1's spin-table zegt na
// de release niets meer over de core (hij is van ons en komt nooit terug),
// dus dít is de bron voor AffinityInfo.
var released [MaxCPUs]bool

// Released meldt of Release deze core al heeft losgelaten.
func Released(cpu int) bool { return cpu >= 0 && cpu < MaxCPUs && released[cpu] }

// Release laat een door m1n1 geparkeerde core los op entry, met ctx in x0 —
// de CPUOn van dit board. Het protocol is m1n1's spin-table (src/smp.c),
// dezelfde vorm als Linux' cpu-release-addr: de secundaire wacht in WFE tot
// het target-woord niet-nul is, leest dan args[0..3] als x0..x3 en springt.
// Volgorde is de m1n1-volgorde: args eerst, dan target, dsb, sev (dev.SEV is
// dsb sy + sev). De core komt aan op EL2 met MMU uit, op m1n1's stack.
//
// release is het adres van het target-woord; args[0] ligt er 8 bytes achter
// (struct spin_table: mpidr, flag, target, args[4], retval). false = deze core
// is door m1n1 niet gestart (geen release-adres in het blok).
func Release(cpu int, entry, ctx uint64) bool {
	p, ok := Params()
	if !ok || cpu < 0 || cpu >= MaxCPUs || p.Release[cpu] == 0 {
		return false
	}
	released[cpu] = true
	rel := uintptr(p.Release[cpu])
	dev.Write64(rel+8, ctx)
	dev.Write64(rel+16, 0)
	dev.Write64(rel+24, 0)
	dev.Write64(rel+32, 0)
	dev.MB()
	dev.Write64(rel, entry)
	dev.SEV()
	return true
}

// ConfigText geeft de config-tekst die de loader op CfgBase legde ("" als er
// geen is: het gebied is dan nul of het magic klopt niet). Eén keer gelezen en
// gekopieerd: het gebied is van de boot, niet van de heap.
func ConfigText() string {
	cfgOnce.Do(func() {
		if _, ok := Params(); !ok {
			return
		}
		b := make([]byte, 0, CfgSize)
		for p := uintptr(CfgBase); p < uintptr(CfgBase+CfgSize); p++ {
			c := dev.Read8(p)
			if c == 0 {
				break
			}
			b = append(b, c)
		}
		cfgText = string(b)
	})
	return cfgText
}

var (
	cfgOnce sync.Once
	cfgText string
)

// BootParamAll geeft alle waarden van een sleutel uit de config-tekst.
func BootParamAll(key string) []string { return bootcfg.All(ConfigText(), key) }

// BootParam geeft de eerste waarde van een sleutel ("" = niet aanwezig).
func BootParam(key string) string { return bootcfg.First(BootParamAll(key)) }
