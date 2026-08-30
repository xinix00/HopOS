// De M-mode-switch: de RISC-V-tegenhanger van cpu/el2/switch.s, en bedoeld om
// naast dat bestand gelezen te worden — het is dezelfde machine, andere letters.
//
//	ARM                        RISC-V
//	EL2-vector (el2entry)      mtvec → mentry
//	HVC-yield uit EL1          ecall uit S-mode (mcause 9)
//	VTTBR omzetten             satp + pmpcfg0/pmpaddr0..7 omzetten
//	VMID zegt wie draait       SchedCurrent zegt het
//	ERET                       mret
//	19 EL1-sysregs bewaren     satp/stvec/sscratch bewaren
//	parkeerlus op EL2          wfi op een CLINT-wekker (of spinnen zonder)
//
// Waarom dit in HÓP's image staat en niet in de kooi-stub: met twee bewoners op
// één hart mag de code die tussen hen wisselt niet in het geheugen van één van
// hen liggen — app A zou hem herschrijven en zo app B overnemen. Dat kan alleen
// omdat de PMP-entries niet meer gelockt zijn (kern/cage): gelockt bindt PMP óók
// M-mode, en dan zou deze code buiten de partitie niet eens uitvoerbaar zijn.
//
// Het WISSELmoment is een EXPLICIETE yield, net als op ARM: geen timer, geen
// interrupt-controller. Een app die nooit yieldt starft zijn buren — dat is per
// ontwerp (compute hoort op een eigen core, en HOP's liveness ziet de gestokte
// heartbeat). Dat blijft zo, en de kill-tick hieronder verandert het niet: die
// hervat altijd dezelfde bewoner en roteert nooit.
//
// DE KILL-TICK. Er is één ding dat een coöperatieve rotatie niet kan, en het is
// geen scheduling maar een isolatie-invariant: HOP moet een bewoner kunnen
// BEËINDIGEN die niet meewerkt. Op een hart dat in reset te zetten is doet het
// SoC-resetblok dat (kern/slots cageRevoke, gemeten 30-07). Maar sinds HOP naar
// de kleine core verhuisde (de loterij, board/licheerv/hop/cpuinit_riscv64.s) is de core waar hij vandaan
// komt precies zo'n hart NIET: hij komt nooit uit reset, want daar draait deze
// switcher. Voor die core armt machine mode daarom zijn eigen comparator vóór
// elke mret naar een bewoner (SchedTickTicks). Bij elke tick kijkt hij naar één
// woord — CtxRevoke — en gaat bij niet-nul rechtstreeks naar teardown.
//
// GEVOLG voor dit bestand: er staat nu WÉL een mie-bit aan terwijl een app
// draait, en een machine-timer-interrupt in supervisor-modus wordt dan altijd
// genomen (ongeacht mstatus.MIE). De trap-entry moet de interruptbit van mcause
// dus als EERSTE uitsorteren — anders leest een tick als een fault en meldt hij
// een kerngezonde bewoner dood. Op een hart zonder tick (SchedTickTicks = 0)
// blijft alles exact zoals het was: geen mie, geen tick, reset is het mes.
//
// Maar de yield draagt sinds 31-07 wél een WEKTIJD (a0): "hervat me niet vóór
// deze tick". Zonder dat getal is "ik heb even niets te doen" niet te
// onderscheiden van "geef me meteen mijn beurt terug", en dan pingpongen twee
// wachtende apps op volle snelheid tegen elkaar aan — gemeten: allebei 36% van
// het hart, en geen van beide deed iets. Met de wektijd slaat de rotatie ze
// over en mag het hart in de tussentijd écht slapen (zie park). Ook die timer is
// geen preemptie-mechanisme: hij WEKT alleen, en alleen als er niemand te
// draaien is.
//
// De kooi-stub geeft ons twee dingen mee vóór hij de app binnenlaat:
//	mtvec    = mentry (dit bestand, adres via EntryPC)
//	mscratch = het sched-blok van dit hart (HOP-eigen geheugen, buiten élke
//	           partitie — dus onbereikbaar voor de app)
//
// Drie dingen die ANDERS zijn dan op ARM, en het zijn alle drie eigenschappen
// van deze architectuur en geen ontwerpkeuze:
//
//  1. **Geen vrij register bij de trap-entry.** ARM heeft SP_EL2 al staan; hier
//     is `csrrw sp, mscratch, sp` het enige dat aan een bruikbare pointer komt.
//     Daarna staat de sp van de app in mscratch — dáár lezen we hem, we ruilen
//     niet nog een keer terug.
//  2. **mepc + 4.** Bij een ecall wijst mepc naar de instructie zélf. Zonder de
//     +4 yieldt een bewoner bij elke hervatting opnieuw. ELR_EL2 staat na een
//     HVC al voorbij, dus ARM heeft dit probleem niet.
//  3. **Cache-onderhoud tussen HOP en dit hart.** Wij draaien zonder MMU, dus
//     cachebaar, en zijn niet coherent met HOP's hart. Elk woord dat HOP schrijft
//     moet vóór het lezen geïnvalideerd worden en elk woord dat HOP leest ná het
//     schrijven weggeveegd (th.dcache.cipa + th.sync.is — hetzelfde mechanisme als
//     dev.Push/Pull). Op ARM is dit blok device-gemapt en is dat gratis.
//
//     Wat hier NIET gebeurt: een veeg bij een gewone bewonerswissel. De C906-cache
//     is VIPT — virtueel geïndexeerd, fysiek getagd — dus twee bewoners op hetzelfde
//     virtuele adres krijgen elkaars regels niet: de tag beslist. Wat de vertalingen
//     scheidt is de sfence bij de satp-wissel, plus het feit dat geen slot-PTE de
//     G-vlag draagt (kern/cage). Er heeft hier wél zo'n veeg gestaan, als gok bij
//     een crash die uiteindelijk een stack-overschrijving in de yield-stub bleek;
//     hij is eruit omdat hij niets deed behalve een cache weggooien die de bewoner
//     juist nodig had. De veeg op het boot- en teardown-pad blijft: die lost een
//     ánder probleem op (zie daar).
//
// REGISTERS: dit bestand gebruikt uitsluitend X-nummers (plus SP=X2, TP=X4,
// g=X27, de drie namen die de Go-assembler eist). NIET T0/T1/T2 — die zijn
// aliassen van X5/X6/X7, en één regel die beide vormen mengt klobbert stil zijn
// eigen pointer. De ctx-offsets zijn zo ook met het oog te controleren:
// register xN staat op CtxGPRs + 8*(N-1) = 16 + 8N.
//
// Alle offsets zijn literals uit abi/layout (Sched*/Ctx*/Ctrl*); layout.go
// benoemt die koppeling. CSR-toegang via WORD-encodings met rs1 = x5: de
// Go-assembler kent deze namen niet, en de encodings staan uitgerekend in het
// commentaar.

