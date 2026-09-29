# trapstub — de eerste instructies van het MONITOR-blob (0x80000000, M-mode).
#
# Drie taken, in volgorde:
#  1. T-Head CPU-init: wij vervangen OpenSBI, dus wij erven zijn plicht.
#     Waarden = vendor-OpenSBI (thead/c9xx): caches/BTB aan, T-Head-
#     extensies open, prefetch aan. Zonder deze CSR's is de C906 na de
#     FSBL een kale kern (caches uit, th.*-instructies illegal).
#  2. Vroege trap-vector: élke trap vóór tamago's eigen mtvec (Hwinit1)
#     dumpt mcause/mepc/mtval als hex op UART0 en parkeert — het
#     meetinstrument voor de bring-up (de FSBL-vector zei alleen "E:ra=",
#     en dat is het ra-régister van de getrapte context, geen oorzaak).
#  3. 'S'-marker en de sprong naar het ELF-entry. Entry en BSS-range staan
#     op vaste offsets 8/16/24 vooraan (label `slots`) — mkmonitor patcht
#     ze na het assembleren (contract: zie tools/mkmonitor/main.go).
#
# Assembleren: riscv64-elf-as -march=rv64imac_zicsr (geen relocaties nodig;
# alles pc-relatief). Trap-uitvoer: "X <mcause> <mepc> <mtval>" en hangen.

	.option	norvc			# geen compressed: mtvec-target moet 4-aligned
	.section .text
	.globl	_start
_start:
	j	begin			# de slots staan op vaste offsets 8/16/24
	.align	3			# (mkmonitor-contract, zie daar waarom)
slots:
	.dword	0			# +8  ELF-entry
	.dword	0			# +16 bss-start
	.dword	0			# +24 bss-einde
begin:
	# --- 1. T-Head CPU-init (OpenSBI-erfenis) ---
	li	t1, 0x70013
	csrw	0x7c2, t1		# mcor: I$/D$/BTB invalideren + aanzetten
	li	t1, 0x11ff
	csrw	0x7c1, t1		# mhcr: cache/branch-gedrag
	li	t1, 0x638000
	csrw	0x7c0, t1		# mxstatus: THEADISAEE, MAEE, CLINT-bits
	li	t1, 0x16e30c
	csrw	0x7c5, t1		# mhint: prefetch

	# --- 2. vroege trap-vector (direct mode; traphex is 4-aligned) ---
	la	t0, traphex
	csrw	mtvec, t0

	# --- 3. mapbaddr voorlezen: het silicium zegt zélf waar zijn
	#     PLIC/CLINT-complex hangt (C906 CSR 0xfc1, read-only). De dts
	#     beweert PLIC=0x70000000/CLINT=0x74000000, maar onze eerste
	#     CLINT-read gaf een bus-fout (gemeten 30-07: mcause=5,
	#     mtval=0x7400bffc) — dit is de scheidsrechter.
	li	t2, 0x04140000		# UART0 (FSBL heeft 115200 al gezet)
	li	t3, 'B'
	jal	putc1
	csrr	s0, 0xfc1
	jal	puthex
	li	t3, ' '
	jal	putc1

	# mxstatus terúglezen: schreef stap 1 echt 0x638000 (met CLINTEE,
	# bit 17 — de aan/uit-knop van het CLINT-interface op de C906)?
	li	t3, 'M'
	jal	putc1
	csrr	s0, 0x7c0
	jal	puthex
	li	t3, ' '
	jal	putc1

	# --- 4. BSS nullen — de plicht van elke loader. De FSBL kopieert
	#     alleen Filesz; op écht DDR (zeker na een warme reset) staat er
	#     daarna rotzooi waar de Go-runtime vanaf instructie één nullen
	#     verwacht (in QEMU maskeert vers-nul DRAM dit). De range komt
	#     uit de slots (mkmonitor patcht ze; 8-aligned gegarandeerd).
	la	t0, slots
	ld	t4, 8(t0)		# bss-start
	ld	t5, 16(t0)		# bss-einde
1:	bgeu	t4, t5, 2f
	sd	zero, 0(t4)
	addi	t4, t4, 8
	j	1b
2:
	# --- 5. marker + sprong naar het ELF-entry ---
	li	t3, 'S'
	sw	t3, 0(t2)
	ld	t0, 0(t0)
	jr	t0

	# traphex: dump "X <mcause> <mepc> <mtval>" en parkeer. Geen stack,
	# geen calls behalve jal→puthex (ra is van ons; de getrapte context
	# komt nooit terug). t2 blijft de UART-basis.
	.align	2
traphex:
	li	t2, 0x04140000
	li	t3, 'X'
	jal	putc1
	csrr	s0, mcause
	jal	puthex
	li	t3, ' '
	jal	putc1
	csrr	s0, mepc
	jal	puthex
	li	t3, ' '
	jal	putc1
	csrr	s0, mtval
	jal	puthex
hang:
	j	hang

	# putc1: t3 → UART (poll LSR.THRE op +0x14, reg-shift=2)
putc1:
	lw	t4, 0x14(t2)
	andi	t4, t4, 0x20
	beqz	t4, putc1
	sw	t3, 0(t2)
	ret

	# puthex: s0 als 16 hex-nibbles (msb eerst); clobbert t3/t4/s1
puthex:
	li	s1, 16
1:	srli	t3, s0, 60
	andi	t3, t3, 0xf
	li	t4, 10
	blt	t3, t4, 2f
	addi	t3, t3, 39		# 'a'-'0'-10
2:	addi	t3, t3, 48		# '0'
3:	lw	t4, 0x14(t2)
	andi	t4, t4, 0x20
	beqz	t4, 3b
	sw	t3, 0(t2)
	slli	s0, s0, 4
	addi	s1, s1, -1
	bnez	s1, 1b
	ret

