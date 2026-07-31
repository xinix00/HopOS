//go:build riscv64

package layout

// validatePlanArch checkt wat de kooi-mechaniek van deze architectuur eist.
//
// Er is géén stage-2: de kooi is een PMP-whitelist (kern/cage) en die
// zit in registers per hart, dus tabelblokken en een EL2-vectortabel bestaan
// hier niet. RevokeVecPA blijft daarom leeg.
//
// Plan.Stage2PA is er wél nodig, ondanks zijn ARM-naam: in díe regio wonen ook
// de per-slot **ctx-blokken** (het levensteken dat slots.Get en de
// deel-machinerie lezen) en de park-mailboxen. Dat is HOP's eigen boekhouding,
// geen vertaal-hardware — en zonder plek leest slots.Get vanaf adres nul.
// GEMETEN 30-07: dat gaf een load access fault in een achtergrond-goroutine,
// dus liever hier hard falen met een reden dan straks een exception zonder
// context. 4KB-aligned is voldoende; de 2KB-VBAR-eis van ARM geldt hier niet.
func validatePlanArch(p Plan) {
	if p.Stage2PA == 0 || p.Stage2PA&0xFFF != 0 {
		panic("layout: Plan.Stage2PA ontbreekt of niet 4KB-aligned (ctx-blokken + park-mailboxen)")
	}
}