//go:build tamago && riscv64

#include "textflag.h"

// ARMTICK zet de comparator van DIT hart op de volgende kill-tick en doet MTIE
// aan — vlak vóór elke overgang naar een bewoner (resume, cold boot, en de
// terugkeer van een tick zelf). Clobbert x5/x6/x7, dus hij staat overal waar die
// drie meteen daarna tóch overschreven worden.
//
// Zonder comparator (SchedClintPA = 0) of zonder periode (SchedTickTicks = 0)
// doet hij niets en blijft mie leeg: dat is de stand op elk hart dat HOP gewoon
// in reset kan zetten, en daarmee blijft dit bestand daar bit-voor-bit het
// bewezen gedrag van vóór de tick houden.
//
// De schrijfvolgorde van mtimecmp is die van de privileged spec (lo=alles-één,
// hi, lo): deze CLINT weigert 64-bit MMIO, en zonder die volgorde staat er
// tijdens het schrijven even een waarde in het verleden die meteen vuurt.
#define ARMTICK(done)			\
	MOV	232(SP), X6;		\
	BEQZ	X6, done;		\
	MOV	64(SP), X7;		\
	BEQZ	X7, done;		\
	WORD	$0xc01022f3;		\
	ADD	X5, X7, X7;		\
	MOV	$-1, X5;		\
	MOVW	X5, 0(X6);		\
	SRL	$32, X7, X5;		\
	MOVW	X5, 4(X6);		\
	MOVW	X7, 0(X6);		\
	MOV	$0x80, X5;		\
	WORD	$0x3042a073;		\
	done:

// Werkregisters: x5/x6/x7 (spill op SchedScratch+0/8/16 = 16/24/32).
// x6 is overal de pointer naar het ctx-blok van het slot in behandeling.
TEXT mentry(SB),NOSPLIT|NOFRAME,$0
	// sp omruilen met het sched-blok: het enige dat we mogen aanraken vóór we
	// ergens kunnen opslaan. Ná deze instructie wijst sp naar HOP's blok en
	// staat de sp van de app in mscratch.
	WORD	$0x34011173		// csrrw sp, mscratch, sp
	MOV	X5, 16(SP)
	MOV	X6, 24(SP)
	MOV	X7, 32(SP)

	// Waarom trappen we?
	WORD	$0x342022f3		// csrr x5, mcause

	// EERST de interruptbit (63), vóór élke andere vergelijking. Sinds de
	// kill-tick staat MTIE aan terwijl een bewoner draait, en een
	// machine-timer-interrupt in supervisor-modus wordt altijd genomen —
	// ongeacht mstatus.MIE. Zonder deze twee regels valt zo'n tick in de
	// fault-tak en meldt hij een kerngezonde bewoner dood.
	SRL	$63, X5, X6
	BNEZ	X6, tick

	// mcause 9 = ecall uit supervisor-modus: het coöperatieve pad van een
	// bewoner. Al het andere is een fault-rapport: een kooi-overtreding, een
	// illegale instructie, een verboden CSR — óók een ecall uit machine mode,
	// want een bewoner draait per definitie in supervisor-modus. (Tot 17-08
	// was M-mode-ecall de melding van de teleport-hop; die is geKAMd — een
	// core komt hier nu binnen via de loterij-adoptie, als verse boot.)
	MOV	$9, X6
	BEQ	X5, X6, yield
	JMP	fault

