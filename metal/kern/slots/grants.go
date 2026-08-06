// grants.go — het registratiepunt voor DeviceGrants (docs/gui-ontwerp.md §7):
// beleid dat bij de slot-lifecycle een device-venster toekent leeft búíten
// kern (metal/gui/fbgrant is de eerste; latere kooi-drivers volgen dezelfde
// naad). cmd bedraadt het in zijn gui-smaak (cmd/hopos/gui.go); kaal gebouwd
// is er niets geregistreerd en zijn grants gewoon uit. Zo bevat kern/slots
// zelf nul gui-beleid — alleen de lifecycle-haakjes.
package slots

import "fmt"

// GrantHooks zijn de drie lifecycle-haakjes van één grant-provider:
//   - Env (prepStart): mag de slot-env aanvullen (bv. FB_* voor de houder);
//     geeft de (eventueel gekopieerde) env terug.
//   - Arm (armSlot, ná de kooi-bouw en vóór de dispatch): mapt het venster
//     in de kooi van de houder; no-op voor andere slots.
//   - Window (optioneel): geeft het venster van de houder terug. Nodig voor
//     kooien die vooráf compleet moeten zijn i.p.v. gemapt-na-de-bouw: de
//     PMP-kooi op RISC-V programmeert alle vensters in één keer en kan er
//     naderhand niets meer bij doen (locked entries zijn definitief tot de
//     hart-reset). Providers die alleen ARM ondersteunen laten dit nil.
//   - Release (releaseSlot): geeft de grant terug bij het vrijkomen.
type GrantHooks struct {
	Env     func(i int, env map[string]string) map[string]string
	Arm     func(i int) error
	Window  func(i int) (base, size uint64, ok bool)
	Release func(i int)
}

var grant GrantHooks

// RegisterGrant zet de provider — één keer, uit een init/main vóór de eerste
// Start (daarna lezen de lifecycles hem zonder lock).
func RegisterGrant(h GrantHooks) { grant = h }

func grantEnv(i int, env map[string]string) map[string]string {
	if grant.Env != nil {
		return grant.Env(i, env)
	}
	// Diagnose i.p.v. stilte: een desktop-jobspec op een kale build.
	if env["FB"] == "1" {
		fmt.Printf("slot %d: fb grant requested, but no grant provider linked (headless build)\n", i)
	}
	return env
}

// grantWindow geeft het venster van de houder van slot i (zie GrantHooks.Window);
// ok=false als dit slot geen grant heeft of de provider geen vensters meldt.
func grantWindow(i int) (base, size uint64, ok bool) {
	if grant.Window == nil {
		return 0, 0, false
	}
	return grant.Window(i)
}

func grantArm(i int) error {
	if grant.Arm == nil {
		return nil
	}
	return grant.Arm(i)
}

func grantRelease(i int) {
	if grant.Release != nil {
		grant.Release(i)
	}
}
