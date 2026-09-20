//go:build tamago && arm64

package idle

import (
	"runtime"
	"sync/atomic"
	"unsafe"
)

// De IRQ-deur: een interrupt is een wek-signaal, geen plek om runtime-code
// te draaien. tamago's handleInterrupt roept os/signal.Relay aan ín de
// exception-context, op de stack van wat er onderbroken werd — op de M4 gaf
// dat de findTimer-crash (toolchain-patch 0003) en op de Ampere een stille
// hang binnen seconden na de eerste NIC-interrupts (19-09, L83: zes flips
// op rij, met de watchdog uit en de UART eraan: geen exception, geen regel,
// gewoon weg). Dus hier het HopOS-contract: de vector (irqvec_arm64.s) zet
// alléén irqFlag en keert terug met I gemaskeerd; de governor ziet de vlag
// in zijn ronde — dezelfde poort als rxDoor/workDoor, alleen vanuit de
// scheduler-idle — en wekt de ISR-goroutine, die claimt, ackt en I weer
// opent. Is HOP niet idle, dan blijft de lijn staan tot de volgende
// governor-ronde; de pomp pollt ondertussen op zijn eigen vangrail.

// irqFlag: gezet door hopIRQVector; gelezen (en gewist) door irqDoor.
var irqFlag uint32

var irqWake atomic.Pointer[func()]

// IRQWoken telt de wekken via de IRQ-deur.
var IRQWoken atomic.Uint64

func hopIRQVector()
func syncVector(addr uintptr)

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint

func irqPending() bool { return atomic.LoadUint32(&irqFlag) != 0 }

// irqDoor: de vlag staat → de ISR-goroutine wekken (alleen vanuit de
// scheduler-idle, zie workDoor). Level: de lijn staat nog, dus een ronde
// die niet mag wekken komt hier gewoon terug.
func irqDoor() bool {
	if atomic.LoadUint32(&irqFlag) == 0 {
		return false
	}
	if !runtime.IdleMayReady() {
		return false
	}
	w := irqWake.Load()
	if w == nil {
		return false
	}
	atomic.StoreUint32(&irqFlag, 0)
	(*w)()
	IRQWoken.Add(1)
	return true
}

// installIRQVector hangt hopIRQVector in tamago's vectortabel (RamStart:
// +0x80 = IRQ vanuit EL0/SP_EL0, +0x280 = IRQ op huidige EL met SP_ELx), in
// tamago's eigen vorm: ldr x18, #8; br x18; .quad handler.
func installIRQVector() {
	fn := hopIRQVector
	pc := **(**uint64)(unsafe.Pointer(&fn))
	for _, off := range []uintptr{0x80, 0x280} {
		p := uintptr(ramStart) + off
		*(*uint32)(unsafe.Pointer(p)) = 0x58000052     // ldr x18, #8
		*(*uint32)(unsafe.Pointer(p + 4)) = 0xd61f0240 // br x18
		*(*uint64)(unsafe.Pointer(p + 8)) = pc
		syncVector(p)
	}
}

// ServeIRQFlag is de irq.Use-service voor dit contract: vector installeren,
// I openen, wachten op de deur, de dispatcher draaien, I weer openen.
func ServeIRQFlag(isr func()) {
	c := make(chan struct{}, 1)
	wake := func() {
		select {
		case c <- struct{}{}:
		default:
		}
	}
	irqWake.Store(&wake)
	installIRQVector()
	for {
		irqEnable()
		<-c
		isr()
	}
}
