//go:build qemuvirt || rpi4 || rpi5 || rk3566

package main

// Gedeelde slot-helpers van álle embed-mains (virt/pi4/pi5) — stonden
// byte-identiek in virt_main.go én raspi_main.go, hier één keer.

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/kern/slots"
)

// drainLogs abonneert op het logkanaal van de actieve servicer van een slot
// en multiplext de regels geprefixt naar de console — wat HOP's
// LogBroadcaster (GetStdout) doet. Per Start opnieuw aanroepen: elke start
// krijgt een verse servicer (en dus een vers kanaal). count (optioneel) telt
// de regels voor de acceptatie-asserts.
func drainLogs(slot int, count *int) {
	for line := range slots.Logs(slot) {
		fmt.Printf("[slot%d] %s\n", slot, line)
		if count != nil {
			*count++
		}
	}
}

// waitExit wacht (begrensd) tot een slot exit meldt en geeft de exitcode.
func waitExit(slot int, timeout time.Duration) (uint64, error) {
	deadline := time.Now().Add(timeout)
	for slots.Get(slot).App != layout.StatusExited {
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("slot %d meldt geen exit", slot)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return slots.Get(slot).ExitCode, nil
}

// De must*-helpers dragen de vaste ceremonie rond élke demo-stap (start +
// log-drain, ready-wacht, stop, exit-assert) — die stond tientallen keren
// uitgeschreven in de mains. Twee dingen verschillen per main en worden dus
// vooraf gezet: failf (virt faalt met zijn eigen marker, de Pi-acceptatie
// met een board-prefix) en demoApp (elk image draagt zijn eigen app-blob).
var (
	failf   func(what string, err error)
	demoApp []byte
)

// mustStart start een slot met de demo-app en hangt er de log-drain aan.
// count (optioneel) telt regels voor de acceptatie-asserts.
func mustStart(what string, slot int, mem uint64, cores int, env map[string]string,
	mounts map[string]string, ports map[string]int, count *int) {
	if err := slots.Start(slot, demoApp, mem, cores, env, mounts, ports, ""); err != nil {
		failf(what, err)
	}
	go drainLogs(slot, count)
}

// mustStartShared is mustStart voor een mede-bewoner: kooi cage op de
// (gedeelde) core, met de kooi in de foutmelding — bij zwermen wil je weten wíé.
func mustStartShared(what string, core, cage int, mem uint64, env map[string]string) {
	if err := slots.StartShared(core, cage, demoApp, mem, env, nil, nil, ""); err != nil {
		failf(what, fmt.Errorf("kooi %d: %w", cage, err))
	}
	go drainLogs(cage, nil)
}

// mustReady wacht tot een slot READY meldt.
func mustReady(what string, slot int, timeout time.Duration) {
	if err := slots.WaitReady(slot, timeout); err != nil {
		failf(what, err)
	}
}

// mustStop stopt een slot (kill-flag → escalatie) en faalt op een timeout.
func mustStop(what string, slot int, timeout time.Duration) {
	if err := slots.Stop(slot, timeout); err != nil {
		failf(what, err)
	}
}

// mustExit wacht op een nette exit met precies deze code.
func mustExit(what string, slot int, timeout time.Duration, want uint64) {
	code, err := waitExit(slot, timeout)
	if err != nil || code != want {
		failf(what, fmt.Errorf("exit=%d (wil %d), err=%v", code, want, err))
	}
}

// stopIfOn ruimt bij een sectie-overgang de slots op die nog draaien.
func stopIfOn(what string, slotNums ...int) {
	for _, slot := range slotNums {
		if slots.Get(slot).CoreOn {
			mustStop(what, slot, 3*time.Second)
		}
	}
}

// mustFault start de app met env en wacht tot de EL2-vector de core velt;
// een nette exit of een doordraaiende app is een fail. Geeft de eindstatus
// terug voor de vec/ESR/FAR-asserts van de aanroeper.
func mustFault(what string, slot int, mem uint64, env map[string]string) slots.Status {
	mustStart(what, slot, mem, 1, env, nil, nil, nil)
	mustReady(what, slot, 5*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for slots.Get(slot).CoreOn {
		if time.Now().After(deadline) {
			failf(what, fmt.Errorf("app draait door zonder fault (env %v)", env))
		}
		time.Sleep(10 * time.Millisecond)
	}
	s := slots.Get(slot)
	if s.App == layout.StatusExited {
		failf(what, fmt.Errorf("app exitte netjes (%d) — fault verwacht", s.ExitCode))
	}
	return s
}
