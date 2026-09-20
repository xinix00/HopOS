package hop

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xinix00/HopOS/metal/v2/board"
	"github.com/xinix00/HopOS/metal/v2/board/uefi"
	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/driver/scmi"
	"github.com/xinix00/HopOS/metal/v2/fw/acpi"
)

// scp.go — wat de O6N via zijn SCP (System Control Processor, SCMI) doet:
// de thermometer (sensor-protocol over de mailbox) en de klok (de SCMI-
// fastchannels uit de _CPC). Beide zijn firmware-feiten van dit bord, geen
// ACPI-universalia, en wonen daarom hier.

// SCMIChannel is het shared-memory-kanaal dat de DSDT van het bord zelf
// gebruikt voor zijn _TMP (device PMMX, OperationRegion MBXO 0x065d0000,
// doorbell BEEL op +0x80). Wij lopen exact dezelfde bytes.
const SCMIChannel = 0x065d0000

var (
	thermMu      sync.Mutex
	thermOnce    bool
	thermCh      *scmi.Channel
	thermSensors []scmi.Sensor // de Celsius-sensoren die meetellen
	thermLast    int
	thermAt      time.Time
)

// Thermometer-contract (board.Thermometer): de node stuurt dit op zijn
// heartbeat naar HOP.
var _ board.Thermometer = machine{}

// TempMilliC is de heetste CPU-sensor van de SCP in milligraden (0 = geen
// meting). Eén SCMI-ronde per seconde hoogstens: de heartbeat mag vaker
// vragen, de mailbox niet vaker draaien.
func (machine) TempMilliC() int {
	thermMu.Lock()
	defer thermMu.Unlock()
	if !thermOnce {
		thermOnce = true
		thermInit()
	}
	if thermCh == nil {
		return 0
	}
	if time.Since(thermAt) < time.Second {
		return thermLast
	}
	thermAt = time.Now()
	max := 0
	for _, s := range thermSensors {
		if r, err := thermCh.Reading(s.ID); err == nil {
			if mC := s.MilliC(r); mC > max && mC < 200_000 { // 1200°C-glitches (bekend van acpitz) wegfilteren
				max = mC
			}
		}
	}
	thermLast = max
	return max
}

// thermInit opent het kanaal en kiest de sensoren: Celsius-sensoren met
// "CPU" in de naam; zijn die er niet (andere naamgeving in een andere
// firmware), dan álle Celsius-sensoren — liever de heetste van het bord dan
// niets.
func thermInit() {
	if !IsCix() || !uefi.MapHigh(SCMIChannel, 0x1000) {
		return
	}
	ch := &scmi.Channel{Base: SCMIChannel}
	if _, err := ch.Version(scmi.ProtoSensor); err != nil {
		fmt.Printf("hwmon: SCMI sensor protocol not answering (%v) - temperature off\n", err)
		return
	}
	sensors, _ := ch.Sensors()
	var cpu, all []scmi.Sensor
	for _, s := range sensors {
		if s.Type != 2 {
			continue
		}
		all = append(all, s)
		if strings.Contains(strings.ToUpper(s.Name), "CPU") {
			cpu = append(cpu, s)
		}
	}
	if len(cpu) == 0 {
		cpu = all
	}
	if len(cpu) == 0 {
		fmt.Printf("hwmon: SCMI lists %d sensors, none in Celsius - temperature off\n", len(sensors))
		return
	}
	thermCh, thermSensors = ch, cpu
	names := make([]string, 0, len(cpu))
	for _, s := range cpu {
		names = append(names, s.Name)
	}
	fmt.Printf("hwmon: SCMI sensors %v (of %d) - hottest goes on the heartbeat\n", names, len(sensors))
}

// ---- klok -------------------------------------------------------------

// domain is één DVFS-domein: het desired-perf-register (SCMI-fastchannel,
// een 32-bit MHz-woord) en de grenzen uit de _CPC, plus de MADT-indices van
// de cores erin.
type domain struct {
	reg             uintptr
	lowest, highest uint32
	cores           []int
}

