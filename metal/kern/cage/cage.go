// Package cage is de isolatie-invariant van HopOS op RISC-V: de kooi waarin
// een app-slot draait. Het is de tegenhanger van kern/stage2 (ARM), maar de
// mechaniek is fundamenteel anders en dat is silicium-eigen, geen keuze:
//
// De XuanTie C906 heeft géén H-extensie, dus het stage-2-equivalent bestaat
// niet. In plaats daarvan is de kooi een **PMP-whitelist**, geprogrammeerd in
// M-mode:
//
//   - PMP matcht op laagste index eerst, dus de allow-vensters staan vóór de
//     afsluitende deny-all.
//
//   - **Eén adresseringsmodus: TOR.** Een PMP-entry kan NAPOT (macht van twee, op
//     eigen maat uitgelijnd) of TOR (losse onder- en bovengrens, twee entries).
//     Alleen TOR, want NAPOT kan geen willekeurige partitie uitdrukken: een job
//     van 124MB werd 128MB en die past op dit board nergens uitgelijnd. Twee
//     modussen naast elkaar zou betekenen dat de maat van een partitie bepaalt
//     hoe zijn kooi gecodeerd is — één vorm is er één om te snappen en één om te
//     testen. De prijs is twee entries per venster; er zijn er acht.
//
//   - De entries zijn NIET gelockt, en dat is bewust. PMP bindt S-mode en
//     U-mode altijd; de L-bit bindt daarbovenop óók M-mode. Zolang een app in
//     M-mode draaide was locken het énige dat hem binnenhield — nu hij in
//     supervisor-modus draait doet de whitelist dat al, en zou locken alleen
//     HOP zélf vastzetten.
//
//     En dát is geen detail: een gelockte deny-all sluit óók HOP's eigen
//     M-mode-code buiten, dus zou de switcher die twee bewoners op één hart
//     afwisselt niet buiten hun partities kunnen wonen. Binnen een partitie kan
//     app A hem overschrijven en zo app B overnemen. Ongelockt is de kooi dus
//     niet zwakker maar sterker: de invariant zit in de privilege-grens (de app
//     kan PMP vanuit S-mode niet eens aanraken) en HOP houdt de vrijheid die hij
//     nodig heeft om de kooi te wisselen.
//
//   - Wat blijft is de tweede helft van de invariant: **terúglezen vóór de
//     sprong** (Verify). Een kooi die niet aantoonbaar staat, wordt niet
//     gedispatcht.
//
// Op silicium bewezen (LicheeRV Nano, 30-07, toen de app nog in M-mode draaide
// en de entries gelockt waren): de verboden store trapt met mcause 7, de
// unlock-poging wordt door WARL genegeerd, en na een hart-reset leest pmpcfg0
// weer 0. Dat de kooi hóudt is daarmee gemeten; dat locken daarvoor niet meer
// nodig is, volgt uit de privilege-grens. Twee dingen die de
// meting kostte en die hier vastgelegd horen:
//
//   - **TOR is op dit silicium nog niet gemeten.** De eerste "TOR matcht
//     niet"-conclusie leunde op een meetfout (niet-gedrainde writes), dus er is
//     nooit een geldige meting geweest — niet tegen, en niet vóór. Het faalpad is
//     wel veilig: matcht een TOR-entry niet, dan valt de toegang in de deny-all
//     en faultt de app meteen. Fail-closed, en zichtbaar in het fault-rapport.
//   - **De stub is de plek waar dit gebeurt, niet HOP.** HOP legt het plan
//     vast (Plan), de loader-stub op het app-hart programmeert en verifieert
//     de kooi vóórdat hij de app binnenspringt. HOP kan dat niet zelf doen:
//     PMP is per hart.
//
// De ARM/RISC-V-correspondentie die hierboven wordt aangeraakt (privilege-mode,
// begrenzen, verplaatsen, kill) staat VOLLEDIG en LEIDEND in
// docs/technical/isolation.md. Wijzigt daar iets, dan die tabel eerst.
//
// Wat dit pakket over het silicium aanneemt staat NIET hier maar in het
// CPU-profiel cpu/thead: entry-aantal, adresbreedte, korrel en de uitgebreide
// PTE-attributen. Dit bestand is de codering, en die is RISC-V-spec.
package cage

import (
	"fmt"

	"hop-os/metal/cpu/thead"
)

// Window is één toegestaan venster in de kooi: basis + maat, met rechten.
// De maat moet een macht van twee zijn en ≥ 4KB (PMP-granulariteit op de
// C906), en de basis moet op zijn eigen maat gealigneerd zijn — dat is wat
// NAPOT kan coderen.
type Window struct {
	Base    uint64
	Size    uint64
	R, W, X bool
}

