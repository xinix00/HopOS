//go:build tamago && arm64

package el2

// De publieke PC-accessors met de plan-regio-omschakeling (docs/kern-flip.md):
// kern/stage2 kopieert de drie blobs bij InitVectors naar de plan-regio en
// meldt de kopie-adressen hier aan. Vanaf dat moment voeren app-cores nooit
// meer kern-image-bytes uit — de randvoorwaarde om het kern-venster bij een
// kern-flip te kunnen verlaten. Vóór de install (en op elk pad dat zonder
// install draait) geven de accessors het image-adres: exact het oude gedrag.
var relocEntry, relocTramp, relocSMP uint64

// SetRelocated schakelt de accessors om naar de plan-kopieën. Eén schrijver
// (stage2.InitVectors, achter vectorsOnce), vóór de eerste dispatch.
func SetRelocated(entry, tramp, smp uint64) {
	relocEntry, relocTramp, relocSMP = entry, tramp, smp
}

// ImageBlobs geeft de kopieerbronnen: (start, einde) per blob, in image-adressen.
func ImageBlobs() (entry, entryEnd, tramp, trampEnd, smp, smpEnd uint64) {
	return entryPC(), entryEndPC(), s2trampPC(), s2trampEndPC(), smpTrampPC(), smpTrampEndPC()
}

// EntryPC geeft het fysieke adres van de EL2-switch (switch.s) — het
// sprongdoel van de vector-thunks die kern/stage2 genereert.
func EntryPC() uint64 {
	if relocEntry != 0 {
		return relocEntry
	}
	return entryPC()
}

// S2TrampPC geeft het fysieke adres van de EL2-trampoline (el2.s): het
// CPU_ON-entrypoint voor app-cores onder stage-2-isolatie.
func S2TrampPC() uint64 {
	if relocTramp != 0 {
		return relocTramp
	}
	return s2trampPC()
}

// S2SMPTrampPC geeft het adres van de EL2 SMP-trampoline (smp.s): het
// CPU_ON-entrypoint dat HOP op de control-page publiceert.
func S2SMPTrampPC() uint64 {
	if relocSMP != 0 {
		return relocSMP
	}
	return smpTrampPC()
}
