//go:build tamago

package dev

// MB is een volledige geheugenbarrière (DMB SY) — publiceer data vóór de
// index-update, en andersom bij het lezen. Zie dev_<arch>.s
// (arm64: DMB SY, riscv64: FENCE).
func MB()

// SEV genereert een event (WFE-wakeup) voor álle cores in het domein — HOP
// gebruikt het om een geparkeerde app-core te dispatchen (mailbox schrijven,
// dan SEV). Bevat een DSB SY zodat de mailbox-write zichtbaar is vóór de wake.
// Zie dev_<arch>.s. Op RISC-V bestaat SEV niet: daar is dit alleen de
// barrière, en het wekken loopt via het reset-blok/IPI van het board.
func SEV()

// CleanInv veegt [addr, addr+size) uit alle caches: DC CIVAC per regel
// (broadcast inner-shareable, alle cores/levels) + DSB. Nodig overal waar HOP
// ongecached schrijft aan geheugen dat een app-core cacheable raakt(e):
//
//   - vóór het laden van een image: de vórige app draaide cacheable op deze
//     fysieke regels — een achtergebleven dirty line zou bij evictie de verse
//     bytes overschrijven. Dus eerst vegen, dan schrijven.
//   - ná het schrijven van stage-2-tabellen/vectoren: de page-table-walker
//     leest ze cacheable (VTCR IRGN/ORGN=WB) — stale (clean) lines zouden hem
//     een oude tabel laten walken. Dus schrijven, dan vegen.
//
// QEMU/TCG modelleert geen caches (daar is dit een no-op); het bewijs is het
// board. Zie dev_<arch>.s.
func CleanInv(addr, size uintptr)

// Counter is de rauwe stand van de vrijlopende teller van de architectuur
// (arm64: CNTVCT_EL0, riscv64: de TIME-CSR) — dezelfde teller waarin apps
// hun wektijd uitdrukken (CtrlWakeAt, CtxWake), zodat HOP's wekker ermee kan
// vergelijken. Zie dev_<arch>.s. Geen frequentie hier: die is board-kennis.
func Counter() uint64
