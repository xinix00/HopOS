// Package layout is het geheugenplan van HopOS, in twee lagen:
//
//   - De IPA-ABI (constanten): wat een ápp ziet — zijn canonieke linkadres
//     (SlotBase), zijn control-page (CtrlBase), zijn ringen. Dit is het
//     contract tussen HOP en elk app-image en is op élk board identiek: één
//     canoniek artifact, de stage-2 vertaalt. Deze constanten wijzigen =
//     app-ABI breken.
//   - Het PA-plan (Plan, per board via UsePlan): wáár dat alles fysiek ligt —
//     control-pages, ringen, stage-2-tabellen en de partitie-pool. QEMU zet
//     een bewust van de IPA's afwijkend plan (bewijst de splitsing); de Pi
//     legt de pool op zijn volledige DRAM (geen artificiële limiet).
//
// HOP-code gebruikt de *PA-accessors (CtrlPagePA, RingOutboxPA, ...);
// app-code (applib/appnet/smp) de IPA-functies (CtrlPage, RingOutbox, ...).
// Het linkadres (TEXT_START = SlotBase(1) + 0x10000) staat in
// image/qemu-run.sh en moet met de IPA-constanten in sync blijven.
package layout

const (
	// Core 0 — de HOP-kern op QEMU -M virt (RAM begint daar op 0x40000000; op
	// de Pi is HOP's thuis het EEPROM-laadadres en heeft de board-main eigen
	// waarden). De bovenste 16MB van de partitie is DMA-regio (virtio-ringen/
	// buffers) en valt buiten de RAM-declaratie van de runtime, zodat hij
	// device-gemapt en dus niet gecached is.
	HopRAMStart = 0x40000000
	HopRAMSize  = 0x0F000000 // 240MB voor de Go-runtime
	DMABase     = 0x4F000000
	DMASize     = 0x01000000 // 16MB

	// Verdeling van de DMA-regio over de drivers (elk een eigen sub-regio,
	// geen gedeelde allocator nodig): virtio-net onderin, NVMe bovenin.
	NetDMABase  = DMABase
	NetDMASize  = 0x00800000
	NVMeDMABase = DMABase + NetDMASize
	NVMeDMASize = DMASize - NetDMASize

	// App-slots (IPA-ABI): het canonieke adresbeeld van een app. Elke image is
	// op het slot-1-bereik gelinkt; de stage-2 legt dat IPA-venster op de
	// fysieke partitie die partAlloc uit de pool van het board sneed (precies
	// job.MemoryLimit groot — de werkelijke RAM-declaratie wordt bij het laden
	// gepatcht). De stride is dus een IPA-vorm, geen fysieke reservering; de
	// fysieke capaciteit is de pool (Plan.Pool). Canoniek IPA binnen één GB →
	// stage-2 mapt met één L2-tabel (tevens de per-app maat-grens, zie
	// maxLimitFor in slots).
	SlotsBase  = 0x50000000
	SlotStride = 0x20000000 // 512MB IPA-venster per slot

	// SlotCap is de compile-time bovengrens op het aantal slots: de fysieke
	// per-slot regio's (control/ringen/net-ringen/stage-2) worden hiervoor
	// gereserveerd in de carve, en de stub-claim (init.s) dekt hem. Een board
	// gebruikt er runtime MaxSlots van (= zijn ontdekte app-cores). 128 dekt
	// de Ampere Altra (127 app-cores); de Pi's/QEMU zetten MaxSlots lager en
	// laten de rest ongebruikt.
	SlotCap = 128

	// Control-pages (IPA-ABI): buiten alle RAM-declaraties → door alle MMU's
	// als device gemapt → coherent zonder cache-onderhoud. Uitsluitend
	// gealigneerde 64-bit loads/stores gebruiken (zie metal/dev).
	// Pagina 0 (= CtrlBase) is de boot-scratch: cpuinit schrijft er vóór de
	// EL-drop het boot-EL; de EL2-eis van de mains (BootEL ≥ 2, anders
	// weigeren) leest 'm. Slots gebruiken 1..MaxSlots. Fysiek liggen de pages
	// op Plan.CtrlPA (de stage-2 vertaalt); alleen de boot-scratch heeft een
	// eigen fysieke plek (Plan.BootScratchPA — cpuinit draait vóór alles).
	CtrlBase    = 0xB0000000
	CtrlStride  = 0x1000
	BootScratch = CtrlBase
	// DTBPtr (IPA): cpuinit legt op de scratch-page (offset +8) de DTB-pointer
	// neer die de firmware in x0 meegaf; board.MemTotal parset 'm met
	// metal/fw/fdt. HOP leest 'm fysiek via DTBPtrPA().
	DTBPtr = BootScratch + 8

	// hop-ABI ringen per slot (IPA-ABI): outbox (app → HOP: logs én
	// RPC-requests) en inbox (HOP → app: RPC-responses). Fysiek op Plan.RingPA.
	RingBase    = 0xB1000000
	RingStride  = 0x10000 // 64KB per slot
	OutboxOff   = 0x0
	InboxOff    = 0x8000
	RingDataCap = 0x7000 // datacapaciteit per ring (28KB, 8-voud)

	// Stage-2-gebied: door HOP geschreven, door de EL2-trampoline/walker
	// gelezen, voor app-cores onzichtbaar (staat in geen enkele stage-2-map) —
	// dus puur fysiek, geen IPA: de basis is Plan.Stage2PA. De indeling ervan
	// is wél universeel: +0x0 de gedeelde EL2-vectoren van de app-cores
	// (2KB-aligned), en per slot i ≥ 1 een tabelblok op +i*Stage2Stride
	// (L1 +0x0, L2 +0x1000/+0x2000, L3-ctrl +0x3000, L3-ring +0x4000).
	// De revoke-vectoren van de HOP-core staan apart (Plan.RevokeVecPA):
	// dat is de tabel waar cpuinit VBAR_EL2 van core 0 heen zette — een board
	// mag daar zijn eigen boot-diagnostiek in hebben (rpi5: de faultdump-
	// tabel); InitVectors plugt er alleen de HVC-handler in.
	Stage2Stride = 0x10000

	// Parkeer-machinerie (in het slot-0-blok, ná de vectoren; QEMU's
	// revoke-tabel zit op +0x800..+0x1000): HopOS bezit zijn cores — een
	// gestopte app-core gaat NIET terug naar de firmware (PSCI CPU_OFF is op
	// de Pi 5-stockfirmware een one-way door, gemeten 2026-07-10) maar
	// parkeert op EL2 in een WFE-lus op zijn mailbox. HOP herstart 'm door
	// {ctx, doel-PC} in de mailbox te schrijven + SEV; de lus springt dan de
	// (idempotente) trampoline in. PSCI CPU_ON is alleen nog de éérste
	// bring-up per core. Mailbox-woord 0: 0 = cold (nooit geparkeerd),
	// 1 = geparkeerd, 2 = dispatch bevestigd, anders = ctx (startschot);
	// woord 1: doel-PC.
	parkCodeOff = 0x1000
	parkMboxOff = 0x1100
	ParkMboxLen = 16 // bytes per core-mailbox (ctx + pc)

	// Frame-ringen per slot (IPA-ABI, per-slot netwerk): elke app draait een
	// eigen netstack over rauwe Ethernet-frames; HOP is enkel een L2-switch
	// die frames ring-naar-ring kopieert (metal/net/hopswitch). Per slot één
	// 2MB-blok — TX (app → switch) onderin, RX (switch → app) bovenin —
	// zodat de stage-2-kooi het als één blockRW mapt. Device-gemapt, buiten
	// alle RAM-declaraties → coherent. Fysiek op Plan.NetRingPA.
	NetRingBase    = 0xB3000000
	NetRingStride  = 0x200000 // 2MB per slot
	NetTXOff       = 0x0
	NetRXOff       = 0x100000
	NetRingDataCap = 0xFF000 // datacapaciteit per richting (1MB - 4KB slack)
)

