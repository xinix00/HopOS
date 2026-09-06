package layout

import "testing"

func expectPlanPanic(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Error("invalid geometry accepted")
		}
	}()
	f()
}

func testPlan() Plan {
	return Plan{NodeCtrlPA: 0xc0000000, CagePA: 0xc2000000,
		TrapVecPA: 0xc2000800, BootScratchPA: 0xb0000000,
		Pool: []Region{{Base: 0x50000000, Size: 0x60000000}, {Base: 0xb1000000, Size: 0xf000000}}}
}

func TestUsePlanPoolCannotOverlapAdminOrItself(t *testing.T) {
	old := plan
	t.Cleanup(func() { plan = old })
	for _, tc := range []struct {
		name   string
		change func(*Plan)
	}{
		{"pool overlap", func(p *Plan) { p.Pool = append(p.Pool, p.Pool[0]) }},
		{"overflow", func(p *Plan) { p.Pool = []Region{{Base: ^uint64((2 << 20) - 1), Size: 2 << 20}} }},
		{"alignment", func(p *Plan) { p.Pool[0].Base++ }},
		{"empty region", func(p *Plan) { p.Pool[0].Size = 0 }},
		{"control", func(p *Plan) { p.NodeCtrlPA = 0x50000000 }},
		{"cage", func(p *Plan) { p.CagePA = 0x50000000 - 0x800 }},
		{"scratch", func(p *Plan) { p.BootScratchPA = 0x50000000 - 0x80 }},
		{"trap", func(p *Plan) { p.TrapVecPA = 0x50000000 }},
		{"usb tail", func(p *Plan) { p.USBDMAPA = 0x50000000 - 0x1000 }},
		{"black box", func(p *Plan) { p.BlackBoxPA, p.BlackBoxSize = 0x50000000-8, 16 }},
		{"admin overflow", func(p *Plan) { p.CagePA = ^uint64(0x7ff) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPlan()
			tc.change(&p)
			expectPlanPanic(t, func() { UsePlan(p) })
		})
	}
}

func TestUsePlanPreservesOrderedBanksAndNestedAdmin(t *testing.T) {
	old := plan
	t.Cleanup(func() { plan = old })
	p := testPlan()
	p.Pool[0], p.Pool[1] = p.Pool[1], p.Pool[0]
	UsePlan(p) // trap lives inside cage block 0 by design
	if Pool()[0] != p.Pool[0] {
		t.Fatal("plan changed the canonical first bank")
	}
}

func TestCarvePoolCoalescesDuplicateAndAdjacentBanks(t *testing.T) {
	banks := []Region{{Base: 4 << 20, Size: 4 << 20}, {Base: 2 << 20, Size: 4 << 20}, {Base: 8 << 20, Size: 2 << 20}}
	pool := CarvePool(banks, []Region{{Base: 5 << 20, Size: 0}}, 2<<20)
	if len(pool) != 1 || pool[0] != (Region{Base: 2 << 20, Size: 8 << 20}) {
		t.Fatalf("pool=%v", pool)
	}
	if banks[0].Base != 4<<20 {
		t.Fatal("modified firmware banks")
	}
}

func TestCarvePoolRejectsOverflowRatherThanFallback(t *testing.T) {
	bad := Region{Base: ^uint64(7), Size: 16}
	expectPlanPanic(t, func() { CarvePool([]Region{bad}, nil, 2<<20) })
	expectPlanPanic(t, func() { CarvePool([]Region{{Base: 0, Size: 8 << 20}}, []Region{bad}, 2<<20) })
	if out := CarvePool([]Region{{Base: ^uint64(7), Size: 7}}, nil, 0); len(out) != 0 {
		t.Fatalf("alignment wrapped into low memory: %v", out)
	}
}

func TestStageAddrRejectsWindowOverflow(t *testing.T) {
	if _, _, ok := StageAddr(^uint64(7), 32, 8); ok {
		t.Fatal("staging window overflow accepted")
	}
}

// Geometrie van board/rk3566/plan.go: de boardbron bewaakt dezelfde grenzen
// bovendien met compile-time assertions, zodat adreswijzigingen niet driften.
func TestRK3566ReservedCapacity(t *testing.T) {
	old, oldMax := plan, MaxSlots
	t.Cleanup(func() { plan, MaxSlots = old, oldMax })
	const ctrl, cage, trap = 0x06200000, 0x06220000, 0x062f0000
	const capacity = (trap-cage)/CageStride - 1
	if capacity != 12 || ctrl+(capacity+1)*CtrlStride > cage || cage+(capacity+1)*CageStride > trap {
		t.Fatal("RK3566 capacity exceeds its reserved administration")
	}
	SetMaxSlots(capacity)
	UsePlan(Plan{NodeCtrlPA: ctrl, CagePA: cage, TrapVecPA: trap,
		BootScratchPA: 0x7f000, NetDMAPA: 0x06400000, USBDMAPA: 0x06c00000,
		Pool: []Region{{Base: 0x07800000, Size: 0x20000000}}})
}