yield:
	// Yield of exit? ARM heeft daar twee HVC-immediates voor (hvc #1 = yield,
	// hvc #0 = klaar, zie applib/park_arm64.s), maar mcause draagt geen
	// immediate — dus staat het nummer in a7: 0 = yield, ≠0 = exit.
	//
	// Waarom exit een eigen pad MOET zijn: zonder dit is een app die klaar is
	// "saved" en dus hervatbaar, en blijft de rotatie hem beurten geven. HOP
	// wacht dan op CtxDead dat nooit komt, en de enige uitweg was het hart
	// resetten — wat op een gedeeld hart de BUREN meeneemt. Precies daarop liep
	// fase 2 van de tweede app stuk (gemeten 31-07: "place staged slot 2: loader
	// still live on shared core 1").
	BNEZ	X17, exit

	// --- yield -------------------------------------------------------------
	// Het ctx-blok van de bewoner die yieldt. SchedCurrent (48) ligt in ONZE
	// cacheline (zie layout.go), dus geen invalidatie nodig; SchedS2PA (224)
	// is HOP's regel maar wordt één keer bij de plan-init geschreven en daarna
	// nooit meer — de rotate-invalidatie hieronder dekt hem.
	MOV	48(SP), X5		// slot
	MOV	224(SP), X6		// Plan.Stage2PA
	SLL	$16, X5, X7		// slot × Stage2Stride
	ADD	X7, X6, X6
	MOV	$0x6000, X7		// layout.CtxOff
	ADD	X7, X6, X6		// x6 = ctx-blok van de yielder

	// GPRs (CtxGPRs = 24, één woord per registernummer). x1/x3/x4 en x8..x31
	// rechtstreeks; x2 uit mscratch; x5..x7 uit de spill hierboven.
	MOV	X1, 24(X6)
	WORD	$0x340022f3		// csrr x5, mscratch — de sp van de app
	MOV	X5, 32(X6)
	MOV	X3, 40(X6)
	MOV	TP, 48(X6)		// x4 heet TP bij de Go-assembler
	MOV	16(SP), X5
	MOV	X5, 56(X6)
	MOV	24(SP), X5
	MOV	X5, 64(X6)
	MOV	32(SP), X5
	MOV	X5, 72(X6)
	MOV	X8, 80(X6)
	MOV	X9, 88(X6)
	MOV	X10, 96(X6)
	MOV	X11, 104(X6)
	MOV	X12, 112(X6)
	MOV	X13, 120(X6)
	MOV	X14, 128(X6)
	MOV	X15, 136(X6)
	MOV	X16, 144(X6)
	MOV	X17, 152(X6)
	MOV	X18, 160(X6)
	MOV	X19, 168(X6)
	MOV	X20, 176(X6)
	MOV	X21, 184(X6)
	MOV	X22, 192(X6)
	MOV	X23, 200(X6)
	MOV	X24, 208(X6)
	MOV	X25, 216(X6)
	MOV	X26, 224(X6)
	MOV	g, 232(X6)		// x27 is de g-pointer
	MOV	X28, 240(X6)
	MOV	X29, 248(X6)
	MOV	X30, 256(X6)
	MOV	X31, 264(X6)

	// Hervat-PC en status (CtxResume = 288). mepc + 4: zie punt 2 in de kop.
	WORD	$0x341022f3		// csrr x5, mepc
	ADD	$4, X5, X5
	MOV	X5, 288(X6)
	WORD	$0x100022f3		// csrr x5, sstatus
	MOV	X5, 296(X6)

	// Het regime dat de volgende bewoner NIET mag erven (CtxRegime = 304): zijn
	// map en zijn kooi. Op ARM zijn dit negentien EL1-sysregs; hier zijn het
	// deze acht, en dát is de reden dat dit bestand korter is.
	WORD	$0x180022f3		// csrr x5, satp
	MOV	X5, 304(X6)
	WORD	$0x105022f3		// csrr x5, stvec
	MOV	X5, 312(X6)
	WORD	$0x140022f3		// csrr x5, sscratch
	MOV	X5, 320(X6)
	WORD	$0x3a0022f3		// csrr x5, pmpcfg0
	MOV	X5, 328(X6)
	WORD	$0x3b0022f3		// csrr x5, pmpaddr0
	MOV	X5, 336(X6)
	WORD	$0x3b1022f3		// csrr x5, pmpaddr1
	MOV	X5, 344(X6)
	WORD	$0x3b2022f3		// csrr x5, pmpaddr2
	MOV	X5, 352(X6)
	WORD	$0x3b3022f3		// csrr x5, pmpaddr3
	MOV	X5, 360(X6)
	// pmpaddr4..7 horen er net zo bij: de kooi-stub programmeert ACHT entries
	// (elk TOR-venster kost er twee), dus vier bewaren zou een bewoner met een
	// device-grant hervatten met de halve kooi van zijn voorganger. Vandaag komt
	// een kooi zonder grant net op vier uit — toevallig genoeg, en dat is precies
	// het soort toeval dat een isolatie-invariant niet mag dragen.
	WORD	$0x3b4022f3		// csrr x5, pmpaddr4
	MOV	X5, 368(X6)
	WORD	$0x3b5022f3		// csrr x5, pmpaddr5
	MOV	X5, 376(X6)
	WORD	$0x3b6022f3		// csrr x5, pmpaddr6
	MOV	X5, 384(X6)
	WORD	$0x3b7022f3		// csrr x5, pmpaddr7
	MOV	X5, 392(X6)

	// De WEKTIJD die de bewoner meegaf in a0 (layout.CtxWake): vóór deze
	// timebase-tick hoeft hij niet hervat te worden. Zonder dit getal is
	// "niets te doen" niet te onderscheiden van "nu meteen weer", en dan
	// pingpongen twee lege apps op volle snelheid tegen elkaar aan — gemeten
	// 31-07: allebei 36% van het hart, en geen van beide deed iets.
	//
	// Nul is "nu", en dat is ook wat een bewoner krijgt die niets meegeeft — het
	// oude gedrag blijft dus de terugval, op elk pad waar dit getal onzin wordt.
	//
	// Eigen cacheline (blok-offset 464): alleen de switcher schrijft hier, HOP
	// raakt hem nooit aan. Dat is geen netheid maar de non-coherentie-regel uit
	// layout.go — twee schrijvers in één regel op dit silicium is dataverlies.
	MOV	X10, 464(X6)		// a0

	// Staat → saved (CtxSaved = 2), als LAATSTE write in deze cacheline en met
	// een veeg erachter: HOP polt deze staat en moet hem in DRAM vinden. De
	// veeg dekt regel 0 van het blok (staat + x1..x5); de rest van het blok is
	// van ons alleen en mag gecachet blijven staan.
	MOV	$2, X5
	MOV	X5, 0(X6)
	FENCE
	WORD	$0x02b3000b		// th.dcache.cipa x6
	WORD	$0x01b0000b		// th.sync.is
	JMP	mrotate(SB)