// MaxSlots is het aantal app-slots dat deze node gebruikt — geen kunstmatige
// limiet maar de FYSIEKE grens van het board: het aantal ontdekte app-cores
// (127 op de Ampere Altra, 3 op de Pi, 11 op de O6N). Een board zet het bij
// het laden met SetMaxSlots uit zijn discovery; default 3 voor boards die het
// (nog) niet doen. De fysieke per-slot regio's worden voor SlotCap
// gereserveerd, dus MaxSlots mag runtime variëren zonder de carve te raken.
var MaxSlots = 3

// SetMaxSlots zet het gebruikte slot-aantal (geklemd op [1, SlotCap]).
// Aanroepen vóór het eerste slot-gebruik (board-init, vóór UsePlan/NumSlots).
func SetMaxSlots(n int) {
	switch {
	case n < 1:
		n = 1
	case n > SlotCap:
		n = SlotCap
	}
	MaxSlots = n
}

// Region is een aaneengesloten stuk vrij DRAM (fysiek).
type Region struct{ Base, Size uint64 }

// Plan is de fysieke (PA-)kant van het geheugenplan: wáár op dít board de
// control-pages, ringen, stage-2-tabellen en de partitie-pool echt liggen.
// Het board zet zijn plan bij het laden met UsePlan; HOP-code leest het via
// de *PA-accessors. Apps zien hier niets van — hun IPA-beeld (de constanten
// hierboven) is op elk board gelijk en de stage-2 vertaalt.
type Plan struct {
	CtrlPA      uint64 // control-pages: MaxSlots+1 pagina's (4KB-aligned)
	RingPA      uint64 // hop-ABI-ringen: MaxSlots × RingStride (4KB-aligned)
	NetRingPA   uint64 // frame-ringen: MaxSlots × NetRingStride (2MB-aligned!)
	Stage2PA    uint64 // app-core-vectoren + tabelblokken: (MaxSlots+1) × Stage2Stride (2KB-aligned)
	RevokeVecPA uint64 // EL2-vectortabel van de HOP-core (2KB-aligned): waar
	// cpuinit VBAR_EL2 van core 0 heen zette. InitVectors plugt er alleen de
	// HVC-revoke-handler in (offset 0x400) en laat de rest staan — een board
	// mag daar zijn boot-diagnostiek hebben (rpi5: de faultdump-tabel).
	BootScratchPA uint64 // boot-EL-scratch + DTB-pointer (cpuinit-vast, board-asm)
	NetDMAPA      uint64 // NIC-DMA-regio (NetDMASize; buiten élke RAM-declaratie
	// → device-gemapt → coherent met de NIC zonder cache-onderhoud). Optioneel
	// (0 = board gebruikt een eigen constante of heeft geen NIC-DMA-plan):
	// QEMU houdt de vaste NetDMABase binnen HOP's partitie, de Pi-boards
	// leggen 'm hier vast en DTBPool snijdt 'm uit de pool.
	Pool []Region // vrij DRAM voor app-partities (2MB-korrel)
}

