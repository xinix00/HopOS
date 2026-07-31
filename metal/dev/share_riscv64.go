//go:build riscv64 && !linkcpuinit

package dev

// De HOP-kant (geen linkcpuinit = dit is geen app-image): hier is Push/Pull écht
// werk. HOP draait zonder translatie, dus zonder map die de ABI-regio's
// ongecachet maakt — en DRAM is op de C906 altijd cachebaar. HOP en het app-hart
// zijn niet coherent: een READY die in de D-cache van de ander blijft staan
// bestaat voor deze kant niet (gemeten 30-07).
//
// De app-kant heeft dit níet nodig en doet het ook niet: zijn gedeelde regio's
// zijn device gemapt door de kooi (kern/slots slotMap), net als op ARM. Zie
// share_riscv64_app.go.
//
// Push: mijn schrijfacties de cache uit, zodat de andere kant ze in DRAM vindt.
// Pull: mijn (mogelijk verouderde) regels weg, zodat ik hún schrijfacties zie.
// Beide zijn clean+invalidate — dat dekt de twee richtingen met één op, en de
// regels zijn hier klein (kop van een ring, één 64-bit veld).
func Push(addr, size uintptr) {
	MB()
	CleanInv(addr, size)
}

func Pull(addr, size uintptr) {
	CleanInv(addr, size)
	MB()
}
