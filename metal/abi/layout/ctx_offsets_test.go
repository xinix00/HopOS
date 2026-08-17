package layout

import "testing"

// TestCtxRevokeOwnLine bewaakt de regel waar de intrekking op RISC-V aan hangt:
// CtxRevoke heeft ÉÉN schrijver (HOP) en moet in een cacheline liggen waar de
// switcher niets in schrijft.
//
// Waarom dat geen netheid is. HOP schrijft dit woord terwijl de bewoner LEEFT —
// dat is per definitie het moment waarop de switcher op datzelfde hart zijn
// eigen velden in het ctx-blok zet (CtxState bij een yield, de GPR's, CtxWake).
// Die twee harten zijn niet coherent en de switcher draait zonder MMU, dus
// cachebaar: wie zijn regel terugschrijft, schrijft de bytes van de ander terug
// zoals ze bij zíjn fetch stonden. Belandt CtxRevoke in zo'n regel, dan is de
// faalmodus dat een intrekking soms verdwijnt — en dat is precies de vorm waarin
// je hem nooit reproduceert.
//
// De velden die de switcher schrijft staan hieronder met naam; de test rekent
// alleen na dat CtxRevoke niet in hun regels valt en binnen het blok blijft.
func TestCtxRevokeOwnLine(t *testing.T) {
	const line = 64

	// Alles wat cpu/mmode of cpu/el2 zelf in het ctx-blok schrijft, met de
	// lengte die het beslaat.
	written := []struct {
		name string
		off  int
		size int
	}{
		{"CtxState", CtxState, 8},
		{"CtxGPRs", CtxGPRs, 31 * 8},
		{"CtxSP", CtxSP, 16},
		{"CtxResume", CtxResume, 16},
		{"CtxRegime", CtxRegime, 19 * 8}, // ARM heeft er de meeste
		{"CtxWake", CtxWake, 8},
	}

	rl := CtxRevoke / line
	for _, w := range written {
		for off := w.off; off < w.off+w.size; off += 8 {
			if off/line == rl {
				t.Errorf("CtxRevoke (%d) deelt cacheline %d met %s (%d..%d) — de switcher schrijft daar, dus een intrekking kan verloren gaan",
					CtxRevoke, rl, w.name, w.off, w.off+w.size-8)
				break
			}
		}
	}
	if CtxRevoke%8 != 0 {
		t.Errorf("CtxRevoke = %d: niet 8-byte-gealigneerd", CtxRevoke)
	}
	// Het ctx-blok zit op CtxOff binnen het stride-blok van een slot en mag daar
	// niet uit groeien.
	if CtxOff+CtxRevoke+8 > Stage2Stride {
		t.Errorf("CtxRevoke (%d) valt buiten het stride-blok van een slot", CtxRevoke)
	}
}
