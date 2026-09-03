package slots

// Slot-adoptie voor de kern-flip (docs/kern-flip.md): een nieuwe kern neemt de
// bewoners over die de vorige achterliet, zónder ze aan te raken.
//
// Dat kan omdat een app-wereld volledig in zijn eigen partitie woont: control
// page, hop-ABI-ringen, frame-ringen en ring-koppen liggen in de ABI-staart,
// en zijn kooi-tabellen, ctx-blok en sched-blok in de plan-regio. Geen van
// beide verhuist bij een flip. Wat wél verdwijnt is de BOEKHOUDING van de
// vertrokken kern (welke partitie van wie is, welk slot op welke core woont,
// wie zijn logs draint, welke poorten gepubliceerd zijn) — en dat is precies
// wat hier terugkomt.
//
// De harde regel: liveness wordt GEMETEN, niet aangenomen. Een slot dat zijn
// heartbeat niet laat lopen wordt niet geadopteerd maar opgeruimd — dan
// degradeert de flip voor dat slot naar het bestaande gedrag (task weg,
// monitor herstart hem) in plaats van een spookpartitie te erven.

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/net/hopswitch"
)

// De grenzen van wat het handoff-blob per slot draagt. Ze staan hier zodat
// SnapshotForFlip ze kan afdwingen vóór de sprong; kern/kernflip toetst
// dezelfde waarden bij het teruglezen (import andersom: kernflip kent slots,
// niet omgekeerd — vandaar twee plekken, met deze verwijzing als koppeling).
const (
	maxFlipPorts  = 64
	maxFlipJob    = 256
	maxFlipMounts = 32
	maxFlipPath   = 256
)

// SlotState is alles wat een volgende kern over één levende bewoner moet
// weten. Puur data (geen pointers, vaste maten): kern/kernflip serialiseert
// hem in het handoff-blob.
type SlotState struct {
	Slot     int
	PartBase uint64
	PartSize uint64
	Core     int
	Cores    int
	Job      string      // object-store-naamruimte van de task ("" = geen)
	Ports    []uint16    // gepubliceerde node-poorten (tcp+udp, zoals Start ze zette)
	Mounts   [][2]string // {local, shared} — de volume-tabel van de servicer
}

