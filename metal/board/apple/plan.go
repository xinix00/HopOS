package apple

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/dev"
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
// De pool komt uit het param-blok (DRAM-basis/-grootte en het bruikbare einde
// uit m1n1's boot-args) en niet uit een constante: de mini bestaat in 16/24/32GB.
// Geen 1GB-blokken voor dat DRAM (mmu.go): MapDRAM hoort vóór het eerste
// gebruik van de pool te draaien — cmd/hopos/board_apple.go doet dat samen met
// SetupPlan.

// SetupPlan registreert het plan. Alleen op de boot-core: een app-core die dit
// pakket linkt heeft er niets te zoeken.
func SetupPlan() {
	if dev.MPIDR()&0xFFFFFF != BootMPIDR()&0xFFFFFF {
		return
	}
	p := layout.Plan{
		NodeCtrlPA:    NodeCtrlPA,
		Stage2PA:      Stage2PA,
		RevokeVecPA:   RevokeVec,
		BootScratchPA: BootScratch,
		NetDMAPA:      NetDMAPA,
		RAMBase:       DRAMBase,
	}
	if pr, ok := Params(); ok {
		// Alle app-cores: elke cpu behalve de boot-cpu (M4: 9). Vóór UsePlan,
		// want de sched-blokken en de core-telling rekenen ermee.
		layout.SetAppCores(pr.NCPU - 1)

		p.Pool = carvePool(pr)
	}
	if len(p.Pool) == 0 {
		fmt.Printf("WARNING HOPOS_POOL_FALLBACK: no param block from the loader — partition pool falls back to a fixed 4GB\n")
		p.Pool = []layout.Region{{Base: PoolBase, Size: 0x1_0000_0000}}
	}
	layout.UsePlan(p)

	// Pariteit ná UsePlan (ervoor weigert layout elke accessor): cpuinit.s
	// bouwt de EL2-vectortabel op EL2_VECTORS en zet VBAR_EL2 erop; HOP's
	// InitVectors plugt de revoke-handler in RevokeVecPA. Wijken die af, dan
	// verdwijnt de hard-kill stil.
	if uint64(layout.RevokeVecPA()) != EL2Vectors {
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
// Zonder die getallen (een loader van vóór param-versie 3) valt alles onder
// PoolBase weg: veilig, en 4GB duurder.
func carvePool(pr P) []layout.Region {
	end := pr.UsableEnd
	if end == 0 || end > pr.DRAMBase+pr.DRAMSize {
		end = pr.DRAMBase + pr.DRAMSize
	}
	if pr.FW.Base == 0 || pr.FW.Size == 0 || pr.FW.Placed <= pr.FW.Base {
		bank := []layout.Region{{Base: pr.DRAMBase, Size: pr.DRAMSize}}
		holes := []layout.Region{
			{Base: pr.DRAMBase, Size: PoolBase - pr.DRAMBase},
			{Base: end, Size: pr.DRAMBase + pr.DRAMSize - end},
		}
		return layout.CarvePool(bank, holes, 2<<20)
	}
	bank := []layout.Region{{Base: pr.FW.Base, Size: pr.FW.Size}}
	holes := []layout.Region{
		{Base: pr.FW.Base, Size: pr.FW.Placed - pr.FW.Base}, // van de firmware
		{Base: RamBase, Size: PoolBase - RamBase},           // HOP + zijn structuren
	}
	return layout.CarvePool(bank, holes, 2<<20)
}
