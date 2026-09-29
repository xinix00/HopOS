// Package irq is het interrupt-contract van HopOS: een lijn, een controller,
// en één werkwoord — Wait. Het is de doorbell (cpu/idle/rxdoor.go), maar dan
// voor HOP's eigen core: waar een app door de switcher gewekt wordt als zijn
// ring groeit, wordt HOP hier door het silicium gewekt als de NIC iets heeft.
//
// Tot 02-09 had HopOS géén interrupt-afhandeling: DAIF gemaskeerd, alles
// gepold, en HOP's RX-lus sliep 300µs per ronde — ruim 3.000 wekmomenten per
// seconde op een node die niets doet, en 300µs latency op élk app-pakket
// (alle app-verkeer gaat door HOP's switch). Precies het getal dat de doorbell
// voor apps al sloopte (3.113 → 21 wekken/s).
//
// Het model is dat van tamago zelf: de IRQ-vector (EL1 op arm64, S/M-mode op
// riscv64) meldt een signaal, ServiceInterrupts wekt daarop een goroutine en
// die roept hier dispatch aan. Dat is de enige plek waar een interrupt Go
// wordt — een gewone goroutine, geen Go op exception-niveau. Dispatch vraagt
// de controller wélke lijn vuurde, ackt het device (Line.Ack), wekt wie erop
// wacht en completeert de lijn. Wat er dan gebeurt is aan de wachter: de
// RX-lus pompt zijn ring leeg en wacht opnieuw.
//
// Twee regels, beide isolatie:
//
//   - Interrupts zijn uitsluitend HOP-werk. Een app-core wordt nooit een
//     target van de controller en houdt zijn maskers dicht; een app heeft
//     geen lijn, geen controller en geen Wait. De controller routeert élke
//     lijn expliciet naar HOP's core.
//   - Een verloren flank mag nooit een hang worden: Wait heeft een maximum,
//     en de wachter behandelt een time-out als "kijk toch maar" — dezelfde
//     huisregel als in cpu/idle (wakeAt: liever pollen dan hangen).
//
// Dit pakket importeert geen architectuur: de board-bedrading geeft de
// servicer van zijn tamago-arch mee (arm64.ServiceInterrupts,
// RV64.ServiceInterrupts) en een Controller (driver/gicv3, straks de Apple
// AIC, een PLIC). Boards zonder interrupt-bedrading blijven pollen; hopnet
// kiest op board.NICInterrupter.
package irq

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Line is één interruptlijn zoals de controller hem nummert (GICv3: INTID,
// SPI n = 32+n; AIC: het hw-nummer; PLIC: de source-id).
type Line struct {
	ID int
	// Ack is de device-kant van de bevestiging: wat het device nodig heeft om
	// zijn lijn weer los te laten (virtio: InterruptACK), gedaan in dispatch
	// vóór de wachter gewekt wordt. nil = het device heeft geen ack (een
	// edge-lijn, of een controller die het zelf doet).
	Ack func()
}

// Controller is wat een interrupt-controller moet kunnen. Vier werkwoorden,
// en niets over prioriteiten of groepen: die zijn van de driver.
type Controller interface {
	// Enable maakt de lijn scherp én routeert hem naar de aanroepende core.
	Enable(l Line) error
	Disable(l Line)
	// Claim geeft de lijn die vuurde (en bevestigt hem bij de controller);
	// ok=false = niets (meer) te claimen.
	Claim() (Line, bool)
	// Complete sluit de afhandeling van l af (EOI, waar dat los van Claim is).
	Complete(l Line)
}

var (
	mu     sync.Mutex
	ctrl   Controller
	lines  = map[int]*line{}
	fired  atomic.Uint64 // geclaimde interrupts, totaal — het bewijs dat ze aankomen
	passes atomic.Uint64 // isr-rondes

)

// Fired geeft hoeveel interrupts er tot nu toe geclaimd zijn (alle lijnen).
// Zonder dit getal is "de RX-lus wordt wakker" niet te onderscheiden van "de
// vangrail van Wait liep af": beide lopen. Dít zegt of het silicium sprak.
func Fired() uint64 { return fired.Load() }

// line is een geregistreerde lijn met zijn wek-kanaal: gebufferd op 1, zodat
// een interrupt die vuurt terwijl niemand wacht de eerstvolgende Wait meteen
// laat terugkeren — level-triggered gedrag, geen verloren wek.
type line struct {
	Line
	fired chan struct{}
}

// ErrNoController: er is (nog) geen controller geregistreerd.
var ErrNoController = errors.New("irq: no interrupt controller on this board")

// Use registreert de controller en start de bediening. service is de
// ServiceInterrupts van de tamago-arch van dit board: hij blokkeert de
// goroutine tot de IRQ-vector een signaal meldt en roept dan isr aan. Eén
// keer, vanuit de board-bedrading, ná de controller-init en vóór Enable.
func Use(c Controller, service func(isr func())) {
	mu.Lock()
	ctrl = c
	mu.Unlock()
	go service(dispatch)
}

// Ready meldt of er een controller is.
func Ready() bool {
	mu.Lock()
	defer mu.Unlock()
	return ctrl != nil
}

// Enable registreert l als wek-doel en maakt hem scherp bij de controller.
func Enable(l Line) error {
	mu.Lock()
	defer mu.Unlock()
	if ctrl == nil {
		return ErrNoController
	}
	lines[l.ID] = &line{Line: l, fired: make(chan struct{}, 1)}
	return ctrl.Enable(l)
}

// dispatch is de isr: alle gevuurde lijnen claimen, per lijn het device
// acken, de wachter wekken en de lijn completeren. Draait als gewone
// goroutine (tamago's ServiceInterrupts), dus mag alles wat Go mag.
func dispatch() {
	// Eén isr-ronde claimt tot de controller niets meer heeft. Een lijn die
	// binnen één ronde blijft terugkomen is een level-bron die niemand laat
	// zakken: een lijn die de firmware aan liet staan (UEFI-timer, UART, een
	// watchdog-waarschuwing) en die wij niet kennen, of een device waarvan de
	// ack de lijn niet laat vallen. Zonder grens spint deze goroutine dan
	// voor eeuwig in Claim→EOI→Claim met I gemaskeerd — de O6N-"freeze bij de
	// eerste wachttijd" (17-09/18-09): geen pets meer, watchdog na 12 s. Dus:
	// onbekend = meteen uit, bekend maar blijvend = na strayLimit uit, en
	// beide één regel. Wait valt dan terug op zijn maximum: pollen, geen hang.
	seen := map[int]int{}
	if n := passes.Add(1); n <= 5 {
		defer fmt.Printf("irq: isr pass #%d done\n", n) // de eerste vijf rondes, als bewijs dat de dispatcher terugkeert
	}
	for {
		l, ok := ctrl.Claim()
		if !ok {
			return
		}
		if n := fired.Add(1); n <= 3 {
			fmt.Printf("irq: claim #%d is INTID %d\n", n, l.ID) // de eerste drie, als bewijs dat het pad leeft
		}
		mu.Lock()
		ln := lines[l.ID]
		mu.Unlock()
		seen[l.ID]++
		if ln == nil {
			ctrl.Disable(l)
			ctrl.Complete(l)
			fmt.Printf("irq: INTID %d fired but nobody serves it (left enabled by the firmware?) — line disabled\n", l.ID)
			continue
		}
		if ln.Ack != nil {
			ln.Ack()
		}
		select {
		case ln.fired <- struct{}{}:
		default: // al gewekt en nog niet opgehaald: één is genoeg
		}
		ctrl.Complete(l)
		if seen[l.ID] > strayLimit {
			// Een BEDIENDE lijn die binnen één ronde blijft terugkomen is
			// geen vastzitter maar een NIC onder last (tg3: een status-
			// update per frame, de mailbox-ack en de NOW-hertrigger houden
			// de lijn bij 25k frames/s praktisch continu hoog). Die lijn
			// voorgoed uitzetten was de M4-dood van vanmiddag: ná een pull
			// 0 interrupts/s, elke frame op de failsafe van 10 ms, 118 → 45
			// MB/s, en de O6N kreeg de schuld (bundels 47-55, 20-09). Dus:
			// de ronde afbreken (de pomp en de wachter komen aan de beurt,
			// de device-ack houdt de lijn intussen laag), niets uitzetten,
			// één regel per pass. Onbekende lijnen gaan hierboven nog wél uit.
			if n := strayPasses.Add(1); n <= 3 {
				fmt.Printf("irq: INTID %d came back %d times in one pass — pass ended, line stays enabled\n", l.ID, strayLimit)
			}
			return
		}
	}
}

// strayPasses telt de afgebroken isr-rondes (de eerste drie melden zich).
var strayPasses atomic.Uint64

// strayLimit: hoe vaak één lijn binnen één isr-ronde mag terugkomen voordat
// hij als vastzittend geldt. Ruim boven wat een NIC-burst legitiem doet
// (een ack per claim laat de lijn zakken), ver onder "voor eeuwig".
const strayLimit = 256

// Wait blokkeert de aanroeper tot lijn l vuurde, of tot max verstreken is
// (true = gevuurd). Een lijn die niet geregistreerd is wacht gewoon max —
// dat is de poll-terugval, niet een fout.
func Wait(l Line, max time.Duration) bool {
	mu.Lock()
	ln := lines[l.ID]
	mu.Unlock()
	if ln == nil {
		time.Sleep(max)
		return false
	}
	t := time.NewTimer(max)
	defer t.Stop()
	select {
	case <-ln.fired:
		return true
	case <-t.C:
		return false
	}
}
