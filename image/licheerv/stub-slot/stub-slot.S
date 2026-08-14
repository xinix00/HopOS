# stub-slot — de loader-stub van een APP-SLOT op het app-hart (C906L) van de
# LicheeRV Nano. Dit is op RISC-V wat de stage-2-map op ARM is: het laatste
# stukje code dat HOP's vertrouwen draagt, en dat de app in zijn kooi
# achterlaat.
#
# Draait op SlotBase in M-mode, direct na de hart-reset (board/licheerv/hop:
# HartOn), en doet in deze volgorde:
#
#  1. **T-Head-CPU-init.** Een verse C906L heeft caches uit en THEADISAEE
#     dicht (`th.*` = illegal). Op het HOP-hart erven we die init van de
#     vendor-OpenSBI (image/licheerv/trapstub); het app-hart moet hem van óns
#     krijgen.
#  2. **BSS nullen.** De loader kopieert alleen Filesz; de Go-runtime eist
#     nullen. Range komt uit de slot-tabel (door mkmonitor gepatcht).
#  3. **De kooi programmeren** uit de slot-tabel: pmpaddr0..7 en pmpcfg0
#     zoals kern/cage die heeft uitgerekend (HOP patcht ze vóór HartOn). Acht
#     entries omdat elk venster als TOR gecodeerd is — onder- en bovengrens, dus
#     twee entries. Deze stub neemt géén beslissingen: de rekenkunde staat in Go
#     en is daar getest, hier staat alleen het wegschrijven.
#  4. **CageVerify**: pmpcfg0 terúglezen en met de bedoelde waarde vergelijken.
#     Daarmee staat vast dat élke entry actief is en dat de deny-all erin staat.
#     Mismatch = **parken, nooit dispatchen** — het RISC-V-equivalent van "een
#     EL1-boot wordt geweigerd".
#  5. Sprong naar het ELF-entry van de app.
#
# Alles wat de app hierna aan privileged state kan slopen is per definitie
# alleen zijn eigen hart — en dat hart reset HOP bij een kill, wat de kooi
# tegelijk opruimt (gemeten 30-07).
#
# Slot-tabel, op VASTE offsets vooraan (mkmonitor-contract; vaste offsets en
# niet de staart omdat de assembler achter de laatste .dword nog vulling zet):
#	+8  ELF-entry        (mkmonitor, build-time)
#	+16 bss-start        (mkmonitor)
#	+24 bss-einde        (mkmonitor)
#	+32  pmpcfg0         (HOP, runtime — kern/cage.Encode)
#	+40  pmpaddr0..7     (HOP, runtime; ACHT, want kern/cage codeert elk venster
#	..   ..               als TOR = twee entries. Ongebruikte entries zijn 0 en
#	+96  ..               hun cfg-byte is 0 = A(OFF), dus inert.)
#	+104 scratch-adres   (HOP, runtime; 0 = de MBOX-default van de demo)
#	+112 map-wortel      (HOP, runtime; 0 = dit board verplaatst niet)
#	+120 mtvec           (HOP, runtime: cpu/mmode — 0 = onze eigen trap-handler)
#	+128 mscratch        (HOP, runtime: het sched-blok van dit hart)
#
# Scratch (het adres uit +104, of de MBOX-default in de demo):
#	+0  0xA1=stub leeft  0xA2=BSS genuld  0xA3=kooi geverifieerd, springen
#	    0xFA11=CageVerify FAALDE · 0x5107=hart heeft geen S-mode
#	    (beide: geparkeerd, de app is NOOIT gestart)
#	+8  pmpcfg0-readback
#	+16/+24/+32  mcause/mepc/mtval van een trap vóór de app zijn eigen mtvec
#	    zet (daarna is een fault het probleem van de app)
#	+40/+48/+56  misa/marchid/mimpid van DIT hart (stap 1b) — per-hart-CSR's
#	    die HOP zelf niet kan lezen
#	+64 mxstatus zoals teruggelezen (welke bits het silicium accepteerde)

	# Het vangnet-scratch (lege slot-tabel = de pre-agent-demo): in de VRIJE
	# top van de 2MB-staart, buiten élke plan-regio. Het oude 0x8FF10000 ligt
	# sinds de staart-hersnit (14-08) midden in slot-blok 15 — een vangnet dat
	# HOP's boekhouding beschrijft is erger dan geen vangnet.
	.equ	MBOX,	0x8FFF0000	# board/licheerv.StubMbox

	# mb: control-page-write die het ándere hart écht ziet. Caches staan na
	# stap 1 AAN, dus een fence is niet genoeg: de regel staat write-back in
	# onze eigen D$. th.dcache.cpa schrijft hem naar DRAM, sync.is wacht tot
	# dat klaar is. (GEMETEN 30-07: zonder dit las HOP overal nul terwijl de
	# stub gewoon liep — de write buffer draint niet als de core parkeert.)
	# Clobbert a0.
	.macro	mb reg, off
	sd	\reg, \off(s9)
	addi	a0, s9, \off
	.word	0x0295000b		# th.dcache.cpa a0
	.word	0x01b0000b		# th.sync.is
	.endm

	.option	norvc
	.globl	_start