// SnapshotForFlip beschrijft elke levende bewoner voor het handoff-blob, en
// weigert als er iets bij zit dat deze versie niet kan overdragen. Weigeren is
// de veilige kant: de flip gaat dan gewoon niet door en de node draait door op
// de zittende kern.
//
// v1-grenzen (docs/kern-flip.md):
//   - een SMP-app (cores > 1). De boekhouding erváán zou passen (Cores staat in
//     dit record, de secundaire cores draaien app-code op hun eigen mailbox in
//     de plan-regio, en CtrlSMPTramp wijst sinds de blob-verhuizing naar de
//     plan-kopie) — maar er is nooit een SMP-app dóór een flip gehaald, en dit
//     is geen plek voor een onbewezen aanname.
//
// MOUNTS gaan sinds 01-09 wél mee, en de reden dat ze dat eerst niet deden was
// verkeerd geredeneerd. Klopt: hopfs overleeft de flip niet — maar hij
// overleeft een REBOOT evenmin, want hij is bewust vluchtig (kern/hopfs: "géén
// persistentie … bij boot is alles per definitie leeg", de bron is S3). De
// flip maakt het dus niet erger dan de bestaande update-weg; hij maakt het
// alleen zichtbaar, omdat de app blijft leven terwijl zijn volume leeg wordt.
// Wat een geadopteerde app nodig heeft is dus niet zijn oude inhoud maar zijn
// mount-PUNTEN terug — anders schrijft hij vanaf nu in het niets. Die gaan mee
// (AdoptSlots maakt de dirs opnieuw aan, net als armSlot bij een gewone start).
func SnapshotForFlip() ([]SlotState, error) {
	partOnce.Do(poolInit)
	var out []SlotState
	for i := 1; i <= layout.MaxSlots; i++ {
		base, size, ok := partitionOf(i)
		if !ok {
			continue
		}
		if !ctxLive(ctxState(i)) {
			continue // partitie zonder levende bewoner: laat hem gewoon achter
		}
		n := coreCount(i)
		if n > 1 {
			return nil, fmt.Errorf("slot %d is an SMP app (%d cores) — not adoptable in this version", i, n)
		}
		svcMu.Lock()
		s := servicers[i]
		svcMu.Unlock()
		st := SlotState{
			Slot: i, PartBase: base, PartSize: size,
			Core: coreOf(i), Cores: n,
			Ports: hopswitch.PublishedPorts(i),
		}
		if s != nil {
			st.Job, st.Mounts = s.job, s.mounts
		}
		// Wat de overdracht niet kan dragen, hoort HIER te stranden en niet ná
		// de sprong: een blob dat de nieuwe kern weigert wordt in zijn geheel
		// weggegooid, en dán staat de adoptie uit terwijl er wél bewoners
		// draaien — de verse-boot-paden zouden hun plan-regio vegen. De grenzen
		// spiegelen die van decodeHandoff (kern/kernflip).
		if len(st.Ports) > maxFlipPorts || len(st.Job) > maxFlipJob || len(st.Mounts) > maxFlipMounts {
			return nil, fmt.Errorf("slot %d: %d published port(s) / %d-byte job name / %d mount(s) exceeds what the handoff blob carries (%d/%d/%d)",
				i, len(st.Ports), len(st.Job), len(st.Mounts), maxFlipPorts, maxFlipJob, maxFlipMounts)
		}
		for _, m := range st.Mounts {
			if len(m[0]) > maxFlipPath || len(m[1]) > maxFlipPath {
				return nil, fmt.Errorf("slot %d: mount path longer than %d bytes", i, maxFlipPath)
			}
		}
		out = append(out, st)
	}
	return out, nil
}

// SetFlipCapable meldt of deze node zichzelf later mag vervangen (de
// platform-config, `hopos.flip.enable`). Aanroepen ZEER VROEG in de boot:
// vóór de eerste EnsureVectors of slot-start, want de beslissing bepaalt waar
// een app-core zijn instructies vandaan haalt en dat moet vaststaan vóór de
// eerste dispatch.
//
// Uit = de switch-code blijft in het kern-image en deze node gedraagt zich
// byte-voor-byte als vóór de kern-flip bestond. Een flip is dan onmogelijk, en
// dat weigert hij ook netjes: zonder kopie geeft de som nul en stopt Flip
// zodra er bewoners leven.
func SetFlipCapable(v bool) { cageSetFlipCapable(v) }

