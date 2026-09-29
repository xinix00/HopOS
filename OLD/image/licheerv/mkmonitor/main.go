// mkmonitor — zet een tamago ELF om naar een flat MONITOR-blob voor het
// CV181x/SG200x fip.bin formaat.
//
// De vendor-FSBL laadt het MONITOR-slot integraal op MONITOR_RUNADDR
// (0x80000000 = DRAM-start) en springt daar in M-mode naartoe. Deze tool
// bouwt dat blob: een jump-stub op offset 0 (0x80000000) die naar het ELF
// entry point springt, met daarachter alle PT_LOAD segmenten op hun
// paddr-relatieve plek (tamago .text staat op 0x80010000).
//
// Met -base werkt hij ook voor een APP-SLOT-blob (slot/stub-slot.S op
// SlotBase i.p.v. de monitor op DRAM-start) — zelfde stub-contract.
//
//	usage: mkmonitor [-base 0x80000000] <app.elf> <out.bin> [stub.bin]
package main

import (
	"debug/elf"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
)

func main() {
	baseFlag := flag.Uint64("base", 0x80000000, "laadadres van het blob")
	flag.Parse()
	args := flag.Args()
	if len(args) != 2 && len(args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: mkmonitor [-base addr] <app.elf> <out.bin> [stub.bin]")
		os.Exit(1)
	}
	base := *baseFlag

	f, err := elf.Open(args[0])
	if err != nil {
		fatal(err)
	}
	defer f.Close()

	entry := uint32(f.Entry)

	// bepaal blob-grootte uit de PT_LOAD segmenten
	var end uint64
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD || p.Filesz == 0 {
			continue
		}
		if p.Paddr < base {
			fatal(fmt.Errorf("segment paddr 0x%x < base 0x%x", p.Paddr, base))
		}
		if e := p.Paddr + p.Filesz - base; e > end {
			end = e
		}
	}

	blob := make([]byte, end)

	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD || p.Filesz == 0 {
			continue
		}
		off := p.Paddr - base
		if off < 4096 { // stub-budget (gegenereerd óf tools/trapstub)
			fatal(fmt.Errorf("segment op 0x%x overlapt de jump-stub", p.Paddr))
		}
		if _, err := p.ReadAt(blob[off:off+p.Filesz], 0); err != nil {
			fatal(err)
		}
	}

	// De stub vervangt óók OpenSBI's T-Head CPU-init (gemeten 30-07: zonder
	// deze CSR's boot de C906 de Go-runtime half en trapt terug in de
	// FSBL-vector, "E:RESET:panic"). Na de FSBL staan I$/D$ uit en de
	// T-Head-extensies dicht; vendor-OpenSBI (thead/c9xx) zet daarom vóór
	// elke payload: mcor=0x70013 (caches+BTB invalideren en aanzetten),
	// mhcr=0x11ff (cache/branch-gedrag), mxstatus=0x638000 (o.a.
	// THEADISAEE bit22 — anders is élke th.*-instructie illegal — en
	// CLINT/MM-bits), mhint=0x16e30c (prefetch). Wij zijn de OpenSBI-
	// vervanger, dus dit is ónze plicht. csrrw x0,csr,x6 per stuk.
	csrInit := []struct{ csr, val uint32 }{
		{0x7c2, 0x70013},  // mcor
		{0x7c1, 0x11ff},   // mhcr
		{0x7c0, 0x638000}, // mxstatus
		{0x7c5, 0x16e30c}, // mhint
	}
	var stub []uint32
	for _, c := range csrInit {
		lo := c.val & 0xfff
		hi := c.val >> 12
		if lo >= 0x800 {
			hi++
		}
		stub = append(stub,
			hi<<12|6<<7|0x37,           // lui  t1, hi20
			lo<<20|6<<15|6<<7|0x13,     // addi t1, t1, lo12
			c.csr<<20|6<<15|1<<12|0x73, // csrrw x0, csr, t1
		)
	}
	// 'T'-marker op UART0: bewijst dat de stub draaide (bisectiepunt vóór
	// de eerste Go-instructie; de FSBL heeft de UART al op 115200 gezet).
	stub = append(stub,
		0x04140<<12|7<<7|0x37,   // lui  t2, 0x04140 (UART0)
		0x54<<20|28<<7|0x13,     // addi t3, x0, 'T'
		28<<20|7<<15|2<<12|0x23, // sw   t3, 0(t2)
	)
	// jump naar het ELF-entry: li t0, entry (zero-extended); jr t0
	// (lui sign-extendt op RV64, vandaar slli/srli — zie ook mkbios)
	lo := entry & 0xfff
	hi := entry >> 12
	if lo >= 0x800 {
		hi++
	}
	stub = append(stub,
		hi<<12|5<<7|0x37,             // lui  t0, hi20
		lo<<20|5<<15|5<<7|0x13,       // addi t0, t0, lo12
		32<<20|5<<15|1<<12|5<<7|0x13, // slli t0, t0, 32
		32<<20|5<<15|5<<12|5<<7|0x13, // srli t0, t0, 32
		5<<15|0x67,                   // jalr x0, 0(t0)
	)
	for i, insn := range stub {
		binary.LittleEndian.PutUint32(blob[i*4:], insn)
	}

	// Optioneel derde argument: een geassembleerde stub (tools/trapstub of
	// hello-lrv/slot/stub-slot.S) die de gegenereerde vervangt.
	//
	// CONTRACT: het blob begint met een sprong en heeft daarna op VASTE
	// offsets 8/16/24 zijn slots — entry, bss-start, bss-einde. Vaste
	// offsets vooraan, niet de staart: de RISC-V-assembler plakt achter de
	// laatste .dword nog een nop-vulling, dus "de laatste 24 bytes" is geen
	// betrouwbaar anker (30-07: stub-slot werd 340 bytes). De stub nult [bss-start, bss-einde) vóór
	// de sprong: de FSBL laadt alleen Filesz, en op écht DDR is de rest
	// rotzooi waar de Go-runtime nullen verwacht (QEMU maskeert dit met
	// vers-nul RAM — gemeten 30-07 op de LicheeRV: stille hang vóór Hwinit1).
	if len(args) == 3 {
		ext, err := os.ReadFile(args[2])
		if err != nil {
			fatal(err)
		}
		if len(ext) < 32 {
			fatal(fmt.Errorf("stub %s: te kort voor de slots (%d bytes)", args[2], len(ext)))
		}
		if len(ext) > 4096 {
			fatal(fmt.Errorf("stub %s (%d bytes) past niet vóór het eerste segment", args[2], len(ext)))
		}
		var bssLo, bssHi uint64
		for _, p := range f.Progs {
			if p.Type != elf.PT_LOAD || p.Memsz <= p.Filesz {
				continue
			}
			if bssLo != 0 {
				fatal(fmt.Errorf("meerdere segmenten met BSS-staart — stub-contract kent er één"))
			}
			bssLo = p.Paddr + p.Filesz
			bssHi = p.Paddr + p.Memsz
		}
		if bssLo%8 != 0 || bssHi%8 != 0 {
			fatal(fmt.Errorf("BSS-range [0x%x,0x%x) niet 8-aligned", bssLo, bssHi))
		}
		copy(blob, ext)
		binary.LittleEndian.PutUint64(blob[8:], uint64(entry))
		binary.LittleEndian.PutUint64(blob[16:], bssLo)
		binary.LittleEndian.PutUint64(blob[24:], bssHi)
		fmt.Printf("mkmonitor: stub %d bytes, BSS [0x%x, 0x%x) door de stub genuld\n",
			len(ext), bssLo, bssHi)
	}

	if err := os.WriteFile(args[1], blob, 0644); err != nil {
		fatal(err)
	}
	fmt.Printf("mkmonitor: entry 0x%08x, %d segmenten, blob %d bytes → %s\n",
		entry, len(f.Progs), len(blob), args[1])
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "mkmonitor:", err)
	os.Exit(1)
}
