// clint.go: de CLINT-timer, en het bewijs dat hij er is.
//
// WAAROM DIT EEN PROBE IS EN GEEN AANNAME. Op 30-07 is gemeten dat de mtime-
// registeur van deze c900-CLINT niet bestaat: een read op 0xbff8 is een bus-fout
// (mcause=5). Bij dat resultaat stond de conclusie "msip/mtimecmp bestaan wél" —
// maar die is nooit gemeten, alleen aangenomen omdat de SiFive-layout ze belooft.
// Op een silicium dat de helft van die layout weglaat is dat precies het soort
// aanname dat je niet moet doen: het hele slaapplan (een hart dat in wfi wacht
// tot mtimecmp afgaat) staat of valt ermee, en het faalt stil — een hart dat
// nooit meer wakker wordt is een dode node, geen foutmelding.
//
// Dus: één keer bij boot proberen op HOP's EIGEN hart. Dat is de veilige plek —
// wij bepalen daar zelf of er interrupts aanstaan, en een pending-bit zonder
// enable is een bit en geen trap. Slaagt de probe, dan mag de switcher (cpu/mmode)
// op het app-hart met wfi gaan werken; faalt hij, dan blijft de parkeerlus spinnen
// en zegt de console waarom.
//
// 32-BIT TOEGANG, ALTIJD. Dezelfde CLINT weigert 64-bit MMIO (zie licheerv.go),
// dus mtimecmp gaat als twee woorden — en dan in de volgorde die de RISC-V-
// privileged spec voorschrijft (lo=all-ones, hi, lo), zodat er tijdens de
// wijziging nooit een tussenwaarde in het verleden staat die per ongeluk vuurt.

package licheerv

import "fmt"

const (
	clintMSIP     = CLINT_BASE + 0x0000 // 4 bytes per hart
	clintMTimecmp = CLINT_BASE + 0x4000 // 8 bytes per hart

	mipMSIP = 1 << 3 // machine software interrupt pending
	mipMTIP = 1 << 7 // machine timer interrupt pending
)

func mhartid() uint64
func mip() uint64
func mie() uint64
func mstatus() uint64
func wfiMTIE()

// SleepCapTicks is hoe lang een hart hoogstens achter elkaar mag doorslapen:
// 2ms op de vaste 25MHz timebase. Geen zuinigheidsgetal maar een vangnet —
// raakt er ooit een wek-signaal zoek, dan kost dat 2ms latency in plaats van
// een hart dat nooit meer opkijkt. Eén constante voor beide slapers (HOP's
// eigen governor hier, de switcher-park via HartWaker) — het is één invariant.
const SleepCapTicks = 2 * 25_000

// MtimecmpAddr geeft het mtimecmp-adres van een hart. De switcher heeft dit
// nodig als constante in zijn asm; deze functie is de enige plek waar de
// layout-rekensom staat.
func MtimecmpAddr(hart uint64) uint64 { return clintMTimecmp + 8*hart }

// MsipAddr geeft het msip-adres van een hart — het IPI-kanaal waarmee HOP een
// slapend app-hart wakker maakt.
func MsipAddr(hart uint64) uint64 { return clintMSIP + 4*hart }

// SetMtimecmp zet de timer van een hart, in de spec-volgorde die geen valse
// tussenwaarde kan achterlaten. Een waarde van ^uint64(0) is "nooit".
func SetMtimecmp(hart, val uint64) {
	a := MtimecmpAddr(hart)
	write32(a, 0xffffffff) // lo eerst op oneindig: hi mag nu veilig omlaag
	write32(a+4, uint32(val>>32))
	write32(a, uint32(val))
}

func getMtimecmp(hart uint64) uint64 {
	a := MtimecmpAddr(hart)
	return uint64(read32(a+4))<<32 | uint64(read32(a))
}