var plan Plan

// UsePlan registreert het PA-plan van het board. Eenmalig, in het init() van
// het board-pakket (elke binary importeert zijn board al). Valideert de
// uitlijningseisen die de stage-2-structuur stelt — liever hier hard falen
// dan een scheve map op een core.
func UsePlan(p Plan) {
	switch {
	case p.CtrlPA == 0 || p.CtrlPA&0xFFF != 0:
		panic("layout: Plan.CtrlPA ontbreekt of niet 4KB-aligned")
	case p.RingPA == 0 || p.RingPA&0xFFF != 0:
		panic("layout: Plan.RingPA ontbreekt of niet 4KB-aligned")
	case p.NetRingPA == 0 || p.NetRingPA&(NetRingStride-1) != 0:
		panic("layout: Plan.NetRingPA ontbreekt of niet 2MB-aligned")
	case p.Stage2PA == 0 || p.Stage2PA&0x7FF != 0:
		panic("layout: Plan.Stage2PA ontbreekt of niet 2KB-aligned (VBAR-eis)")
	case p.RevokeVecPA == 0 || p.RevokeVecPA&0x7FF != 0:
		panic("layout: Plan.RevokeVecPA ontbreekt of niet 2KB-aligned (VBAR-eis)")
	case p.BootScratchPA == 0:
		panic("layout: Plan.BootScratchPA ontbreekt")
	case len(p.Pool) == 0:
		panic("layout: Plan.Pool is leeg — geen partitie-geheugen")
	}
	plan = p
}

