//go:build tamago

package kernflip

import (
	"bytes"
	"fmt"
	"runtime"
	"time"

	"github.com/xinix00/lean/leanelf"

	"github.com/xinix00/HopOS/metal/abi/checksum"
	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/abi/place"
	"github.com/xinix00/HopOS/metal/dev"
	"github.com/xinix00/HopOS/metal/kern/slots"
	"github.com/xinix00/HopOS/metal/net/hopswitch"
)

// FlipFromURL haalt een flip-bundel op, controleert zijn SHA-256 tegen wat de
// platform-config zegt, en flipt erin. Keert alleen terug met een fout; de node
// draait dan gewoon door op de zittende kern.
func FlipFromURL(url, sha string) error {
	fmt.Printf("kernflip: fetching %s\n", url)
	img, err := fetchBundle(url, sha)
	if err != nil {
		return err
	}
	if len(img) == 0 {
		return fmt.Errorf("kernflip: no URL configured")
	}
	stage(stFetched)
	fmt.Printf("kernflip: fetched %d bytes, sha256 verified\n", len(img))
	// Draaien we al uit precies deze bundel? Dan niet opnieuw springen: dat is
	// een bootlus die alleen met een stekker te doorbreken is. De vorige kern
	// schreef de som van zijn eigen bundel in het handoff-blob, dus dit kost
	// geen tweede lezing van wat er draait.
	if curSum != 0 && curSum == checksum.FNV64(img) {
		stageClear()
		return fmt.Errorf("kernflip: already running the kernel from %s (generation %d) — staying put", url, curGen)
	}
	if err := Flip(img); err != nil {
		// Niet gesprongen: de kern leeft en heeft de fout gemeld — het spoor
		// is verteld en mag weg, anders meldt een latere gewone reboot een
		// flip-mislukking die er geen was.
		stageClear()
		return err
	}
	return nil
}

// Flip plaatst de bundel in een uit de pool geleend venster en springt erin.
// Keert alleen terug met een fout (en heeft dan niets blijvends veranderd:
// de lening gaat terug de pool in). Bij succes boot de nieuwe kern zijn
// volledige cpuinit→main-pad op het nieuwe venster; deze kern houdt op te
// bestaan op het moment van de HVC.
//
// Zittende apps gaan mee: ze worden geïnventariseerd en aan de nieuwe kern
// overgedragen, die ze adopteert zonder ze aan te raken. Wat níet mee kan,
// weigert deze functie vóór er iets geleend of geschreven is — node-SMP,
// SMP-apps, gemounte volumes, en een nieuwe kern met andere switch-code (zie
// docs/kern-flip.md voor het waarom van elk).
// kernHeader is de ruimte onder het linkadres die een kern-image vrij houdt
// (de boot-header van mkkernel): de vloer voor place.Build, zoals cageFloor dat
// voor een app is.
const kernHeader = 64