// SetMsip zet of wist de IPI van een hart.
func SetMsip(hart uint64, on bool) {
	v := uint32(0)
	if on {
		v = 1
	}
	write32(MsipAddr(hart), v)
}

// clintOK onthoudt de uitslag van de probe: pas als dit waar is mag er ergens
// een wfi op een CLINT-wekker vertrouwen. ownTimecmp is de mtimecmp van het
// hart waarop de probe draaide — HOP's eigen hart, en dus het adres dat MSleep
// gebruikt.
var (
	clintOK bool // timerketen bewezen (mtimecmp houdt vast, vuurt, wfi keert terug)
	msipOK  bool // IPI-kanaal bewezen — optioneel bovenop de timer (zie de probe)
)

// CLINTUsable meldt of de timerketen bewezen is — de voorwaarde voor slaap.
func CLINTUsable() bool { return clintOK }

// MsipUsable meldt of het IPI-kanaal bewezen is. Zonder blijft slapen gewoon
// aan; alleen een directe wek van HOP (die vandaag nog niemand stuurt) zou dan
// op de 2ms-cap wachten.
func MsipUsable() bool { return msipOK }

// MSleep laat het EIGEN hart slapen tot timebase-tick wake (geklemd op
// SleepCapTicks) en geeft terug hoe lang het sliep. Dit is de M-mode-helft van
// wat een gekooide app via zijn ecall-yield krijgt: dezelfde stap — mtimecmp
// armen, wfi, ontwapenen — zonder de trap, want HOP stáát al in machine mode.
// De probe hieronder heeft deze functie zelf bewezen (stap 4 roept hem aan),
// dus wat er bij boot getest is, is letterlijk wat hier 's nachts draait.
//
// Alleen aanroepen op het hart waar de probe liep (HOP draait single-hart, dus
// dat is vanzelf zo); voor de app-harts bestaat hetzelfde in de switcher-park.
func MSleep(wake uint64) uint64 {
	start := rdtime()
	if wake <= start || !clintOK {
		return 0
	}
	if wake > start+SleepCapTicks {
		wake = start + SleepCapTicks
	}
	SetMtimecmp(mhartid(), wake)
	wfiMTIE()
	SetMtimecmp(mhartid(), ^uint64(0)) // ontwapend achterlaten, altijd
	return rdtime() - start
}

