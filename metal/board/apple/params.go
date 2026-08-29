package apple

import (
	"net"
	"sync"

	"github.com/xinix00/HopOS/metal/dev"
	"github.com/xinix00/HopOS/metal/fw/bootcfg"
)

// Het param-blok: wat de loader uit de ADT en m1n1's toestand haalt en op
// ParamBase neerlegt vóór hij springt. Het is de vervanging van "x0 = DTB" op
// een platform zonder FDT van de fabrikant — dezelfde rol als de UEFI-handoff
// in board/uefi, en bewust plat (64-bit woorden op vaste offsets) zodat
// cpuinit.s er met MMU uit ook in kan lezen (de dockchannel-basis voor de
// EL2-foutmelder). Pariteit: PARAM_* in cpuinit.s, en de struct.pack in
// image/apple/load-probe.py.
//
// Straks, als m1n1 ons uit zijn eigen payload boot (m1n1.bin + image
// achter elkaar, zonder laptop), vult een stub dit blok uit de ADT zelf; het
// contract naar de rest van het board verandert dan niet.
const (
	// "HOPAPPLE", little-endian als één woord.
	ParamMagic   = 0x454C505041504F48
	ParamVersion = 3

	paramMagic   = 0x00
	paramVersion = 0x08
	paramDock    = 0x10 // dockchannel-UART-basis (0 = geen)
	paramUART0   = 0x18 // Samsung-UART-basis (0 = geen)
	paramADT     = 0x20 // ADT: fysiek adres
	paramADTSize = 0x28
	paramDRAM    = 0x30 // DRAM-basis en -grootte (ADT /chosen)
	paramDRAMSz  = 0x38
	paramFB      = 0x40 // iBoot-framebuffer: basis, stride, breedte, hoogte
	paramFBStr   = 0x48
	paramFBW     = 0x50
	paramFBH     = 0x58
	paramNCPU    = 0x60  // aantal cores in de ADT
	paramBootCPU = 0x68  // m1n1's index van de boot-core
	paramMAC     = 0x78  // MAC van de ingebouwde NIC (ADT local-mac-address, 48 bits)
	paramUsable  = 0x70  // einde van het bruikbare RAM (iBoot's carveouts beginnen daar)
	paramRelease = 0x80  // + 8×cpu: spin-table release-adres (0 = niet gestart)
	paramMPIDR   = 0x100 // + 8×cpu: MPIDR volgens m1n1's spin-table

	// Opslag (versie 2). ANS is Apple's NVMe-coprocessor: geen PCIe-device maar
	// een RTKit-mailbox met een SART als adresfilter. Op M4 zitten de
	// NVMMU-registers in reg[3] en de NVMe-registers in reg[9] van de ans-node.
	paramANS     = 0x180 // ASC/mailbox-basis (ans reg[0])
	paramNVMMU   = 0x188 // ans reg[3]
	paramNVMe    = 0x190 // ans reg[9]
	paramSART    = 0x198 // sart-ans reg[0]
	paramSARTVer = 0x1a0 // ADT sart-version (3 op t8132)

	// Het RAM-contract van de firmware (versie 3). Dezelfde drie getallen die
	// een Linux-kernel op dit platform krijgt: wat van ons is, en tot waar de
	// firmware het zelf al gevuld heeft.
	paramFWBase   = 0x1a8 // boot-args phys_base
	paramFWSize   = 0x1b0 // boot-args mem_size
	paramFWPlaced = 0x1b8 // boot-args top_of_kernel_data
	paramEnd      = 0x1c0

	// MaxCPUs is de breedte van de per-core tabellen in het blok.
	MaxCPUs = 16
)

// CfgBase/CfgSize: de platform-config als tekst (`key=waarde`-regels, het
// hopos.cfg-formaat van fw/bootcfg), door de loader neergelegd — dit board heeft
// geen bootargs en geen initrd. 4KB is ruim voor node/cluster/apikey/jobspecs.
const (
	CfgBase = RamBase + 0xF000
	CfgSize = 0x1000
)

// P is het gelezen param-blok.
type P struct {
	Dock, UART0        uint64
	ADT, ADTSize       uint64
	DRAMBase, DRAMSize uint64
	FB                 struct{ Base, Stride, W, H uint64 }
	NCPU, BootCPU      int
	UsableEnd          uint64
	MAC                uint64
	ANS                struct{ Base, NVMMU, NVMe, SART, SARTVer uint64 }
	// FW is wat de firmware over het geheugen zegt: Base+Size is van ons,
	// alles onder Placed heeft zij zelf al gevuld. Nul = loader van vóór
	// versie 3; het plan valt dan terug op zijn oude marge.
	FW      struct{ Base, Size, Placed uint64 }
	Release [MaxCPUs]uint64
	MPIDR   [MaxCPUs]uint64
}

// Params leest het blok; ok=false als het magic ontbreekt (image zonder
// loader, of een loader van een andere versie).
func Params() (p P, ok bool) {
	if dev.Read64(ParamBase+paramMagic) != ParamMagic || dev.Read64(ParamBase+paramVersion) != ParamVersion {
		return p, false
	}
	r := func(off uintptr) uint64 { return dev.Read64(ParamBase + off) }
	p.Dock, p.UART0 = r(paramDock), r(paramUART0)
	p.ADT, p.ADTSize = r(paramADT), r(paramADTSize)
	p.DRAMBase, p.DRAMSize = r(paramDRAM), r(paramDRAMSz)
	p.FB.Base, p.FB.Stride, p.FB.W, p.FB.H = r(paramFB), r(paramFBStr), r(paramFBW), r(paramFBH)
	p.ANS.Base, p.ANS.NVMMU, p.ANS.NVMe = r(paramANS), r(paramNVMMU), r(paramNVMe)
	p.ANS.SART, p.ANS.SARTVer = r(paramSART), r(paramSARTVer)
	p.FW.Base, p.FW.Size, p.FW.Placed = r(paramFWBase), r(paramFWSize), r(paramFWPlaced)
	p.NCPU, p.BootCPU = int(r(paramNCPU)), int(r(paramBootCPU))
	p.UsableEnd, p.MAC = r(paramUsable), r(paramMAC)
	if p.NCPU > MaxCPUs {
		p.NCPU = MaxCPUs
	}
	for i := 0; i < MaxCPUs; i++ {
		p.Release[i] = r(paramRelease + uintptr(i)*8)
		p.MPIDR[i] = r(paramMPIDR + uintptr(i)*8)
	}
	return p, true
}

// released: welke cores wij al losgelaten hebben. m1n1's spin-table zegt na
// de release niets meer over de core (hij is van ons en komt nooit terug),
// dus dít is de bron voor AffinityInfo.
var released [MaxCPUs]bool

// Released meldt of Release deze core al heeft losgelaten.
func Released(cpu int) bool { return cpu >= 0 && cpu < MaxCPUs && released[cpu] }

// NICMAC geeft het MAC-adres van de ingebouwde NIC uit het param-blok (nil als
// de loader er geen meegaf). De chip zelf weet het na een PERST niet meer —
// MAC_ADDR_0 draagt dan Broadcom's default — dus dit is de bron.
func NICMAC() net.HardwareAddr {
	p, ok := Params()
	if !ok || p.MAC == 0 {
		return nil
	}
	m := make(net.HardwareAddr, 6)
	for i := range m {
		m[i] = byte(p.MAC >> (40 - 8*i))
	}
	return m
}

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
	if !ok || cpu < 0 || cpu >= p.NCPU || p.Release[cpu] == 0 {
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