// NetDMAPA geeft de fysieke NIC-DMA-regio van het plan (NetDMASize groot).
// Alleen geldig op boards die 'm zetten (zie het Plan-veld).
func NetDMAPA() uintptr {
	if plan.NetDMAPA == 0 {
		panic("layout: Plan.NetDMAPA niet gezet — dit board heeft geen NIC-DMA-plan")
	}
	return pa(plan.NetDMAPA)
}

// pa bewaakt dat niemand het PA-plan raakt vóór een board het zette.
func pa(v uint64) uintptr {
	if plan.CtrlPA == 0 {
		panic("layout: geen PA-plan — board-init mist layout.UsePlan")
	}
	return uintptr(v)
}

// CtrlPagePA geeft de fysieke control-page van slot i (HOP-kant; de app leest
// dezelfde page via IPA CtrlPage(i)).
func CtrlPagePA(i int) uintptr { return pa(plan.CtrlPA + uint64(i)*CtrlStride) }

// BootScratchPA/DTBPtrPA: de fysieke boot-scratch (cpuinit-vast).
func BootScratchPA() uintptr { return pa(plan.BootScratchPA) }
func DTBPtrPA() uintptr      { return pa(plan.BootScratchPA + 8) }

// RingOutboxPA/RingInboxPA: de fysieke hop-ABI-ringen van slot i.
func RingOutboxPA(i int) uintptr {
	return pa(plan.RingPA + uint64(i-1)*RingStride + OutboxOff)
}
func RingInboxPA(i int) uintptr {
	return pa(plan.RingPA + uint64(i-1)*RingStride + InboxOff)
}

// NetRingTXPA/NetRingRXPA: de fysieke frame-ringen van slot i.
func NetRingTXPA(i int) uintptr {
	return pa(plan.NetRingPA + uint64(i-1)*NetRingStride + NetTXOff)
}
func NetRingRXPA(i int) uintptr {
	return pa(plan.NetRingPA + uint64(i-1)*NetRingStride + NetRXOff)
}

// VecBasePA is de fysieke basis van de gedeelde EL2-vectoren (app-cores);
// RevokeVecPA die van de vectortabel van de HOP-core (cpuinit-asm moet
// hiermee overeenkomen — het board checkt dat in zijn init).
func VecBasePA() uintptr   { return pa(plan.Stage2PA) }
func RevokeVecPA() uintptr { return pa(plan.RevokeVecPA) }

// Stage2TablePA geeft de fysieke basis van het stage-2-tabelblok van slot i.
func Stage2TablePA(i int) uintptr {
	return pa(plan.Stage2PA + uint64(i)*Stage2Stride)
}

// ParkCodePA is de fysieke plek van de EL2-parkeerlus (door InitVectors
// gegenereerd; de vectoren springen erheen i.p.v. PSCI CPU_OFF te doen).
func ParkCodePA() uintptr { return pa(plan.Stage2PA + parkCodeOff) }

// ParkMboxPA geeft de parkeer-mailbox van een core (16 bytes: ctx + doel-PC).
func ParkMboxPA(core int) uintptr {
	return pa(plan.Stage2PA + parkMboxOff + uint64(core)*ParkMboxLen)
}

// Pool geeft de partitie-pool van het board (voor slots/partmem).
func Pool() []Region {
	pa(plan.CtrlPA) // guard
	return plan.Pool
}

// CarvePool bouwt een partitie-pool uit de fysieke geheugenbanken (uit de DTB)
// minus alle holes (HOP-kern, control-regio's, DTB, /memreserve/). Pure
// interval-rekenkunde — geen DTB-kennis, board-neutraal. Elk resultaat wordt
// naar binnen 2MB-uitgelijnd (stage-2-blokken zijn 2MB) en stukken < min
// vallen weg. Zo benut een board zijn volledige RAM (meerdere banken, ook
// boven 4GB) zonder ooit een hole uit te delen. Leeg = de aanroeper valt terug.
func CarvePool(banks, holes []Region, min uint64) []Region {
	regs := append([]Region(nil), banks...)
	// Elke hole uit elke overlappende bank knippen (kan 'm splitsen).
	for _, h := range holes {
		hEnd := h.Base + h.Size
		var next []Region
		for _, r := range regs {
			rEnd := r.Base + r.Size
			if hEnd <= r.Base || h.Base >= rEnd { // geen overlap
				next = append(next, r)
				continue
			}
			if h.Base > r.Base { // stuk vóór de hole
				next = append(next, Region{Base: r.Base, Size: h.Base - r.Base})
			}
			if hEnd < rEnd { // stuk ná de hole
				next = append(next, Region{Base: hEnd, Size: rEnd - hEnd})
			}
		}
		regs = append([]Region(nil), next...)
	}
	// 2MB-uitlijnen (naar binnen) en te kleine stukken droppen.
	const mb2 = 2 << 20
	var out []Region
	for _, r := range regs {
		base := (r.Base + mb2 - 1) &^ (mb2 - 1)
		end := (r.Base + r.Size) &^ (mb2 - 1)
		if end > base && end-base >= min {
			out = append(out, Region{Base: base, Size: end - base})
		}
	}
	return out
}

