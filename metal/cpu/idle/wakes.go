package idle

// De WEK-TELLER: hoe vaak de scheduler van dit slot zijn idle-ronde deed.
//
// Waarom dit er is (31-07). Twee lege apps op één hart lazen allebei 36% busy.
// Dat rook naar een meetfout — "hij telt het schakelen als werk" — en er stond
// hier even een correctie die het gat tussen twee idle-rondes als idle bijtelde.
// Die deed NIETS: het cijfer bleef exact 36%, en dat is zelf de meting. De
// correctie gold tot 40µs, dus een onveranderd cijfer bewijst dat de eigen-werk-
// stretch van een "lege" app lánger dan 40µs is. Er wordt dus geen idle-tijd als
// busy geteld — de app is écht bezig.
//
// Waarmee bezig, dat zegt deze teller. Idle-tijd alleen kan niet onderscheiden
// tussen "één keer 100ms echt werk" en "3333 keer per seconde wakker worden om
// niets te vinden", terwijl dat voor stroom en warmte het hele verschil is. Met
// de wekken erbij is de rekensom rond:
//
//	tijd per wek = (1 − idle-fractie) / wekken-per-seconde
//
// en dan zie je meteen of een app duur is per wek of gewoon vaak wakker.
//
// Kosten: één 64-bit store per idle-ronde, op dezelfde control-page en in
// dezelfde ronde als de idle-teller die er al stond. Geen extra pad, geen
// ABI-uitbreiding aan de app-kant — HOP leest het woord, of leest het niet.
import (
	"sync/atomic"

	"hop-os/metal/dev"
)

var (
	wakes    atomic.Uint64
	wakeAddr atomic.Uintptr
)

// PublishWakes laat de wek-teller op addr landen — het CtrlWakes-woord van de
// eigen control-page. applib roept dit naast Publish.
func PublishWakes(addr uintptr) { wakeAddr.Store(addr) }

// countWake telt de ronde en publiceert hem. Aangeroepen door beide governors,
// op precies de plek waar ze de idle-teller al wegschrijven.
func countWake() {
	n := wakes.Add(1)
	if a := wakeAddr.Load(); a != 0 {
		dev.Write64(a, n)
	}
}
