//go:build arm64

package frameq

import "github.com/xinix00/HopOS/metal/v2/dev"

func PublishPayload(virtual, physical, size uintptr) { dev.CleanInv(virtual, size) }
func AcquirePayload(virtual, physical, size uintptr) { dev.CleanInv(virtual, size) }
