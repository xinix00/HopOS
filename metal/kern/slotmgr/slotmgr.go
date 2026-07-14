// Package slotmgr adapteert HopOS' slot-primitieven (metal/kern/slots) naar het
// SlotManager-contract dat HOP definieert (hop/pkg/hopos) en waar HOP's
// HopRunner op draait. De compile-time assertie onderaan bewijst dat de
// bare-metal kant het contract exact vervult — drift wordt zo een buildfout,
// niet een runtime-verrassing op het board.
//
// Alleen voor GOOS=tamago (het importeert metal/kern/slots → MMIO/PSCI).

//go:build tamago

package slotmgr

import (
	"time"

	"hop/pkg/hopos"

	"hop-os/metal/kern/slots"
)

// Manager implementeert hopos.SlotManager tegen metal/kern/slots.
type Manager struct{}

func New() *Manager { return &Manager{} }

func (Manager) NumSlots() int             { return slots.NumSlots() }
func (Manager) CoreClass(slot int) string { return slots.CoreClass(slot) }

func (Manager) StartLoader(slot int, memLimit uint64, env map[string]string) error {
	return slots.StartLoader(slot, memLimit, env)
}

func (Manager) StartStaged(slot int, memLimit uint64, cores int, env map[string]string, mounts map[string]string, ports map[string]int) error {
	return slots.StartStaged(slot, memLimit, cores, env, mounts, ports)
}

func (Manager) Stop(slot int, timeout time.Duration) error {
	return slots.Stop(slot, timeout)
}

func (Manager) Status(slot int) hopos.SlotStatus {
	s := slots.Get(slot)
	return hopos.SlotStatus{
		CoreOn:    s.CoreOn,
		App:       s.App,
		ExitCode:  s.ExitCode,
		Heartbeat: s.Heartbeat,
		RAMSize:   s.RAMSize,
	}
}

func (Manager) Logs(slot int) <-chan string { return slots.Logs(slot) }

// Contractbewijs: Manager MOET hopos.SlotManager zijn.
var _ hopos.SlotManager = (*Manager)(nil)