_start:
	j	begin			# slot-tabel op vaste offsets, zie de kop
	.align	3
slots:
	.dword	0			# +8  entry
	.dword	0			# +16 bss-start
	.dword	0			# +24 bss-einde
	.dword	0			# +32 pmpcfg0
	.dword	0			# +40 pmpaddr0
	.dword	0			# +48 pmpaddr1
	.dword	0			# +56 pmpaddr2
	.dword	0			# +64 pmpaddr3
	.dword	0			# +72 pmpaddr4
	.dword	0			# +80 pmpaddr5
	.dword	0			# +88 pmpaddr6
	.dword	0			# +96 pmpaddr7
	.dword	0			# +104 scratch-adres (HOP, runtime; 0 = MBOX-default)
	.dword	0			# +112 map-wortel (HOP, runtime; 0 = niet verplaatsen)
	.dword	0			# +120 mtvec vóór de sprong (HOP; 0 = onze eigen trap)
	.dword	0			# +128 mscratch vóór de sprong (HOP: sched-blok)
begin:
	# Scratch-adres: HOP zet het per slot (layout.AbiStubOff, in de ABI-staart van
	# de partitie), want ná stap 3 mag dit hart niets buiten zijn kooi meer
	# aanraken — en de control page is van de app. Nul → de vaste MBOX: het
	# terugvalpad van de gesloopte pre-agent-demo (04-08), bewust intact gelaten
	# als vangnet voor een lege tabel.
	la	s10, slots
	li	s9, MBOX
	ld	t0, 96(s10)
	beqz	t0, 1f
	mv	s9, t0
1:

	# --- 1. T-Head-CPU-init (wat OpenSBI voor het HOP-hart doet) ---
	li	t1, 0x70013
	csrw	0x7c2, t1		# mcor:     I$/D$/BTB invalideren + aan
	li	t1, 0x11ff
	csrw	0x7c1, t1		# mhcr:     cache/branch-gedrag
	li	t1, 0x638000
	csrw	0x7c0, t1		# mxstatus: THEADISAEE, MAEE, CLINT-bits
	li	t1, 0x16e30c
	csrw	0x7c5, t1		# mhint:    prefetch

	la	t0, trap
	csrw	mtvec, t0
	li	t0, 0xA1
	mb	t0, 0
	# De trap-woorden nullen: dit is DRAM, en wat er van een vorige huurder of
	# een koude start in staat is deterministische ruis. cageWhy leest ze bij
	# élke doodsoorzaak — zonder deze drie regels rapporteert een gewone crash
	# "app trapte (mcause 0xa480d282...)" met onzinwoorden (gemeten 31-07).
	mb	zero, 16
	mb	zero, 24
	mb	zero, 32

	# --- 1b. wie is dit hart? ---
	# misa/marchid/mimpid zijn per-hart, dus HOP (op hart 0) kan ze niet voor
	# ons lezen — alleen code die HIER draait kan dat. En het antwoord bepaalt
	# of dit board ooit meerdere apps kan dragen: staat de S-bit in misa, dan
	# heeft de C906L supervisor-modus en kan een app uit M-mode (dán kan HOP dit
	# hart preempten en de kooi omschakelen — nu niet, want boven M-mode zit
	# niets en de PMP-locks gaan alleen los bij reset). De vendor-DT beschrijft
	# alléén hart 0, dus dit is de enige betrouwbare bron.
	#
	# Bewust alleen csrr, geen csrw: een LEES van deze drie is in M-mode altijd
	# legaal, dus dit kan nooit trappen en nooit een boot kosten. satp proberen
	# (schrijven+teruglezen) hoort in een aparte, bewuste stap.
	csrr	t0, misa
	mb	t0, 40
	csrr	t0, marchid
	mb	t0, 48
	csrr	t0, mimpid
	mb	t0, 56
	# En mxstatus zoals het silicium hem WERKELIJK accepteerde (we schreven net
	# 0x638000). Dat is de tweede vraag die multi-app blokkeert: als een app uit
	# M-mode gaat, moet hij zijn eigen cache-onderhoud (th.dcache.*, waar
	# dev.Push/Pull op staat) nog kúnnen doen — en dat hangt aan de
	# user-cache-management-bit hierin. Teruglezen is de enige eerlijke bron:
	# geschreven is niet hetzelfde als geaccepteerd.
	csrr	t0, 0x7c0
	mb	t0, 64

	# --- 2. BSS nullen ---
	ld	t4, 8(s10)		# bss-start
	ld	t5, 16(s10)		# bss-einde
