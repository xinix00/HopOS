// Package layout is het geheugenplan van HopOS, in twee lagen:
//
//   - De slot-ABI: wat een app ziet. Dat is één regio — zijn eigen partitie.
//     Onderin zijn RAM (RamStart/RamSize, door HOP in élk image gepatcht),
//     bovenin een staart van AbiTail bytes met zijn control page, zijn
//     hop-ABI-ringen en zijn frame-ringen. Een app rekent dus alles uit twee
//     waarden die al in zijn image staan en kent geen enkel absoluut adres.
//     Deze indeling wijzigen = de app-ABI breken; daarom staat er een versie op
//     (ABIVersion) en weigert HOP bij plaatsing een image van een andere versie.
//   - Het PA-plan (Plan, per board via UsePlan): waar HOP's éigen structuren
//     fysiek liggen — de partitie-pool waar slots uit gesneden worden, de
//     stage-2-tabellen met hun ctx-blokken en park-mailboxen, de boot-scratch,
//     en de control-pages van HOP's eigen node-cores (die hebben geen partitie).
//
// Het canonieke linkadres (SlotBase(1) + 0x10000) hoort bij de MAP-helft van de
// kooi: die legt elke partitie op hetzelfde adres, zodat één artifact in elk slot
// draait. Op ARM doet de stage-2-tabel dat (en tegelijk het begrenzen); op RISC-V
// doet een aparte tabel het (kern/cage Relocate) naast de PMP-whitelist
// die begrenst. Verplaatst een architectuur niet, dan ís het linkadres de
// partitie zelf en bestaat er maar één slot — zie cageRelocates in de kooi-naad
// (kern/slots). image/qemu-run.sh moet met SlotBase in sync blijven.
package layout

