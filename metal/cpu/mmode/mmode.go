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
//
// Sinds de kern-flip (docs/kern-flip.md) woont hij bij de boot bovendien niet
// meer ín het kern-image maar als KOPIE in de plan-regio: een hart dat hier
// middenin staat mag niet omvallen als HopOS zichzelf vervangt. De blob is
// [mentry, mmodeEnd) en is volledig positie-onafhankelijk (AUIPC/JAL), dus die
// kopie kost geen enkele patch.
package mmode

// De IMAGE-adressen van de blob (asm; switch.s) — de kopieerbron voor de
// plan-regio-install in kern/slots.
func entryImagePC() uint64
func parkEnterImagePC() uint64
func blobEndPC() uint64

// relocEntry/relocPark zijn de plan-kopie (0 = nog niet geïnstalleerd, dan
// geven de accessors het image-adres: exact het oude gedrag).
var relocEntry, relocPark uint64

// SetRelocated schakelt de accessors om naar de plan-kopie. Eén schrijver
// (kern/slots cageInit, achter vectorsOnce), vóór de eerste dispatch.
func SetRelocated(entry, park uint64) { relocEntry, relocPark = entry, park }

// ImageBlob geeft (start, einde) van de te kopiëren blob in image-adressen.
func ImageBlob() (start, end uint64) { return entryImagePC(), blobEndPC() }

// EntryPC geeft het fysieke adres van de trap-/switch-entry (mentry). HOP zet
// het in de slot-tabel van de kooi-stub, die het vóór zijn sprong in mtvec
// schrijft — zo landt élke trap van een app in HOP's eigen code in plaats van
// in een stukje assembly binnen de partitie van die app.
func EntryPC() uint64 {
	if relocEntry != 0 {
		return relocEntry
	}
	return entryImagePC()
}

// ParkEnterPC geeft het fysieke adres van parkenter: de boot-intrek van een
// parkerende core. kern/slots' cageInit adopteert de loterij-geparkeerde core
// hiermee (HartOn), zodat de switcher er vanaf de boot draait en een plaatsing
// daar altijd de gewone rotatie-route heeft.
func ParkEnterPC() uint64 {
	if relocPark != 0 {
		return relocPark
	}
	return parkEnterImagePC()
}