exit:
	// De bewoner is klaar (applib.parkExit): status stond al op de control-page,
	// er is niets te rapporteren. Rechtstreeks naar de teardown, die het
	// ctx-blok zelf opzoekt.
	JMP	teardown

tick:
	// DE KILL-TICK (zie de kop). Eén vraag: wil HOP deze bewoner dood hebben?
	// Alles wat we hier aanraken is x5/x6/x7 en die staan al in de spill, dus
	// de bewoner merkt hier niets van behalve de tijd die het kost.
	MOV	48(SP), X5		// layout.SchedCurrent
	BEQZ	X5, tickret		// niemand draait: niets in te trekken
	MOV	224(SP), X6		// layout.SchedS2PA
	SLL	$16, X5, X7		// slot × Stage2Stride
	ADD	X7, X6, X6
	MOV	$0x6000, X7		// layout.CtxOff
	ADD	X7, X6, X6		// x6 = ctx-blok van de draaiende bewoner
	MOV	$512, X7		// layout.CtxRevoke
	ADD	X7, X6, X7

	// Vers lezen, élke tick. HOP schrijft dit woord vanaf zijn eigen hart en wij
	// zijn niet coherent met hem: een gecachete kopie betekent "de intrekking
	// komt nooit aan", en dat is precies het geval waarvoor deze tick bestaat.
	// Eén schrijver en een eigen cacheline — layout.go legt uit waarom dat hier
	// geen netheid is maar een voorwaarde.
	WORD	$0x02b3800b		// th.dcache.cipa x7
	WORD	$0x01b0000b		// th.sync.is
	MOV	0(X7), X7
	BNEZ	X7, teardown		// ingetrokken: dood, zonder hervatting

tickret:
	// Niets aan de hand: opnieuw armen en terug de bewoner in alsof er niets
	// gebeurd is. GEEN rotatie — dit is nadrukkelijk geen preemptie — en GEEN
	// +4 op mepc: bij een interrupt wijst mepc al naar de instructie die nog
	// moet gebeuren, en optellen zou er stilletjes één overslaan.
	ARMTICK(tickarmed)
	MOV	16(SP), X5
	MOV	24(SP), X6
	MOV	32(SP), X7
	WORD	$0x34011173		// csrrw sp, mscratch, sp — sp terug naar de bewoner
	WORD	$0x30200073		// mret

fault:
	// Fault-rapport op de control-page van de bewoner (mcause/mtval/vec) —
	// dezelfde velden die de ARM-kant vult, met mcause in de rol van ESR en
	// mtval in die van FAR. De GPRs bewaren we niet: er komt geen hervatting.
	MOV	48(SP), X5		// slot
	MOV	224(SP), X6
	SLL	$16, X5, X7
	ADD	X7, X6, X6
	MOV	$0x6000, X7
	ADD	X7, X6, X6		// ctx-blok
	MOV	8(X6), X7		// layout.CtxCtrlPA — zijn control-page

	WORD	$0x342022f3		// csrr x5, mcause
	MOV	X5, 0x58(X7)		// layout.CtrlFaultESR
	WORD	$0x343022f3		// csrr x5, mtval
	MOV	X5, 0x60(X7)		// layout.CtrlFaultFAR
	MOV	$1, X5
	MOV	X5, 0x68(X7)		// layout.CtrlFaultVec = 1 (er is één vector)

	// Het spoor voor het post-mortem: waar was hij (mepc), waar kwam hij vandaan
	// (ra), waar stond zijn stack (sp, nog in mscratch van de entry-ruil) — en met
	// wélke vertaling hij keek (satp). HOP weet welke map-wortel bij dít slot
	// hoort, dus dat laatste maakt "keek hij door de tabel van zijn buurman?" een
	// feit in plaats van een vermoeden. Alles op plekken die na een fault toch
	// niemand meer nodig heeft.
	WORD	$0x341022f3		// csrr x5, mepc
	MOV	X5, 288(X6)		// layout.CtxResume
	MOV	X1, 24(X6)		// x1 = ra
	WORD	$0x340022f3		// csrr x5, mscratch
	MOV	X5, 32(X6)		// x2 = sp
	WORD	$0x180022f3		// csrr x5, satp
	MOV	X5, 304(X6)		// layout.CtxRegime
	FENCE
	MOV	$0x40, X5		// rapport-regel van de control-page (0x40..0x7f)
	ADD	X5, X7, X7
	WORD	$0x02b3800b		// th.dcache.cipa x7
	MOV	$288, X5
	ADD	X5, X6, X5
	WORD	$0x02b2800b		// th.dcache.cipa x5 — regel met mepc/satp
	WORD	$0x01b0000b		// th.sync.is

	// --- teardown ----------------------------------------------------------
	// Eén staart voor exit én fault: de bewoner is dood, en dan moet er precies
	// hetzelfde gebeuren. Dat het gescheiden was, was een gat — het fault-pad
	// miste de volledige cache-veeg die het exit-pad wél deed, dus liet een
	// gecrashte app zijn dirty regels achter om later over een HERGEBRUIKTE
	// partitie heen te schrijven (HOP plaatst de volgende app er bovenop zodra
	// hij CtxDead ziet).
	//
	// Op het dedicated pad ruimt de hart-reset de cache op; hier leeft het hart
	// door voor de buren, dus doen wij het: alles naar DRAM en niets meer in de
	// cache. Daarna pas de dood melden — anders kan HOP de partitie hergebruiken
	// terwijl er nog iets van de vorige in de cache staat.
teardown:
	WORD	$0x0030000b		// th.dcache.ciall
	WORD	$0x01b0000b		// th.sync.is
	MOV	48(SP), X5		// slot (opnieuw: de fault-tak klobberde x5)
	MOV	224(SP), X6
	SLL	$16, X5, X7
	ADD	X7, X6, X6
	MOV	$0x6000, X7
	ADD	X7, X6, X6		// ctx-blok
	MOV	$4, X5			// layout.CtxDead
	MOV	X5, 0(X6)
	FENCE
	WORD	$0x02b3000b		// th.dcache.cipa x6
	WORD	$0x01b0000b		// th.sync.is
	JMP	mrotate(SB)

