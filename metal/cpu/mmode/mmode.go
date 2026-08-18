// Package mmode is de laag die HOP bezit op RISC-V: de machine-mode-code die op
// een app-hart draait zodra de app daar trapt of yieldt. Het is de tegenhanger
// van cpu/el2 op ARM, en bedoeld om ernaast gelezen te worden — dezelfde machine,
// andere letters:
//
//	ARM                     RISC-V
//	EL2                     machine mode
//	EL1-app                 supervisor-mode-app
//	HVC-yield               ecall-yield
//	stage-2 (VTTBR)         map (satp) + whitelist (PMP)
//	ERET                    mret
//
// Eén bestand, één taak: twee bewoners op één hart afwisselen (switch.s). Wat
// daar staat is een vertaling van cpu/el2/switch.s, niet een tweede ontwerp — het
// contract (bewaar de staat die de volgende niet mag erven, roteer over de
// bewonerslijst, hervat of cold-boot) is op ARM al bewezen.
//
// Waarom deze code in HOP's image woont en niet in de kooi-stub van een slot: met
// twee bewoners mag de code die tussen hen wisselt niet in het geheugen van één
// van hen liggen. Dat kan alleen sinds de PMP-entries niet meer gelockt zijn
// (kern/cage): locken bindt PMP óók aan machine mode, en dan zou deze code buiten
// een partitie niet eens uitvoerbaar zijn.
package mmode

// EntryPC geeft het fysieke adres van de trap-/switch-entry (switch.s). HOP zet
// het in de slot-tabel van de kooi-stub, die het vóór zijn sprong in mtvec
// schrijft — zo landt élke trap van een app in HOP's eigen code in plaats van in
// een stukje assembly binnen de partitie van die app.
func EntryPC() uint64

// ParkEnterPC geeft het fysieke adres van parkenter (switch.s): de
// boot-intrek van een parkerende core. kern/slots' cageInit adopteert de
// loterij-geparkeerde core hiermee (HartOn), zodat de switcher er vanaf de
// boot draait en een plaatsing daar altijd de gewone rotatie-route heeft.
func ParkEnterPC() uint64
