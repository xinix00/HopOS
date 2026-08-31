// pstate.go — de kloksnelheid van de clusters.
//
// iBoot levert de SoC af op p-state 1: 0,9 GHz, de bodem van de tabel. Daar
// blijft hij ook staan, want anders dan op de Pi (waar de VideoCore-firmware
// na de boot blijft draaien en de ARM-klok regelt) is er op Apple na de boot
// niemand meer die dat doet — m1n1 heeft voor t8132 niet eens een tabel, dus
// ook onder de loader stond alles op de bodem. GEMETEN 31-08: de LCG-benchmark
// gaf op een E-core en een P-core exact hetzelfde getal (300 Msteps/s, 1,00×),
// en 300M × 3 cycles/MADD = 0,9 GHz = precies p-state 1 van beide tabellen.
//
// Het recept is m1n1's cpufreq_init_cluster voor de naaste familie (t8122/
// t6030, de M3's — t8132 zit er niet in), per cluster:
//
//  1. p-state naar 1 (waar we al staan; pariteit met m1n1's volgorde);
//  2. de features, elk gelezen van de pmgr-node: cpu-apsc=1 op deze machine
//     (dus de APSC-disable-bit wissen: de hardware klokt dan zélf terug bij
//     idle — de governor zit in het silicium, wij zetten alleen het plafond),
//     ppt/llc-thrtl staan op 0 en amx/pll-relock ontbreken (M4 heeft geen
//     AMX) — die bits gaan dus uit, ook m1n1-gedrag;
//  3. het onbegrepen woord base+0x440f8 = 1 (m1n1 doet dit onverklaard voor
//     de hele M3/M4-era familie);
//  4. p-state naar het doel.
//
// De clusterbasis komt niet uit een magisch getal maar uit de boom: elke core
// meldt zijn cpu-impl-reg, en het DVFM-blok van zijn cluster ligt op
// (impl &^ 0xFFFFFF) | 0xe00000 — het patroon dat m1n1 van t8103 t/m t6031
// hardcodet, hier geverifieerd tegen de ADT-dump van deze machine
// (E: 0x210050000+n×0x100000 → 0x210e00000, P: 0x211... → 0x211e00000).
//
// De frequentietabel staat óók in de boom: voltage-states1 (E) en
// voltage-states5 (P) op de pmgr-node, entries van (raw u32, mV u32) waar
// freq ≈ 65536e9 / raw Hz — geijkt op de bekende M4-kloks (E-max 2,89 GHz,
// P-max 4,46 GHz komen er exact uit).
//
// Het doel is bewust NIET de top van de tabel: E=5 (2,17 GHz @ 785mV) en
// P=6 (2,35 GHz @ 780mV), m1n1's defaults voor de buurchips. De thermometer
// (SMC) is nog niet bedraad, en blind naar de 1150mV-hoek klokken op een node
// die je niet kunt zien is vragen om problemen. hopos.pstate=E,P zet het
// plafond anders; hopos.pstate=off laat alles staan en meldt alleen.
package apple

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/dev"
)

const (
	clPState   = 0x20020         // CLUSTER_PSTATE (m1n1 cpufreq.c)
	clUnk440f8 = 0x440f8         // onverklaard, m1n1 schrijft er 1 in
	psBusy     = uint64(1) << 31 // wissel loopt nog
	psSet      = uint64(1) << 25 // "voer DESIRED1 uit"
	psAPSCDis  = uint64(1) << 23 // M2_APSC_DIS: hardware-governor UIT
	psAPSCBusy = uint64(1) << 7
	psPLL      = uint64(1) << 42 // FIXED_FREQ_PLL_RECLOCK
	psDesired1 = uint64(0x1F)    // bits 4:0: de gevraagde p-state

	psDefaultE = 5 // 2,17 GHz @ 785mV — m1n1's t6030-default
	psDefaultP = 6 // 2,35 GHz @ 780mV
)

// psSpins begrenst het wachten op een p-state-wissel; de hardware doet er
// microseconden over (m1n1 wacht er 400), dit is een ruime bovengrens.
const psSpins = 1 << 20

// clusterBase leidt het DVFM-blok van een cluster af uit de cpu-impl-reg van
// (een van) zijn cores. 0 = geen core met een impl-reg gevonden.
func clusterBase(cluster int) uintptr {
	for _, c := range CPUs() {
		if c.Cluster == cluster && c.Impl != 0 {
			return uintptr(c.Impl&^0xFFFFFF | 0xe00000)
		}
	}
	return 0
}

// pstateRaws leest de frequentietabel van een cluster: de raw-helft van elke
// (raw, millivolt)-entry. Lege uitkomst = tabel niet in de boom.
func pstateRaws(cluster int) []uint32 {
	t, ok := ADT()
	if !ok {
		return nil
	}
	pm, ok := t.Path("/arm-io/pmgr")
	if !ok {
		return nil
	}
	name := "voltage-states1" // cluster 0, sawtooth
	if cluster == 1 {
		name = "voltage-states5" // cluster 1, everest
	}
	addr, size, ok := t.Prop(pm, name)
	if !ok || size < 8 {
		return nil
	}
	raws := make([]uint32, 0, size/8)
	for i := uint32(0); i+8 <= size; i += 8 {
		raws = append(raws, dev.Read32(addr+uintptr(i)))
	}
	return raws
}

