// hopos-lrv is de HOP-kern op de LicheeRV Nano — het eerste RISC-V-board van
// HopOS. Hij draait op het C906-hart (waar de vendor-FSBL ons image start) en
// beheert het C906L-hart als app-slot.
//
// Waarom een eigen main naast cmd/hopos: die main is gebouwd op de ARM-kooi
// (kern/slots + kern/stage2 + EL2-trampolines + PSCI). De kern-mechaniek is op
// dit silicium fundamenteel anders — PMP-whitelist i.p.v. stage-2, reset-blok
// i.p.v. PSCI — en die abstractie zit nog niet in kern/slots. Wat hier al wél
// gedeeld is: het board-contract (board.Board, riscv64-helft), de kooi-encoding
// (kern/cage, host-getest) en straks de agent (hop/pkg/agentboot) zodra de NIC
// in bedrijf is. Deze main is dus de bewezen slot-lifecycle, in de boom, met de
// echte HopOS-lagen — en de plek waar de agent aan komt te hangen.
//
// Wat hij nu doet: het app-slot vullen met een gekooide Go-app, de kooi laten
// verifiëren vóór dispatch, en de control page bewaken (inclusief een kill +
// herstart, want dat is de andere helft van het contract).
package main

import (
	_ "embed"
	"encoding/binary"
	"fmt"
	"time"
	"unsafe"
	_ "unsafe"

	"hop-os/metal/board"
	"hop-os/metal/board/licheerv"
	_ "hop-os/metal/board/licheerv/hop" // registreert het board (init)
	"hop-os/metal/cpu/memlimit"
	"hop-os/metal/kern/cage"
)

// slot.bin = de kooi-stub + het slotdemo-image, gelinkt op de app-partitie.
// Gebouwd door image/licheerv-agent.sh (gitignored, zoals cmd/hopos-embed).
//
//go:embed slot.bin
var slotBlob []byte

// De slot-tabel in het blob (contract: image/licheerv/stub-slot). De stub leest
// hier zijn kooi uit; HOP schrijft die er runtime in, want kern/cage rekent hem
// uit en de stub neemt geen beslissingen.
const (
	slotCfgOff  = 32 // pmpcfg0
	slotAddrOff = 40 // pmpaddr0..7
	slotAddrMax = 8  // ACHT: kern/cage codeert elk venster als TOR (onder- én
	// bovengrens), dus een plan van drie vensters + deny-all vult ze precies.
	// Stond op 4 — dan vielen de laatste vier entries STIL weg, inclusief het
	// deny-paar, en CageVerify zag dat niet: die leest alleen pmpcfg0 terug, en
	// dát woord was wél goed. De guard in place() is er nu voor.
)

// control page, stub-helft (image/licheerv/stub-slot) en app-helft (app/slotdemo)
const (
	fStub = 0 // 0xA1 leeft · 0xA2 BSS · 0xA3 kooi OK, app gestart
	// 0xFA11 CageVerify faalde · 0x5107 hart zonder S-mode · 0xDEAD app trapte
	fCfg    = 1 // pmpcfg0-readback
	fMcause = 2
	fMepc   = 3
	fMtval  = 4
	fBeat   = 6 // hartslag van de app
	fWork   = 7
)

// stubWhy benoemt het voortgangswoord van de kooi-stub. Zonder dit leest elke
// stall als hetzelfde probleem, terwijl "de kooi klopte niet" en "dit hart heeft
// geen supervisor-modus" verschillende antwoorden vragen.
func stubWhy(step uint64) string {
	switch step {
	case 0:
		return "geen teken van leven"
	case 0xA1:
		return "gestart, kwam niet door het nullen van BSS"
	case 0xA2:
		return "BSS genuld, kwam niet door CageVerify"
	case 0xFA11:
		return "CAGEVERIFY FAALDE — app nooit gestart"
	case 0x5107:
		return "hart heeft geen supervisor-modus — zonder die laag bindt de kooi hem niet"
	case 0xDEAD:
		return "app trapte en werd geparkeerd"
	}
	return "onbekend voortgangswoord"
}

// slotPlan is de kooi van het app-slot: zijn eigen partitie, de control page,
// en UART0 als granted MMIO. Al het andere — inclusief HOP's eigen RAM — valt
// onder de deny-all die cage.Encode erachter zet.
func slotPlan() cage.Plan {
	return cage.Plan{Allow: []cage.Window{
		{Base: licheerv.SlotBase, Size: licheerv.SlotSize, R: true, W: true, X: true},
		{Base: licheerv.CtrlPage, Size: 4 << 10, R: true, W: true},
		{Base: licheerv.UART0_BASE, Size: 4 << 10, R: true, W: true},
	}}
}

// ctrl leest een veld van de control page (cache-vers: de clusters zijn niet
// coherent, dus élke read invalideert eerst zijn regel).
func ctrl(field int) uint64 {
	addr := uintptr(licheerv.CtrlPage + uint64(field*8))
	licheerv.CacheCleanInval(addr, 8)
	return *(*uint64)(unsafe.Pointer(addr))
}