// ProbeCLINT test op het eigen hart of de wek-keten écht bestaat: mtimecmp
// schrijven en terugleden, de timer daadwerkelijk laten vuren (mip.MTIP), een
// wfi die op die wekker terugkeert — dát drietal draagt de slaap — en tot slot
// msip, het optionele IPI-kanaal. Geeft één console-regel terug; de aanroeper
// logt hem.
//
// Veilig omdat er niets ge-enabled wordt: MTIP/MSIP mogen pending worden, maar
// zonder de bijbehorende mie-bit is dat een bit in een register en geen trap.
// Staan die bits toch aan (een runtime die zelf iets met interrupts doet), dan
// slaat de probe het vuur-deel over in plaats van een trap te riskeren.
func ProbeCLINT() string {
	h := mhartid()
	old := getMtimecmp(h)
	defer SetMtimecmp(h, ^uint64(0)) // altijd ontwapend achterlaten

	// 1. Houdt het register een waarde vast? Een gat in de adresruimte leest
	//    als nul of als de vorige bus-waarde terug, en dan stopt het hier.
	SetMtimecmp(h, ^uint64(0))
	if got := getMtimecmp(h); got != ^uint64(0) {
		return fmt.Sprintf("CLINT: mtimecmp[%d] not writable (wrote all-ones, read %#x, was %#x) — hart sleep stays disabled", h, got, old)
	}
	const probeDelay = 25_000 // 1ms op de vaste 25MHz timebase
	want := rdtime() + probeDelay
	SetMtimecmp(h, want)
	if got := getMtimecmp(h); got != want {
		return fmt.Sprintf("CLINT: mtimecmp[%d] readback %#x, wrote %#x — hart sleep stays disabled", h, got, want)
	}

	// 2. Vuurt de comparator ook? Alleen als een trap uitgesloten is.
	if e := mie(); e&mipMTIP != 0 {
		return fmt.Sprintf("CLINT: mtimecmp[%d] holds values, fire test skipped (mie=%#x has MTIE set)", h, e)
	}
	for rdtime() < want+probeDelay { // ruime marge, geen vaste lus-telling
	}
	if p := mip(); p&mipMTIP == 0 {
		return fmt.Sprintf("CLINT: mtimecmp[%d] holds values but never fired (mip=%#x) — hart sleep stays disabled", h, p)
	}

	// 3. Keert een wfi ook écht terug op die wekker? Dit is de stap waar alles
	//    aan hangt en die je niet uit een datasheet mag geloven. mstatus.MIE
	//    moet daarvoor uit staan (anders wordt de pending wekker een genomen
	//    trap in plaats van alleen een wake — en zo'n trap overleeft HopOS
	//    niet), en de comparator staat al pending uit stap 2, dus de wfi mag
	//    per spec niet blijven hangen. De regel hieronder gaat er bewust VOOR
	//    naar de console: blijft hij het laatste teken van leven, dan is de
	//    diagnose al gesteld.
	if s := mstatus(); s&(1<<3) != 0 {
		return fmt.Sprintf("CLINT: mtimecmp[%d] ok, wfi test skipped (mstatus=%#x has MIE set) — hart sleep stays disabled", h, s)
	}
	fmt.Println("board: CLINT: testing wfi wake (silence after this line = wfi never returned)")
	SetMtimecmp(h, rdtime()) // per direct pending
	wfiMTIE()
	SetMtimecmp(h, ^uint64(0))
	clintOK = true // de timerketen draagt de slaap; msip is een optioneel kanaal

	// 4. En de IPI — het kanaal waarmee HOP een slapend hart ooit direct gaat
	//    wekken. OPTIONEEL: zonder msip slaapt een hart gewoon op de timer en
	//    is de wek-latency hoogstens de cap (2ms) — precies het ontwerp zolang
	//    niemand IPI't. Dus een msip-gebrek schakelt de slaap NIET uit, het
	//    onthoudt alleen het kanaal (msipOK → HartWaker geeft dan 0 door).
	//
	//    MET GEDULD gemeten, en dat is de les van de eerste boot (01-08): de
	//    instant-read las "msip never pended", maar het signaal CLINT→mip is
	//    geen combinatorische draad — de mtimecmp-vuurtest gunde zichzelf 2ms
	//    en deze test geen enkele cycle. Nu krijgt elke flank tot 1ms om aan
	//    te komen; pas wat dán nog uitblijft is een eigenschap van het silicium.
	msip := "no ipi (MSIE set, untested)"
	if mie()&mipMSIP == 0 {
		SetMsip(h, true)
		fired := mipSettles(mipMSIP, true)
		SetMsip(h, false)
		cleared := mipSettles(mipMSIP, false)
		switch {
		case fired && cleared:
			msip, msipOK = "msip ok", true
		case !fired:
			msip = "no ipi (msip never pended)"
		default:
			msip = "no ipi (msip stuck pending)"
		}
	}
	return fmt.Sprintf("CLINT: mtimecmp[%d] ok (fired within %dus), wfi wakes, %s — hart sleep available",
		h, probeDelay*2*1000/25_000, msip)
}

// mipSettles wacht tot bit in mip de gevraagde stand aanneemt, hoogstens 1ms.
// De propagatie CLINT→hart kost cycles; wie meteen leest meet zijn eigen
// ongeduld in plaats van het silicium.
func mipSettles(bit uint64, want bool) bool {
	deadline := rdtime() + 25_000 // 1ms
	for rdtime() < deadline {
		if (mip()&bit != 0) == want {
			return true
		}
	}
	return false
}