import (
	"fmt"
	"sort"
)

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

	// De boot-scratch (IPA): buiten alle RAM-declaraties → door alle MMU's als
	// device gemapt → coherent zonder cache-onderhoud. Uitsluitend gealigneerde
	// 64-bit loads/stores (zie metal/dev). cpuinit schrijft er vóór de EL-drop
	// het boot-EL; de EL2-eis van de mains (BootEL ≥ 2, anders weigeren) leest 'm.
	// Fysiek: Plan.BootScratchPA (cpuinit draait vóór alles).
	//
	// Dit is het énige IPA-venster buiten de partitie dat een slot nog ziet, en
	// het is read-only. De control-pages van app-slots woonden hier ook — die
	// liggen sinds ABIVersion 2 in de partitie-staart (de slot-ABI hierboven);
	// wat er nog van deze regio over is, is Plan.NodeCtrlPA voor HOP's eigen
	// cores.
	CtrlBase    = 0xB0000000
	CtrlStride  = 0x1000
	BootScratch = CtrlBase
	// DTBPtr (IPA): cpuinit legt op de scratch-page (offset +8) de DTB-pointer
	// neer die de firmware in x0 meegaf; board.MemTotal parset 'm met
	// metal/fw/fdt (de Pi-boards lezen hem via hun eigen DTBPtr-constante).
	DTBPtr = BootScratch + 8

	// FB-grant-venster (IPA-ABI, kern/slots/fbgrant.go): de firmware-
	// framebuffer van de display-houder wordt híer gemapt — GB0 is vrij in
	// het canonieke beeld, en de fysieke fb mag boven de 4GB liggen (32-bit
	// IPA, VTCR.T0SZ=32) dus identity kan niet. De app krijgt het adres als
	// FB_BASE-env (FbIPA + offset-in-blok); alleen de houder heeft dit GB.
	FbIPA = 0x20000000

	// hop-ABI-ringen per slot: outbox (app → HOP: logs én RPC-verzoeken) en
	// inbox (HOP → app: antwoorden). Ze liggen in de ABI-staart van de partitie
	// (AbiRingOff); dit zijn de offsets binnen die 64KB.
	RingStride  = 0x10000 // 64KB per slot: twee ringen van 32KB
	OutboxOff   = 0x0
	InboxOff    = 0x8000
	RingDataCap = RingStride/2 - 0x1000 // per ring, minus de ringkop (28KB, 8-voud)

	// Stage-2-gebied: door HOP geschreven, door de EL2-trampoline/walker
	// gelezen, voor app-cores onzichtbaar (staat in geen enkele stage-2-map) —
	// dus puur fysiek, geen IPA: de basis is Plan.Stage2PA. De indeling ervan
	// is wél universeel: +0x0 de gedeelde EL2-vectoren van de app-cores
	// (2KB-aligned), en per slot i ≥ 1 een tabelblok op +i*Stage2Stride
	// (L1 +0x0, L2 +0x1000/+0x2000, L3-ctrl +0x3000, L3-ring +0x4000,
	// net-ring-L2 +0x5000, en op +CtxOff het switch-contextblok van het slot —
	// zie hieronder). De revoke-vectoren van de HOP-core staan apart
	// (Plan.RevokeVecPA): dat is de tabel waar cpuinit VBAR_EL2 van core 0
	// heen zette — een board mag daar zijn eigen boot-diagnostiek in hebben
	// (rpi5: de faultdump-tabel); InitVectors plugt er alleen de HVC-handler in.
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
	// Per core is de mailbox uitgegroeid tot een sched-blok van 256 bytes:
	// de parkeer-mailbox (woord 0/1) plus de staat van de coöperatieve
	// core-deling (cpu/el2/switch.s) — register-scratch voor de vector-thunks,
	// de bewonerslijst (round-robin over slots die deze core delen) en de
	// plan-PA's die de EL2-switch nodig heeft. TPIDR_EL2 van de core wijst
	// naar dit blok; SP_EL2 naar +SchedScratch (door de trampolines gezet).
	// (MaxSlots+1) blokken vanaf parkMboxOff: tot +0x9200, ruim binnen het
	// slot-0-stride-blok — slot 0 draagt geen tabellen/context, dus de
	// CtxOff-overlap met een "slot-0-context" bestaat niet.
	// LET OP: de Sched*-offsets staan als literals in cpu/el2/switch.s en in
	// de thunk-generator (kern/stage2) — bij verplaatsen beide aanpassen.
	ParkMboxLen = 256

	// Sched-blok-indeling (offsets binnen het 256B-blok van een core).
	//
	// De indeling is verdeeld naar SCHRIJVER, niet naar onderwerp, en de grens
	// loopt op cachelines. Dat is geen netheid: op RISC-V zijn HOP's hart en het
	// app-hart niet coherent (gemeten 30-07), en de switcher daar draait in
	// machine mode zónder MMU — zijn writes zijn dus cachebaar. Twee schrijvers
	// in één 64B-regel betekent dan dataverlies: wie zijn regel terugschrijft,
	// schrijft de bytes van de ander terug zoals ze bij zíjn fetch stonden. Dat
	// is dezelfde les als bij de ringkoppen (abi/ring, ABI 3), en hij geldt hier
	// scherper omdat HOP de bewonerslijst muteert terwijl de switcher roteert.
	//
	//	regel 0 (0..63)     ALLEEN de arch-laag op de core zelf
	//	regel 1..3 (64..)   ALLEEN HOP (lijst, lengte, plan-PA)
	//
	// De park-mailbox (woord 0/1) hoort formeel bij HOP, maar bestaat alleen op
	// ARM — en daar is dit blok device-gemapt en dus coherent. Op RISC-V is er
	// geen parkeerlus (een hart draait of staat in reset), dus schrijft HOP daar
	// niets in regel 0. Behalve bij residentReset: dan staat het hart stil en is
	// zijn cache per definitie leeg.
	SchedScratch = 16 // 4×8B: werkregisters van de getrapte app (ARM: x0..x3 in
	// de vector-thunks; RISC-V: x5..x7 bij de trap-entry, één woord blijft vrij)
	// SchedCurrent/SchedRotor: de staat van de rotatie zoals de switcher hem
	// bijhoudt. Alleen RISC-V (cpu/mmode) gebruikt ze:
	//
	//   - Current = welk slot er NU draait. Op ARM staat dat antwoord in het VMID
	//     van VTTBR, dus leest de EL2-switch het uit het silicium; RISC-V kent
	//     geen VMID en moet het getal onthouden.
	//   - Rotor = de laatst gedispatchte lijst-index, de RISC-V-tegenhanger van
	//     SchedCursor. Een eigen veld en niet SchedCursor hergebruiken, want dat
	//     woord ligt in HOP's regel — zie de cacheline-verdeling hierboven.
	SchedCurrent = 48
	SchedRotor   = 56
	SchedCursor  = 80  // ARM: laatst geplande lijst-index (u64)
	SchedCount   = 88  // lijstlengte (u64, monotoon; 0-bytes zijn gaten)
	SchedList    = 96  // SlotCap × 1 byte: slotnummers van de bewoners
	SchedS2PA    = 224 // fysieke Plan.Stage2PA (switch.s: ctx/VTTBR/park afleiden)

	// De wekker van dit hart, voor de slaap-stand van de switcher (RISC-V).
	// HOP vult ze pas ná zijn CLINT-probe (board/licheerv/clint.go): staat
	// SchedClintPA op nul, dan slaapt de parkeerlus niet maar spint hij, precies
	// zoals vóór de slaap-stand bestond. Fail-safe in die richting is de hele
	// reden dat het twee losse velden zijn en geen vlag — een hart dat níet
	// wakker wordt is een dode node, en dat mag nooit de standaard zijn.
	SchedClintPA  = 232 // PA van mtimecmp van DIT hart (0 = geen wekker)
	SchedSleepCap = 240 // maximale slaapduur in timebase-tikken
	SchedMsipPA   = 248 // PA van msip van DIT hart (het wek-IPI van HOP)

	// Switch-contextblok per slot (op Stage2TablePA(i)+CtxOff): de EL1-staat
	// van een geyielde bewoner van een gedeelde core, gesaved/gerestored door
	// cpu/el2/switch.s. CtxState is tevens het slot-levensteken dat HOP leest
	// (kern/slots): de vector-paden zetten 'm op dead bij een exit of fault.
	// LET OP: alle offsets staan als literals in switch.s — samen wijzigen.
	CtxOff   = 0x6000
	CtxState = 0 // CtxEmpty..CtxDead (zie onder)
	// CtxCtrlPA: de control-page-PA van de bewoner van dit slot. HOP zet hem bij
	// élke start (armSlot). Twee lezers, en die hebben hem nodig omdát de ABI in
	// de partitie woont: de EL2-trampoline krijgt hem als x0 bij een cold boot,
	// en het fault-rapport van switch.s vindt er de page waar hij ESR/FAR/vec
	// neerlegt. Vroeger rekende die asm 'm uit (plan-basis + slot<<12) — dat kan
	// niet meer, en hoeft ook niet: het adres staat hier.
	CtxCtrlPA = 8
	CtxBootPC = 16 // HOP → de arch-entry: trampoline/vector-PA voor een cold boot

	// De rest is de bewaarde staat van een geyielde bewoner. Eén indeling voor
	// beide architecturen, want het is één begrip — "alles wat de volgende
	// bewoner NIET mag erven" — met per architectuur andere registers erin:
	//
	//	CtxGPRs    31 registers: ARM x0..x30, RISC-V x1..x31
	//	CtxSP      ARM sp_el0/sp_el1; RISC-V ongebruikt (sp ís x2, zit in CtxGPRs)
	//	CtxResume  hervat-PC + status: ARM elr_el2/spsr_el2, RISC-V sepc/sstatus
	//	CtxRegime  het vertaal-/kooi-regime:
	//	             ARM    19 EL1-sysregs (volgorde: cpu/el2/switch.s)
	//	             RISC-V satp, stvec, sscratch, pmpcfg0, pmpaddr0..7 — ACHT
	//	                    adres-entries, want kern/cage codeert elk venster als
	//	                    TOR (onder- én bovengrens)
	CtxGPRs   = 24
	CtxSP     = 272
	CtxResume = 288
	CtxRegime = 304 // einde blok: ARM 456, RISC-V 400
	// CtxWake: de wektijd die de bewoner bij zijn yield meegaf (ARM: x1 bij de
	// hvc #1, RISC-V: a0 bij de ecall) — vóór deze tellerstand (CNTVCT/rdtime)
	// hoeft de rotatie hem niet te hervatten. 0 = nu, en dat is ook wat een
	// bewoner krijgt die niets meegeeft: het oude gedrag is de terugval. Winst:
	// twee wachtende apps pingpongen niet meer tegen elkaar aan en het hart mag
	// slapen zolang niemand due is (RISC-V: mtimecmp+wfi; ARM: de WFE-lus die
	// er al stond). Preemptie is het NIET — de wekker haalt nooit een lopende
	// app van zijn core, hij beëindigt alleen de slaap van het hart zelf.
	//
	// Ná het regime van beide architecturen (464 > 456), en één schrijver: de
	// switcher. HOP leest noch schrijft dit woord — op het niet-coherente
	// RISC-V-hartpaar mág dat ook niet zomaar (zie de sched-blok-regels).
	CtxWake = 464
	// FP staat bewust NIET in dit blok, op geen van beide architecturen: de laag
	// die HOP bezit draait met zijn MMU uit (Device-geheugen) en een SIMD-store
	// naar Device faultt op ijzer. De yielder bewaart zijn eigen callee-saved
	// FP-staat op zijn eigen stack (Normal geheugen). Tot 0x6000 is vrij.

	// CtxState-waarden. HOP schrijft Empty/BootPending/Running (bij een
	// mailbox-dispatch), de EL2-switch schrijft Running/Saved/Dead.
	CtxEmpty       = 0 // geen bewoner (vers of vrijgegeven)
	CtxBootPending = 1 // HOP zette BootCtx/PC klaar; EL2 cold-boot bij rotatie
	CtxSaved       = 2 // context geldig — geyield, hervatbaar
	CtxRunning     = 3 // draait nu op zijn core
	CtxDead        = 4 // geëindigd (exit, fault of revoke) — EL2 slaat 'm over

	// Frame-ringen per slot (IPA-ABI, per-slot netwerk): elke app draait een
	// eigen netstack over rauwe Ethernet-frames; HOP is enkel een L2-switch
	// die frames ring-naar-ring kopieert (metal/net/hopswitch). Per slot één
	// 2MB-blok — TX (app → switch) onderin, RX (switch → app) bovenin —
	// zodat de stage-2-kooi het als één blockRW mapt. Device-gemapt, buiten
	// alle RAM-declaraties → coherent. Fysiek: in de ABI-staart van de eigen
	// partitie van het slot (RamSize = partitie − AbiTail, zie kern/slots) —
	// ring-geheugen schaalt zo mee met wat er écht draait, geen statische
	// SlotCap-reservering in het board-plan.
	// NetRingBase heeft een EIGEN GB (niet de ctrl/ring-GB): 128 × 2MB = 256MB
	// past nooit boven CtrlBase in het 0x80000000-GB — de oude basis
	// (0xB3000000) liet slot ≥ 105 over de 1GB-L2-grens lopen: ring-IPA
	// ongemapt → stage-2-fault op de eerste ring-read (FAR 0xC0000010,
	// gemeten op de Altra 15-07: precies 104 slots leefden, 23 stierven).
	// De kooi mapt dit GB met een eigen L2 (stage2 l2Net).
	// Frame-ringen: wat er in de staart ná de mailbox-regio (AbiNetOff) overblijft,
	// eerlijk in twee richtingen — TX onderin, RX erboven. Afgeleid en niet met de
	// hand uitgerekend: dan is "past het?" een eigenschap van de indeling en geen
	// hoop. Wijzigt AbiTail of AbiNetOff, dan schuiven deze mee.
	NetRingHalf    = (AbiTail - AbiNetOff) / 2 // 960KB per richting
	NetTXOff       = 0x0
	NetRXOff       = NetRingHalf
	NetRingDataCap = NetRingHalf - 0x1000 // minus de ringkop
)

