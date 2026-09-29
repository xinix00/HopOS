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
	"github.com/xinix00/HopOS/metal/v2/driver/dvfs"
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
// een 32-bit perf-woord op de abstracte _CPC-schaal, geen MHz) en de grenzen uit de _CPC, plus de MADT-indices van
// de cores erin. quiet is de stil-stand van de governor: LowestNonlinear
// (onder dat punt spaart zakken geen energie meer per instructie, alleen
// tijd — Linux' cppc_cpufreq legt zijn minimum daar ook), anders Lowest.
type domain struct {
	reg             uintptr
	lowest, highest uint32
	quiet           uint32
	cores           []int
	cpc             acpi.CPC // de eerste _CPC van het domein: frequenties en tellers
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
			q := c.Lowest
			if c.LowestNL > c.Lowest && c.LowestNL <= c.Highest {
				q = c.LowestNL
			}
			out = append(out, domain{reg: uintptr(c.DesiredReg), lowest: c.Lowest, highest: c.Highest, quiet: q, cores: []int{idx}, cpc: c})
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

// DefaultClock geldt zonder hopos.clock= in de config: "dvfs" = de klok
// volgt de idle-teller (driver/dvfs, hetzelfde beleid als de Pi), "max" =
// élk domein vast op zijn plafond, "firmware" = laten staan. Bouwtijd-
// instelbaar (-ldflags -X …/board/o6n/hop.DefaultClock=dvfs, of LDX= in
// image/flip-bundle.sh): zelfde A/B-reden als DefaultNICIRQ. Sinds 23-09
// "dvfs": gemeten op het board (L83 p66) — de knop schaalt exact met de klok
// (867 tegen 267 Msteps/s = 2600/800 MHz), het beleid zakt na 30s en klokt
// onder last binnen ~15ms op. De firmware zelf liet de grote cores op 1,5 GHz.
var DefaultClock = "dvfs"

// StartClock kiest het klokbeleid. Zonder OS-ingreep blijven de cores op de
// boot-OPP staan (1,8GHz of lager; FreeBSD-rapport: "stuck at 1 GHz"): SCMI-
// perf is OS-gestuurd, de SCP zet wat je vraagt en regelt alleen thermisch
// zelf terug (85°C passief in de DSDT, eigen limieten in de firmware), dus vol
// is veilig. Knoppen in hopos.cfg: hopos.clock=dvfs|max|firmware, en
// hopos.mhz=N klemt het plafond van alle domeinen op N MHz (of hun maximum als
// dat lager is) — in beide standen die schrijven. De omrekening naar de
// abstracte perf-schaal gaat via _CPC's NominalFrequency; draagt een package
// die niet, dan geldt N als perf-waarde.
func StartClock(param func(string) string) {
	ds := domains()
	if len(ds) == 0 {
		fmt.Println("clock: no _CPC fast channels in the DSDT - staying on the firmware OPP")
		return
	}
	// De external abort van 17-09 op de eerste read was de _CPC-parser
	// (fw/acpi/cpc.go: het adres één byte te vroeg gelezen, 0x659009c03 i.p.v.
	// 0x0659009c); met het juiste adres is dit het gewone MMIO-woord dat
	// Linux' cppc_cpufreq ook schrijft.
	sel := DefaultClock
	if v := param("hopos.clock"); v != "" {
		sel = v
	}
	if sel != "max" && sel != "dvfs" {
		for _, d := range ds {
			fmt.Printf("clock: cores %v: fast channel %#x, range %d..%d - left on the firmware OPP (hopos.clock=%s)\n",
				d.cores, d.reg, d.lowest, d.highest, sel)
		}
		return
	}
	capMHz := uint32(0)
	if v := param("hopos.mhz"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			capMHz = uint32(n)
		}
	}
	k := cpcKnob{}
	for _, d := range ds {
		if !uefi.MapHigh(uint64(d.reg), 4) {
			fmt.Printf("clock: fast channel %#x unreachable - domain left alone\n", d.reg)
			continue
		}
		if capMHz != 0 {
			perf, ok := d.cpc.Perf(capMHz)
			if !ok {
				fmt.Printf("clock: cores %v: _CPC carries no frequencies - hopos.mhz=%d taken as a perf value\n", d.cores, capMHz)
			}
			if perf < d.highest {
				d.highest = max(perf, d.lowest)
			}
		}
		d.quiet = min(d.quiet, d.highest)
		k = append(k, d)
		now := dev.Read32(d.reg)
		fmt.Printf("clock: cores %v: perf now %d (%d MHz), range %d..%d, quiet %d (%d MHz), ceiling %d (%d MHz); %s\n",
			d.cores, now, d.cpc.MHz(now), d.lowest, d.highest, d.quiet, d.cpc.MHz(d.quiet), d.highest, d.cpc.MHz(d.highest), d.cpc)
	}
	if len(k) == 0 {
		return
	}
	if sel == "max" {
		got, _ := k.Full()
		fmt.Printf("clock: fixed at the ceiling - %s (hopos.clock=max)\n", got)
		return
	}
	fmt.Println("clock: policy dvfs - clock follows idle (up within ~20 ms, down after 30 s quiet)")
	dvfs.Run(k)
}

// cpcKnob is de O6N-knop voor driver/dvfs: per domein één perf-woord in het
// fastchannel. Het beleid is node-breed zoals op de Pi (één drukke core zet
// alle domeinen vol); per domein schakelen vraagt een idle-teller per fysieke
// core, en die kent de control page niet — eerst meten of dat het waard is.
type cpcKnob []domain

func (k cpcKnob) write(pick func(domain) uint32) (string, bool) {
	var b strings.Builder
	for i, d := range k {
		dev.Write32(d.reg, pick(d))
		if i > 0 {
			b.WriteByte('/')
		}
		fmt.Fprintf(&b, "%d", pick(d))
	}
	dev.MB()
	b.WriteString(" perf")
	return b.String(), true
}

func (k cpcKnob) Full() (string, bool)  { return k.write(func(d domain) uint32 { return d.highest }) }
func (k cpcKnob) Quiet() (string, bool) { return k.write(func(d domain) uint32 { return d.quiet }) }

// Telemetry leest het woord terug: dat is wat we gevraagd hebben, niet wat de
// SCP levert (thermisch terugregelen ziet hier niemand) — de temperatuur staat
// ernaast om dat verschil te kunnen vermoeden.
func (k cpcKnob) Telemetry() string {
	var b strings.Builder
	mC := machine{}.TempMilliC()
	fmt.Fprintf(&b, "%d.%dC, asked", mC/1000, mC%1000/100)
	for i, d := range k {
		if i == 0 {
			b.WriteByte(' ')
		} else {
			b.WriteByte('/')
		}
		fmt.Fprintf(&b, "%d", dev.Read32(d.reg))
	}
	b.WriteString(" perf")
	return b.String()
}
