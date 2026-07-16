// Zelfplaatsing (15-07, idee Derek): de apploader plaatst de gedownloade app
// zélf voor, in plaats van dat HOP op core 0 de bytes schuift. De kooi maakt
// dat veilig — alles wat dit stubje binnen de eigen partitie doet (of fout
// doet) raakt uitsluitend dit slot; het geprivilegieerde deel (stage-2 bouwen,
// ringen, dispatch) blijft bij HOP. Winst: het kopieerwerk verhuist van één
// geserialiseerde kern-core naar 127 parallelle app-cores, en HOP hoeft een
// app-image niet eens meer te kúnnen lezen.
//
// Mechanisme: na de download parseert de loader de ELF uit zijn eigen staging
// (gewone cacheable reads), valideert dezelfde invarianten als HOP legacy
// deed, en genereert een plat instructie-stubje (à la stage2's
// vectorgenerator: movz/movk met absolute adressen — geen PIC, geen data) net
// onder de staging. HOP dispatcht de geparkeerde core straks naar dat stubje
// (CtrlPlaceEntry); het draait op EL1 met de MMU uit (de normale boot-staat
// na de EL2-trampoline) en doet exact wat HOP's placeFromStaging deed:
//
//  1. DC CIVAC over heel app-RAM — de dirty lines van de loader (deze zelfde
//     core!) naar DRAM en weg, zodat de ongecachte stappen hierna nergens
//     door een latere eviction overschreven worden;
//  2. segmenten staging → linkadres (de staging ligt boven élk segment-einde,
//     door de validatie afgedwongen, dus voorwaarts kopiëren is veilig);
//  3. BSS nullen;
//  4. RamStart/RamSize/slotHint patchen (waarden kent de loader: zijn eigen
//     gepatchte RAMStart/RAMSize en Slot);
//  5. dsb; ic iallu; dsb; isb — het verse .text de I-zijde in (de trampoline
//     veegde vóór het stubje, dit veegt ná de moves);
//  6. br app-entry — de app boot alsof HOP hem plaatste.
//
// Faalt het parsen of past iets niet, dan blijft CtrlPlaceEntry 0 en plaatst
// HOP legacy vanaf de staging — zelfde nette foutmeldingen als altijd.
package applib

import (
	"bytes"
	"fmt"
	"unsafe"

	"hop-os/metal/abi/a64"
	"hop-os/metal/abi/place"
	"hop-os/metal/dev"
)

const (
	// stubWin is het venster net onder de staging voor het gegenereerde
	// stubje. Segmenten mogen er niet in reiken (gevalideerd) en de
	// stub-code moet erin passen (ruim: ~30 woorden vast + ~40 per segment).
	stubWin = 0x10000 // 64KB
)

// spLoopEnd sluit een voorwaartse lus af: cmp x<ra>,x<rb>; b.lo →loopStart.
// loopStart is de index van de eerste lus-instructie in code; de cmp en b.lo
// worden hier toegevoegd, de branch-offset (t.o.v. de b.lo zelf, in woorden)
// volgt uit de posities.
func spLoopEnd(code []uint32, ra, rb uint32, loopStart int) []uint32 {
	code = append(code, 0xEB00001F|rb<<16|ra<<5) // cmp xa, xb = subs xzr,xa,xb
	off := uint32(loopStart-len(code)) & 0x7FFFF // b.lo staat op len(code)
	return append(code, 0x54000003|off<<5)       // b.lo (cond CC/LO)
}

