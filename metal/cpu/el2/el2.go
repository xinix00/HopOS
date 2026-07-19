// Package el2 draagt de gedeelde EL2-machinerie van HopOS: el2.s (app-core
// onder stage-2-isolatie, fase 4.2), smp.s (secundaire SMP-core in een al
// draaiende app-runtime, fase 5) en switch.s (coöperatieve core-deling:
// meerdere kooien om één core, fase 6). Er staat hier níéts board-specifieks:
// geen GIC (de hard-kill loopt via stage2.Revoke), geen MPIDR (de vectoren
// halen het slot uit VTTBR_EL2.VMID), en geen adres-#defines — alles is
// data-gedreven via de control-page en het per-core sched-blok
// (layout.Sched*). Eén bewezen implementatie voor QEMU, de Pi's en de O6N —
// een nieuw board levert alleen nog zijn cpuinit, zijn PA-plan
// (layout.UsePlan) en de verificatie op het board.
//
// De PC-accessors staan in pc_tamago.go (asm) met host-stubs in pc_host.go:
// kern/stage2 (host-getest) importeert dit pakket voor el2entry's adres.
package el2