// place kopieert het slot-blob naar de app-partitie, schrijft de uitgerekende
// kooi in de slot-tabel, wist de control page en laat het hart lopen. Opnieuw
// aanroepen is kill + verse start: HartOn asserteert eerst reset, en dát is wat
// de PMP-locks van het vorige slot wist.
func place(b board.Board, hart int, addrs []uint64, cfg uint64) error {
	// Past de kooi in de tabel? Zo niet: weigeren, nooit stil afkappen. Een
	// weggevallen entry is een kooi die anders is dan HOP denkt — en de laatste is
	// de deny-all. Dezelfde guard als het agent-pad (kern/slots cagePrepare).
	if len(addrs) > slotAddrMax {
		return fmt.Errorf("kooi vraagt %d PMP-entries, de slot-tabel draagt %d", len(addrs), slotAddrMax)
	}
	blob := make([]byte, len(slotBlob))
	copy(blob, slotBlob)
	binary.LittleEndian.PutUint64(blob[slotCfgOff:], cfg)
	for i := range slotAddrMax {
		var v uint64
		if i < len(addrs) {
			v = addrs[i]
		}
		binary.LittleEndian.PutUint64(blob[slotAddrOff+i*8:], v)
	}

	// Het hart eerst stilzetten: we schrijven in het geheugen waar hij in kan
	// staan draaien.
	if err := b.HartOff(hart); err != nil {
		return err
	}
	dst := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(licheerv.SlotBase))), len(blob))
	copy(dst, blob)
	for f := range 8 {
		p := uintptr(licheerv.CtrlPage + uint64(f*8))
		*(*uint64)(unsafe.Pointer(p)) = 0
	}
	// Onze D$ naar DRAM: het app-hart heeft caches uit tot zijn stub ze
	// aanzet en leest dus écht geheugen.
	licheerv.CacheClean(uintptr(licheerv.SlotBase), uintptr(len(blob)))
	licheerv.CacheCleanInval(uintptr(licheerv.CtrlPage), 64)

	return b.HartOn(hart, licheerv.SlotBase)
}

func main() {
	memlimit.Arm() // geheugenplafond uit het RAM-raam — zie cpu/memlimit
	b := board.Current()
	fmt.Printf("\r\n\r\nHopOS on %s — hart %d is HOP (%s), app slot on hart %v\r\n",
		licheerv.Model(), b.CoreID(), b.CoreClass(0), b.AppHarts())
	if b.BootMode() != 3 {
		fmt.Printf("HOPOS_BOOT_REFUSED: mode %d, M-mode vereist voor de kooi\r\n", b.BootMode())
		park()
	}
	fmt.Printf("RAM %d MB — HOP %#x..%#x, app partition %#x + %d MB\r\n",
		b.MemTotal()>>20, uint64(licheerv.HopBase), uint64(licheerv.SlotBase),
		uint64(licheerv.SlotBase), licheerv.SlotSizeMB)

	addrs, cfg, err := cage.Encode(slotPlan())
	if err != nil {
		fmt.Printf("HOPOS_CAGE_PLAN_INVALID: %v\r\n", err)
		park()
	}
	fmt.Printf("cage: %d windows + deny-all, pmpcfg0=%#x\r\n", len(addrs)-1, cfg)

	hart := b.AppHarts()[0]
	if err := place(b, hart, addrs, cfg); err != nil {
		fmt.Printf("HOPOS_SLOT_START_FAILED: %v\r\n", err)
		park()
	}

	// Wachten tot de stub klaar is: hij verifieert de kooi en dispatcht de app
	// alleen als die aantoonbaar vast zit.
	var stub uint64
	for range 40 {
		if stub = ctrl(fStub); stub == 0xA3 || stub == 0xFA11 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	switch stub {
	case 0xA3:
		fmt.Printf("slot %d: cage verified (pmpcfg0=%#x), app dispatched\r\n", hart, ctrl(fCfg))
	case 0xFA11:
		fmt.Printf("slot %d: HOPOS_CAGE_VERIFY_FAILED (readback %#x) — app NOT started\r\n",
			hart, ctrl(fCfg))
		park()
	default:
		fmt.Printf("slot %d: stub stalled: %s (step=%#x mcause=%d mepc=%#x mtval=%#x)\r\n",
			hart, stubWhy(stub), stub, ctrl(fMcause), ctrl(fMepc), ctrl(fMtval))
		park()
	}

	// Vanaf hier praat HOP alleen nog via de control page — precies zoals de
	// agent straks doet (mem-telemetrie, liveness).
	var last uint64
	for i := range 12 {
		time.Sleep(2 * time.Second)
		beat, work := ctrl(fBeat), ctrl(fWork)
		state := "ALIVE"
		if beat == last {
			state = "STALLED"
		}
		last = beat
		fmt.Printf("t=%2ds  slot %d: beat=%-6d work=%-10d %s  hart=%s\r\n",
			(i+1)*2, hart, beat, work, state, b.HartState(hart))

		// Halverwege de andere helft van het contract: kill midden in de
		// rekenlus en opnieuw plaatsen. Hartslag terug naar nul = schoon slot.
		if i == 5 {
			fmt.Printf("slot %d: kill + restart (app is mid-computation)\r\n", hart)
			if err := place(b, hart, addrs, cfg); err != nil {
				fmt.Printf("HOPOS_SLOT_RESTART_FAILED: %v\r\n", err)
				park()
			}
			last = 0
		}
	}

	fmt.Println("\r\nHOPOS_SLOT_OK: cage verified before dispatch, app ran, kill restarted it clean.")
	park()
}

func park() {
	for {
		time.Sleep(time.Hour)
	}
}

// De RAM-declaratie van de HOP-kern: zelfde conventie als cmd/hopos'
// board_*.go (de main declareert, niet het board — het board levert alleen de
// geometrie). Onder HopBase ligt vuil gebied waar de FSBL U-Boot uitpakt.
//
//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = licheerv.HopBase

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = licheerv.SlotBase - licheerv.HopBase