func Flip(bundle []byte) error {
	// Het lifecycle-venster over de HELE flip, en dat is geen voorzorg maar
	// een correctheidseis: tussen de inventarisatie van de bewoners en de
	// sprong zitten honderden milliseconden (242MB vegen, 13MB kopiëren, beide
	// met yields erin). Zou de agent daarin een slot starten, dan stond die
	// bewoner niet in de overdracht terwijl zijn core wél doordraait — en de
	// volgende kern zou zijn partitie als vrije pool zien. Dat is de
	// dubbeluitgifte van 31-08, dan via een race. Bij succes keert deze functie
	// nooit terug, dus de unlock is er voor de faalpaden.
	//
	// Dit venster is ook een WACHTPLEK: draait er net een slot-start of -stop,
	// dan staat de flip hier stil tot die klaar is. Vandaar de regel ervoor —
	// een flip die hier blijft hangen ziet er anders uit als een flip die
	// nooit begon.
	fmt.Printf("kernflip: waiting for the slot lifecycle window\n")
	defer slots.LifecycleWindow()()
	stage(stWindowHeld)
	fmt.Printf("kernflip: lifecycle window held, validating the bundle\n")

	if err := archPreflight(); err != nil {
		return err
	}
	bun, err := ParseBundle(bundle)
	if err != nil {
		return fmt.Errorf("kernflip: %w", err)
	}
	if bun.FlipABI != ABI {
		return fmt.Errorf("kernflip: bundel spreekt flip-ABI %d, deze kern %d — niet springen", bun.FlipABI, ABI)
	}
	if n := nodeCoresActive(); n > 0 {
		return fmt.Errorf("kernflip: %d extra node core(s) active — a flip needs hopos.cores=1 (v1)", n)
	}

	// Het plaatsingsplan komt uit abi/place — dezelfde parser, dezelfde grenzen
	// en dezelfde RAM-symbolen als een app-plaatsing (kern/slots
	// placeFromStaging): de nieuwe kern is dezelfde soort bewoner als een app,
	// alleen zonder slot-ABI-stempel (abi 0) en met de header-ruimte als vloer.
	// Alles wat hier faalt, faalt vóór de lening.
	f, err := leanelf.Open(bytes.NewReader(bun.ELF), int64(len(bun.ELF)))
	if err != nil {
		return fmt.Errorf("kernflip: elf parse: %w", err)
	}
	if f.Entry != bun.Entry {
		return fmt.Errorf("kernflip: ELF-entry %#x ≠ staart-entry %#x", f.Entry, bun.Entry)
	}
	plan, err := place.Build(bytes.NewReader(bun.ELF), int64(len(bun.ELF)),
		bun.LinkLoad, bun.FlatSize, kernHeader, bun.FlatSize, 0, 0)
	if err != nil {
		return fmt.Errorf("kernflip: %w", err)
	}
	// De patchwaarden zijn hier anders dan bij een app (het venster, niet het
	// linkadres), dus alleen de adressen komen uit het plan; place heeft ze al
	// gevalideerd (uitgelijnd, binnen de payload).
	syms, err := f.Lookup(place.SymRAMStart, place.SymRAMSize)
	if err != nil {
		return fmt.Errorf("kernflip: symbolen (bundel met -s gebouwd?): %w", err)
	}

	// De bewoners, en of ze deze flip kunnen overleven. Twee eisen, allebei
	// hier — vóór er iets geleend of geschreven wordt:
	//
	//  1. hun wereld moet overdraagbaar zijn (SnapshotForFlip weigert wat niet
	//     kan: SMP, gemounte volumes);
	//  2. de nieuwe kern moet EXACT dezelfde switch-code dragen. Een geyielde
	//     of parkerende app-core staat op dit moment ín die code (de kopie in
	//     de plan-regio); komt de nieuwe kern met andere bytes, dan zou hij ze
	//     moeten overschrijven onder een levende core. Gelijke som = niets te
	//     overschrijven en de cores merken de wissel niet.
	residents, err := slots.SnapshotForFlip()
	if err != nil {
		return fmt.Errorf("kernflip: %w", err)
	}
	// Geparkeerde cores tellen NIET mee: die staan in de parkeerlus, en die
	// genereert kern/stage2 zelf, met dezelfde bytes in elke kern — een
	// ABI, net als de mailbox waar de lus op wacht. De volgende kern schrijft
	// hem identiek terug en dispatcht met een vers doel-PC, dus zo'n core
	// merkt de wissel niet. Alleen wie ín de switch-code staat (geyield of
	// draaiend) bindt de bytes. (Er stond hier een dag een guard op
	// geparkeerde cores plus een reset-poging; de reset bleek op de M4 niets
	// te stoppen en de guard blokkeerde élke switch-code-wijziging, 02-09.)
	if len(residents) > 0 {
		mine := switchCodeHash()
		theirs, err := bundleSwitchHash(f)
		if err != nil {
			return fmt.Errorf("kernflip: %d resident(s) alive but the new kernel's switch code cannot be verified: %w", len(residents), err)
		}
		if mine == 0 || mine != theirs {
			return fmt.Errorf("kernflip: the new kernel's switch code differs (%#x vs %#x) — cannot flip with %d live resident(s); stop them or use a reboot update",
				theirs, mine, len(residents))
		}
	}

	// Het venster: even groot als de eigen RAM-declaratie — de nieuwe kern is
	// dezelfde soort bewoner als wij. Plus de handoff-staart erboven.
	me0, me1 := runtime.MemRegion()
	ramSize := uint64(me1 - me0)
	if bun.FlatSize+handoffTail > ramSize {
		return fmt.Errorf("kernflip: payload (%d MB) past niet in een kern-venster van %d MB", bun.FlatSize>>20, ramSize>>20)
	}
	win, total, err := slots.BorrowKernWindow(ramSize + handoffTail)
	if err != nil {
		return fmt.Errorf("kernflip: %w", err)
	}
	fail := func(err error) error {
		slots.ReturnKernWindow()
		return err
	}
	// Vanaf hier vertelt de flip wat hij doet, stap voor stap. Dat is geen
	// ruis maar het enige diagnosemiddel dat deze operatie heeft: hij eindigt
	// per definitie in een sprong waarna de vorige kern — en dus zijn console
	// en zijn netstack — niet meer bestaat. Blijft hij ergens hangen of valt
	// hij om, dan is de LAATSTE regel die je zag het antwoord op "waar".
	// GEMETEN 01-09 op de M4: tussen "flip requested" en de reboot kwam er
	// niets, en daarmee was elke stap hierboven even verdacht.
	stage(stBorrowed)
	fmt.Printf("kernflip: bundle ok (%d MB payload, %d relocs), borrowed %d MB at %#x\n",
		bun.FlatSize>>20, bun.RelocCount(), total>>20, win)

	// De chainload-handler moet er staan vóór de HVC (en de switch-code-kopie
	// hoort er dan ook al te zijn) — idempotent.
	slots.EnsureVectors()

	// Hygiëne: een vorige huurder van dit venster kan er nog (schone) lines
	// van vasthouden die een latere cacheable read stale zouden maken — zelfde
	// contract als een app-plaatsing. In brokken met een yield: core 0 draagt
	// nu ook de node.
	// De veeg is die van de slot-lifecycle (slots.Scrub) — één lus voor beide,
	// zodat "een app-start doorstaat dit wél" een bruikbare vergelijking is.
	stage(stVectors)
	t0 := time.Now()
	slots.Scrub(uintptr(win), uintptr(total), nil)
	stage(stScrubbed)
	fmt.Printf("kernflip: window scrubbed in %v, placing segments\n", time.Since(t0).Round(time.Millisecond))

	// Segmenten plaatsen: venster + (linkadres − linkbasis), per segment uit
	// het plan (al gevalideerd). Het kopiëren zelf is het enige verschil met
	// een app-plaatsing: die schuift device→device vanuit de staging, dit is
	// een Go-slice — in brokken met een yield (copyRange).
	delta := win - bun.LinkLoad
	for _, sg := range plan.Segs {
		dst := uintptr(sg.Dst + delta)
		copyRange(dst, bun.ELF[sg.Off:sg.Off+sg.Filesz])
		if sg.Memsz > sg.Filesz {
			dev.Clear(dst+uintptr(sg.Filesz), sg.Memsz-sg.Filesz)
		}
	}

	stage(stPlaced)
	// De reloc-pass: elk tabelwoord draagt een absoluut adres op de linkbasis;
	// delta erbij en het wijst het venster in. (Offsets zijn al gevalideerd.)
	for i := 0; i < bun.RelocCount(); i++ {
		a := uintptr(win) + uintptr(uint32(bun.Relocs[i*4])|uint32(bun.Relocs[i*4+1])<<8|
			uint32(bun.Relocs[i*4+2])<<16|uint32(bun.Relocs[i*4+3])<<24)
		dev.Write64(a, dev.Read64(a)+delta)
	}

	// RamStart/RamSize: dezelfde patch die place voor een app doet — de
	// nieuwe kern declareert exact zijn venster (de handoff-staart valt er
	// bewust buiten).
	dev.Write64(uintptr(win+(syms[place.SymRAMStart].Value-bun.LinkLoad)), win)
	dev.Write64(uintptr(win+(syms[place.SymRAMSize].Value-bun.LinkLoad)), total-handoffTail)

	// Het handoff-blob, boven de RAM-declaratie van de nieuwe kern. De
	// conntrack gaat mee: de apps overleven de wissel, dus hun VERBINDINGEN
	// horen dat ook te doen — zonder deze tabel zou elke uitgaande sessie
	// (cloudflared-tunnel, API-call) alsnog breken op een node die verder
	// niets merkte. Snapshot hier, ná de slot-inventarisatie en binnen
	// hetzelfde lifecycle-venster, zodat flows en slots hetzelfde moment
	// beschrijven.
	// De agent-state als allerlaatste, samen met de conntrack: alles wat er ná
	// dit moment nog bij komt (een taak die net startte, een verbinding die
	// net opging) staat niet in de overdracht. Vandaar hier, vlak vóór de
	// sprong — en niet bij de slot-inventarisatie honderden milliseconden
	// geleden.
	stage(stRebased)
	fmt.Printf("kernflip: placed and rebased, capturing state\n")
	nat := hopswitch.SnapshotNAT()
	agentState := snapshotAgent()
	blob, err := encodeHandoff(Handoff{
		OldBase: uint64(me0), OldSize: ramSize,
		Window: win, Total: total,
		Gen:       curGen + 1,
		BundleSum: checksum.FNV64(bundle),
		Slots:     residents,
		NAT:       nat,
		Agent:     agentState,
	}, handoffTail)
	if err != nil {
		return fail(fmt.Errorf("kernflip: %w", err))
	}
	stage(stCaptured)
	hb := uintptr(win + total - handoffTail)
	dev.Clear(hb, handoffTail)
	dev.Copy(hb, blob)

	// Hier stond een tweede veeg over het hele venster, "want de nieuwe kern
	// fetcht met de MMU uit en ziet onze dirty lines niet". Die aanname was
	// FOUT en is 01-09 ingetrokken: HOP mapt al het DRAM buiten zijn eigen
	// venster als DEVICE-geheugen (board/apple MapDRAM → deviceBlock2M), dus
	// élke dev-write hierboven ging al rechtstreeks naar DRAM. Er valt niets
	// te flushen, en het scheelde een tweede veeg van een kwart gigabyte op
	// het zwaarste moment van de operatie.
	//
	// De veeg vóór de plaatsing blijft wél nodig, en om een andere reden: die
	// gaat over de vórige huurder van dit venster, en dát was een app-core die
	// er cacheable in woonde.
	dev.Write64(layout.HandoffPtrPA(), uint64(hb))
	dev.Write64(layout.HandoffPtrPA()+8, handMagic)
	// En de pointer-lijn zelf ook: die 64-byte cachelijn deelt hij op de
	// boot-scratch met woorden die cpuinit en de parkeerstubs straks met de
	// MMU uit beschrijven (DTBPtr, MPIDR, HopAlive) — blijft onze lijn dirty
	// hangen, dan draait een latere eviction die verse waarden stil terug.
	dev.CleanInv(layout.HandoffPtrPA(), 16)
	dev.MB()

	stage(stHandoff)
	entry := win + (bun.Entry - bun.LinkLoad)
	fmt.Printf("kernflip: %d MB placed at %#x (+%d relocs), %d resident(s), %d NAT flow(s) and %d B of agent state handed over, jumping to %#x — HOPOS_FLIP_JUMP\n",
		bun.FlatSize>>20, win, bun.RelocCount(), len(residents), len(nat.Flows), len(agentState), entry)
	// x0 van de nieuwe kern = wat de firmware óns ooit gaf (DTB-pointer of 0).
	stage(stJumping)
	chainload(entry, firmwareArg())
	return fmt.Errorf("kernflip: chainload keerde terug — dat kan niet")
}