// mrotate: de rotatie + parkeerlus, als EIGEN symbool en niet als label in
// mentry — Go-asm-labels zijn functie-lokaal, en er zijn twee binnenkomers
// van buiten: elk pad van mentry hierboven (yield/teardown), én parkenter
// hieronder (de boot-intrek van een parkerende core). Invariant bij
// binnenkomst: SP = het sched-blok van dit hart, mtvec = mentry; alle GPRs
// vrij.
TEXT mrotate(SB),NOSPLIT|NOFRAME,$0
rotate:
	MOV	ZERO, X12		// niet door een IPI gewekt: wektijden gelden
rescan:
	MOV	ZERO, X11		// vroegste wektijd van een nog-niet-due bewoner
	// Round-robin over de bewonerslijst van dit hart, precies als op ARM: vanaf
	// rotor+1 de eerste bewoner met staat boot-pending (1) of saved (2) die
	// ook aan de beurt IS (zijn wektijd is verstreken).
	// Álle GPRs zijn hier vrij — de vorige bewoner is gesaved of dood — behalve
	// x11/x12, die deze twee regels net gezet hebben en de scan door moeten.
	//
	// Eerst de regels van HOP invalideren (64/128/192): lijst en lengte muteren
	// terwijl wij draaien (residentAdd/Remove), dus een gecachete kopie is een
	// verouderde bewonerslijst. Wij schrijven in deze regels NOOIT — daarom is
	// invalideren genoeg en kan er niets van HOP verloren gaan.
	MOV	$64, X5
	ADD	SP, X5, X5
	WORD	$0x02b2800b		// th.dcache.cipa x5
	ADD	$64, X5
	WORD	$0x02b2800b		// th.dcache.cipa x5
	ADD	$64, X5
	WORD	$0x02b2800b		// th.dcache.cipa x5
	WORD	$0x01b0000b		// th.sync.is

	MOV	56(SP), X5		// layout.SchedRotor
	MOV	88(SP), X6		// layout.SchedCount
	BEQZ	X6, park
	MOV	X6, X7			// hoogstens count stappen
next:
	ADD	$1, X5, X5
	BLT	X5, X6, scan
	MOV	ZERO, X5		// wrap
scan:
	MOV	$96, X28		// layout.SchedList
	ADD	SP, X28, X28
	ADD	X5, X28, X28
	MOVBU	0(X28), X28		// kandidaat-slot (0 = gat)
	BEQZ	X28, skip
	MOV	224(SP), X29
	SLL	$16, X28, X30
	ADD	X30, X29, X29
	MOV	$0x6000, X30
	ADD	X30, X29, X29		// x29 = ctx-blok van de kandidaat
	// De staat-regel VERS lezen, elke keer. HOP schrijft BootPending vanaf zijn
	// eigen hart, en een rotatie die hier al eens langskwam heeft deze regel
	// gecachet — zonder invalidatie ziet hij eeuwig de vorige staat en boot een
	// nieuwe bewoner nooit (de "never yielded"-herhaalstorm bij elke herstart).
	// De eerste rotatie na een verse boot mist dit per toeval goed (cache-miss);
	// dat het één keer werkt is precies waarom dit soort fout blijft liggen.
	WORD	$0x02be800b		// th.dcache.cipa x29
	WORD	$0x01b0000b		// th.sync.is
	MOV	0(X29), X30		// layout.CtxState
	MOV	$1, X31
	BEQ	X30, X31, boot		// boot-pending: een verse start wacht nooit
	MOV	$2, X31
	BNE	X30, X31, skip		// leeg of dood

	// Saved — maar is hij al aan de beurt? Zie CtxWake in het yield-pad.
	BNEZ	X12, resume		// een IPI zegt "er is iets veranderd": nu kijken
	MOV	464(X29), X30		// layout.CtxWake
	BEQZ	X30, resume		// 0 = nu
	WORD	$0xc0102ff3		// csrr x31, time
	BGEU	X31, X30, resume	// zijn tijd is gekomen
	// De wektijd wordt VERTROUWD — een bewoner die onzin vraagt benadeelt
	// alleen zichzelf. Dat vertrouwen stelt wél een eis aan de applib: een
	// yield van vóór de wektijd (a0 = residu) las als "wek me over 40 minuten"
	// en dat wás een dove welcome (gemeten 01-08). Oude bewoners horen dus
	// niet op een hart met deze switcher; alle vloot-artifacts zijn herbouwd.
	//
	// Niet due — maar de DOORBELL dan? (Zelfde peek als op ARM, zie el2 en
	// idle/rxdoor.go; alleen het cache-onderhoud is hier extra.) De drempel
	// (CtrlRXDoor) schrijft de bewoner op dít hart — cipa is dan clean+
	// invalidate, dus ook een nog-vuile eigen regel leest correct; het
	// head-woord schrijft HOP vanaf zíjn hart en het RingHeadPA-woord ligt in
	// HOP's ctx-regel (512): allebei vers lezen of de bel is doof.
	MOV	8(X29), X13		// layout.CtxCtrlPA
	MOV	$0x110, X14		// layout.CtrlRXDoor
	ADD	X14, X13, X13
	MOV	$512, X14		// de HOP-regel van het ctx-blok (CtxRingHeadPA)
	ADD	X29, X14, X14
	WORD	$0x02b6800b		// th.dcache.cipa x13
	WORD	$0x02b7000b		// th.dcache.cipa x14
	WORD	$0x01b0000b		// th.sync.is
	MOV	0(X13), X13		// de drempel (bit 63 = gewapend)
	BGEZ	X13, nobell		// niet gewapend: de wektijd regeert
	MOV	520(X29), X14		// layout.CtxRingHeadPA (0 = geen ring)
	BEQZ	X14, nobell
	WORD	$0x02b7000b		// th.dcache.cipa x14
	WORD	$0x01b0000b		// th.sync.is
	MOV	0(X14), X14		// live head (producer: hopswitch)
	SLL	$1, X13, X13		// het gewapend-teken eruit
	SRL	$1, X13, X13
	BNE	X13, X14, resume	// de ring groeide voorbij de drempel: due
