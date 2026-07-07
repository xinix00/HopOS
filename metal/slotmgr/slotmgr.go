// Package slotmgr adapteert HopOS' slot-primitieven (metal/slots) naar het
// SlotManager-contract dat HOP definieert (hop/pkg/hopos) en waar HOP's
// HopRunner op draait. De compile-time assertie onderaan bewijst dat de
// bare-metal kant het contract exact vervult — drift wordt zo een buildfout,
// niet een runtime-verrassing op het board.
//
// Alleen voor GOOS=tamago (het importeert metal/slots → MMIO/PSCI).

//go:build tamago

package slotmgr

import (
	"time"

	"hop/pkg/hopos"

	"hop-os/metal/slots"
)

// Manager implementeert hopos.SlotManager tegen metal/slots.
type Manager struct{}

func New() *Manager { return &Manager{} }

func (Manager) NumSlots() int             { return slots.NumSlots() }
func (Manager) CoreClass(slot int) string { return slots.CoreClass(slot) }

func (Manager) Start(slot int, image []byte, memLimit uint64, env map[string]string, mounts map[string]string, ports map[string]int) error {
	return slots.Start(slot, image, memLimit, env, mounts, ports)
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
