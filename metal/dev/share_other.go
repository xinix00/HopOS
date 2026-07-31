//go:build !riscv64

package dev

// Push/Pull zijn no-ops op deze architecturen: de ABI-regio's tussen HOP en een
// app (control page, ringen) vallen buiten élke RAM-declaratie en worden door
// alle MMU's als device gemapt. Daarmee zijn ze per definitie coherent — er is
// niets om weg te schrijven of te verversen. De aanroepers hoeven dus niet te
// weten op welk silicium ze staan; zie share_riscv64.go voor de kant waar het
// wél werk is.
func Push(addr, size uintptr) {}
func Pull(addr, size uintptr) {}