// selfPlace parseert de gestagede image, valideert de plaatsingsinvarianten
// (dezelfde als kern/slots legacy) en genereert het plaatsings-stubje. Geeft
// de IPA van het stubje terug; een fout betekent "geen zelfplaatsing" — de
// aanroeper laat CtrlPlaceEntry dan 0 en HOP plaatst legacy.
func (a *App) selfPlace(stageAddr uintptr, imgSize int64) (uint64, error) {
	// De validatie en het plan komen uit abi/place — dezelfde bron van
	// waarheid als HOP's legacy-plaatsing. De loader is canoniek gepatcht,
	// dus zijn RAMStart ís de linkbasis; het plafond = het stub-venster
	// (segmenten mogen bron noch stubje raken).
	linkBase := a.RAMStart
	appRAM := a.RAMSize
	stubBase := uint64(stageAddr) - stubWin
	img := unsafe.Slice((*byte)(unsafe.Pointer(stageAddr)), imgSize)
	plan, err := place.Build(bytes.NewReader(img), imgSize, linkBase, appRAM, stubBase-linkBase, a.Slot)
	if err != nil {
		return 0, err
	}
	// De gegenereerde 8-byte-lus eist 8-uitlijning van dst én bron; Go's
	// linker levert dat (Off ≡ Vaddr mod align). Zo niet: legacy.
	for _, s := range plan.Segs {
		if s.Dst%8 != 0 || (uint64(stageAddr)+s.Off)%8 != 0 {
			return 0, fmt.Errorf("segment %#x niet 8-aligned (off %#x)", s.Dst, s.Off)
		}
	}

	// Fase 2: genereren. Registers: x0 cursor, x1 einde/bron, x3 scratch.
	var code []uint32

	// 1. DC CIVAC over heel app-RAM: de dirty lines van deze core (de
	// loader-runtime drááit nog tot de park — zijn laatste stack-regels
	// incluis) naar DRAM, zodat niets hierna nog over de ongecachte moves
	// heen evict. Het stubje draait op dezelfde core, dus dit veegt precies
	// de juiste caches.
	code = a64.Mov64(code, 0, linkBase)
	code = a64.Mov64(code, 1, linkBase+appRAM)
	loop := len(code)
	code = append(code, 0xD50B7E20) // dc civac, x0
	code = append(code, 0x91010000) // add x0, x0, #64
	code = spLoopEnd(code, 0, 1, loop)
	code = append(code, a64.DSBSY)

	// 2. Segmenten kopiëren: 8-byte voorwaartse lus + byte-staart. De bron
	// (staging + Off) ligt boven élk segment-einde (place.Build bewaakt het
	// plafond), dus voorwaarts kopiëren is veilig.
	for _, s := range plan.Segs {
		src := uint64(stageAddr) + s.Off
		if s.Filesz >= 8 {
			words := s.Filesz &^ 7
			code = a64.Mov64(code, 0, s.Dst)
			code = a64.Mov64(code, 1, src)
			code = a64.Mov64(code, 2, s.Dst+words)
			loop = len(code)
			code = append(code, 0xF8408423) // ldr x3, [x1], #8
			code = append(code, 0xF8008403) // str x3, [x0], #8
			code = spLoopEnd(code, 0, 2, loop)
		}
		for k := s.Filesz &^ 7; k < s.Filesz; k++ {
			code = a64.Mov64(code, 0, s.Dst+k)
			code = a64.Mov64(code, 1, src+k)
			code = append(code, 0x39400023) // ldrb w3, [x1]
			code = append(code, 0x39000003) // strb w3, [x0]
		}
		// 3. BSS (memsz − filesz) nullen: eerst losse kop-bytes tot de
		// 8-grens (≤7 — de start = dst+filesz kan scheef staan), dan de
		// 8-byte-lus, dan de staart (≤7). Nooit per-byte over de bulk: een
		// 30MB-BSS zou anders 30M gegenereerde instructies worden.
		if z := s.Memsz - s.Filesz; z > 0 {
			start := s.Dst + s.Filesz
			head := (8 - start%8) % 8
			if head > z {
				head = z
			}
			for k := uint64(0); k < head; k++ {
				code = a64.Mov64(code, 0, start+k)
				code = append(code, 0x3900001F) // strb wzr, [x0]
			}
			start += head
			z -= head
			if words := z &^ 7; words > 0 {
				code = a64.Mov64(code, 0, start)
				code = a64.Mov64(code, 2, start+words)
				loop = len(code)
				code = append(code, 0xF800841F) // str xzr, [x0], #8
				code = spLoopEnd(code, 0, 2, loop)
				start += words
				z -= words
			}
			for k := uint64(0); k < z; k++ {
				code = a64.Mov64(code, 0, start+k)
				code = append(code, 0x3900001F) // strb wzr, [x0]
			}
		}
	}

	// 4. Patches (RamStart/RamSize/slotHint — waarden uit het plan).
	for _, p := range plan.Patches {
		code = a64.Mov64(code, 0, p.Addr)
		code = a64.Mov64(code, 1, p.Val)
		code = append(code, 0xF9000001) // str x1, [x0]
	}

	// 5. Sync + 6. spring de app in.
	code = append(code, a64.DSBSY, a64.ICIALLU, a64.DSBSY, a64.ISB)
	code = a64.Mov64(code, 0, plan.Entry)
	code = append(code, 0xD61F0000) // br x0

	if len(code)*4 > stubWin {
		return 0, fmt.Errorf("stub %d woorden > venster %d", len(code), stubWin/4)
	}

	// Schrijven + naar PoC vegen: het stubje wordt straks met de MMU uit
	// gefetcht (ongecached), dus zijn bytes moeten in DRAM staan.
	for w, ins := range code {
		dev.Write32(uintptr(stubBase)+uintptr(w)*4, ins)
	}
	dev.CleanInv(uintptr(stubBase), uintptr(len(code)*4))
	dev.MB()
	return stubBase, nil
}
