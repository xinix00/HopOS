package apple

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/dev"
)

// Het PA-plan van de Mac mini M4 (24GB LPDDR5 vanaf 1TiB). De vaste adressen
// staan in apple.go (StructBase en wat erin ligt); hier wordt het plan
// geregistreerd en de pool gesneden uit wat de loader over het DRAM meldde:
//
//	0x100_0000_0000  ~56MB  iBoot's boot-args + ADT, m1n1 en zijn heap — van de
//	                        firmware, wij blijven eraf (en HOP leest de
//	                        spin-table daar tot de laatste core gestart is)
//	0x101_0000_0000  256MB  HOP: image + Go-heap (RamBase, HopRAMSize)
//	0x101_1000_0000   32MB  HOP's eigen structuren (StructBase, zie apple.go)
//	0x101_1200_0000  ~23GB  pool: de partities van de slots, tot het bruikbare
//	                        RAM-einde dat m1n1 rapporteert (daarboven iBoot's
//	                        carveouts en de dummy-framebuffer)
//
// De pool komt uit iBoot's boot_args (firmware.go) en niet uit een constante:
// de mini bestaat in 16/24/32GB.
// Geen 1GB-blokken voor dat DRAM (mmu.go): MapDRAM moet vóór het eerste gebruik
// van de pool draaien, en dat gebeurt vanzelf — Boot() heeft het nodig om
// boot_args te kunnen lezen en doet het als eerste.

// SetupPlan registreert het plan. Alleen op de boot-core: een app-core die dit
// pakket linkt heeft er niets te zoeken.
func SetupPlan() {
	if dev.MPIDR()&0xFFFFFF != BootMPIDR()&0xFFFFFF {
		return
	}
	// ALLEREERST de watchdogs van de firmware stilzetten. iBoot laat er meer
	// dan één gewapend achter, en niemand aait ze — natief reset de node dan
	// midden in een gezonde boot (31-08: exact 1:43, elke keer). Onder m1n1 is
	// dat onzichtbaar omdat hij ze bij zijn opstart uitzet, dus dit is precies
	// het soort ding dat je pas ziet als de loader weg is.
	//
	// Hier en niet later: het beleid in cmd/hopos/watchdog.go wapent er straks
	// één van ons met een aai-lus, maar dat gebeurt pas als de agent draait —
	// en tot die tijd tikt die van de firmware door.
	fmt.Printf("watchdog: %s\n", WDTQuiet())

	// Dan de klok. iBoot laat beide clusters op p-state 1 achter (0,9 GHz) en
	// natief is er niemand meer die dat rechtzet — zie pstate.go. Vroeg in de
	// boot: alles daarna (image-kopieën, netwerk-bring-up) profiteert meteen.
	PStateTune()

	// Wie zijn we geworden? Dit is het eerste board-werk van de boot en het
	// draait op de HOP-core, dus dit is de plek om het vast te leggen — daarna
	// mag iedereen het vragen.
	SelfCPU()

	p := layout.Plan{
		NodeCtrlPA:    NodeCtrlPA,
		CagePA:        CagePA,
		TrapVecPA:     RevokeVec,
		FlipScratchPA: FlipScratch,
		BootScratchPA: BootScratch,
		NetDMAPA:      NetDMAPA,
		RAMBase:       DRAMBase,
	}
	// Alle app-cores: elke core behalve die van HOP (M4: 9). Vóór UsePlan,
	// want de sched-blokken en de core-telling rekenen ermee.
	if n := NumCPUs(); n > 1 {
		layout.SetAppCores(n - 1)
	}
	// De firmware-x0 op de UNIVERSELE plek (BootScratch+8, layout.DTBPtr):
	// dat woord is wat kern/kernflip de vólgende kern in x0 meegeeft. Op virt
	// vult de boot het vanzelf; hier bewaarde de stub hem op StubX0, en werd
	// +8 nooit geschreven — waardoor élke geflipte kern x0=0 kreeg, zonder
	// boot_args bootte, MapDRAM oversloeg en op zijn eerste ADT-lees stierf
	// (GEVONDEN 01-09 via de dockchannel: "hopos x0000000000000000" gevolgd
	// door de EL1-exception — zes ijzer-flips lang onzichtbaar).
	dev.Write64(BootScratch+8, FirmwareX0())

	p.Pool = carvePool()
	if len(p.Pool) == 0 {
		fmt.Printf("WARNING HOPOS_POOL_FALLBACK: no boot_args from the firmware — partition pool falls back to a fixed 4GB\n")
		p.Pool = []layout.Region{{Base: PoolBase, Size: 0x1_0000_0000}}
	}
	layout.UsePlan(p)

	// Pariteit ná UsePlan (ervoor weigert layout elke accessor): cpuinit.s
	// bouwt de EL2-vectortabel op EL2_VECTORS en zet VBAR_EL2 erop; HOP's
	// InitVectors plugt de revoke-handler in TrapVecPA. Wijken die af, dan
	// verdwijnt de hard-kill stil.
	if uint64(layout.TrapVecPA()) != EL2Vectors {
		panic("apple: EL2_VECTORS in cpuinit.s wijkt af van het PA-plan")
	}
}

// carvePool snijdt de partitie-pool uit wat de firmware ons geeft.
//
// Het contract is dat van elke Apple-kernel: `[phys_base, phys_base+mem_size)`
// is van ons, en `top_of_kernel_data` is waar de spullen die de firmware er zelf
// in legde (kernel-image, ADT, trust cache) ophouden. Dat zijn de enige twee
// grenzen die er zijn; al het andere is óns beslag, en dat kennen we zelf.
//
// Er blijven dus twee stukken over, en dat is met opzet: onder ons venster
// (vanaf waar de firmware klaar is tot RamBase) en erboven (vanaf PoolBase tot
// het einde van het bruikbare RAM). Het gat ertussen is HOP zelf plus zijn
// structuren. CarvePool doet het knippen, het uitlijnen op 2MB en het
// wegstrepen van te kleine restjes.
//
// Waarom het lage stuk mag: daar stond m1n1 met zijn heap, en die bestaat niet
// meer zodra hij naar ons gesprongen is — hij zet zijn MMU uit en komt nooit
// terug. Wat de firmware nog wél nodig heeft (de ADT waar het param-blok naar
// wijst, de trust cache) ligt onder `top_of_kernel_data` en valt dus in het gat.
//
// Ontbreekt boot_args, dan is er geen pool: liever geen partities dan partities
// op geraden geheugen.
func carvePool() []layout.Region {
	ba, ok := Boot()
	if !ok || ba.PhysBase == 0 || ba.MemSize == 0 || ba.TopOfKernelData <= ba.PhysBase {
		return nil
	}
	bank := []layout.Region{{Base: ba.PhysBase, Size: ba.MemSize}}
	holes := []layout.Region{
		{Base: ba.PhysBase, Size: ba.TopOfKernelData - ba.PhysBase}, // van de firmware
		{Base: RamBase, Size: PoolBase - RamBase},                   // HOP + zijn structuren
	}
	return layout.CarvePool(bank, holes, 2<<20)
}
