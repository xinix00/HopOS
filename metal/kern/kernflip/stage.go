package kernflip

// De vluchtrecorder. Een flip eindigt in een sprong waarna de vorige kern niet
// meer bestaat; gaat er iets mis, dan reset de watchdog de node en is élke
// diagnose weg. De console helpt daar principieel niet bij — de node serveert
// hem zelf, dus de laatste regels vóór een crash halen de lezer nooit en de
// reboot wist de ring. Eén woord in DRAM overleeft dat wél: elke stap schrijft
// waar hij is, en de VOLGENDE boot vertelt hoe ver de vorige kwam.
//
// Het woord staat op de boot-scratch (layout.FlipStageOff), buiten elke
// RAM-declaratie en dus van geen enkele kern eigendom. Het draagt een tag in
// de bovenste helft zodat willekeurige DRAM-rommel na een koude start nooit
// als "vorige flip" gelezen wordt.

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/dev"
)

// stageTag ("FLIP" in de bovenste 32 bits) scheidt een echte stap van rommel.
const stageTag = uint64(0x464C4950) << 32

// De stappen, in de volgorde waarin Flip ze doorloopt. Namen zijn wat de
// console zou zeggen; de recorder heeft alleen het nummer nodig.
const (
	stFetched = iota + 1
	stWindowHeld
	stBundleOK
	stBorrowed
	stVectors
	stScrubbed
	stPlaced
	stRebased
	stCaptured
	stHandoff
	stJumping
	// Vanaf hier schrijft de NIEUWE kern (alleen op een flip-boot): de sprong
	// is dan gelukt en de vraag wordt "hoe ver komt zijn boot".
	stLanded
	stNetUp
)

var stageNames = [...]string{
	stFetched:    "bundle fetched and verified",
	stWindowHeld: "slot lifecycle window held",
	stBundleOK:   "bundle validated",
	stBorrowed:   "window borrowed from the pool",
	stVectors:    "vectors installed",
	stScrubbed:   "window scrubbed",
	stPlaced:     "segments placed",
	stRebased:    "relocations applied",
	stCaptured:   "residents, NAT and agent state captured",
	stHandoff:    "handoff blob written",
	stJumping:    "about to jump into the new kernel",
	stLanded:     "jump landed, handoff consumed — died during the new kernel's boot (board/net bring-up on live hardware?)",
	stNetUp:      "new kernel's network up — died between net and agent",
}

// stage legt vast hoe ver we zijn. Rechtstreeks naar DRAM geveegd: dit woord
// moet een crash één instructie later nog overleven.
func stage(n int) {
	pa := layout.FlipStagePA()
	if pa == 0 {
		return
	}
	dev.Write64(pa, stageTag|uint64(n))
	dev.CleanInv(pa, 8)
	dev.MB()
}

// stageClear wist de recorder — na een geslaagde adoptie, en bij het lezen.
func stageClear() {
	pa := layout.FlipStagePA()
	if pa == 0 {
		return
	}
	dev.Write64(pa, 0)
	dev.CleanInv(pa, 8)
	dev.MB()
}

// MarkNetUp: de geflipte kern heeft zijn netwerk op — de recorder schuift
// mee, zodat een dood tússen net en agent zich onderscheidt van een dood in
// de bring-up. Alleen op een flip-boot aanroepen.
func MarkNetUp() { stage(stNetUp) }

// BootLanded wist de recorder: de geflipte kern heeft zijn agent draaiend en
// daarmee is de flip pas ECHT geland. Niet eerder — een kern die de handoff
// al gelezen had maar daarna in zijn boot stierf, hoorde een spoor achter te
// laten (de eerste ijzer-flips leken juist daardoor spoorloos te mislukken).
func BootLanded() { stageClear() }

// ReportLastFlip meldt bij de boot hoe ver een eerdere flip kwam, en wist de
// recorder. Stilte = er is geen flip geweest, of de vorige is netjes geland
// (die wist hem zelf, pas bij een dráaiende agent). NIET aanroepen op een
// flip-boot: daar staat net stLanded in, en dat is geen mislukking maar de
// normale gang. Vroeg in main aanroepen, vóór er iets nieuws gebeurt.
func ReportLastFlip() {
	pa := layout.FlipStagePA()
	if pa == 0 {
		return
	}
	v := dev.Read64(pa)
	if v&^0xFFFFFFFF != stageTag {
		return
	}
	n := int(v & 0xFFFFFFFF)
	stageClear()
	what := "unknown stage"
	if n >= 0 && n < len(stageNames) && stageNames[n] != "" {
		what = stageNames[n]
	}
	// "did not complete its handover" en niet "did not land": bij de laatste
	// stap ís er gesprongen, alleen kwam de nieuwe kern nooit tot zijn
	// adoptie. Het stapnummer is de waarheid; de tekst mag die niet inkleuren.
	fmt.Printf("kernflip: WARNING — the previous flip did not complete its handover; it got as far as step %d/%d (%s) HOPOS_FLIP_STALLED\n",
		n, stJumping, what)
}