nobell:
	// Nog niet aan de beurt. Onthouden wie het eerst weer moet — dát is de tijd
	// waarop het hart hoogstens mag doorslapen (zie park).
	BEQZ	X11, keepwake
	BGEU	X30, X11, skip		// een ander wil er eerder uit
keepwake:
	MOV	X30, X11
skip:
	ADD	$-1, X7, X7
	BNEZ	X7, next
	JMP	park

	// x5 = lijst-index, x28 = slot, x29 = ctx-blok van de gekozen bewoner.
boot:
	// Cold boot van een boot-pending bewoner: het hart de kooi-stub van ZIJN
	// partitie in. Die programmeert zijn eigen PMP, zet zijn map en mret't naar
	// de app — exact het pad van een verse start, maar nu vanaf de rotatie in
	// plaats van vanaf een hart-reset. De stub zet mtvec/mscratch opnieuw, dus
	// we komen hier gewoon weer terug.
	MOV	X5, 56(SP)		// rotor bijwerken (onze eigen cacheline)
	MOV	X28, 48(SP)		// SchedCurrent = deze bewoner
	MOV	$3, X30			// layout.CtxRunning
	MOV	X30, 0(X29)
	FENCE
	WORD	$0x02be800b		// th.dcache.cipa x29
	WORD	$0x01b0000b		// th.sync.is
	// SchedCurrent hierboven publiceren: op een core die HOP niet kan resetten
	// is dat woord zijn enige zicht op "draait daar iets?" (zie park). Clean en
	// geen cipa — invalideren zou onze eigen spill weggooien.
	FENCE
	WORD	$0x0291000b		// th.dcache.cpa x2
	WORD	$0x01b0000b		// th.sync.is
	MOV	16(X29), X30		// layout.CtxBootPC = partitiebasis (de stub)

	// De kill-tick armen vóór de sprong naar de stub. Dat die stub nog in
	// machine mode draait maakt niet uit: daar is mstatus.MIE nul, dus een tick
	// die tijdens de stub afgaat blijft pending en wordt pas genomen zodra de
	// stub met mret de app in supervisor-modus zet. Precies waar hij hoort.
	ARMTICK(bootarmed)

	// De hele D-cache terugschrijven vóór we de stub inspringen. Dit is geen
	// voorzorg maar een noodzaak: de eerste stap van die stub is `csrw mcor` met
	// de I$/D$-invalidatiebits, en INVALIDEREN schrijft niet terug. Bij een verse
	// hart-reset valt er niets weg; bij een rotatie-boot draait er al een bewoner
	// op dit hart en staat er dirty van hem in: zijn bewaarde context én zijn
	// eigen heap en stack. Zonder deze veeg vervalt dat naar wat DRAM had, en
	// hervat hij op onzin — GEMETEN 31-07 op het bordje: mcause 12
	// (instruction page fault) op 0xffffffedfff79ff2, een PC die nergens bestaat.
	//
	// Alleen op dit pad en op een échte bewonerswissel in resume — daar om de
	// omgekeerde reden (de nieuwe bewoner mag de gecachete data van de vorige
	// niet onder zijn eigen adressen vinden). Een bewoner die zichzelf hervat
	// veegt niets: die hoort zijn cache juist te houden.
	WORD	$0x0030000b		// th.dcache.ciall — schoon + leeg, niets dirty over
	WORD	$0x01b0000b		// th.sync.is
	JMP	(X30)

