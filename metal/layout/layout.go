// Package layout legt het fysieke geheugenplan van HopOS op QEMU -M virt
// vast (fase 1). Eén bron van waarheid voor alle images: de HOP-kern
// (core 0) en de app-slots. QEMU: -m 2G → RAM 0x40000000..0xBFFFFFFF.
//
// De linkadressen (TEXT_START = slotbase + 0x10000) staan in
// image/qemu-virt-run.sh en moeten met deze constanten in sync blijven.
//
//	0x40000000  HOP-kern (core 0), 256MB
//	0x50000000  slot 1 (core 1), 128MB
//	0x58000000  slot 2 (core 2), 128MB
//	...         t/m slot 11 (core 11) → eindigt op 0xA8000000
//	0xB0000000  control-pages: per slot één pagina (status/kill/heartbeat)
package layout

const (
	// Core 0 — de HOP-kern. De bovenste 16MB van de partitie is DMA-regio
	// (virtio-ringen/buffers) en valt buiten de RAM-declaratie van de
	// runtime, zodat hij device-gemapt en dus niet gecached is.
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

	// App-slots 1..11: vaste stride; de werkelijke RAM-declaratie van een
	// app wordt bij het laden gepatcht naar job.MemoryLimit (≤ stride).
	SlotsBase  = 0x50000000
	SlotStride = 0x08000000 // 128MB partitie per slot
	MaxSlots   = 11

	// Control-pages: buiten alle RAM-declaraties → door alle MMU's als
	// device gemapt → coherent zonder cache-onderhoud. Uitsluitend
	// gealigneerde 64-bit loads/stores gebruiken (zie metal/dev).
	// Pagina 0 (= CtrlBase) is de boot-scratch: cpuinit (board/qemuvirt)
	// schrijft er vóór de EL-drop het boot-EL; de PSCI-conduitkeuze
	// (EL2-boot ⇒ SMC, EL1-boot ⇒ HVC) leest 'm. Slots gebruiken 1..MaxSlots.
	CtrlBase    = 0xB0000000
	CtrlStride  = 0x1000
	BootScratch = CtrlBase
	// DTBPtr: cpuinit legt hier (primary, MMU uit) de DTB-pointer neer die de
	// firmware in x0 meegaf; board.MemTotal parset 'm met metal/fdt. Zelfde
	// device-page als BootScratch (offset +8), dus coherent zonder cache-werk.
	DTBPtr = BootScratch + 8

	// hop-ABI ringen per slot: outbox (app → HOP: logs én RPC-requests) en
	// inbox (HOP → app: RPC-responses). Later desgewenst een sneller bulkpad
	// per slot — privé HOP↔app, nooit gedeeld tussen apps.
	RingBase    = 0xB1000000
	RingStride  = 0x10000 // 64KB per slot
	OutboxOff   = 0x0
	InboxOff    = 0x8000
	RingDataCap = 0x7000 // datacapaciteit per ring (28KB, 8-voud)

	// Stage-2-gebied (alleen bij een EL2-boot in gebruik): door HOP
	// geschreven, door de EL2-trampoline gelezen, voor app-cores onzichtbaar
	// (staat in geen enkele stage-2-map). +0x0: EL2-parkeervectoren (2KB-
	// aligned); per slot i een tabelblok op Stage2Base + i*Stage2Stride
	// (L1 +0x0, L2-laag +0x1000/+0x2000, L3-ctrl +0x3000, L3-ring +0x4000).
	Stage2Base   = 0xB2000000
	Stage2Stride = 0x10000

	// Frame-ringen per slot (per-slot netwerk): elke app draait een eigen
	// netstack over rauwe Ethernet-frames; HOP is enkel een L2-switch die
	// frames ring-naar-ring kopieert (metal/hopswitch). Per slot één
	// 2MB-blok — TX (app → switch) onderin, RX (switch → app) bovenin —
	// zodat de stage-2-kooi het als één blockRW mapt. Device-gemapt, buiten
	// alle RAM-declaraties → coherent. 11 slots = 0xB3000000..0xB4600000.
	NetRingBase    = 0xB3000000
	NetRingStride  = 0x200000 // 2MB per slot
	NetTXOff       = 0x0
	NetRXOff       = 0x100000
	NetRingDataCap = 0xFF000 // datacapaciteit per richting (1MB - 4KB slack)
)

// Stage2Table geeft de basis van het stage-2-tabelblok van slot i.
func Stage2Table(i int) uintptr {
	return uintptr(Stage2Base + uint64(i)*Stage2Stride)
}

// RingOutbox geeft het outbox-ringadres (app → HOP) van slot i.
func RingOutbox(i int) uintptr {
	return uintptr(RingBase + uint64(i-1)*RingStride + OutboxOff)
}

// RingInbox geeft het inbox-ringadres (HOP → app) van slot i.
func RingInbox(i int) uintptr {
	return uintptr(RingBase + uint64(i-1)*RingStride + InboxOff)
}

// NetRingTX geeft de frame-TX-ring (app → switch) van slot i; tevens de
// (2MB-gealigneerde) basis van het net-ring-blok voor de stage-2-map.
func NetRingTX(i int) uintptr {
	return uintptr(NetRingBase + uint64(i-1)*NetRingStride + NetTXOff)
}

// NetRingRX geeft de frame-RX-ring (switch → app) van slot i.
func NetRingRX(i int) uintptr {
	return uintptr(NetRingBase + uint64(i-1)*NetRingStride + NetRXOff)
}

// SlotBase geeft de partitiebasis van slot i (1-based, = core-index).
func SlotBase(i int) uint64 {
	return SlotsBase + uint64(i-1)*SlotStride
}

// CtrlPage geeft het control-page-adres van slot i.
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

	// Net-config van dit slot (HOP → app; gelezen door applib/appnet):
	// CtrlNetIP bits 0..31 = eigen IPv4 (big-endian als uint32), bits
	// 32..39 = prefixlengte; 0 = geen netwerk ingericht. CtrlNetGW bits
	// 0..31 = gateway-IPv4 (HOP op het interne net). De MAC is
	// deterministisch 02:00:00:00:00:<slot> — geen veld nodig.
	CtrlNetIP = 0x48
	CtrlNetGW = 0x50

	// Door de EL2-vectoren (stage2.InitVectors) geschreven vlak vóór de
	// CPU_OFF, zodat HOP kan loggen wáárom een slot viel. LET OP: deze
	// offsets staan als str-immediates in de vector-encodings — bij
	// verplaatsen ook stage2.InitVectors aanpassen.
	CtrlFaultESR = 0x58 // ESR_EL2: exception syndrome
	CtrlFaultFAR = 0x60 // FAR_EL2: faultadres
	CtrlFaultVec = 0x68 // vectorindex + 1 (0 = geen fault gezien)

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
)

// CtrlFaultVec-waarden (vectorindex + 1; de relevante paden benoemd).
const (
	FaultNone = 0  // geen fault gezien sinds de laatste start
	FaultSync = 9  // synchroon vanuit EL1 (idx 8): stage-2-fault, ESR/FAR geldig
	FaultIRQ  = 10 // IRQ vanuit EL1 (idx 9): hard-kill-SGI van HOP
)
