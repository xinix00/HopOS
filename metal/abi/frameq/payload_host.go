//go:build !arm64 && !riscv64

package frameq

func PublishPayload(virtual, physical, size uintptr) {}
func AcquirePayload(virtual, physical, size uintptr) {}