1:	bgeu	t4, t5, 2f
	sd	zero, 0(t4)
	addi	t4, t4, 8
	j	1b
2:	li	t0, 0xA2
	mb	t0, 0

	# --- 3. de kooi programmeren (waarden uit de tabel) ---
	ld	t1, 32(s10)
	csrw	pmpaddr0, t1
	ld	t1, 40(s10)
	csrw	pmpaddr1, t1
	ld	t1, 48(s10)
	csrw	pmpaddr2, t1
	ld	t1, 56(s10)
	csrw	pmpaddr3, t1
	ld	t1, 64(s10)
	csrw	pmpaddr4, t1
	ld	t1, 72(s10)
	csrw	pmpaddr5, t1
	ld	t1, 80(s10)
	csrw	pmpaddr6, t1
	ld	t1, 88(s10)
	csrw	pmpaddr7, t1
	ld	t3, 24(s10)		# pmpcfg0 zoals bedoeld
	csrw	pmpcfg0, t3

	# --- 4. CageVerify: geen dispatch vóór de kooi aantoonbaar vast zit ---
	csrr	t2, pmpcfg0
	mb	t2, 8
	beq	t2, t3, 3f
	li	t0, 0xFA11
	mb	t0, 0			# mismatch: parken, de app start NIET
	j	park
3:	li	t0, 0xA3
	mb	t0, 0

	# --- 5. de app in, en ALLEEN in supervisor-modus ---
	#
	# S-mode is hier een EIS, geen voorkeur. De whitelist bindt supervisor- en
	# user-mode altijd, maar machine mode alleen als de entries gelockt zijn — en
	# dat zijn ze bewust niet meer (kern/cage legt uit waarom: HOP moet de kooi
	# kunnen wisselen). Een app in M-mode zou dus in een kooi zitten die hem niet
	# begrenst: geen isolatie, en niets dat dat zichtbaar maakt.
	#
	# Daarom faalt een hart zonder misa.S gesloten: parken, nooit dispatchen,
	# precies zoals een CageVerify-mismatch. Hier stond een M-mode-fallback "voor
	# compatibiliteit"; die kwam uit de tijd dat de app in M-mode hoorde en de
	# entries gelockt waren, en is sinds de ontlocking een gat.
	# GEMETEN op de C906L (31-07): misa = 0x800000000094112d → acdfimsux, dus S
	# en U zijn er.
	#
	# medeleg blijft nul, dus élke trap van de app komt in M-mode uit — bij HOP's
	# switcher (of, tot die gezet is, bij `trap:` hieronder) en niet in de app zijn
	# eigen stvec-handler. Dat is opzet: HOP wil de mcause/mepc/mtval zien.
	csrr	t1, misa
	srli	t1, t1, 18
	andi	t1, t1, 1
	bnez	t1, 5f
	li	t0, 0x5107		# "geen S-mode" — geen isolatie mogelijk
	mb	t0, 0
	j	park
