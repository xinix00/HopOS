package gicv3

import (
	"fmt"
	"github.com/xinix00/HopOS/metal/v2/dev"
)

const (
	firstSPI     = 32
	firstSpecial = 1020
	affMask      = uint64(0xFF)<<32 | 0xFFFFFF
)

func lineReg(gicd, gicr uintptr, id int) (uintptr, uintptr, uint32) {
	if id < firstSPI {
		return gicr + 0x10000, 0, 1 << uint(id)
	}
	return gicd, uintptr(id / 32), 1 << uint(id%32)
}

// GICD_IROUTER[32] starts at 0x6100: the INTID-indexed base is 0x6000.
// Route and group are published before enabling the interrupt.
func enableLine(gicd, gicr uintptr, id int, mpidr uint64) error {
	if id < 0 || id >= firstSpecial {
		return fmt.Errorf("gicv3: invalid interrupt %d", id)
	}
	base, n, bit := lineReg(gicd, gicr, id)
	if id >= firstSPI {
		dev.Write64(gicd+0x6000+8*uintptr(id), mpidr&affMask)
	}
	dev.Write32(base+0x80+4*n, dev.Read32(base+0x80+4*n)&^bit)
	dev.MB()
	dev.Write32(base+0x100+4*n, bit)
	dev.MB()
	return nil
}
