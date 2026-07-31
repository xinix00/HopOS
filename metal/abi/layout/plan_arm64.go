//go:build arm64

package layout

// validatePlanArch checkt de plan-velden die de ARM-kooi eist: de stage-2-
// tabelblokken en de EL2-vectortabel. Beide moeten 2KB-aligned zijn — dat is
// een VBAR_EL2-hardware-eis, en liever hier hard falen dan een scheve
// vectortabel op een core.
func validatePlanArch(p Plan) {
	switch {
	case p.Stage2PA == 0 || p.Stage2PA&0x7FF != 0:
		panic("layout: Plan.Stage2PA ontbreekt of niet 2KB-aligned (VBAR-eis)")
	case p.RevokeVecPA == 0 || p.RevokeVecPA&0x7FF != 0:
		panic("layout: Plan.RevokeVecPA ontbreekt of niet 2KB-aligned (VBAR-eis)")
	}
}
