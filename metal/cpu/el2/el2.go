// Package el2 draagt de gedeelde EL2-trampolines van HopOS: el2.s (app-core
// onder stage-2-isolatie, fase 4.2) en smp.s (secundaire SMP-core in een al
// draaiende app-runtime, fase 5). Er staat hier níéts board-specifieks: geen
// GIC (de hard-kill loopt via stage2.Revoke), geen MPIDR (de vectoren halen
// het slot uit VTTBR_EL2.VMID), en geen adres-#defines — de trampolines zijn
// data-gedreven: PSCI CPU_ON geeft de fysieke control-page als ctx en dáár
// staat alles (vectoren-PA, stage-2-tabel, entry, VMID; layout.Ctrl*-velden,
// de offsets staan als literals in de .s-bestanden). Eén bewezen
// implementatie voor QEMU, de Pi's en de O6N — een nieuw board levert alleen
// nog zijn cpuinit (boot is échte board-kennis), zijn PA-plan
// (layout.UsePlan) en de verificatie op het board.
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