// AdoptSlots neemt de bewoners uit het handoff-blob over. Geeft terug hoeveel
// er daadwerkelijk leefden; de rest is opgeruimd (partitie terug de pool in).
//
// Aanroepen ná hopswitch.Up() en ná UseFS/UseStore — de servicer die hier
// start bedient meteen weer RPC's — en vóór de agent zijn eerste plaatsing
// doet.
func AdoptSlots(states []SlotState) int {
	if len(states) == 0 {
		return 0
	}
	defer lifecycleWindow()()
	// De vectoren moeten staan (idempotent): bij een adoptie schrijft
	// InitVectors bewust niets aan de app-core-regio, maar de revoke/chainload-
	// handler van DEZE kern moet er zijn vóór er iets te revoken valt.
	vectorsOnce.Do(cageInit)

	// Hield de arch-laag de adoptie-stand vast? cageInit hierboven verifieert
	// dat de zittende switch-code écht de onze is; blijkt dat niet zo, dan
	// heeft hij de plan-regio inmiddels vers neergezet en zijn de bewoners
	// hoe dan ook weg. Dan is overnemen liegen: hun partities zouden bezet
	// blijven voor apps die niet meer draaien.
	if !cageAdoptable() {
		fmt.Printf("HOPOS_FLIP_ADOPT_ABORT: the cage layer could not preserve the residents — releasing %d partition(s) instead\n", len(states))
		return 0
	}

	live := 0
	for _, st := range states {
		if st.Slot < 1 || st.Slot > layout.MaxSlots || st.PartSize == 0 {
			continue
		}
		i := st.Slot
		// De partitie eerst uit de pool knippen: vanaf dit moment kan geen
		// plaatsing hem meer uitdelen, ook niet als de liveness-meting hieronder
		// nog loopt.
		if err := partAdopt(i, st.PartBase, st.PartSize); err != nil {
			fmt.Printf("HOPOS_FLIP_ADOPT_FAIL slot %d: %v\n", i, err)
			continue
		}
		// De core-grens is het aantal FYSIEKE app-cores, niet de slot-capaciteit:
		// hostCore is op MaxSlots gedimensioneerd, maar een corenummer daarboven
		// zou de rotatie op een sched-blok laten wijzen dat bij geen enkele core
		// hoort.
		if st.Core >= 1 && st.Core <= layout.NumAppCores() {
			hostCore[i] = st.Core
		}
		smpCores[i] = 1

		// LEEFT hij ook echt? De ctx-staat zegt "de rotatie kent hem", maar
		// alleen een OPLOPENDE heartbeat bewijst dat er nog een app in draait —
		// en dat is precies het verschil tussen een bewoner overnemen en een
		// spookpartitie erven. De app schrijft hem elke ~50ms (applib), dus dit
		// venster is ruim; de flip zelf duurde langer dan dit.
		if !adoptLives(i) {
			fmt.Printf("slot %d: no heartbeat after the flip — releasing it instead of adopting HOPOS_FLIP_ADOPT_DEAD\n", i)
			releaseSlot(i, true)
			continue
		}

		// De ringen blijven zoals ze zijn: hun koppen staan in de partitie en de
		// app is er middenin bezig. Alleen de LEZERS komen terug — de servicer
		// (ring.Open, geen Init) en de switch-poort.
		appRAM, err := appRAMSize(st.PartSize)
		if err != nil {
			fmt.Printf("HOPOS_FLIP_ADOPT_FAIL slot %d: %v\n", i, err)
			releaseSlot(i, true)
			continue
		}
		// De mount-punten terug. hopfs is vluchtig (zie de kop hierboven), dus de
		// INHOUD is weg — maar de app leeft door en moet wél weer ergens kunnen
		// lezen en schrijven. Zelfde stap als armSlot bij een gewone start: de
		// eigen root en de shared dirs bestaan, leeg. Een fout hier kost de
		// mounts van dit slot, niet de app.
		if fsys != nil {
			root := fmt.Sprintf("/.tasks/slot%d", i)
			if err := fsys.MkdirAll(root); err != nil {
				fmt.Printf("slot %d: adopt root: %v\n", i, err)
			}
			for _, m := range st.Mounts {
				if err := fsys.MkdirAll(m[1]); err != nil {
					fmt.Printf("slot %d: adopt mount %q: %v\n", i, m[1], err)
				}
			}
		} else if len(st.Mounts) > 0 {
			fmt.Printf("slot %d: %d mount(s) handed over but this kernel has no storage layer — the app will get errors\n", i, len(st.Mounts))
		}

		hopswitch.Attach(i, layout.NetRingBaseAt(st.PartBase, appRAM))
		for _, p := range st.Ports {
			// Zelfde paar als Start publiceert; een fout hier kost de poort,
			// niet de app.
			if err := hopswitch.Publish("tcp", p, i, p); err != nil {
				fmt.Printf("slot %d: re-publish tcp/%d: %v\n", i, p, err)
			}
			if err := hopswitch.Publish("udp", p, i, p); err != nil {
				fmt.Printf("slot %d: re-publish udp/%d: %v\n", i, p, err)
			}
		}
		go registerServicer(i, fmt.Sprintf("/.tasks/slot%d", i), st.Job, st.Mounts).run()
		// De core-boekhouding van de plaatser terug (pool.go): zonder dit ziet
		// PlaceCage élke core als vrij en zet hij de volgende job ongevraagd
		// naast een geadopteerde bewoner — precies de stille core-deling die het
		// ontwerp verbiedt (timing-zijkanalen; delen hoort een keuze te zijn).
		adoptCage(i, coreOf(i))
		refreshShared(coreOf(i))
		live++
		fmt.Printf("slot %d: adopted — partition %d MB @ %#x on core %d, %d mount(s), heartbeat running\n",
			i, st.PartSize>>20, st.PartBase, coreOf(i), len(st.Mounts))
	}
	return live
}

