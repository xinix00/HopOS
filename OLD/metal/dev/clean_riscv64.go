//go:build tamago && riscv64

package dev

// Clean is op riscv64 gewoon CleanInv: de T-Head-cacheops kennen geen aparte
// clean-zonder-invalidate die hier iets zou winnen, en de aanroeper
// (share_arm64.go) bestaat hier niet.
func Clean(addr, size uintptr) { CleanInv(addr, size) }
