//go:build !tamago

package dev

// Host-kant (unit-tests op de ontwikkelmachine): de Read/Write/Copy-helpers
// werken daar gewoon op heap-geheugen, maar er zijn geen andere cores, geen
// device-geheugen en geen caches om te onderhouden — de barrières en het
// cache-onderhoud zijn no-ops. Tests bewijzen dus het protocol (indexen,
// records, tabellen), níét de barrière-plaatsing; dat bewijs blijft het board.

func MB() {}

func SEV() {}

func CleanInv(addr, size uintptr) {}
