//go:build riscv64

package frameq

import "github.com/xinix00/HopOS/metal/v2/dev"

// T-Head's cache operation is by physical address. HOP supplies the partition
// base once; the descriptor itself still carries only a validated offset.
func PublishPayload(virtual, physical, size uintptr) { dev.CleanInv(physical, size) }
func AcquirePayload(virtual, physical, size uintptr) { dev.CleanInv(physical, size) }