// TopAddr is het hoogste fysieke adres dat het PA-plan aanraakt (regio's +
// pool). RequiredRAM (TopAddr − HopRAMStart) is wat de QEMU-vormige mains als
// ondergrens tegen MemTotal houden; een board-main met een eigen thuisadres
// bewaakt zijn plan zelf (de pool ís daar al op MemTotal gesneden).
func TopAddr() uint64 {
	pa(plan.CtrlPA) // guard
	top := plan.CtrlPA + uint64(MaxSlots+1)*CtrlStride
	for _, c := range []uint64{
		plan.RingPA + uint64(MaxSlots)*RingStride,
		plan.NetRingPA + uint64(MaxSlots)*NetRingStride,
		plan.Stage2PA + uint64(MaxSlots+1)*Stage2Stride,
	} {
		if c > top {
			top = c
		}
	}
	for _, r := range plan.Pool {
		if end := r.Base + r.Size; end > top {
			top = end
		}
	}
	return top
}

// RequiredRAM is hoeveel aaneengesloten DRAM vanaf HopRAMStart het plan eist.
// Minder dan dit ⇒ slots/ringen vallen buiten het fysieke RAM: HopOS moet
// dan weigeren i.p.v. fantoom-geheugen uit te delen.
func RequiredRAM() uint64 { return TopAddr() - HopRAMStart }

// RingOutbox geeft het outbox-ringadres (app → HOP) van slot i — IPA (app-kant;
// HOP gebruikt RingOutboxPA).
func RingOutbox(i int) uintptr {
	return uintptr(RingBase + uint64(i-1)*RingStride + OutboxOff)
}

// RingInbox geeft het inbox-ringadres (HOP → app) van slot i — IPA.
func RingInbox(i int) uintptr {
	return uintptr(RingBase + uint64(i-1)*RingStride + InboxOff)
}

// NetRingTX geeft de frame-TX-ring (app → switch) van slot i — IPA; tevens de
// (2MB-gealigneerde) basis van het net-ring-blok in de stage-2-map.
func NetRingTX(i int) uintptr {
	return uintptr(NetRingBase + uint64(i-1)*NetRingStride + NetTXOff)
}

// NetRingRX geeft de frame-RX-ring (switch → app) van slot i — IPA.
func NetRingRX(i int) uintptr {
	return uintptr(NetRingBase + uint64(i-1)*NetRingStride + NetRXOff)
}

// SlotBase geeft de canonieke IPA-basis van slot i (1-based, = core-index) —
// het linkadres-bereik; de fysieke partitie komt uit de pool (partAlloc).
func SlotBase(i int) uint64 {
	return SlotsBase + uint64(i-1)*SlotStride
}

// CtrlPage geeft het control-page-adres van slot i — IPA (app-kant; HOP
// gebruikt CtrlPagePA).
func CtrlPage(i int) uintptr {
	return uintptr(CtrlBase + uint64(i)*CtrlStride)
}

