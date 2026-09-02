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
	mu    sync.Mutex
	ctrl  Controller
	lines = map[int]*line{}
	fired atomic.Uint64 // geclaimde interrupts, totaal — het bewijs dat ze aankomen
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
	for {
		l, ok := ctrl.Claim()
		if !ok {
			return
		}
		fired.Add(1)
		mu.Lock()
		ln := lines[l.ID]
		mu.Unlock()
		if ln != nil {
			if ln.Ack != nil {
				ln.Ack()
			}
			select {
			case ln.fired <- struct{}{}:
			default: // al gewekt en nog niet opgehaald: één is genoeg
			}
		}
		ctrl.Complete(l)
	}
}

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
