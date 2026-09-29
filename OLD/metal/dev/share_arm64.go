//go:build tamago && arm64

package dev

// Push/Pull op ARM64: no-ops zolang de ABI-regio's device- of NC-gemapt zijn
// (per definitie coherent, niets te vegen), en echt cache-onderhoud zodra een
// venster met memattr.NormalWB gecached is (MarkCached). Waarom dat laatste
// nodig is terwijl de cores onderling coherent zijn: de EL2-switcher op de
// app-core draait met de MMU uit (cpu/el2/switch.s) en leest de ringkop en de
// deurbel dus rechtstreeks uit DRAM. Een kop die nog vuil in HOP's of de app's
// cache staat, ziet hij niet. Push = MB + clean+invalidate na een schrijf,
// Pull = clean+invalidate + MB vóór een lees — dezelfde twee stappen als op
// riscv64 (share_riscv64.go), en om dezelfde reden: er is een lezer zonder
// cache. CIVAC en niet IVAC bij Pull: by-VA-onderhoud broadcast over de
// cores, en een kale invalidate zou een vuile regel van de ándere kant
// weggooien. Kop en staart van een ring liggen sinds ABI 3 elk in een eigen
// cacheline, dus het onderhoud van de één raakt de ander niet.
//
// Wie dit betaalt: NIET de ringdata. HOP en app zijn cache-coherent, dus een
// ring in een gecached venster slaat Push/Pull over (ring.coherent) en cleant
// alleen zijn kop (de switcher peekt hem). Wat hier langskomt zijn de
// control-page-woorden (ctrlRead/ctrlWrite, de deurbel, de heartbeat): acht
// bytes per keer, een paar duizend keer per seconde — en het fault-rapport dat
// de switcher zonder cache schreef, dat HOP zonder invalidate nooit zou zien.
func Push(addr, size uintptr) {
	if IsCached(addr, int(size)) {
		Clean(addr, size)
	}
}

func Pull(addr, size uintptr) {
	if IsCached(addr, int(size)) {
		CleanInv(addr, size)
	}
}