// Coalesce sorteert regio's op basis en smelt overlappende/aangrenzende
// samen. Firmware-memory-maps (UEFI) beschrijven aaneengesloten RAM als
// duizenden losse descriptors (Conventional/BSData/BSCode om en om) —
// descriptor-grenzen zijn administratie, geen RAM-grenzen. Zonder mergen
// raakt een pool "vol of gefragmenteerd" terwijl er honderden GB vrij is
// (Altra, gemeten 14-07: 300GB pool, geen gat ≥ 96MB meer na 12 taken).
// Aanroepen VÓÓR uitlijnen/trimmen: elke kunstmatige grens kost anders tot
// 4MB en laat snippers < korrel sterven.
func Coalesce(regs []Region) []Region {
	sort.Slice(regs, func(i, j int) bool { return regs[i].Base < regs[j].Base })
	out := regs[:0]
	for _, r := range regs {
		if n := len(out); n > 0 && r.Base <= out[n-1].Base+out[n-1].Size {
			if end := r.Base + r.Size; end > out[n-1].Base+out[n-1].Size {
				out[n-1].Size = end - out[n-1].Base
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// StageAddr is het staging-contract tussen HOP en de apploader: de image
// landt 8-uitgelijnd tegen de bovenkant van het app-RAM. Beide kanten rekenen
// met dezelfde functie — de app in IPA (RAMStart+RAMSize), HOP in PA
// (partitiebasis+appRAM) — dus de compiler bewaakt de pariteit die eerst een
// comment moest bewaken ("dezelfde formule, anders lezen we naast de image").
// ok=false: de image past niet (of imgSize is onzin).
func StageAddr(ramBase, ramSize uint64, imgSize int64) (addr, staged uint64, ok bool) {
	staged = (uint64(imgSize) + 7) &^ 7
	if imgSize <= 0 || staged >= ramSize {
		return 0, staged, false
	}
	return ramBase + ramSize - staged, staged, true
}

// IP4Str formatteert een adres uit het layout-net-plan (uint32,
// host-volgorde) als dotted-quad — de string-vorm hoort bij de bron van het
// plan, niet gekopieerd bij elke gebruiker (hopswitch, appnet).
func IP4Str(v uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d", byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// MaxSlots is de KOOI-capaciteit: het hoogste slot-(=kooi-)nummer dat deze node
// mag gebruiken. Sinds de coöperatieve core-deling is een kooi (eigen partitie/
// stage-2/netstack) losgekoppeld van een fysieke core — meerdere kooien mogen
// één core delen (sharegroup), dus er mogen MÉÉR kooien dan cores zijn. De
// per-kooi fysieke regio's (ctrl/ringen/stage-2) zijn voor SlotCap gereserveerd
// in de carve, dus dit mag tot SlotCap oplopen zonder de pool te raken. Default
// = SlotCap (de carve reserveert het toch); een board hoeft dit niet te zetten.
var MaxSlots = SlotCap

// numAppCores is de FYSIEKE grens: het aantal app-cores van dit board (127 op de
// Ampere Altra, 3 op de Pi, 11 op de O6N). Waar MaxSlots de kooien telt, telt
// dit de cores waarop HOPOS ze plaatst (dedicated of gedeeld via een pool). Een
// board zet het met SetAppCores uit zijn discovery; default 3 (Pi/QEMU, die de
// PSCI-probe in kern/slots het verder laat verfijnen). Nooit boven SlotCap.
var numAppCores = 3

// SetAppCores zet het aantal fysieke app-cores (geklemd op [1, SlotCap]).
// Aanroepen vóór het eerste slot-gebruik (board-init, vóór UsePlan/NumSlots).
func SetAppCores(n int) {
	switch {
	case n < 1:
		n = 1
	case n > SlotCap:
		n = SlotCap
	}
	numAppCores = n
}

// NumAppCores is de fysieke core-grens (zie numAppCores). kern/slots gebruikt
// dit voor de PSCI-probe, de SMP-core-ranges en de sched-blok-init (per core),
// waar MaxSlots de kooi-grens is (checkSlot, ctx-blokken, de switch-ports).
func NumAppCores() int { return numAppCores }

// SetMaxSlots klemt de kooi-capaciteit (tot SlotCap). Zelden nodig — default is
// al SlotCap; een test of een board dat de kooi-cap bewust wil verlagen zet 'm.
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
	NodeCtrlPA uint64 // control-pages van HOP's ÉIGEN cores (node-SMP): die
	// hebben geen partitie en dus geen ABI-staart om hun handoff in te leggen.
	// MaxSlots+1 pagina's, 4KB-aligned. App-slots staan hier NIET in — die
	// dragen hun control page in hun eigen partitie (de slot-ABI hierboven).
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

	// RAMBase is waar het DRAM van dit board fysiek begint — het meetpunt van
	// RequiredRAM. Optioneel: 0 betekent HopRAMStart, de statische
	// qemuvirt-waarde waar de check historisch tegen rekende. Zet hem op elk
	// board waar het DRAM ergens anders begint, anders vergelijkt RequiredRAM
	// een adres uit dít plan met een basis uit een ánder board en komt er
	// onzin uit (gemeten 30-07 op de LicheeRV: "layout requires 1216 MB" op
	// een 256MB-board, puur door dat verschil). board/uefi sloeg de check om
	// dezelfde reden over.
	RAMBase uint64
}

var plan Plan

// UsePlan registreert het PA-plan van het board. Eenmalig, in het init() van
// het board-pakket (elke binary importeert zijn board al). Valideert de
// uitlijningseisen die de stage-2-structuur stelt — liever hier hard falen
// dan een scheve map op een core.
func UsePlan(p Plan) {
	switch {
	case p.NodeCtrlPA == 0 || p.NodeCtrlPA&0xFFF != 0:
		panic("layout: Plan.NodeCtrlPA ontbreekt of niet 4KB-aligned")
	case p.BootScratchPA == 0:
		panic("layout: Plan.BootScratchPA ontbreekt")
	case len(p.Pool) == 0:
		panic("layout: Plan.Pool is leeg — geen partitie-geheugen")
	}
	// Wat de kooi-mechaniek van dit board nog eist, checkt de arch-helft:
	// stage-2-tabellen en EL2-vectoren bestaan alleen op ARM (plan_arm64.go),
	// een PMP-kooi heeft ze niet (plan_riscv64.go).
	validatePlanArch(p)
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
	if plan.NodeCtrlPA == 0 {
		panic("layout: geen PA-plan — board-init mist layout.UsePlan")
	}
	return uintptr(v)
}

// NodeCtrlPA geeft de control-page van HOP's eigen core (node-SMP): de
// handoff-scratch waar de EL2-trampoline de M-context van de nieuwe core leest.
// Alléén voor node-cores — een app-slot vindt zijn control page in zijn partitie
// (CtrlPageAt), en kern/slots is de enige die dát adres kent.
func NodeCtrlPA(core int) uintptr { return pa(plan.NodeCtrlPA + uint64(core)*CtrlStride) }

// BootScratchPA: de fysieke boot-scratch (cpuinit-vast).
func BootScratchPA() uintptr { return pa(plan.BootScratchPA) }

// De fysieke net-ring-basis van een slot (in de ABI-staart van zijn
// partitie) is bewust GEEN accessor hier: partities leven per job, dus die PA
// bestaat alleen tijdens een lifecycle. kern/slots berekent hem (base+appRAM)
// en geeft hem als parameter door aan ring-init, hopswitch.Attach en
// stage2.Build — er is geen register dat stale kan worden.

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
	pa(plan.NodeCtrlPA) // guard
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
		regs = next // per hole-ronde vers opgebouwd — geen kopie nodig
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
	pa(plan.NodeCtrlPA) // guard
	top := plan.NodeCtrlPA + uint64(MaxSlots+1)*CtrlStride
	// De enige andere vaste plan-regio: de stage-2-tabellen (ctrl/ringen/
	// net-ringen wonen sinds de slot-ABI in de partitie-staart zelf).
	if c := plan.Stage2PA + uint64(MaxSlots+1)*Stage2Stride; c > top {
		top = c
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
func RequiredRAM() uint64 { return TopAddr() - RAMBase() }

// RAMBase is het meetpunt van RequiredRAM: waar het DRAM van dit board begint
// (Plan.RAMBase), of de statische HopRAMStart als het board het niet zet.
func RAMBase() uint64 {
	if plan.RAMBase != 0 {
		return plan.RAMBase
	}
	return HopRAMStart
}

// --- De slot-ABI: de staart van de eigen partitie -------------------------
//
// Een slot heeft één regio die het moet kennen: zijn eigen partitie. Onderin
// zit de app (RamSize bytes: image, heap, stack), bovenin AbiTail bytes die de
// ABI met HOP dragen — control page, hop-ABI-ringen, frame-ringen. De app hoeft
// dus geen enkel absoluut adres te kennen: RamStart en RamSize staan al in zijn
// image (HOP patcht ze bij plaatsing) en al het andere volgt daaruit.
//
// Beide kanten rekenen met dezelfde functies, alleen met hun eigen basis:
//   - de app geeft wat er in RamStart/RamSize staat: op BEIDE architecturen het
//     canonieke linkadres. ARM legt de partitie daar met zijn stage-2; RISC-V met
//     een aparte map-tabel onder satp (Sv39), die de kooi-stub aanzet vóór hij de
//     app binnenlaat. (Dit stond hier als "op RISC-V is adres = adres, er is geen
//     tweede fase" — dat was waar tot de verplaatsing er kwam.);
//   - HOP geeft de fysieke partitiebasis met dezelfde app-RAM-maat.
//
// Waarom dit coherent is zonder cache-onderhoud op ARM: de staart valt buiten
// de RAM-declaratie van de app, en tamago's stage-1 mapt alles daarbuiten als
// device — precies de reden dat de frame-ringen hier al woonden. HOP schrijft
// dezelfde bytes via zijn eigen ongecachete mapping. Op RISC-V bepaalt de
// sysmap de attributen (DRAM is er altijd cachebaar), dus doen de accessors daar
// cache-onderhoud; dat is een eigenschap van die architectuur, niet van deze
// indeling.
//
// HOP's ÉIGEN cores (node-SMP) hebben geen partitie en houden hun control page
// daarom in de plan-regio (NodeCtrlPA): gereserveerde slots waar nooit een app
// komt.
const (
	AbiTail = 0x200000 // 2MB staart per slot, uit RamSize gesneden

	AbiCtrlOff = 0x0     // control page (CtrlStride groot)
	AbiRingOff = 0x1000  // hop-ABI-ringen: outbox + inbox (RingStride groot)
	AbiStubOff = 0x11000 // scratch van de kooi-stub (zie hieronder)
	AbiMapOff  = 0x12000 // map-tabel van de kooi (zie hieronder)
	AbiNetOff  = 0x20000 // frame-ringen: TX + RX (2 × NetRingDataCap)

	// AbiStubOff is voor architecturen waar HOP een kooi-stub vóór de app zet
	// (RISC-V: die programmeert de PMP-kooi en verifieert hem). Die stub moet
	// zijn voortgang ergens kwijt kunnen — en dat kan niet de control page zijn,
	// want die is van de app (status/heartbeat staan er) en niet HOP's plan-regio,
	// want ná het locken van de kooi mag dit hart daar niet meer bij. In de eigen
	// partitie dus, in de slack tussen de ringen en de net-regio:
	//	+0  0xA1 stub leeft · 0xA2 BSS genuld · 0xA3 kooi geverifieerd
	//	    0xFA11 = CageVerify FAALDE (geparkeerd, de app is nooit gestart)
	//	+8  pmpcfg0-readback
	//	+16/+24/+32  mcause/mepc/mtval van een trap vóór de app zijn mtvec zet
	//	+40/+48/+56  misa/marchid/mimpid van dit hart · +64 mxstatus teruggelezen

	// AbiMapOff draagt de MAP-helft van de kooi: de tabel die het canonieke
	// linkadres van een slot naar zijn echte partitie vertaalt (kern/cage
	// Relocate). Op ARM bestaat die niet apart — daar doet de stage-2-tabel
	// begrenzen én verplaatsen, en die woont in HOP's eigen plan-regio.
	//
	// Waarom hier, ín de partitie: de hardware die deze tabel doorloopt is zélf
	// aan de kooi onderworpen, dus een tabel buiten de kooi zou de walk laten
	// faulten. Dat de app erbij kan is geen gat — hij bereikt er nooit iets
	// buiten zijn eigen partitie mee, dus hertekenen schaadt alleen hemzelf. De
	// invariant is de whitelist, niet deze tabel.
	//
	// Twee pagina's is genoeg zolang een partitie binnen één gigabyte valt
	// (wortel + één niveau); de slack tot AbiNetOff (56KB) draagt er veertien.

	// ABIVersion is de versie van álles hierboven: de indeling van de staart, de
	// control-page-offsets en de ringgeometrie. HOP weigert bij plaatsing een
	// image dat tegen een andere versie gelinkt is (abi/place) — een app die op
	// het verkeerde adres leest is anders een stille misread, en dat is precies
	// de klasse fouten die dagen kost. Verhogen bij élke wijziging hierboven die
	// de app-kant raakt.
	//
	// 3 (30-07): head en tail van elke ring liggen elk in hun eigen cacheline
	// (abi/ring) — nodig zodra HOP en de app niet-coherent zijn.
	//
	// AbiMapOff (31-07) verhoogt de versie NIET: die regio raakt de app-kant niet
	// (hij rekent er geen adres uit en leest er nooit), hij vult alleen slack die
	// al gereserveerd was.
	ABIVersion = 3
)

// AbiTailAt geeft de basis van de ABI-staart van een slot: net boven zijn
// app-RAM. ram/ramSize zijn RamStart/RamSize (app-kant) of partitiebasis/app-RAM
// (HOP-kant) — dezelfde rekensom, andere basis.
func AbiTailAt(ram, ramSize uint64) uintptr { return uintptr(ram + ramSize) }

// CtrlPageAt geeft de control page van het slot dat op ram leeft.
func CtrlPageAt(ram, ramSize uint64) uintptr {
	return AbiTailAt(ram, ramSize) + AbiCtrlOff
}

// RingOutboxAt/RingInboxAt geven de hop-ABI-ringen: app → HOP (logs en
// RPC-verzoeken) en HOP → app (antwoorden).
func RingOutboxAt(ram, ramSize uint64) uintptr {
	return AbiTailAt(ram, ramSize) + AbiRingOff + OutboxOff
}

func RingInboxAt(ram, ramSize uint64) uintptr {
	return AbiTailAt(ram, ramSize) + AbiRingOff + InboxOff
}

// NetRingBaseAt geeft de basis van de net-regio in de staart: de switch krijgt
// dít adres en telt zelf NetTXOff/NetRXOff erbij (hopswitch.Attach), net als de
// app-kant doet met de twee accessors hieronder.
func NetRingBaseAt(ram, ramSize uint64) uintptr {
	return AbiTailAt(ram, ramSize) + AbiNetOff
}

// NetRingTXAt/NetRingRXAt geven de frame-ringen (app ↔ hopswitch).
func NetRingTXAt(ram, ramSize uint64) uintptr {
	return AbiTailAt(ram, ramSize) + AbiNetOff + NetTXOff
}

func NetRingRXAt(ram, ramSize uint64) uintptr {
	return AbiTailAt(ram, ramSize) + AbiNetOff + NetRXOff
}

// SlotBase geeft de canonieke IPA-basis van slot i (1-based, = core-index) —
// het linkadres-bereik; de fysieke partitie komt uit de pool (partAlloc).
func SlotBase(i int) uint64 {
	return SlotsBase + uint64(i-1)*SlotStride
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
	// CtrlSMPTcr/CtrlSMPMair/CtrlSMPVbar: de ÁCTIEVE TCR/MAIR/VBAR_EL1 van de
	// dispatchende primaire — sámen met CtrlSMPTtbr0 het complete EL1-
	// vertaalregime dat een secundaire core (app-SMP én node) 1-op-1 erft.
	// Gelezen van de levende registers (smp.Configure/ConfigureNode), blind
	// gezet door de EL1-stub — geen hardcoded kopieën of afleidingen meer.
	// Voor apps zijn de waarden tamago's InitMMU-constanten; voor de node kan
	// het mmu48's 48-bit-wereld zijn (extendVA — de Altra-UART en de SBSA-
	// watchdog wonen op 16TB; de 39-bit-default kon die niet vertalen: fault
	// → watchdog-reset, gemeten 17-07 via de debug-kabel).
	CtrlSMPTcr  = 0xD8
	CtrlSMPVbar = 0x78
	CtrlSMPMair = 0xF8
	// CtrlIdle (app → HOP): idle-teller — geaccumuleerde idle-TIJD in
	// generic-timer-ticks (de governor telt de counterstand rond elke WFE op,
	// metal/cpu/idle; sinds 18-07 tijd i.p.v. rondes — rondes bleken op ijzer
	// door SEV-ruis opgeblazen). Een vol idle core stijgt ~CNTFRQ per
	// seconde, een rekenende core staat stil. Lezers: de klokwachter
	// (metal/driver/dvfs, Pi) en de per-slot CPU-meting (kern/slotmgr) —
	// HOP-beleid, de app is oblivious. Bij SMP delen de cores van een slot
	// deze teller: lezers vermenigvuldigen het verwachte tempo met CtrlCores.
	//
	// Op 0x48 (het gat tussen CtrlWallOff en CtrlVecPA) — stond op 0xD8 en
	// botste daar met CtrlSMPTcr (Dereks vondst 18-07): de ~1,2ms-teller van
	// de primaire kon het zojuist neergelegde vertaalregime van een lazy
	// SMP-bring-up overschrijven — de secundaire erfde dan een tellerstand
	// als TCR. De uniekheidstest (ctrl_offsets_test.go) bewaakt dit voortaan.
	CtrlIdle = 0x48

	// Apploader → HOP: de grootte van de image die de loader in de staging
	// bovenin zijn eigen partitie heeft gedownload. HOP leest 'm bij StatusStaged
	// en plaatst de echte app vanaf de staging (StartStaged). Niet vertrouwd voor
	// isolatie: een verkeerde maat faalt hooguit de ELF-parse van deze partitie.
	CtrlStagedSize = 0xE0

	// App → HOP: de werkelijke geheugen-draw van de app — runtime MemStats.Sys,
	// alles wat de Go-runtime uit zijn RamSize heeft geclaimd (heap+stacks+
	// runtime; de statische image zit er niet in, die kent HOP al van het
	// artifact). De applib-watch ververst hem elke ~2s naast de heartbeat;
	// HOP leest hem in slots.Get en rapporteert per task ("HOP weet niet
	// alleen wat een app mág, maar ook wat hij gebrúíkt"). 0 = nog niet
	// gerapporteerd (de control-page wordt bij elke start geveegd).
	CtrlMemSys = 0xE8

	// Apploader → HOP (zelfplaatsing, 15-07): IPA van het door de loader
	// gegenereerde plaatsings-stubje — een platte instructielijst net onder de
	// staging die op de eigen core de segmenten op hun linkadressen schuift,
	// BSS nult, RamStart/RamSize/slotHint patcht en de app-entry inspringt.
	// HOP dispatcht de geparkeerde core hierheen i.p.v. zelf bytes te
	// schuiven: het kopieerwerk wordt per-slot-parallel en core 0 houdt alleen
	// kooi+dispatch over. 0 = geen zelfplaatsing → HOP plaatst legacy vanaf de
	// staging (óók het vangnet voor een image die de loader niet kon parsen).
	// Niet vertrouwd voor isolatie: het stubje draait ín de kooi — wijst het
	// verkeerd, dan fault alleen dit slot (vec/ESR/FAR als elke overtreding).
	CtrlPlaceEntry = 0xF0

	// CtrlShared (HOP → app): 1 als dit slot zijn fysieke core deelt met een
	// ander slot (coöperatieve core-deling, fase 6). De idle-governor
	// (metal/cpu/idle) leest dit: 0 = dedicated core → gewone WFE-slaap;
	// 1 = gedeeld → expliciete HVC-yield naar de EL2-switch (cpu/el2/switch.s)
	// zodat de mede-bewoner zijn beurt krijgt. HOP zet 'm dynamisch: 1 zodra er
	// een tweede bewoner bij komt, terug naar 0 als er weer één overblijft
	// (kern/slots refreshShared). App-code merkt er niets van — puur OS-laag.
	CtrlShared = 0x100

	// CtrlWakes (app → HOP): oplopend aantal idle-rondes van de scheduler van
	// dit slot. De tweelingbroer van CtrlIdle: die zegt HOEVEEL tijd er niets
	// gedaan is, deze zegt HOE VAAK er gekeken is (metal/cpu/idle/wakes.go).
	//
	// BEWUST ONGELEZEN (besluit Derek 06-08). De app publiceert 'm, HOP leest
	// 'm niet, en dat blijft zo tot er een aanleiding is.
	//
	// Hier stond dat je ze pas samen kon duiden. Dat was overdreven: het
	// cpu-percentage in kern/slotmgr is gewoon "verwachte tikken min geslapen
	// tikken, gedeeld door het totaal" — slaap ís geen load en wordt ook niet
	// als load geteld. Dat is dezelfde vorm die elk ander OS gebruikt, en het
	// getal klopt zonder deze teller.
	//
	// Waar hij wél voor is: één specifiek geval, namelijk een percentage dat
	// onverklaarbaar hoog staat. Wakker worden kost cycli die geen werk zijn
	// (yield, staat saven, EL2-rondje, terug) en die tellen als niet-slapend.
	// Een app die duizenden keren per seconde wakker wordt kan zo op tientallen
	// procenten staan zonder iets nuttigs te doen — en dát is te repareren
	// (timers samenvoegen) in plaats van echt werk. Zie je zulke load, dan is
	// dit veld één regel in liveStatus en één kolom in de usage-lus verderop.
	//
	// Gemeten op de Radxa 06-08, en de reden dat het niet nu al gebeurt: zes
	// GUI-apps op drie gedeelde cores lazen 0/0/1/1/4/6% — nette load, niets
	// om te verklaren.
	CtrlWakes = 0x108

	// Env-blob: door HOP geschreven "key=val\n..."-bytes die de app-lib bij
	// start inleest (de Docker-vorm: env meegegeven bij het starten). Vervangt
	// het kernel-envp dat bare metal niet heeft.
	CtrlEnvData = 0x110
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