// bundleSwitchHash rekent de switch-code-som over de blobs ín de bundel: de
// tegenhanger van stage2.SwitchCodeHash, maar dan over de kern die we nog niet
// draaien. Zelfde symbolen, zelfde volgorde, zelfde FNV — dus een gelijke som
// betekent letterlijk dezelfde bytes.
//
// De verzameling en haar volgorde komen uit blobSymbols() — dezelfde lijst
// waar stage2 zijn kopie en zijn som mee maakt, zodat een vierde blob er nooit
// maar half bij kan komen. Ontbreken de symbolen, dan is dit een kern van vóór
// de flip-naad en kan er niet met bewoners geflipt worden; dat is precies de
// fout die de aanroeper meldt.
func bundleSwitchHash(f *leanelf.File) (uint64, error) {
	var names []string
	for _, p := range blobSymbols() {
		names = append(names, p[0], p[1])
	}
	syms, err := f.Lookup(names...)
	if err != nil {
		return 0, fmt.Errorf("symbols: %w", err)
	}
	var blobs []byte
	for _, p := range blobSymbols() {
		lo, ok1 := syms[p[0]]
		hi, ok2 := syms[p[1]]
		if !ok1 || !ok2 {
			return 0, fmt.Errorf("symbol %s/%s missing — kernel predates the flip seam", p[0], p[1])
		}
		if hi.Value <= lo.Value || hi.Value-lo.Value > maxBlobSize() {
			return 0, fmt.Errorf("symbol %s..%s spans an impossible %#x", p[0], p[1], hi.Value-lo.Value)
		}
		b := make([]byte, hi.Value-lo.Value)
		if err := f.ReadAtPaddr(b, lo.Value); err != nil {
			return 0, fmt.Errorf("read %s: %w", p[0], err)
		}
		blobs = append(blobs, b...)
	}
	return checksum.FNV64(blobs), nil
}

// copyRange schuift bytes de (device-gemapte) bestemming in, in brokken met
// een yield ertussen: één ononderbroken kopie van een kern-image verhongert
// op één core de netstack en de heartbeat-lezers (zelfde les als coopCleanInv
// in kern/slots).
func copyRange(dst uintptr, src []byte) {
	const chunk = 1 << 20
	for len(src) > 0 {
		n := len(src)
		if n > chunk {
			n = chunk
		}
		dev.Copy(dst, src[:n])
		dst += uintptr(n)
		src = src[n:]
		runtime.Gosched()
	}
}