// domains groepeert de _CPC's op desired-perf-register en koppelt ze via de
// UID aan de MADT-volgorde. Leeg = geen _CPC (of geen SystemMemory-register).
func domains() []domain {
	t := uefi.Tables()
	if t == nil {
		return nil
	}
	cpcs := t.CPCs()
	cpus := uefi.MADTCPUs()
	var out []domain
	for _, c := range cpcs {
		if c.DesiredReg == 0 || c.DesiredBit != 32 {
			continue
		}
		idx := -1
		for i, cpu := range cpus {
			if cpu.UID == c.UID {
				idx = i
				break
			}
		}
		if idx < 0 {
			continue
		}
		found := false
		for k := range out {
			if out[k].reg == uintptr(c.DesiredReg) {
				out[k].cores = append(out[k].cores, idx)
				found = true
				break
			}
		}
		if !found {
			out = append(out, domain{reg: uintptr(c.DesiredReg), lowest: c.Lowest, highest: c.Highest, cores: []int{idx}})
		}
	}
	return out
}

// cpcOf geeft de _CPC van MADT-index i (ok=false zonder).
func cpcOf(i int) (acpi.CPC, bool) {
	t := uefi.Tables()
	cpus := uefi.MADTCPUs()
	if t == nil || i < 0 || i >= len(cpus) {
		return acpi.CPC{}, false
	}
	for _, c := range t.CPCs() {
		if c.UID == cpus[i].UID {
			return c, true
		}
	}
	return acpi.CPC{}, false
}

// DefaultClock geldt zonder hopos.clock= in de config: "cpc" = de knop
// draaien, "firmware" = laten staan. Bouwtijd-instelbaar (-ldflags -X
// …/board/o6n/hop.DefaultClock=cpc): zelfde A/B-reden als DefaultNICIRQ.
var DefaultClock = "firmware"

// StartClock zet élk DVFS-domein op zijn hoogste prestatie uit de _CPC —
// zonder OS-ingreep blijven de cores op de boot-OPP staan (1,8GHz of lager;
// FreeBSD-rapport: "stuck at 1 GHz"). Thermisch terugregelen doet de SCP
// zelf (85°C passief in de DSDT, eigen limieten in de firmware), dus vol is
// veilig. Knoppen in hopos.cfg: hopos.mhz=N klemt alle domeinen op N (of
// hun maximum als dat lager is); hopos.clock=firmware laat alles staan.
// Een idle-gestuurde governor (driver/dvfs) is een volgende stap: de knop
// is nu één MMIO-woord per domein, precies wat die governor nodig heeft.
func StartClock(param func(string) string) {
	ds := domains()
	if len(ds) == 0 {
		fmt.Println("clock: no _CPC fast channels in the DSDT - staying on the firmware OPP")
		return
	}
	// De external abort van 17-09 op de eerste read was de _CPC-parser
	// (fw/acpi/cpc.go: het adres één byte te vroeg gelezen, 0x659009c03 i.p.v.
	// 0x0659009c); met het juiste adres is dit het gewone MMIO-woord dat
	// Linux' cppc_cpufreq ook schrijft. hopos.clock=firmware laat alles staan.
	sel := DefaultClock
	if v := param("hopos.clock"); v != "" {
		sel = v
	}
	if sel == "firmware" {
		for _, d := range ds {
			fmt.Printf("clock: cores %v: fast channel %#x, range %d..%d - left on the firmware OPP (hopos.clock=firmware)\n",
				d.cores, d.reg, d.lowest, d.highest)
		}
		return
	}
	cap := uint32(0)
	if v := param("hopos.mhz"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cap = uint32(n)
		}
	}
	for _, d := range ds {
		if !uefi.MapHigh(uint64(d.reg), 4) {
			fmt.Printf("clock: fast channel %#x unreachable - domain left alone\n", d.reg)
			continue
		}
		want := d.highest
		if cap != 0 && cap < want {
			want = cap
		}
		if want < d.lowest {
			want = d.lowest
		}
		was := dev.Read32(d.reg)
		dev.Write32(d.reg, want)
		dev.MB()
		fmt.Printf("clock: cores %v: %d -> %d MHz (range %d..%d, fast channel %#x)\n",
			d.cores, was, want, d.lowest, d.highest, d.reg)
	}
}