5:
	ld	t0, 0(s10)		# ELF-entry

	# mcounteren.TM: rdtime is het ENIGE tijdpad van een slot (de CLINT zit niet
	# in zijn kooi), en zonder deze bit is die CSR-lees in S-mode illegaal. De
	# bit bestaat gegarandeerd: misa.U is gezet, en dan eist de spec mcounteren.
	li	t1, 2
	csrw	mcounteren, t1

	# --- 5b. de MAP-helft van de kooi aanzetten ---
	# De whitelist begrenst, deze tabel verplaatst: hij legt de partitie van dit
	# slot op het canonieke linkadres, zodat één artifact in élk slot draait
	# (kern/cage Relocate rekent hem uit; wij schrijven alleen het register).
	# Nul = dit board verplaatst niet — dan blijft het linkadres de partitie zelf.
	#
	# Onze eigen PC raakt dit niet: satp vertaalt alléén supervisor- en
	# user-mode, en wij zijn machine mode — ook de instructies en loads hierna
	# blijven dus gewoon fysiek (zoals elke OpenSBI onder een draaiende kernel).
	# Het geldt pas voor de app, vanaf de mret; het entry in mepc is dan ook een
	# linkadres. De sfence gooit weg wat er van een vorige huurder nog in de TLB
	# kon staan — een verse hart-reset heeft die niet, maar gratis zekerheid.
	ld	t2, 104(s10)		# map-wortel
	beqz	t2, 7f
	csrw	satp, t2
	sfence.vma
7:

	# --- 5c. de trap-entry overdragen aan HOP ---
	# Tot hier is ONZE trap-handler de vangnet (hij legt mcause/mepc/mtval in de
	# scratch en parkeert). Vanaf hier hoort een trap bij HOP: die kan méér dan
	# parkeren — hij kan naar een mede-bewoner van dit hart wisselen. Dat kán
	# alleen als de handler búiten élke partitie woont, want binnen een partitie
	# zou app A hem kunnen herschrijven en zo app B overnemen (cpu/mmode).
	#
	# mscratch krijgt het sched-blok van dit hart: HOP-eigen geheugen dat in geen
	# enkele kooi zit. De trap-entry daar heeft geen vrij register en ruilt sp
	# ermee — zonder deze waarde is er geen pointer om iets in op te slaan.
	#
	# ORDE: eerst mscratch, dan mtvec. Andersom zou een trap tussen de twee in
	# HOP's handler bereiken vóórdat die zijn pointer heeft. En 0 = dit board
	# heeft (nog) geen switcher: dan blijft onze eigen handler staan en gedraagt
	# het slot zich als vóór multi-app.
	ld	t1, 120(s10)		# mscratch (sched-blok)
	beqz	t1, 8f
	csrw	mscratch, t1
	ld	t1, 112(s10)		# mtvec (HOP's switcher)
	beqz	t1, 8f
	csrw	mtvec, t1
8:

	# mstatus.MPP (bits 12:11) = 01 = supervisor, dan mret naar het entry.
	li	t2, 0x1800
	csrrc	zero, mstatus, t2	# MPP = 00
	li	t2, 0x800
	csrrs	zero, mstatus, t2	# MPP = 01
	csrw	mepc, t0
	mret

park:
	j	park

	.align	2
trap:
	csrr	t0, mcause
	mb	t0, 16
	csrr	t0, mepc
	mb	t0, 24
	csrr	t0, mtval
	mb	t0, 32
	# En het voortgangswoord op een eigen waarde. Zonder dit blijft daar 0xA3
	# staan ("de app is ingesprongen") en leest een trap NÁ de sprong als een
	# geslaagde start — precies wat 31-07 een boot-cyclus kostte: status Booting,
	# geen logregel, geen reden. Nu is "getrapt" een eigen toestand.
	li	t0, 0xDEAD
	mb	t0, 0
	j	park