resume:
	MOV	X5, 56(SP)		// rotor bijwerken
	MOV	X28, 48(SP)		// SchedCurrent = deze bewoner
	MOV	$3, X5
	MOV	X5, 0(X29)		// layout.CtxRunning — HOP's boot-poll leest dit
	FENCE
	WORD	$0x02be800b		// th.dcache.cipa x29
	WORD	$0x01b0000b		// th.sync.is
	FENCE
	WORD	$0x0291000b		// th.dcache.cpa x2 — SchedCurrent publiceren
	WORD	$0x01b0000b		// th.sync.is


	// Eerst de kooi, dan de map: zolang de kooi van de vórige bewoner nog staat
	// mag deze niets aanraken.
	MOV	336(X29), X5
	WORD	$0x3b029073		// csrw pmpaddr0, x5
	MOV	344(X29), X5
	WORD	$0x3b129073		// csrw pmpaddr1, x5
	MOV	352(X29), X5
	WORD	$0x3b229073		// csrw pmpaddr2, x5
	MOV	360(X29), X5
	WORD	$0x3b329073		// csrw pmpaddr3, x5
	MOV	368(X29), X5
	WORD	$0x3b429073		// csrw pmpaddr4, x5
	MOV	376(X29), X5
	WORD	$0x3b529073		// csrw pmpaddr5, x5
	MOV	384(X29), X5
	WORD	$0x3b629073		// csrw pmpaddr6, x5
	MOV	392(X29), X5
	WORD	$0x3b729073		// csrw pmpaddr7, x5
	MOV	328(X29), X5
	WORD	$0x3a029073		// csrw pmpcfg0, x5

	MOV	304(X29), X5
	WORD	$0x18029073		// csrw satp, x5
	WORD	$0x12000073		// sfence.vma x0, x0 — álles weg. Anders dan ARM's
	// VMID-getagde entries moeten de vertalingen van de vorige bewoner hier écht
	// verdwijnen, en dat kan alleen omdat geen enkele slot-PTE de G-vlag draagt
	// (kern/cage relocate.go legt uit waarom die vlag hier een softwarefout is).
	// sync.is erachter: zo schrijft de C906-handleiding het voor — de sfence is
	// pas gegarandeerd doorgewerkt ná deze barrière, en wij springen er meteen
	// een andere adresruimte in.
	WORD	$0x01b0000b		// th.sync.is
	MOV	312(X29), X5
	WORD	$0x10529073		// csrw stvec, x5
	MOV	320(X29), X5
	WORD	$0x14029073		// csrw sscratch, x5

	// Hervat-PC en status. sstatus VÓÓR de MPP-bits: sstatus is een venster op
	// mstatus, dus andersom zou de write de doelmodus weer wegvegen.
	MOV	288(X29), X5
	WORD	$0x34129073		// csrw mepc, x5
	MOV	296(X29), X5
	WORD	$0x10029073		// csrw sstatus, x5
	MOV	$0x1800, X5
	WORD	$0x3002b073		// csrc mstatus, x5 — MPP = 00
	MOV	$0x800, X5
	WORD	$0x3002a073		// csrs mstatus, x5 — MPP = 01 (supervisor)

	// mscratch = het sched-blok: de invariant terwijl een app draait, en de
	// enige reden dat de volgende trap aan een pointer komt. Moet vóór het
	// herstellen van x2 (sp) gebeuren, want sp ís nu nog dat blok.
	WORD	$0x34011073		// csrw mscratch, sp

	// De kill-tick armen. Hier, want hierna is sp niet meer het sched-blok en
	// zijn x5/x6/x7 van de bewoner — en ze worden hieronder toch uit zijn
	// ctx-blok teruggeladen, dus clobberen is gratis.
	ARMTICK(resumearmed)

	// GPRs als allerlaatste: x2 vlak vóór het einde, x29 (de pointer zelf)
	// helemaal aan het eind.
	MOV	24(X29), X1
	MOV	40(X29), X3
	MOV	48(X29), TP
	MOV	56(X29), X5
	MOV	64(X29), X6
	MOV	72(X29), X7
	MOV	80(X29), X8
	MOV	88(X29), X9
	MOV	96(X29), X10
	MOV	104(X29), X11
	MOV	112(X29), X12
	MOV	120(X29), X13
	MOV	128(X29), X14
	MOV	136(X29), X15
	MOV	144(X29), X16
	MOV	152(X29), X17
	MOV	160(X29), X18
	MOV	168(X29), X19
	MOV	176(X29), X20
	MOV	184(X29), X21
	MOV	192(X29), X22
	MOV	200(X29), X23
	MOV	208(X29), X24
	MOV	216(X29), X25
	MOV	224(X29), X26
	MOV	232(X29), g
	MOV	240(X29), X28
	MOV	256(X29), X30
	MOV	264(X29), X31
	MOV	32(X29), X2		// sp van de bewoner
	MOV	248(X29), X29		// x29 als laatste
	WORD	$0x30200073		// mret

park:
	// Niemand draaibaar: iedereen dood, leeg, of nog niet aan de beurt. Op ARM
	// parkeert de core in de EL2-lus tot HOP zijn mailbox schrijft en een SEV
	// stuurt; hier is de wekker de CLINT.
	//
	// TWEE STANDEN, en de veilige is de standaard. Vult HOP SchedClintPA niet in
	// (board zonder wekker, of de CLINT-probe faalde — board/licheerv/clint.go),
	// dan draait hier de pauzelus die er altijd stond: een teller, dan opnieuw de
	// lijst aflopen. Dat kost stroom maar kan niet hangen, en dat is de goede
	// kant om naar te falen — een hart dat niet meer wakker wordt is een dode
	// node en geen foutmelding.
	// "Ik draai niemand" — en dat is meteen het antwoord waar HOP op polt
	// (kern/slots coreRunning). Op een hart dat in reset gaat is het reset-blok
	// die waarheid; op een core die nooit meer uit reset komt omdat HOP er zelf
	// vandaan gehopt is, is dit woord het enige eerlijke antwoord.
	//
	// Clean en geen cipa: regel 0 is van ons alleen, en invalideren zou onze
	// eigen spill weggooien.
	MOV	ZERO, X5
	MOV	X5, 48(SP)		// layout.SchedCurrent = niemand
	FENCE
	WORD	$0x0291000b		// th.dcache.cpa x2
	WORD	$0x01b0000b		// th.sync.is

	MOV	232(SP), X6		// layout.SchedClintPA
	BEQZ	X6, spin
	// SchedSleepCap = 0 is de derde stand: de comparator is bruikbaar (de
	// kill-tick draait erop) maar slapen mag niet. Dat onderscheid is gemeten en
	// geen voorzichtigheid — zie layout.go bij SchedSleepCap.
	MOV	240(SP), X7		// layout.SchedSleepCap (tikken)
	BEQZ	X7, spin

	// Slapen tot de vroegste wektijd, maar nooit langer dan de cap. Die cap is
	// geen zuinigheid maar het vangnet: gaat er ooit een wek-IPI verloren, dan
	// kost dat latency en geen liveness.
	WORD	$0xc0102ff3		// csrr x31, time
	ADD	X31, X7, X7
	BEQZ	X11, arm		// niemand met een eigen wektijd: de cap regeert
	BGEU	X11, X7, arm		// die wektijd ligt voorbij de cap
	MOV	X11, X7