// Control-page indeling: 64-bit scalars in de kop, env-blob in de staart.
const (
	CtrlStatus    = 0x00 // app-status (zie Status*-constanten)
	CtrlExitCode  = 0x08 // gezet door app bij exit
	CtrlKill      = 0x10 // HOP → app: 1 = stop jezelf (coöperatief)
	CtrlHeartbeat = 0x18 // app: oplopende teller (hang-detectie)
	CtrlRAMSize   = 0x20 // app: eigen runtime.MemRegion-maat (bewijs van patch)
	CtrlEnvLen    = 0x28 // HOP → app: lengte van de env-blob in bytes
	CtrlEntry     = 0x30 // HOP → EL2-trampoline: app-entry (EL1) voor de ERET
	CtrlS2Table   = 0x38 // HOP → EL2-trampoline: fysiek adres stage-2 L1-tabel
	CtrlWallOff   = 0x40 // HOP → app: klok-offset (wall-ns bij tellerstand 0;
	// de generic-timer-teller is gedeeld over alle cores, dus HOP's offset
	// geldt exact voor elke app — int64 als uint64-bits, 0 = geen klok)

	// De EL2-trampolines (metal/cpu/el2) zijn data-gedreven: PSCI CPU_ON krijgt de
	// fysieke control-page als ctx en de trampoline leest er alles van. HOP
	// schrijft deze velden bij Start; de offsets staan als literals in de asm —
	// bij verplaatsen ook metal/cpu/el2/*.s aanpassen.
	CtrlVecPA = 0x50 // HOP → tramp: fysieke basis EL2-vectoren (VBAR_EL2)

	// Door de EL2-vectoren (stage2.InitVectors) geschreven vlak vóór de
	// CPU_OFF, zodat HOP kan loggen wáárom een slot viel. LET OP: deze
	// offsets staan als str-immediates in de vector-encodings — bij
	// verplaatsen ook stage2.InitVectors aanpassen. Zowel een echte
	// kooi-overtreding (app greep buiten zijn slot) als HOP's hard-kill
	// (stage-2-intrekking) landen hier als FaultSync: beide zijn een
	// synchrone stage-2-fault. Bij een hard-kill kent HOP de context (het
	// riep Stop → Revoke aan); een spontane FaultSync = een ontsnappingspoging.
	CtrlFaultESR = 0x58 // ESR_EL2: exception syndrome
	CtrlFaultFAR = 0x60 // FAR_EL2: faultadres
	CtrlFaultVec = 0x68 // vectorindex + 1 (0 = geen fault gezien)

	// SMP (fase 5): één app over meerdere cores, gedeelde heap. HOP zet bij
	// Start het aantal cores en waar de app zijn extra cores mag opbrengen; de
	// app-runtime (OS-laag, niet app-code) leest ze en brengt de secundaire
	// cores op via goos.Task. De app zelf is oblivious — hij krijgt N cores
	// "as is" en parallelt via GOMAXPROCS.
	CtrlCores    = 0x70 // HOP → app: aantal cores (≥1; 1 = geen SMP)
	CtrlSMPTramp = 0x80 // HOP → app: fysiek adres EL2 SMP-trampoline (HOP-image)

	// SMP-handoff (app → secundaire core): goos.Task schrijft hier de M-context
	// voor de core die het opbrengt, de EL2-trampoline leest ze. Onder een
	// mutex geschreven (één core-boot tegelijk), dus één handoff-venster volstaat.
	CtrlSMPSp    = 0x88 // stacktop voor de nieuwe M (IPA)
	CtrlSMPMp    = 0x90 // *m (IPA)
	CtrlSMPG0    = 0x98 // *g (g0 van de nieuwe M, IPA)
	CtrlSMPFn    = 0xA0 // entry (mstart, IPA)
	CtrlSMPStub  = 0xA8 // app-IPA van de EL1-stub waar de EL2-tramp naar ERET't
	CtrlSMPTtbr0 = 0xB0 // stage-1 L1-tabel voor de nieuwe core (= RamStart+0x4000,
	// IPA); zo hoeft de EL1-stub géén geheugen te lezen vóór zijn MMU aan staat
	// (elke pre-MMU-lees zou een primaire-gecachte waarde stale kunnen zien)
	CtrlSlot = 0xB8 // HOP → tramp: slotnummer = VMID (de app is oblivious)
	// CtrlSMPReq (app → HOP): core-index die de app-runtime als extra SMP-core
	// wil (goos.Task). De app kan geparkeerde cores niet zelf dispatchen (de
	// mailboxen zijn bewust buiten elke stage-2-map); HOP's servicer ziet het
	// verzoek, valideert het tegen CtrlCores en dispatcht. 0 = geen verzoek.
	CtrlSMPReq = 0xC0
	// CtrlMboxPA (HOP → tramp): fysiek adres van de parkeer-mailbox van déze
	// core; de trampoline zet 'm in TPIDR_EL2 zodat de parkeerlus 'm terugvindt
	// zonder MPIDR-decodering. CtrlSMPMbox: idem voor de secundaire SMP-core
	// die HOP dispatcht (de primaire ctrl-page is gedeeld, dus de secundaire
	// mailbox komt via dit aparte veld dat HOP vlak vóór de dispatch zet).
	CtrlMboxPA  = 0xC8
	CtrlSMPMbox = 0xD0
	// CtrlIdle (app → HOP): idle-tik-teller. De idle-governor (metal/cpu/idle)
	// verhoogt hem één keer per idle-ronde (event-stream, ~1,2ms bij 54MHz);
	// tikt de teller op vol tempo dan is de core idle, staat hij stil dan
	// draait er code. De klokwachter (metal/driver/dvfs, OS-taak op de HOP-core)
	// leest de delta's en klokt de node op/terug — HOP is oblivious. Bij
	// SMP delen de cores van een slot deze teller: de wachter deelt het
	// verwachte tempo door CtrlCores.
	CtrlIdle = 0xD8

	// Apploader → HOP: de grootte van de image die de loader in de staging
	// bovenin zijn eigen partitie heeft gedownload. HOP leest 'm bij StatusStaged
	// en plaatst de echte app vanaf de staging (StartStaged). Niet vertrouwd voor
	// isolatie: een verkeerde maat faalt hooguit de ELF-parse van deze partitie.
	CtrlStagedSize = 0xE0

	// Env-blob: door HOP geschreven "key=val\n..."-bytes die de app-lib bij
	// start inleest (de Docker-vorm: env meegegeven bij het starten). Vervangt
	// het kernel-envp dat bare metal niet heeft.
	CtrlEnvData = 0x100
	CtrlEnvMax  = CtrlStride - CtrlEnvData
)

