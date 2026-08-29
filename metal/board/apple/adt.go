// adt.go — het silicium uitlezen uit de Apple Device Tree.
//
// De boom staat in het geheugen van deze machine en boot_args zegt waar
// (firmware.go). Dit is de enige bron voor wat erin staat: geen param-blok
// ernaast, geen terugval, geen tweede antwoord.
package apple

import (
	"net"

	"github.com/xinix00/HopOS/metal/dev"
	"github.com/xinix00/HopOS/metal/fw/adt"
)

var (
	adtTree adt.Tree
	adtOK   bool
	adtRead bool

	cpuList []CPU
	cpuRead bool
)

// ADT geeft de boom die de firmware achterliet. Eén keer geopend: hij verandert
// niet meer, en dit draait op elk moment van de boot.
func ADT() (adt.Tree, bool) {
	if adtRead {
		return adtTree, adtOK
	}
	adtRead = true
	if ba, ok := Boot(); ok {
		adtTree, adtOK = adt.Open(uintptr(ba.ADT), ba.ADTSize)
	}
	return adtTree, adtOK
}

// ADTReg zoekt het i-de registervenster van een pad in de boom, met de
// ranges-vertaling die er een fysiek adres van maakt. 0 = niet gevonden.
func ADTReg(path string, i int) uint64 {
	t, ok := ADT()
	if !ok {
		return 0
	}
	base, _, ok := t.RegOf(path, i)
	if !ok {
		return 0
	}
	return base
}

// NICMAC geeft het MAC-adres van de ingebouwde NIC. Dezelfde bron die m1n1 voor
// Linux in de device tree patcht — en de enige, want na een PERST draagt de chip
// alleen nog Broadcom's default.
func NICMAC() net.HardwareAddr {
	t, ok := ADT()
	if !ok {
		return nil
	}
	n, ok := t.Path("/arm-io/apcie/pci-bridge2/lan-1gb")
	if !ok {
		return nil
	}
	a, size, ok := t.Prop(n, "local-mac-address")
	if !ok || size < 6 {
		return nil
	}
	m := make(net.HardwareAddr, 6)
	for i := range m {
		m[i] = dev.Read8(a + uintptr(i))
	}
	return m
}

// Serial geeft het serienummer van de machine uit de wortel van de boom — de
// identiteit die de node zonder loader nergens anders vandaan haalt. Twee nodes
// op één LAN mogen nooit dezelfde naam krijgen, en dit is het enige dat per
// machine verschilt en dat wij zelf kunnen lezen.
func Serial() string {
	t, ok := ADT()
	if !ok {
		return ""
	}
	a, size, ok := t.Prop(0, "serial-number")
	if !ok || size == 0 {
		return ""
	}
	b := make([]byte, 0, size)
	for i := uint32(0); i < size; i++ {
		c := dev.Read8(a + uintptr(i))
		if c == 0 {
			break
		}
		b = append(b, c)
	}
	return string(b)
}

// CPU is één core zoals de firmware hem beschrijft. Core en Cluster komen uit
// het reg-woord; Impl is het registerblok van die core, waar onder meer RVBAR
// in staat — het adres waar hij uit reset landt.
type CPU struct {
	Core, Cluster, Die int
	Impl               uint64
}

// CPUs leest de core-lijst uit de boom. Dit is wat een eigen core-start nodig
// heeft, en het is het enige dat vertelt wélke core zuinig is: cluster 0 is
// sawtooth, cluster 1 everest.
func CPUs() []CPU {
	if cpuRead {
		return cpuList
	}
	cpuRead = true
	t, ok := ADT()
	if !ok {
		return nil
	}
	cpus, ok := t.Path("/cpus")
	if !ok {
		return nil
	}
	t.Children(cpus, func(n adt.Node) bool {
		reg := t.U32(n, "reg", 0xffffffff)
		if reg == 0xffffffff {
			return true
		}
		cpuList = append(cpuList, CPU{
			Core:    int(reg & 0xff),
			Cluster: int(reg >> 8 & 0x7),
			Die:     int(reg >> 11 & 0xf),
			Impl:    t.U64(n, "cpu-impl-reg", 0),
		})
		return true
	})
	return cpuList
}

// NumCPUs is het aantal cores dat de firmware beschrijft.
func NumCPUs() int { return len(CPUs()) }

// ANSAddrs geeft de adressen van de opslag-coprocessor: zijn mailbox-blok, de
// NVMMU, het NVMe-registerblok en het adresfilter. Bij voorkeur uit de boom;
// wat daar niet staat komt uit het param-blok, zodat een oudere loader blijft
// werken.
//
// Op deze generatie (`nvme-secure-bar` in de ADT) zitten de NVMMU-registers in
// reg[3] en de NVMe-registers in reg[9]; op M1-M3 wijzen ze allebei naar reg[3].
func ANSAddrs() (asc, nvmmu, nvme, sart uint64) {
	asc = ADTReg("/arm-io/ans", 0)
	nvmmu = ADTReg("/arm-io/ans", 3)
	nvme = ADTReg("/arm-io/ans", 9)
	sart = ADTReg("/arm-io/sart-ans", 0)
	if nvme == 0 {
		nvme = nvmmu // M1-M3: één venster voor allebei
	}
	return asc, nvmmu, nvme, sart
}

// SARTVersion is de variant van het adresfilter (3 op t8132).
func SARTVersion() uint32 {
	t, ok := ADT()
	if !ok {
		return 0
	}
	n, ok := t.Path("/arm-io/sart-ans")
	if !ok {
		return 0
	}
	return t.U32(n, "sart-version", 0)
}

// PMGRCPUStart is het registerblok waarmee cores gestart en gestopt worden: de
// PMGR-basis plus de offset van deze familie (t8112/t8122/t8132 delen er één).
// 0 = niet gevonden.
func PMGRCPUStart() uint64 {
	const cpuStartOffT8112 = 0x34000
	if b := ADTReg("/arm-io/pmgr", 0); b != 0 {
		return b + cpuStartOffT8112
	}
	return 0
}