// adoptLives meet of er in slot i nog een app draait: de heartbeat moet binnen
// een halve seconde oplopen (applib tikt elke ~50ms).
func adoptLives(i int) bool {
	if !ctxLive(ctxState(i)) {
		return false
	}
	hb := ctrlRead(i, layout.CtrlHeartbeat)
	return pollUntil(500*time.Millisecond, func() bool {
		return ctrlRead(i, layout.CtrlHeartbeat) != hb
	})
}

// partAdopt claimt een bestaande partitie voor slot i: hij wordt uit de vrije
// lijst geknipt in plaats van eruit gesneden. Fout als het bereik niet (meer)
// vrij is — dan klopt het blob niet bij deze pool en is niet-adopteren het
// enige veilige antwoord.
func partAdopt(i int, base, size uint64) error {
	partOnce.Do(poolInit)
	partMu.Lock()
	defer partMu.Unlock()
	if i < 1 || i > layout.MaxSlots {
		return fmt.Errorf("slot %d buiten bereik", i)
	}
	if partOf[i].size != 0 {
		return fmt.Errorf("slot %d heeft al een partitie", i)
	}
	// HELEMAAL vrij, of niets: een deel-claim zou betekenen dat een stuk van
	// deze partitie al aan iemand anders toebehoort, en dan is het blob niet
	// van deze pool. Weigeren is dan het enige veilige antwoord.
	if !freeSpan(base, base+size) {
		return fmt.Errorf("partitie %#x+%d MB ligt niet vrij in de pool van deze kern", base, size>>20)
	}
	takeRange(base, base+size)
	partOf[i] = region{base, size}
	return nil
}

// Het venster van de VÓRIGE kern hoeft nergens teruggegeven te worden, en dat
// is geen omissie maar de kern van het leen-model (docs/kern-flip.md).
//
// Hier stond een AdoptReleaseWindow die het expliciet in de vrije lijst
// stopte, en dat was een geheugen-dubbeluitgifte: poolInit bouwt de pool uit
// het board-plan en knipt daar alleen het ÉIGEN venster uit (ownRegion). Het
// venster van de vorige kern zit dus al vrij in die pool — nog een keer
// invoegen levert twee overlappende regio's op, en dan krijgen twee slots
// dezelfde partitie. GEMETEN 31-08 op de dubbele-flip-regressie: slot 6 en 7
// lazen elkaars frame-ringen (head=0x205090402020201, een stuk Ethernet-frame
// waar een ringkop hoorde te staan) en de swarm viel om.
//
// Teruggeven ís dus impliciet: elke kern claimt bij boot precies zijn eigen
// venster en laat de rest van de pool met rust — of dat venster nu van het
// board komt of uit een lening. De enige expliciete teruggave die bestaat is
// ReturnKernWindow (partmem.go), en die hoort bij een MISLUKTE flip: dan is de
// lening van deze kern zelf, en die moet terug omdat er nooit iemand in ging
// wonen.