// Status-waarden.
const (
	StatusEmpty   = 0 // HOP heeft de pagina geveegd
	StatusBooting = 1 // HOP heeft CPU_ON gedaan, app-runtime nog niet klaar
	StatusReady   = 2 // app-runtime draait (gezet door applib)
	StatusExited  = 3 // app is gestopt (exitcode in CtrlExitCode)
	StatusStaged  = 4 // apploader heeft de echte image gestaged + geparkeerd;
	// HOP plaatst 'm en her-dispatcht de core (StartStaged)
)

// CtrlFaultVec-waarden (vectorindex + 1; de relevante paden benoemd).
const (
	FaultNone = 0 // geen fault gezien sinds de laatste start
	FaultSync = 9 // synchroon vanuit EL1 (idx 8): stage-2-fault, ESR/FAR geldig.
	// Zowel een kooi-overtreding als HOP's hard-kill (stage-2-intrekking) landen
	// hier: beide zijn een stage-2 translatie-fault. Er is geen aparte IRQ-route
	// meer (de hard-kill gebruikt geen GIC/SGI).
)

// Intern net (per-slot netwerk: metal/net/hopswitch aan de HOP-kant,
// applib/appnet aan de app-kant). Deterministisch, geen tabellen die leren:
// HOP is de gateway op .1, slot i op .(i+1)/24, MAC 02:00:00:00:00:<slot>
// (HOP = ..:00). Eén bron van waarheid, zodat de switch en de app-stack nooit
// uiteenlopen — daarom hoeft HOP dit niet meer per slot op de control-page te
// schrijven; beide kanten leiden het uit het slotnummer af.
const (
	NetPrefix  = 24
	netA, netB = 10, 100 // subnet 10.100.0.0/24
)

// SlotIP4 geeft het interne IPv4 van slot i als big-endian uint32; slot 0 is
// HOP zelf (.1), slot i een app (.(i+1)).
func SlotIP4(i int) uint32 { return netA<<24 | netB<<16 | uint32(i+1) }

// HostIP4 is HOP's interne adres — de gateway die de apps als default route
// krijgen (en waarvoor de switch ARP beantwoordt).
func HostIP4() uint32 { return SlotIP4(0) }

// SlotMAC geeft de deterministische MAC van slot i (HOP = slot 0 → ..:00).
func SlotMAC(i int) [6]byte { return [6]byte{0x02, 0, 0, 0, 0, byte(i)} }
