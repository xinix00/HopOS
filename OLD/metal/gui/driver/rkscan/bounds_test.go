package rkscan

import "testing"

func TestInvalidScanoutRejectedBeforeRegisters(t *testing.T) {
	for _, v := range []struct {
		base         uintptr
		w, h, stride int
	}{{1, 0, 1080, 7680}, {1, 1920, 1080, 4}, {1 << 32, 1920, 1080, 7680}, {0xffff0000, 1920, 1080, 7680}} {
		if VOPScanout(v.base, v.w, v.h, v.stride) == nil {
			t.Fatalf("invalid scanout accepted: %+v", v)
		}
	}
}
