package hop

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/driver/scmi"
)

const (
	pdVPUTop   = 11
	pdVPUCore0 = 12
	pdVPUCores = 4
	// ACPI VPU0.PPRS._ON sets these bits at RCSU+0x21c. SCMI's
	// logical PowerState alone does not prove that these gates are open.
	vpuPowerGates = 0x1ffc
)

func vpuNeedsRecovery(pgctrl uint32, terminate [4]uint32) bool {
	if pgctrl&vpuPowerGates != vpuPowerGates {
		return true
	}
	for _, v := range terminate {
		if v != 0 {
			return true
		}
	}
	return false
}

// cycleVPUDomains is used before registering the codec engine, when this
// kernel owns no sessions. The shared MM hub domains 4/5 are never cycled.
// TF-A performs the board's SRAM/power sequencing, as on first power-on.
func cycleVPUDomains(power func(domain, state uint32) error) error {
	for domain := uint32(pdVPUCore0 + pdVPUCores - 1); domain >= pdVPUTop; domain-- {
		if err := power(domain, scmi.PowerOff); err != nil {
			return fmt.Errorf("vpu: power off domain %d: %w", domain, err)
		}
	}
	for domain := uint32(pdVPUTop); domain < pdVPUCore0+pdVPUCores; domain++ {
		if err := power(domain, scmi.PowerOn); err != nil {
			return fmt.Errorf("vpu: restore domain %d: %w", domain, err)
		}
	}
	return nil
}
