//go:build tamago && arm64

package el2

// S2TrampPC geeft het fysieke adres van de EL2-trampoline (el2.s): het
// CPU_ON-entrypoint voor app-cores onder stage-2-isolatie (de HOP-image is
// identity-geladen, dus symbooladres = fysiek adres).
func S2TrampPC() uint64

// S2SMPTrampPC geeft het adres van de EL2 SMP-trampoline (smp.s). In de
// HOP-image is dat het fysieke CPU_ON-entrypoint dat HOP op de control-page
// publiceert.
func S2SMPTrampPC() uint64

// SMPStubPC geeft het adres van de EL1-stub (smp.s). In een app-image is dat
// de IPA waar goos.Task naar laat ERET'en zodra de gedeelde stage-2 actief is.
func SMPStubPC() uint64

// EntryPC geeft het fysieke adres van de EL2-switch (switch.s) — het
// sprongdoel van de vector-thunks die kern/stage2 genereert.
func EntryPC() uint64
