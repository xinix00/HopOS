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
// Eigendom blijft bestaan tot beëindiging bevestigd is. Een heartbeat is
// gezondheidsinformatie, geen toestemming om geheugen opnieuw uit te geven.

import (
	"fmt"
	"slices"

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
	ShareGroup string
	GroupCores []int // volledige pool, ook tijdelijk lege cores
	Slot       int
	PartBase   uint64
	PartSize   uint64
	Core       int
	Cores      int
	Job        string      // object-store-naamruimte van de task ("" = geen)
	Ports      []uint16    // gepubliceerde node-poorten (tcp+udp, zoals Start ze zette)
	Mounts     [][2]string // {local, shared} — de volume-tabel van de servicer
}

// SnapshotForFlip beschrijft elke levende bewoner voor het handoff-blob, en
// weigert als er iets bij zit dat deze versie niet kan overdragen. Weigeren is
// de veilige kant: de flip gaat dan gewoon niet door en de node draait door op
// de zittende kern.
//
// Mount-punten gaan mee; hopfs-inhoud blijft zoals bij boot vluchtig.
func SnapshotForFlip() ([]SlotState, error) {
	partOnce.Do(poolInit)
	var out []SlotState
	for i := 1; i <= layout.MaxSlots; i++ {
		base, size, ok := partitionOf(i)
		if !ok {
			continue
		}
		if quarantined[i] || !ctxLive(ctxState(i)) {
			return nil, fmt.Errorf("slot %d: reserved owner is not ready for flip", i)
		}
		// Een SMP-app gaat gewoon mee: zijn secundaire cores draaien dezelfde
		// switch-code (de som-toets in kernflip dekt ze), hun ctx-blokken staan
		// op de slotindexen ná de primaire en blijven staan, en de teller
		// hieronder (Cores) laat de nieuwe kern ze als één eenheid overnemen.
		// De weigering die hier stond ("not adoptable in this version") maakte
		// van elke 2-core-app een stop-vóór-de-flip, en dat is precies wat een
		// live kernelwissel niet mag vragen (06-09).
		n := coreCount(i)
		svcMu.Lock()
		s := servicers[i]
		svcMu.Unlock()
		st := SlotState{
			Slot: i, PartBase: base, PartSize: size,
			Core: coreOf(i), Cores: n,
			Ports: hopswitch.PublishedPorts(i),
		}
		st.ShareGroup, st.GroupCores = snapshotGroup(i)
		if len(st.ShareGroup) > maxFlipJob {
			return nil, fmt.Errorf("slot %d: sharegroup name exceeds %d bytes", i, maxFlipJob)
		}
		if s != nil {
			st.Job, st.Mounts = s.job, s.mounts
		}
		// Weiger vóór de sprong wat de volgende kern niet kan lezen.
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
	return out, ValidateAdoption(out)
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

// AdoptSlots herstelt eerst alle eigendomsclaims, daarna de diensten.
// Aanroepen vóór de agent of andere plaatsing. Een onbruikbare overdracht
// stopt de boot: doorgaan met een gedeeltelijke allocator is nooit veilig.
func AdoptSlots(states []SlotState) int {
	if len(states) == 0 {
		return 0
	}
	defer lifecycleWindow()()
	// De vectoren moeten staan (idempotent): bij een adoptie schrijft
	// InitVectors bewust niets aan de app-core-regio, maar de revoke/chainload-
	// handler van DEZE kern moet er zijn vóór er iets te revoken valt.
	vectorsOnce.Do(cageInit)

	if !cageAdoptable() {
		panic("kernflip: cage layer cannot preserve live owners")
	}
	if err := ValidateAdoption(states); err != nil {
		panic(fmt.Sprintf("kernflip: invalid ownership: %v", err))
	}
	// Geen service of nieuwe plaatsing vóór ALLE oude claims terug zijn.
	for _, st := range states {
		if err := partAdopt(st.Slot, st.PartBase, st.PartSize); err != nil {
			panic(fmt.Sprintf("kernflip: cannot reserve owner: %v", err))
		}
		adoptCage(st)
		hostCore[st.Slot], smpCores[st.Slot] = st.Core, st.Cores
	}
	for _, st := range states {
		i := st.Slot
		appRAM, _ := appRAMSize(st.PartSize) // validated before any service starts
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

		mapTailNormal(i, layout.AbiTailAt(st.PartBase, appRAM))
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
		refreshShared(coreOf(i))
		fmt.Printf("slot %d: adopted — partition %d MB @ %#x on core %d (%d core(s)), %d mount(s), ownership restored\n",
			i, st.PartSize>>20, st.PartBase, coreOf(i), coreCount(i), len(st.Mounts))
	}
	return len(states)
}

// ValidateAdoption checks the complete ownership picture both before the jump
// and before restoring claims. There are few cages: pairwise checks keep this
// boot-only path simple and need no second allocator or synchronization.
func ValidateAdoption(states []SlotState) error {
	for j, s := range states {
		if s.Slot < 1 || s.Slot > layout.MaxSlots || s.PartSize == 0 ||
			s.PartBase > ^uint64(0)-s.PartSize || s.PartBase%part2M != 0 || s.PartSize%part2M != 0 {
			return fmt.Errorf("invalid partition for cage %d", s.Slot)
		}
		if _, err := appRAMSize(s.PartSize); err != nil {
			return err
		}
		if s.Cores < 1 || !isAppCore(s.Core) || s.Cores > layout.NumAppCores()-s.Core+1 ||
			(s.Cores > 1 && s.Core != s.Slot) {
			return fmt.Errorf("invalid core span for cage %d", s.Slot)
		}
		if s.ShareGroup == "" {
			if len(s.GroupCores) != 0 {
				return fmt.Errorf("dedicated cage %d has a group pool", s.Slot)
			}
		} else {
			if len(s.ShareGroup) > maxFlipJob || s.Cores != 1 || !slices.Contains(s.GroupCores, s.Core) {
				return fmt.Errorf("invalid group for cage %d", s.Slot)
			}
			for k, c := range s.GroupCores {
				if !isAppCore(c) || slices.Contains(s.GroupCores[:k], c) {
					return fmt.Errorf("invalid group core %d", c)
				}
			}
		}
		for _, p := range states[:j] {
			if s.Slot == p.Slot || (s.PartBase < p.PartBase+p.PartSize && p.PartBase < s.PartBase+s.PartSize) {
				return fmt.Errorf("overlapping owners %d and %d", s.Slot, p.Slot)
			}
			if (s.Cores > 1 && p.Slot > s.Slot && p.Slot < s.Slot+s.Cores) ||
				(p.Cores > 1 && s.Slot > p.Slot && s.Slot < p.Slot+p.Cores) {
				return fmt.Errorf("overlapping contexts %d and %d", s.Slot, p.Slot)
			}
			if s.ShareGroup != "" && s.ShareGroup == p.ShareGroup {
				if !slices.Equal(s.GroupCores, p.GroupCores) {
					return fmt.Errorf("inconsistent group %q", s.ShareGroup)
				}
				continue
			}
			for c := HopReserved() + 1; c <= layout.NumAppCores(); c++ {
				owns := func(v SlotState) bool { return (c >= v.Core && c < v.Core+v.Cores) || slices.Contains(v.GroupCores, c) }
				if owns(s) && owns(p) {
					return fmt.Errorf("core %d has conflicting owners", c)
				}
			}
		}
	}
	return nil
}

// partAdopt claimt een bestaande partitie voor slot i: hij wordt uit de vrije
// lijst geknipt in plaats van eruit gesneden. Fout als het bereik niet (meer)
// vrij is — dan stopt adoptie de boot vóór plaatsing of services.
func partAdopt(i int, base, size uint64) error {
	partOnce.Do(poolInit)
	partMu.Lock()
	defer partMu.Unlock()
	if i < 1 || i > layout.MaxSlots || size == 0 || base > ^uint64(0)-size || base%part2M != 0 || size%part2M != 0 {
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