arm:
	// mtimecmp is 64 bits, maar deze CLINT weigert 64-bit MMIO (zie
	// board/licheerv): twee woorden, in de volgorde die de privileged spec
	// voorschrijft — lo op alles-één, dan hi, dan lo. Zo staat er tijdens het
	// schrijven nooit een halve waarde in het verleden die meteen vuurt.
	MOV	$-1, X30
	MOVW	X30, 0(X6)
	SRL	$32, X7, X30
	MOVW	X30, 4(X6)
	MOVW	X7, 0(X6)

	// mie aan, en ALLEEN hier. Zolang een bewoner draait staan MTIE/MSIE uit, dus
	// kan een wekker nooit als trap binnenkomen — dat zou in de fault-tak landen
	// en een gezonde app doodmelden (een M-interrupt wordt in S-mode altijd
	// genomen als hij enabled is, ongeacht mstatus.MIE). wfi kijkt naar mip&mie
	// en niet naar mstatus.MIE, dus dit is precies genoeg om te wekken zonder
	// ooit een interrupt te NEMEN. En een signaal dat vlak vóór de wfi binnenkomt
	// staat dan al pending en laat hem meteen terugkeren: geen gemiste wake.
	MOV	$0x88, X30		// MTIE (bit 7) | MSIE (bit 3)
	WORD	$0x304f2073		// csrs mie, x30
	WORD	$0x10500073		// wfi
	WORD	$0x304f3073		// csrc mie, x30

	// Wat wekte ons? Een IPI betekent "HOP heeft iets veranderd voor iemand op
	// dit hart", en dan mag de volgende ronde de wektijden negeren: de reden om
	// te wachten kan net vervallen zijn. Te vroeg hervatten is altijd veilig —
	// de bewoner kijkt, vindt niets, en yieldt opnieuw met een verse wektijd.
	// Dit is waarom HOP geen wektijd hoeft te kunnen OVERSCHRIJVEN: dan zou dat
	// woord twee schrijvers hebben, en dat is op dit silicium dataverlies.
	WORD	$0x34402ff3		// csrr x31, mip
	MOV	$0x8, X30
	AND	X30, X31, X12

	// Wekbronnen opruimen, anders keert de volgende wfi meteen terug op een
	// signaal dat we al verwerkt hebben.
	MOV	$-1, X30
	MOVW	X30, 0(X6)
	MOVW	X30, 4(X6)
	MOV	248(SP), X30		// layout.SchedMsipPA
	BEQZ	X30, rescan
	MOVW	ZERO, 0(X30)
	JMP	rescan

spin:
	MOV	$0x4000, X5
pause:
	ADD	$-1, X5, X5
	BNEZ	X5, pause
	JMP	rotate			// lokaal label: een symbool-JMP naar onszelf
	// leest de nosplit-checker van de linker als oneindige recursie

// parkenter: de boot-intrek van een PARKERENDE core — geen bewoner maar de
// switcher zelf. De loterij-adoptie springt hierheen (kern/slots cageInit →
// HartOn → adoptParked) met X11 = het sched-blok van deze core (het
// adoptie-arg-woord, zie board/licheerv/lottery.go). Vanaf hier draait de
// rotatie op dit hart vanaf de boot: een boot-pending wordt gewoon opgepikt,
// er bestaat geen koud pad meer, en de tick/slaap-knoppen van de park gelden
// meteen. Dit verving de tweede parkeerwereld (loterij-park + coreSwitcherUp +
// een liegende HartState + residentReset-uitzonderingen) — de faalmodi van
// boot 7/8 (17-08) kunnen hierdoor niet meer bestaan.
//
// De sprong komt cache-uit binnen (reset-staat van de loterij-park): D-cache
// aan, geen interrupt-erfenis van de FSBL, en de trap-entry wijzen — daarna is
// dit hart een doodnormale switcher-core.
TEXT parkenter(SB),NOSPLIT|NOFRAME,$0
	MOV	$(1<<1), X5
	WORD	$0x7c12a073		// csrrs x0, mhcr, t0 — D-cache aan
	WORD	$0x30401073		// csrw mie, x0 — geen FSBL-erfenis
	// Deze core is nooit in reset geweest, dus élke M-mode-CSR die we niet
	// zelf zetten is FSBL-erfenis — een reset-vers hart heeft deze twee
	// gegarandeerd op nul, wij moeten dat afdwingen:
	//  - mscratch: mentry begint met csrrw sp,mscratch,sp; trapt er iets
	//    vóór de eerste bewoner (die zet hem pas via stub/resume), dan werd
	//    sp die erfenis en spilden drie registers in willekeurig DRAM.
	//  - mstatus.MIE: park zet MTIE|MSIE rond de wfi en rekent erop dat de
	//    interrupt nooit GENOMEN wordt; een geërfde MIE=1 zou hem nemen —
	//    zelfde trap, zelfde garbage-sp.
	MOV	X11, X2			// SP = het sched-blok (adoptie-arg)
	WORD	$0x34011073		// csrw mscratch, sp — sane spill-grond
	WORD	$0x30047073		// csrci mstatus, 8 — MIE uit
	MOV	$mentry(SB), X5
	WORD	$0x30529073		// csrw mtvec, x5
	JMP	mrotate(SB)

// ParkEnterPC geeft het fysieke adres van parkenter — zelfde argument als
// EntryPC: identity-geladen image, symbooladres = fysiek adres.
TEXT ·ParkEnterPC(SB),NOSPLIT,$0-8
	MOV	$parkenter(SB), X5
	MOV	X5, ret+0(FP)
	RET

// EntryPC geeft het fysieke adres van mentry. HOP's image is identity-geladen,
// dus het symbooladres ís het fysieke adres — hetzelfde argument als bij
// cpu/el2.EntryPC.
TEXT ·EntryPC(SB),NOSPLIT,$0-8
	MOV	$mentry(SB), X5
	MOV	X5, ret+0(FP)
	RET