// Plan is het volledige kooi-plan voor één slot: de toegestane vensters, in
// prioriteitsorde (laagste index wint), afgesloten met een impliciete
// deny-all. HOP stelt dit op; de stub programmeert het.
//
// Entry-budget (de C906 heeft 8 PMP-entries, mogelijk 16 — niet gemeten):
//
//	app-partitie   1
//	control page   1
//	granted MMIO   0..n  (alleen wat de app krijgt; géén CLINT — zie hieronder)
//	deny-all       1
//
// Waarom de app géén CLINT-venster krijgt: mtimecmp en msip van beide harts
// liggen 8 bytes uit elkaar op één 4K-page. Met PMP zijn die niet te scheiden,
// dus een CLINT-venster is een DoS-kanaal op HOP's eigen timer/IPI. Het kan
// ook gemist worden: tamago's runtime schrijft nergens mtimecmp (coöperatieve
// scheduler) en de tijd komt uit de TIME CSR (rdtime), die geen venster nodig
// heeft.
type Plan struct {
	Allow []Window
}

// PMP-veldwaarden (RISC-V privileged spec, pmpcfg-byte).
const (
	cfgR = 1 << 0
	cfgW = 1 << 1
	cfgX = 1 << 2

	// De adresseringsmodus in het A-veld. Twee vormen doen mee:
	//
	//	NAPOT  één entry, maar alleen een macht van twee op zijn eigen maat
	//	TOR    twee entries (onder- en bovengrens), élk bereik uitdrukbaar
	//
	// OFF is geen modus maar het uitzetten van een entry: de ondergrens van een
	// TOR-paar is een pmpaddr zónder eigen match, want TOR van entry k leest zijn
	// begin uit pmpaddr[k-1]. Zo'n entry mag dus niets toestaan.
	cfgAOff = 0 << 3
	cfgATOR = 1 << 3

	// Hoeveel entries er zijn, hoe breed het fysieke adres is en welke korrel PMP
	// aanhoudt zijn ALLE DRIE implementation-defined — dus komen ze uit het
	// CPU-profiel (cpu/thead) en staan ze niet hier. Wat in dit bestand overblijft
	// is de codering zelf, en die is spec.
	MaxEntries = thead.PMPEntries
	paBits     = thead.PABits

	// torGrain is de uitlijning die we van een TOR-grens eisen. De PMP-korrel van
	// de C906 is 4KB, en bij een korrel > 4 bytes leest het silicium de onderste
	// bits van pmpaddr als nul — een grens die daar niet op valt dekt dus stil een
	// ánder bereik dan bedoeld. Dat is precies de klasse fout die een kooi lek
	// maakt, dus eisen we het in plaats van erop te hopen.
	torGrain = 0x1000
)

// Encode zet een plan om in de pmpaddr-waarden en de pmpcfg0-woord die de stub
// moet schrijven — plus exact hetzelfde pmpcfg0 dat hij ná het schrijven moet
// terúglezen (Verify's referentie). Dat HOP dit uitrekent en de stub het alleen
// nog wegschrijft is bewust: de rekenkunde staat in Go, testbaar op de host, en
// de stub blijft een handvol instructies zonder beslissingen.
func Encode(p Plan) (addr []uint64, cfg uint64, err error) {
	var bytes []uint64
	for i, w := range p.Allow {
		if e := torOK(w); e != nil {
			return nil, 0, fmt.Errorf("cage: window %d: %w", i, e)
		}
		addr = append(addr, w.Base>>2, (w.Base+w.Size)>>2)
		bytes = append(bytes, cfgAOff, cfgATOR|perm(w))
	}
	// De deny-all: één NAPOT-venster over de hele PA-ruimte zonder rechten.
	// Alles wat niet in een eerder venster viel, valt hier. Ook ongelockt: een
	// entry zónder rechten is voor S-mode een harde weigering, en dat is de laag
	// waar de app zit. HOP's eigen M-mode blijft er vrij van — hij moet de kooi
	// kunnen wisselen.
	// Ook de deny-all is TOR: ondergrens nul, bovengrens de hele PA-ruimte.
	addr = append(addr, 0, uint64(1<<paBits)>>2)
	bytes = append(bytes, cfgAOff, cfgATOR)

	if len(addr) > MaxEntries {
		return nil, 0, fmt.Errorf("cage: %d windows need %d PMP entries (incl. deny-all), there are %d",
			len(p.Allow), len(addr), MaxEntries)
	}
	for k, b := range bytes {
		cfg |= b << (8 * k)
	}
	return addr, cfg, nil
}

// perm zet de rechten van een venster om in de RWX-bits van een pmpcfg-byte.
func perm(w Window) uint64 {
	var b uint64
	if w.R {
		b |= cfgR
	}
	if w.W {
		b |= cfgW
	}
	if w.X {
		b |= cfgX
	}
	return b
}

// torOK toetst wat TOR van een venster eist: een niet-leeg bereik met beide
// grenzen op de PMP-korrel. Géén macht van twee en géén natuurlijke uitlijning —
// dát is precies waarvoor TOR bestaat.
func torOK(w Window) error {
	if w.Size == 0 {
		return fmt.Errorf("size 0")
	}
	if w.Base%torGrain != 0 || w.Size%torGrain != 0 {
		return fmt.Errorf("TOR bounds %#x..%#x are not on the %dKB grain",
			w.Base, w.Base+w.Size, torGrain>>10)
	}
	return nil
}