// psMHz maakt van een tabel-raw een frequentie in MHz (65536e9/raw Hz, de
// schaal die de bekende M4-kloks exact reproduceert).
func psMHz(raw uint32) uint32 {
	if raw == 0 {
		return 0
	}
	return uint32(65_536_000 / uint64(raw))
}

// pmgrFeature leest een feature-woord van de pmgr-node; 0 = uit of afwezig
// (m1n1's pmgr_get_feature maakt datzelfde onderscheid niet).
func pmgrFeature(name string) uint32 {
	t, ok := ADT()
	if !ok {
		return 0
	}
	pm, ok := t.Path("/arm-io/pmgr")
	if !ok {
		return 0
	}
	addr, size, ok := t.Prop(pm, name)
	if !ok || size < 4 {
		return 0
	}
	return dev.Read32(addr)
}

// setPState vraagt p-state ps en wacht tot de wissel klaar is.
func setPState(base uintptr, ps uint64) bool {
	v := dev.Read64(base + clPState)
	v &^= psDesired1
	v |= psSet | (ps & psDesired1)
	dev.Write64(base+clPState, v)
	for i := 0; i < psSpins; i++ {
		if dev.Read64(base+clPState)&psBusy == 0 {
			return true
		}
	}
	return false
}

// pstateTargets: het doel per cluster. hopos.pstate=off → (0,0) = alleen
// kijken; hopos.pstate=E,P → dat; anders de defaults. Alles gecapt op de
// tabel — een doel buiten de tabel is een config-fout, geen reden om de
// bodemklok te houden.
func pstateTargets(states0, states1 int) (int, int, bool) {
	e, p := psDefaultE, psDefaultP
	switch v := BootParam("hopos.pstate"); v {
	case "off":
		return 0, 0, false
	case "":
	default:
		a, b, n := 0, 0, 0
		for i := 0; i < len(v); i++ {
			switch {
			case v[i] >= '0' && v[i] <= '9' && n == 0:
				a = a*10 + int(v[i]-'0')
			case v[i] >= '0' && v[i] <= '9':
				b = b*10 + int(v[i]-'0')
			case v[i] == ',':
				n++
			}
		}
		if n == 1 && a >= 1 && b >= 1 {
			e, p = a, b
		}
	}
	if e > states0 {
		e = states0
	}
	if p > states1 {
		p = states1
	}
	return e, p, true
}

// PStateTune voert het recept uit op beide clusters en meldt per cluster één
// regel: waar hij stond, waar hij heen is. Aangeroepen uit SetupPlan; onder
// hopos.pstate=off meet hij alleen.
func PStateTune() {
	apsc := pmgrFeature("cpu-apsc") != 0

	raws := [2][]uint32{pstateRaws(0), pstateRaws(1)}
	wantE, wantP, write := pstateTargets(len(raws[0]), len(raws[1]))
	want := [2]int{wantE, wantP}
	names := [2]string{"E", "P"}

	for cl := 0; cl < 2; cl++ {
		base := clusterBase(cl)
		if base == 0 || len(raws[cl]) == 0 {
			fmt.Printf("cpufreq: cluster %d (%s): no impl-reg or no voltage-states in the ADT — leaving the boot clock\n", cl, names[cl])
			continue
		}
		v := dev.Read64(base + clPState)
		cur := int(v & psDesired1)
		curMHz := uint32(0)
		if cur >= 1 && cur <= len(raws[cl]) {
			curMHz = psMHz(raws[cl][cur-1])
		}
		if v == ^uint64(0) || v&psBusy != 0 {
			fmt.Printf("cpufreq: cluster %d (%s): CLUSTER_PSTATE reads 0x%x — not touching it\n", cl, names[cl], v)
			continue
		}
		if !write {
			fmt.Printf("cpufreq: cluster %d (%s): pstate %d/%d (%d MHz), hopos.pstate=off — measuring only\n",
				cl, names[cl], cur, len(raws[cl]), curMHz)
			continue
		}

		// Het m1n1-recept, in zijn volgorde.
		setPState(base, 1)
		v = dev.Read64(base + clPState)
		if apsc {
			v &^= psAPSCDis
		} else {
			v |= psAPSCDis
		}
		v &^= psPLL // afwezig in deze boom → uit (m1n1-gedrag)
		dev.Write64(base+clPState, v)
		if apsc {
			for i := 0; i < psSpins && dev.Read64(base+clPState)&psAPSCBusy != 0; i++ {
			}
		}
		// Throttle-features: op deze machine 0 of afwezig → bits uit.
		for _, off := range []uintptr{0x48400, 0x48408, 0x40270, 0x40250} {
			dev.Write64(base+off, dev.Read64(base+off)&^(uint64(1)<<63))
		}
		dev.Write64(base+clUnk440f8, 1)

		ok := setPState(base, uint64(want[cl]))
		after := int(dev.Read64(base+clPState) & psDesired1)
		afterMHz := uint32(0)
		if after >= 1 && after <= len(raws[cl]) {
			afterMHz = psMHz(raws[cl][after-1])
		}
		status := ""
		if !ok {
			status = " (WARNING: switch still busy after timeout)"
		}
		fmt.Printf("cpufreq: cluster %d (%s): pstate %d (%d MHz) -> %d/%d (%d MHz), apsc=%v%s\n",
			cl, names[cl], cur, curMHz, after, len(raws[cl]), afterMHz, apsc, status)
	}
}
